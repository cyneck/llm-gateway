package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/gateway/catalogdata"
)

// RegisterAdminRoutes 注册管理 API
func RegisterAdminRoutes(mux *http.ServeMux, store *config.Store, g *Gateway) {
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			cfg := store.Get()
			if cfg.AdminKey != "" {
				auth := r.Header.Get("Authorization")
				const prefix = "Bearer "
				if !strings.HasPrefix(auth, prefix) || auth[len(prefix):] != cfg.AdminKey {
					w.Header().Set("WWW-Authenticate", "Bearer")
					writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
					return
				}
			}
			next(w, r)
		}
	}

	// 网关信息（不强制鉴权，保持探针友好）
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		cfg := store.Get()
		writeJSON(w, 200, map[string]any{
			"name":    "llm-gateway",
			"version": "1.0.0",
			"listen":  cfg.Listen,
		})
	})

	// 读配置
	mux.HandleFunc("/api/config", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, store.Get())
		case http.MethodPut, http.MethodPost:
			var cfg config.Config
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				writeJSON(w, 400, map[string]any{"error": "invalid config JSON"})
				return
			}
			if err := store.Save(&cfg); err != nil {
				writeJSON(w, 500, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
		default:
			writeJSON(w, 405, map[string]any{"error": "method not allowed"})
		}
	}))

	// 健康检查
	mux.HandleFunc("/api/health", auth(func(w http.ResponseWriter, r *http.Request) {
		cfg := store.Get()
		type healthItem struct {
			Name    string `json:"name"`
			OK      bool   `json:"ok"`
			Detail  string `json:"detail"`
			Latency string `json:"latency"`
		}
		results := make([]healthItem, 0, len(cfg.Upstreams))
		for _, up := range cfg.Upstreams {
			ok, detail, latency := probeUpstream(g.client, &up)
			results = append(results, healthItem{Name: up.Name, OK: ok, Detail: detail, Latency: latency})
		}
		writeJSON(w, 200, results)
	}))

	// 请求日志
	mux.HandleFunc("/api/logs", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, g.logs.List())
	}))

	// 清空日志
	mux.HandleFunc("/api/logs/clear", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			g.logs = NewLogBuffer(300)
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
	}))

	// 导出 Codex 模型目录（写入 Codex 的 model_catalog_json 即可看到自定义模型）
	mux.HandleFunc("/api/model-catalog", auth(func(w http.ResponseWriter, r *http.Request) {
		cfg := store.Get()
		writeJSON(w, 200, buildModelCatalog(cfg, g))
	}))

	// 重新拉取官方模型清单（内置上游只读，界面靠它刷新）
	mux.HandleFunc("/api/official-models/refresh", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]any{"error": "method not allowed"})
			return
		}
		cfg := store.Get()
		var up *config.Upstream
		for i := range cfg.Upstreams {
			if config.IsBuiltinUpstream(cfg.Upstreams[i]) {
				up = &cfg.Upstreams[i]
				break
			}
		}
		if up == nil {
			writeJSON(w, 404, map[string]any{"error": "未配置官方透传上游"})
			return
		}
		models, err := g.fetchOfficialModels(r.Context(), up.BaseURL, r.Header.Get("Authorization"))
		if err != nil {
			writeJSON(w, 200, map[string]any{
				"ok": false, "models": []string{}, "fallback": up.Models,
				"detail": err.Error(),
			})
			return
		}
		if len(models) == 0 {
			writeJSON(w, 200, map[string]any{
				"ok": false, "models": []string{}, "fallback": up.Models,
				"detail": "官方返回的模型清单为空，当前展示配置中的静态列表",
			})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "models": models, "count": len(models)})
	}))
}

// catalogReasoningLevels Codex 识别的最低限度推理档位
var catalogReasoningLevels = []map[string]string{
	{"effort": "low", "description": "Fast responses with lighter reasoning"},
	{"effort": "medium", "description": "Balances speed and reasoning depth for everyday tasks"},
	{"effort": "high", "description": "Greater reasoning depth for complex problems"},
}

