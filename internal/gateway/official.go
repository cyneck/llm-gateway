package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 官方透传上游的模型清单来源：
//  1. 优先复用客户端（Codex）请求里带来的订阅凭证 —— 无需在网关侧配置任何密钥
//  2. 其次读取本机 Codex 登录态 ~/.codex/auth.json（供 H5 界面手动刷新时使用）
//  3. 都拿不到就返回空，由调用方降级到配置里的静态列表
//
// 端点为依据 Codex 二进制中的 remote_models 实现推断的 <base_url>/models，
// 例如 https://chatgpt.com/backend-api/codex/models。

// officialCacheTTL 官方模型清单缓存时长（官方清单变更不频繁，避免每次请求都打上游）
const officialCacheTTL = 5 * time.Minute

// officialClientVersion 官方接口要求的客户端版本参数（缺失会返回 HTTP 400）。
// 实测任意合法版本号均可，与 Codex CLI 版本保持一致即可。
const officialClientVersion = "0.158.0"

// officialModelsURL 官方模型清单端点（含必需的 client_version 参数）
func officialModelsURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/models?client_version=" +
		url.QueryEscape(officialClientVersion)
}

type officialCacheEntry struct {
	models    []string
	expiresAt time.Time
}

var (
	officialMu    sync.Mutex
	officialCache = map[string]officialCacheEntry{}
)

// officialCacheMaxAge 本机 Codex 缓存的可接受新鲜度。
// 超过这个时长会先尝试联网刷新，失败再退回缓存（缓存永远不会被丢弃）。
const officialCacheMaxAge = 24 * time.Hour

// localOfficialModelsCache 读取 Codex 客户端自己拉取并落盘的官方模型清单。
//
// Codex 会周期性向官方同步并把完整清单写进 ~/.codex/models_cache.json。
// 这是最优来源：完全离线、不需要任何凭证，而且与客户端实际可见的列表一致。
// 返回空切片表示不可用（文件不存在或格式无法识别）。
func localOfficialModelsCache() ([]string, time.Time) {
	dir := codexHome()
	if dir == "" {
		return nil, time.Time{}
	}
	data, err := os.ReadFile(filepath.Join(dir, "models_cache.json"))
	if err != nil {
		return nil, time.Time{}
	}
	var parsed struct {
		FetchedAt string          `json:"fetched_at"`
		Models    json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, time.Time{}
	}
	models := parseModelList(parsed.Models)
	if len(models) == 0 {
		return nil, time.Time{}
	}
	var at time.Time
	if parsed.FetchedAt != "" {
		if t, err := time.Parse(time.RFC3339, parsed.FetchedAt); err == nil {
			at = t
		}
	}
	return models, at
}

// fetchOfficialModels 获取官方模型清单，按可靠性排序取用：
//
//  1. 本机 Codex 缓存 models_cache.json —— 离线、免凭证、与客户端实际可见一致
//  2. 联网向官方端点拉取 —— 凭证优先用客户端请求带来的，其次本机登录态
//
// 返回错误原因供界面展示：调用方可降级到静态列表，但不应把原因吞掉。
func (g *Gateway) fetchOfficialModels(ctx context.Context, baseURL, authHeader string) ([]string, error) {
	if baseURL == "" {
		return nil, errors.New("官方上游未配置 base_url")
	}
	key := officialModelsURL(baseURL)

	officialMu.Lock()
	if e, ok := officialCache[key]; ok && time.Now().Before(e.expiresAt) {
		officialMu.Unlock()
		return e.models, nil
	}
	officialMu.Unlock()

	// 1. 本机缓存
	cached, fetchedAt := localOfficialModelsCache()
	if len(cached) > 0 {
		// 缓存够新就直接用，不再打扰网络
		if time.Since(fetchedAt) < officialCacheMaxAge {
			g.rememberOfficial(key, cached)
			return cached, nil
		}
		// 缓存偏旧：先试联网，失败也不丢缓存
		if live, err := g.fetchOfficialLive(ctx, key, authHeader); err == nil {
			g.rememberOfficial(key, live)
			return live, nil
		}
		g.rememberOfficial(key, cached)
		return cached, nil
	}

	// 2. 无缓存，只能联网
	models, err := g.fetchOfficialLive(ctx, key, authHeader)
	if err != nil {
		return nil, err
	}
	g.rememberOfficial(key, models)
	return models, nil
}

