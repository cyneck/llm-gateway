// Package config 网关配置的加载、存储与模型路由解析。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Listen 网关监听地址
type Listen struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Upstream 一个上游模型服务
type Upstream struct {
	Name     string   `json:"name"`     // 唯一标识，如 "claude" / "deepseek"
	Protocol string   `json:"protocol"` // "openai" 或 "anthropic"
	BaseURL  string   `json:"base_url"` // 例如 https://api.deepseek.com/v1
	APIKey   string   `json:"api_key"`  // 上游 API Key
	Models   []string `json:"models"`   // 该上游提供的模型列表
}

// Config 网关完整配置
type Config struct {
	Listen    Listen            `json:"listen"`
	Upstreams []Upstream        `json:"upstreams"`
	Routes    map[string]string `json:"routes"` // model 名 -> upstream name（可选，缺省时按 Models 自动匹配）
}

// Default 生成一份可运行的默认配置
func Default() *Config {
	return &Config{
		Listen: Listen{Host: "127.0.0.1", Port: 8318},
		Upstreams: []Upstream{
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

// Save 保存配置到内存并原子落盘
func (s *Store) Save(cfg *Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalize(cfg)
	if err := writeConfig(s.path, cfg); err != nil {
		return err
	}
	s.cfg = cfg
	return nil
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

// ResolveUpstream 根据模型名找到目标上游
// 优先级：Routes 显式映射 > Upstreams 的 Models 列表 > 唯一上游兜底
func (s *Store) ResolveUpstream(model string) *Upstream {
	cfg := s.Get()
	if name, ok := cfg.Routes[model]; ok {
		for i := range cfg.Upstreams {
			if cfg.Upstreams[i].Name == name {
				return &cfg.Upstreams[i]
			}
		}
	}
	for i := range cfg.Upstreams {
		for _, m := range cfg.Upstreams[i].Models {
			if m == model {
				return &cfg.Upstreams[i]
			}
		}
	}
	if len(cfg.Upstreams) == 1 {
		return &cfg.Upstreams[0]
	}
	return nil
}

// SetForTest 直接注入内存配置（不落盘），仅供测试使用。
func (s *Store) SetForTest(cfg *Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalize(cfg)
	s.cfg = cfg
}