// buildModelCatalog 依据配置里声明的模型生成 Codex 模型目录。
//
// 注意：Codex 对目录条目做**严格反序列化**，缺字段会直接报
// "missing field xxx" 并拒绝启动。因此这里必须输出完整字段集。
func buildModelCatalog(cfg *config.Config, g *Gateway) map[string]any {
	models := make([]any, 0)
	seen := map[string]bool{}
	priority := 1

	// 官方透传上游置顶，其余按配置顺序
	ordered := make([]config.Upstream, 0, len(cfg.Upstreams))
	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].Protocol == config.ProtocolOfficial {
			ordered = append(ordered, cfg.Upstreams[i])
		}
	}
	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].Protocol != config.ProtocolOfficial {
			ordered = append(ordered, cfg.Upstreams[i])
		}
	}

	for i := range ordered {
		up := ordered[i]
		ids := up.Models
		if len(ids) == 0 && up.Protocol != config.ProtocolOfficial {
			ids = g.fetchUpstreamModels(context.Background(), &up)
		}
		for _, id := range ids {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			models = append(models, catalogEntry(id, up.Name, priority))
			priority++
		}
	}
	return map[string]any{"models": models}
}

// catalogEntry 构造一条完整的 Codex 模型目录条目
func catalogEntry(slug, upstream string, priority int) map[string]any {
	return map[string]any{
		"slug":                              slug,
		"display_name":                      slug,
		"description":                       "经 llm-gateway 转发（上游：" + upstream + "）",
		"default_reasoning_level":           "medium",
		"supported_reasoning_levels":        catalogReasoningLevels,
		"shell_type":                        "local",
		"visibility":                        "list",
		"supported_in_api":                  true,
		"priority":                          priority,
		"availability_nux":                  nil,
		"upgrade":                           nil,
		"service_tiers":                     []any{},
		"additional_speed_tiers":            []any{},
		"model_messages":                    catalogModelMessages(),
		"include_skills_usage_instructions": false,
		"include_plugin_usage_instructions": false,
		"include_apps_usage_instructions":   false,
		"default_reasoning_summary":         "none",
		"support_verbosity":                 true,
		"default_verbosity":                 "low",
		"apply_patch_tool_type":             "freeform",
		"web_search_tool_type":              "text_and_image",
		"truncation_policy":                 map[string]any{"mode": "tokens", "limit": 10000},
		"supports_image_detail_original":    false,
		"context_window":                    128000,
		"max_context_window":                128000,
		"comp_hash":                         "3000",
		"effective_context_window_percent":  95,
		"experimental_supported_tools":      []any{},
		"input_modalities":                  []any{"text"},
		"supports_search_tool":              false,
		"use_responses_lite":                false,
		"node_repl_auto_review_required":    false,
		"node_repl_disabled":                false,
	}
}

// catalogModelMessages 模型消息层：直接复用官方基线（含 token_budget 等必需子字段）。
// Codex 严格反序列化，缺字段会直接拒绝加载整个目录。
func catalogModelMessages() map[string]any {
	return catalogdata.ModelMessages()
}

// probeUpstream 探测上游连通性与鉴权
func probeUpstream(client *http.Client, up *config.Upstream) (ok bool, detail string, latency string) {
	start := time.Now()
	var url string
	switch up.Protocol {
	case "anthropic":
		url = strings.TrimRight(up.BaseURL, "/") + "/v1/models"
	case config.ProtocolOfficial:
		// 官方订阅上游无公开 /models 端点，无法用 HTTP 探测（凭证由客户端透传）
		return false, "官方透传（需客户端订阅凭证，不参与探测）", ""
	default:
		url = strings.TrimRight(up.BaseURL, "/") + "/models"
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false, "URL 无效", ""
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	if up.Protocol == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	resp, err := client.Do(req)
	latency = time.Since(start).Round(time.Millisecond).String()
	if err != nil {
		return false, "无法连接: " + err.Error(), latency
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == 200:
		return true, "正常", latency
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return false, "鉴权失败 (HTTP " + http.StatusText(resp.StatusCode) + ")", latency
	default:
		return false, "HTTP " + resp.Status, latency
	}
}