// fetchOfficialLive 向官方端点实时拉取模型清单
func (g *Gateway) fetchOfficialLive(ctx context.Context, reqURL, authHeader string) ([]string, error) {
	token := bearerToken(authHeader)
	if token == "" {
		token = localCodexToken()
	}
	if token == "" {
		return nil, fmt.Errorf("未拿到订阅凭证：请求未带 Authorization，本机 %s 与 %s 均不可读",
			filepath.Join(codexHome(), "auth.json"),
			filepath.Join(codexHome(), "models_cache.json"))
	}

	models, err := g.doFetchOfficial(ctx, reqURL, token)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, errors.New("官方返回的模型清单为空或格式无法识别")
	}
	return models, nil
}

// rememberOfficial 写入进程内缓存，避免每次请求都读文件或打网络
func (g *Gateway) rememberOfficial(key string, models []string) {
	officialMu.Lock()
	officialCache[key] = officialCacheEntry{models: models, expiresAt: time.Now().Add(officialCacheTTL)}
	officialMu.Unlock()
}

func (g *Gateway) doFetchOfficial(ctx context.Context, reqURL, token string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败：%w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if acc := localCodexAccountID(); acc != "" {
		req.Header.Set("ChatGPT-Account-Id", acc)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求官方失败（%s）：%w", redactURL(reqURL), err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		return nil, fmt.Errorf("官方返回 HTTP %d：%s", resp.StatusCode, snippet)
	}
	if readErr != nil {
		return nil, fmt.Errorf("读取官方响应失败：%w", readErr)
	}
	return parseModelList(body), nil
}

// redactURL 日志/报错里隐藏查询串，避免把版本号以外的敏感参数带到界面
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery = ""
	return u.String()
}

// modelEntry 模型清单里的单个条目。
// 不同上游字段命名不一致：OpenAI 系用 id，ChatGPT 官方清单用 slug，个别用 model。
type modelEntry struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Model      string `json:"model"`
	Visibility string `json:"visibility"`
}

// visibilityHidden 官方清单里标记为隐藏的模型（内部用途，如 codex-auto-review）。
// 官方客户端也不会把它们列进选择器，网关保持一致，避免污染模型列表。
const visibilityHidden = "hide"

// parseModelList 解析模型清单，兼容三种常见形态：
//
//	{"data":[{"id":..}]}    OpenAI /v1/models 风格
//	{"models":[{"slug":..}]} ChatGPT 官方 codex/models 风格（实测就是这种）
//	[{...}]                 裸数组
func parseModelList(body []byte) []string {
	var wrapped struct {
		Data   []modelEntry `json:"data"`
		Models []modelEntry `json:"models"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil {
		entries := wrapped.Data
		if len(entries) == 0 {
			entries = wrapped.Models
		}
		if len(entries) > 0 {
			return entryIDs(entries)
		}
	}

	var bare []modelEntry
	if err := json.Unmarshal(body, &bare); err == nil {
		return entryIDs(bare)
	}
	return nil
}

func entryIDs(entries []modelEntry) []string {
	out := make([]string, 0, len(entries))
	for _, m := range entries {
		// 只有官方清单带 visibility；普通上游没有该字段，不受影响
		if strings.EqualFold(m.Visibility, visibilityHidden) {
			continue
		}
		if id := firstNonEmpty(m.ID, m.Slug, m.Model); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// bearerToken 从 Authorization 头里取出裸 token
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return strings.TrimSpace(header)
}

// codexHome 定位 Codex 配置目录（可用 CODEX_HOME 覆盖，容器里靠它指到挂载点）
func codexHome() string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// localCodexToken 读取本机 Codex 登录态的 access_token
func localCodexToken() string {
	dir := codexHome()
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return ""
	}
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return ""
	}
	return auth.Tokens.AccessToken
}

// localCodexAccountID 读取本机 Codex 账号 ID（部分官方接口要求该头）
func localCodexAccountID() string {
	dir := codexHome()
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return ""
	}
	var auth struct {
		Tokens struct {
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return ""
	}
	return auth.Tokens.AccountID
}
