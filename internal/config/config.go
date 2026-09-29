// Package config 网关配置的加载、存储与模型路由解析。
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ProtocolOfficial 官方透传协议标识：凭证由客户端透传，模型清单从官方拉取
const ProtocolOfficial = "openai-official"

// Listen 网关监听地址
type Listen struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Upstream 一个上游模型服务
type Upstream struct {
	Name           string   `json:"name"`            // 唯一标识，如 "claude" / "deepseek"
	Protocol       string   `json:"protocol"`        // "openai" 或 "anthropic"
	BaseURL        string   `json:"base_url"`        // 例如 https://api.deepseek.com/v1
	APIKey         string   `json:"api_key"`         // 上游 API Key
	Models         []string `json:"models"`          // 该上游提供的模型列表
	TimeoutSeconds int      `json:"timeout_seconds"` // 单次上游请求超时（0=不限制）
	MaxRetries     int      `json:"max_retries"`     // 对 429/5xx/网络错误的重试次数（0=不重试）
}

// Proxy 统一出站代理：网关访问所有上游时都走它
//
// 典型场景：上游在境外（如 chatgpt.com），需要经由本机 Clash 等代理出站。
type Proxy struct {
	URL     string `json:"url"`      // 如 http://127.0.0.1:7897；空=直连不代理
	NoProxy string `json:"no_proxy"` // 逗号分隔的直连主机，如 "localhost,127.0.0.1"
}

// Config 网关完整配置
type Config struct {
	Listen    Listen            `json:"listen"`
	Upstreams []Upstream        `json:"upstreams"`
	Routes    map[string]string `json:"routes"`    // model 名 -> upstream[:target_model]（缺省时按 Models 自动匹配）
	Failover  bool              `json:"failover"`  // 模型多上游时失败是否自动切换
	AdminKey  string            `json:"admin_key"` // 管理 API Bearer Token（空=不鉴权）
	Proxy     Proxy             `json:"proxy"`     // 统一出站代理
}

// Default 生成一份可运行的默认配置
func Default() *Config {
	return &Config{
		Listen: Listen{Host: "127.0.0.1", Port: 8318},
		Upstreams: []Upstream{
			{
				Name:     "openai-official",
				Protocol: ProtocolOfficial, // 官方透传：gpt-*/codex-*/o1-o4 保留订阅凭证直连 OpenAI
				BaseURL:  "https://chatgpt.com/backend-api/codex",
				APIKey:   "",                                      // 无需 key，透传客户端原始凭证
				Models:   []string{"gpt-5.6-luna", "gpt-5.6-sol"}, // 用于 /v1/models 展示；实际匹配靠模型名前缀
			},
			{
				Name:     "deepseek",
				Protocol: "openai",
				BaseURL:  "https://api.deepseek.com/v1",
				APIKey:   "sk-请替换为你的key",
				Models:   []string{"deepseek-chat", "deepseek-reasoner"},
			},
			{
				Name:     "claude",
				Protocol: "anthropic",
				BaseURL:  "https://api.anthropic.com",
				APIKey:   "sk-ant-请替换为你的key",
				Models:   []string{"claude-sonnet-4-20250514", "claude-opus-4-20250514"},
			},
		},
		Routes: map[string]string{},
	}
}

// Store 配置的线程安全读写封装
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  *Config
}

// Load 从磁盘加载配置；不存在则用默认配置并落盘
func Load(path string) (*Store, error) {
	store := &Store{path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := Default()
			if err := writeConfig(path, cfg); err != nil {
				return nil, fmt.Errorf("写入默认配置失败: %w", err)
			}
			store.cfg = cfg
			return store, nil
		}
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("配置 JSON 解析失败: %w", err)
	}
	normalize(&cfg)
	store.cfg = &cfg
	return store, nil
}

// normalize 补齐缺省字段
func normalize(cfg *Config) {
	if cfg.Listen.Host == "" {
		cfg.Listen.Host = "127.0.0.1"
	}
	if cfg.Listen.Port == 0 {
		cfg.Listen.Port = 8318
	}
	if cfg.Routes == nil {
		cfg.Routes = map[string]string{}
	}
	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].TimeoutSeconds < 0 {
			cfg.Upstreams[i].TimeoutSeconds = 0
		}
		if cfg.Upstreams[i].MaxRetries < 0 {
			cfg.Upstreams[i].MaxRetries = 0
		}
	}
}

// Get 返回配置快照（深拷贝，避免外部改动）
func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := *s.cfg
	cp.Upstreams = make([]Upstream, len(s.cfg.Upstreams))
	copy(cp.Upstreams, s.cfg.Upstreams)
	cp.Routes = make(map[string]string, len(s.cfg.Routes))
	for k, v := range s.cfg.Routes {
		cp.Routes[k] = v
	}
	return &cp
}

// Save 保存配置到内存并原子落盘。
//
// 内置上游（官方透传通道）不受用户提交内容影响：字段一律以现有配置为准，
// 且不允许被删除。它无 API Key、模型清单来自官方实时拉取，界面上是只读的。
func (s *Store) Save(cfg *Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalize(cfg)

	current := s.cfg
	if current != nil {
		cfg.Upstreams = mergeBuiltinUpstreams(cfg.Upstreams, current.Upstreams)
	}

	if err := writeConfig(s.path, cfg); err != nil {
		return err
	}
	s.cfg = cfg
	return nil
}

