// Package gateway 实现 LLM 网关核心：请求转发、协议选择、流式/非流式处理。
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/convert"
)

// Gateway 网关核心
type Gateway struct {
	store  *config.Store
	client *http.Client
	logs   *LogBuffer
}

// New 创建网关
func New(store *config.Store) *Gateway {
	return &Gateway{
		store: store,
		client: &http.Client{
			Transport: &http.Transport{
				// 统一出站代理：每次请求实时读取配置，保存后立即生效，无需重启
				Proxy: func(req *http.Request) (*url.URL, error) {
					return store.Get().Proxy.ProxyFor(req.URL.Hostname())
				},
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logs: NewLogBuffer(300),
	}
}

// HandleOpenAI 处理 /v1/chat/completions（OpenAI 入口）
func (g *Gateway) HandleOpenAI(w http.ResponseWriter, r *http.Request) {
	g.forward(w, r, "openai")
}

// HandleAnthropic 处理 /v1/messages（Anthropic 入口）
func (g *Gateway) HandleAnthropic(w http.ResponseWriter, r *http.Request) {
	g.forward(w, r, "anthropic")
}

// HandleResponses 处理 /v1/responses（OpenAI Responses 入口，Codex wire_api=responses）
func (g *Gateway) HandleResponses(w http.ResponseWriter, r *http.Request) {
	g.forward(w, r, "responses")
}

// HandleModels 处理 /v1/models
//
// 默认返回配置文件中的静态模型清单；带 ?live=1 时额外向上游拉取实时模型列表：
// openai/anthropic 协议走各自 /models 端点（需要 API Key）；
// openai-official 走官方清单端点，凭证取客户端 Authorization 或本机 Codex 登录态。
func (g *Gateway) HandleModels(w http.ResponseWriter, r *http.Request) {
	cfg := g.store.Get()
	data := make([]any, 0)
	seen := map[string]bool{}
	add := func(id, owner string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": owner})
	}

	for _, up := range cfg.Upstreams {
		for _, m := range up.Models {
			add(m, up.Name)
		}
	}

	if r.URL.Query().Get("live") == "1" {
		for i := range cfg.Upstreams {
			up := cfg.Upstreams[i]

			// 官方透传上游本身没有 API Key（凭证由客户端请求带来或读本机登录态），
			// 因此必须排在下方的 APIKey 守卫之前，否则会被误判为"不可探测"而跳过。
			if up.Protocol == config.ProtocolOfficial {
				models, err := g.fetchOfficialModels(r.Context(), up.BaseURL, r.Header.Get("Authorization"))
				if err != nil {
					g.logs.Add(&ReqRecord{
						Time: time.Now(), Method: r.Method, Path: r.URL.Path,
						Entry: "models", Upstream: up.Name,
						UpstreamURL: officialModelsURL(up.BaseURL),
						Status: 0, Err: "官方模型清单拉取失败：" + err.Error(),
					})
					// 拉取失败时保留配置里的静态清单，保证模型不会凭空消失
					for _, m := range up.Models {
						add(m, up.Name)
					}
				}
				for _, id := range models {
					add(id, up.Name)
				}
				continue
			}

			if up.APIKey == "" {
				continue
			}
			for _, id := range g.fetchUpstreamModels(r.Context(), &up) {
				add(id, up.Name)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// upstreamModelsURL 计算上游模型列表端点
func upstreamModelsURL(up *config.Upstream) string {
	base := strings.TrimRight(up.BaseURL, "/")
	if up.Protocol == "anthropic" {
		return base + "/v1/models"
	}
	return base + "/models"
}

// fetchUpstreamModels 向上游拉取模型列表，失败时静默降级为空列表（不影响静态清单）
func (g *Gateway) fetchUpstreamModels(ctx context.Context, up *config.Upstream) []string {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamModelsURL(up), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	if up.Protocol == "anthropic" {
		req.Header.Set("x-api-key", up.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}

	// OpenAI: {"data":[{"id":"..."}]}  Anthropic: {"data":[{"id":"..."}]}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		out = append(out, m.ID)
	}
	return out
}

// retryBaseDelay 测试可调的重试基础间隔
var retryBaseDelay = time.Second

// forward 通用转发：读请求 -> 路由 -> 转换 -> 转发 -> 处理响应
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, entryProtocol string) {
	start := time.Now()
	rec := &ReqRecord{
		Time:   start,
		Method: r.Method,
		Path:   r.URL.Path,
		Entry:  entryProtocol,
	}
	defer func() {
		rec.Duration = time.Since(start)
		g.logs.Add(rec)
	}()

	// 客户端原始请求头（官方透传时整份带过去）
	srcHeaders := r.Header.Clone()

	// 读请求体
	body, err := io.ReadAll(r.Body)
	if err != nil {
		rec.Status = 400
		rec.Err = "读取请求体失败"
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid request body", "type": "invalid_request_error"}})
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		rec.Status = 400
		rec.Err = "JSON 解析失败"
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid JSON", "type": "invalid_request_error"}})
		return
	}

	model := orString(payload["model"])
	rec.Model = model

	candidates := g.store.ResolveUpstreams(model)
	if len(candidates) == 0 {
		rec.Status = 404
		rec.Err = "找不到模型对应的上游"
		writeJSON(w, 404, map[string]any{"error": map[string]any{
			"message": fmt.Sprintf("no upstream configured for model %q", model),
			"type":    "model_not_found",
		}})
		return
	}

	cfg := g.store.Get()

	var lastErr string
	for ci, cand := range candidates {
		upstream := cand.Upstream
		if ci == 0 {
			rec.Upstream = upstream.Name
		} else {
			rec.Upstream = upstream.Name
			lastErr += "; failed over from " + candidates[ci-1].Upstream.Name
		}

		// 模型名重写：Routes 值为 "upstream:target_model" 时改写当前候选的 payload.model
		payloadCopy := shallowCopy(payload)
		targetModel := model
		if cand.TargetModel != "" {
			targetModel = cand.TargetModel
		}
		payloadCopy["model"] = targetModel

		conv := convertPayload(payloadCopy, entryProtocol, upstream.Protocol)
		rec.Convert = conv.label

		targetURL := upstreamTarget(&upstream)
		reqBody, _ := json.Marshal(conv.payload)

		maxRetries := upstream.MaxRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
		for attempt := 0; attempt <= maxRetries; attempt++ {
			resp, err := g.doUpstream(r.Context(), upstream, targetURL, reqBody, srcHeaders)
			if err == nil && resp != nil {
				rec.Status = resp.StatusCode
				rec.UpstreamURL = targetURL

				isStream := false
				if s, ok := conv.payload["stream"].(bool); ok {
					isStream = s
				}

				// 成功响应：直接处理并返回
				if resp.StatusCode < 400 {
					if attempt > 0 {
						lastErr += fmt.Sprintf("; retried %d times", attempt)
					}
					if lastErr != "" {
						rec.Err = strings.TrimPrefix(lastErr, "; ")
					}
					if isStream {
						g.proxyStream(w, resp, entryProtocol, upstream.Protocol, conv.needsConvert)
					} else {
						g.proxyNonStream(w, resp, entryProtocol, upstream.Protocol, conv.needsConvert)
					}
					return
				}

				// 4xx（除 429）不重试，直接透传
				if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(resp.StatusCode)
					rec.Err = "上游返回 " + resp.Status
					if upstream.Protocol == config.ProtocolOfficial && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
						// 官方通道被拒：网关只做透传、不注入凭证，问题一定在客户端登录态或账号套餐上，
						// 这里把排查方向直接写进返回体和日志，避免只看到一个干巴巴的 Unauthorized。
						body, _ := io.ReadAll(resp.Body)
						_, _ = w.Write(augmentOfficialAuthError(body))
						rec.Err += "（官方通道：凭证由客户端自带，网关未注入，请检查账号套餐/登录态）"
					} else {
						_, _ = io.Copy(w, resp.Body)
					}
					_ = resp.Body.Close()
					return
				}

				// 可重试错误：读 body 消耗掉，准备下一轮
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()

				// 重试耗尽则进入 failover
				if attempt == maxRetries {
					lastErr += fmt.Sprintf("; %s returned %s", upstream.Name, resp.Status)
					break
				}

				// 退避
				delay := retryBaseDelay * time.Duration(1<<attempt)
				if resp.StatusCode == http.StatusTooManyRequests {
					if ra := resp.Header.Get("Retry-After"); ra != "" {
						if sec, e := strconv.Atoi(ra); e == nil && sec > 0 {
							delay = time.Duration(sec) * time.Second
						}
					}
				}
				time.Sleep(delay)
				continue
			}

			// 网络错误
			if attempt == maxRetries {
				lastErr += fmt.Sprintf("; %s unreachable: %s", upstream.Name, err.Error())
				break
			}
			time.Sleep(retryBaseDelay * time.Duration(1<<attempt))
		}

		// failover 关闭
		if !cfg.Failover || len(candidates) == 1 {
			break
		}
	}

	// 全部候选失败
	rec.Status = 502
	if lastErr != "" {
		rec.Err = strings.TrimPrefix(lastErr, "; ")
	} else {
		rec.Err = "上游全部失败"
	}
	writeJSON(w, 502, map[string]any{"error": map[string]any{
		"message": rec.Err, "type": "upstream_error",
	}})
}

// convertResult 请求协议转换结果
type convertResult struct {
	payload      map[string]any
	needsConvert bool
	label        string
}

// convertPayload 根据入口与上游协议转换请求体
func convertPayload(payload map[string]any, entryProtocol, upstreamProtocol string) convertResult {
	// 官方透传：原样透传，不做协议转换（官方支持 Responses 协议）
	if upstreamProtocol == config.ProtocolOfficial {
		return convertResult{payload: payload, needsConvert: false, label: "官方透传"}
	}
	needsConvert := entryProtocol != upstreamProtocol || entryProtocol == "responses"
	r := convertResult{payload: payload, needsConvert: needsConvert}
	switch {
	case entryProtocol == "responses":
		payload = convert.ReqResponsesToOpenAI(payload)
		if upstreamProtocol == "anthropic" {
			payload = convert.ReqOpenAIToAnthropic(payload)
			r.label = "responses→anthropic"
		} else {
			r.label = "responses→openai"
		}
	case entryProtocol == "openai" && upstreamProtocol == "anthropic":
		payload = convert.ReqOpenAIToAnthropic(payload)
		r.label = "openai→anthropic"
	case entryProtocol == "anthropic" && upstreamProtocol == "openai":
		payload = convert.ReqAnthropicToOpenAI(payload)
		r.label = "anthropic→openai"
	}
	r.payload = payload
	return r
}

// hopByHopHeaders 逐跳头：只在单条连接上有意义，不能跨连接转发（RFC 7230 6.1）
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"host":                true, // 由上游 URL 重新决定
	"content-length":      true, // 由 Go 按实际请求体重算
}

// doUpstream 对单个上游执行一次请求，带超时
//
// srcHeaders 为客户端原始请求头。官方透传（openai-official）时**整份复制**过去：
// 官方端点要靠 ChatGPT-Account-Id、originator、session_id 等头识别请求，
// 只保留 Authorization 会被判为未授权。第三方上游则只注入自己的 API Key，
// 避免把客户端凭证泄露给外部服务。
func (g *Gateway) doUpstream(ctx context.Context, up config.Upstream, targetURL string, reqBody []byte, srcHeaders http.Header) (*http.Response, error) {
	var reqCtx context.Context = ctx
	var cancel context.CancelFunc
	if up.TimeoutSeconds > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, time.Duration(up.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, "POST", targetURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	if up.Protocol == config.ProtocolOfficial {
		// 官方透传：原样带上客户端的全部非逐跳头，凭证由客户端自带的为准
		for name, values := range srcHeaders {
			if hopByHopHeaders[strings.ToLower(name)] {
				continue
			}
			for _, v := range values {
				req.Header.Add(name, v)
			}
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}
	if up.Protocol == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	return g.client.Do(req)
}

// officialAuthHint 是官方通道返回 401/403 时附加的排查说明。
// 网关对官方通道只做透传、不注入任何凭证，所以被拒与网关配置无关。
const officialAuthHint = "llm-gateway 对官方通道只透传客户端自带的凭证，未做任何改写。" +
	"被拒通常意味着：1) ChatGPT 账号套餐不含 Codex 权限（免费账号无权调用推理接口，只能查模型列表）；" +
	"2) 登录态过期，需重新 codex login；3) 上游风控。可先用 /api/official-models/refresh 验证登录态是否仍然有效。"

// augmentOfficialAuthError 把官方通道的鉴权失败响应补上排查提示。
// 能解析成 JSON 就加一个 hint 字段；解析不了就原样返回，绝不丢原始错误信息。
func augmentOfficialAuthError(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body
	}
	m["hint"] = officialAuthHint
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// proxyNonStream 非流式响应转发（含协议转换）
func (g *Gateway) proxyNonStream(w http.ResponseWriter, resp *http.Response, entryProto, upstreamProto string, needsConvert bool) {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": map[string]any{"message": "read upstream failed", "type": "upstream_error"}})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if !needsConvert {
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}

	var respJSON map[string]any
	if err := json.Unmarshal(respBody, &respJSON); err != nil {
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}

	// 响应转换：按"入口协议 + 上游协议"决定转换函数链
	var out map[string]any
	switch {
	case entryProto == "responses" && upstreamProto == "anthropic":
		out = convert.RespOpenAIToResponses(convert.RespAnthropicToOpenAI(respJSON)) // Anthropic -> Chat -> Responses
	case entryProto == "responses":
		out = convert.RespOpenAIToResponses(respJSON) // Chat -> Responses
	case upstreamProto == "anthropic":
		out = convert.RespAnthropicToOpenAI(respJSON) // 上游 Anthropic -> 客户端 OpenAI
	default:
		out = convert.RespOpenAIToAnthropic(respJSON) // 上游 OpenAI -> 客户端 Anthropic
	}
	writeJSON(w, resp.StatusCode, out)
}

// streamConverter 流式转换器统一接口
type streamConverter interface {
	Convert(data string) []convert.SSEEvent
	Finish() []convert.SSEEvent
}

// proxyStream 流式响应转发（含协议转换）
func (g *Gateway) proxyStream(w http.ResponseWriter, resp *http.Response, entryProto, upstreamProto string, needsConvert bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	if !needsConvert {
		// 直接透传流
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = io.WriteString(w, line+"\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		return
	}

	// 两级转换链：
	// stage1（可选）：上游协议 -> OpenAI chat chunk
	// stage2：OpenAI chat chunk -> 入口协议
	var stage1, stage2 streamConverter
	switch {
	case entryProto == "responses" && upstreamProto == "anthropic":
		stage1 = convert.NewAOStreamConverter()     // Anthropic 事件 -> chat chunk
		stage2 = convert.NewChatToResponsesStream() // chat chunk -> Responses 事件
	case entryProto == "responses":
		stage2 = convert.NewChatToResponsesStream() // chat chunk -> Responses 事件
	case upstreamProto == "anthropic":
		stage2 = convert.NewAOStreamConverter() // 上游 Anthropic 事件 -> OpenAI chunk
	default:
		stage2 = convert.NewOAStreamConverter() // 上游 OpenAI chunk -> Anthropic 事件
	}

	// emit 把 stage1 的产物喂给 stage2（无 stage1 时直接处理上游 data）
	emit := func(data string) {
		var events []convert.SSEEvent
		if stage1 != nil {
			for _, ce := range stage1.Convert(data) {
				events = append(events, stage2.Convert(ce.Data)...)
			}
		} else {
			events = stage2.Convert(data)
		}
		for _, ev := range events {
			writeSSE(w, flusher, ev)
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			// Anthropic 事件名冗余（data 内含 type），转换器直接解析 data
			continue
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			emit(data)
		}
	}
	// 补发收尾：先冲刷 stage1 的残留，再冲刷 stage2
	if stage1 != nil {
		for _, ce := range stage1.Finish() {
			for _, ev := range stage2.Convert(ce.Data) {
				writeSSE(w, flusher, ev)
			}
		}
	}
	for _, ev := range stage2.Finish() {
		writeSSE(w, flusher, ev)
	}
	if flusher != nil {
		flusher.Flush()
	}
}

func writeSSE(w io.Writer, flusher http.Flusher, ev convert.SSEEvent) {
	var sb strings.Builder
	if ev.Event != "" {
		sb.WriteString("event: " + ev.Event + "\n")
	}
	sb.WriteString("data: " + ev.Data + "\n\n")
	_, _ = io.WriteString(w, sb.String())
	if flusher != nil {
		flusher.Flush()
	}
}

// upstreamTarget 计算目标 URL
func upstreamTarget(up *config.Upstream) string {
	base := strings.TrimRight(up.BaseURL, "/")
	switch up.Protocol {
	case "anthropic":
		return base + "/v1/messages"
	case config.ProtocolOfficial:
		return base + "/responses" // ChatGPT 官方 Codex 端点
	default: // openai
		return base + "/chat/completions"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func orString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// shallowCopy 浅拷贝 map[string]any
func shallowCopy(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}
