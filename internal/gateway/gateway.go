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
func (g *Gateway) HandleModels(w http.ResponseWriter, r *http.Request) {
	cfg := g.store.Get()
	data := make([]any, 0)
	for _, up := range cfg.Upstreams {
		for _, m := range up.Models {
			data = append(data, map[string]any{
				"id": m, "object": "model", "owned_by": up.Name,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
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
			resp, err := g.doUpstream(r.Context(), upstream, targetURL, reqBody)
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
					_, _ = io.Copy(w, resp.Body)
					rec.Err = "上游返回 " + resp.Status
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

// doUpstream 对单个上游执行一次请求，带超时
func (g *Gateway) doUpstream(ctx context.Context, up config.Upstream, targetURL string, reqBody []byte) (*http.Response, error) {
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
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	if up.Protocol == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	return g.client.Do(req)
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