// mergeBuiltinUpstreams 用现有配置覆盖同名内置上游，并补回被删掉的内置上游
func mergeBuiltinUpstreams(submitted, current []Upstream) []Upstream {
	builtins := make([]Upstream, 0, 1)
	for _, up := range current {
		if IsBuiltinUpstream(up) {
			builtins = append(builtins, up)
		}
	}
	if len(builtins) == 0 {
		return submitted
	}

	nameOf := map[string]Upstream{}
	for _, up := range builtins {
		nameOf[up.Name] = up
	}

	out := make([]Upstream, 0, len(submitted)+len(builtins))
	seen := map[string]bool{}
	for i := range submitted {
		up := submitted[i]
		if existing, ok := nameOf[up.Name]; ok && IsBuiltinUpstream(existing) {
			seen[up.Name] = true
			out = append(out, existing) // 忽略用户对内置上游的任何改动
			continue
		}
		if seen[up.Name] {
			continue
		}
		out = append(out, up)
	}
	// 补回被删除的内置上游
	for _, up := range builtins {
		if !seen[up.Name] {
			out = append(out, up)
		}
	}
	return out
}

// writeConfig 原子写入：先写临时文件再 rename，避免写坏
func writeConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("配置序列化失败: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写临时配置失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换配置失败: %w", err)
	}
	return nil
}

// Target 路由解析结果：上游 + 可改写后的目标模型名
type Target struct {
	Upstream    Upstream
	TargetModel string // 发给上游时使用的模型名；若无需改写则与原模型名相同
}

// parseRouteValue 解析 "upstream" 或 "upstream:target_model"
func parseRouteValue(v string) (upstreamName, targetModel string) {
	if idx := strings.Index(v, ":"); idx >= 0 {
		return v[:idx], v[idx+1:]
	}
	return v, ""
}

// ProxyFor 返回该请求应使用的代理地址；nil 表示直连。
//
// 代理 URL 无效时返回错误而非静默直连 —— 否则上游访问失败会被误判为网络问题，
// 真正的原因（配置写错）反而被掩盖。
func (p Proxy) ProxyFor(host string) (*url.URL, error) {
	if p.URL == "" || host == "" {
		return nil, nil
	}
	for _, raw := range strings.Split(p.NoProxy, ",") {
		entry := strings.TrimSpace(strings.ToLower(raw))
		if entry == "" {
			continue
		}
		if entry == "*" {
			return nil, nil
		}
		entry = strings.TrimPrefix(entry, ".")
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return nil, nil
		}
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return nil, fmt.Errorf("代理地址无效 %q: %w", p.URL, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("代理地址缺少主机 %q", p.URL)
	}
	return u, nil
}

// IsBuiltinUpstream 判断是否为系统内置上游。
//
// 内置上游（目前只有官方透传通道）由程序自己维护，不依赖用户配置：
// 无 API Key、模型清单从官方实时拉取、在界面上只读不可编辑/删除。
func IsBuiltinUpstream(up Upstream) bool {
	return up.Protocol == ProtocolOfficial
}

// IsOfficialModel 判断是否为 Codex 官方模型（透传到 OpenAI 官方，保留订阅凭证）
func IsOfficialModel(model string) bool {
	if strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "codex-") {
		return true
	}
	// o1 / o3 / o4 及 o1-xxx / o3-xxx / o4-xxx
	if len(model) >= 2 && model[0] == 'o' {
		switch model[1] {
		case '1', '3', '4':
			return len(model) == 2 || model[2] == '-'
		}
	}
	return false
}

// ResolveUpstreams 根据模型名返回候选上游列表（含目标模型名）
// 优先级：官方模型透传 > Routes 显式映射 > Models 自动匹配 > 唯一上游兜底
func (s *Store) ResolveUpstreams(model string) []Target {
	cfg := s.Get()
	var out []Target

	// 0. 官方模型：优先路由到 openai-official 上游（保留订阅凭证透传）
	if IsOfficialModel(model) {
		for i := range cfg.Upstreams {
			if cfg.Upstreams[i].Protocol == ProtocolOfficial {
				return []Target{{Upstream: cfg.Upstreams[i], TargetModel: model}}
			}
		}
	}

	// 1. Routes 显式映射
	if raw, ok := cfg.Routes[model]; ok {
		upName, target := parseRouteValue(raw)
		for i := range cfg.Upstreams {
			if cfg.Upstreams[i].Name == upName {
				tm := target
				if tm == "" {
					tm = model
				}
				out = append(out, Target{Upstream: cfg.Upstreams[i], TargetModel: tm})
				return out
			}
		}
	}

	// 2. Models 列表匹配
	for i := range cfg.Upstreams {
		for _, m := range cfg.Upstreams[i].Models {
			if m == model {
				out = append(out, Target{Upstream: cfg.Upstreams[i], TargetModel: model})
				break
			}
		}
	}
	if len(out) > 0 {
		return out
	}

	// 3. 唯一上游兜底
	if len(cfg.Upstreams) == 1 {
		return []Target{{Upstream: cfg.Upstreams[0], TargetModel: model}}
	}
	return nil
}

// ResolveUpstream 只返回第一个候选（兼容旧入口）
func (s *Store) ResolveUpstream(model string) *Upstream {
	cands := s.ResolveUpstreams(model)
	if len(cands) == 0 {
		return nil
	}
	return &cands[0].Upstream
}

// SetForTest 直接注入内存配置（不落盘），仅供测试使用。
func (s *Store) SetForTest(cfg *Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalize(cfg)
	s.cfg = cfg
}
