package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"llm-gateway/internal/config"
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
}

// probeUpstream 探测上游连通性与鉴权
func probeUpstream(client *http.Client, up *config.Upstream) (ok bool, detail string, latency string) {
	start := time.Now()
	var url string
	if up.Protocol == "anthropic" {
		url = strings.TrimRight(up.BaseURL, "/") + "/v1/models"
	} else {
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
