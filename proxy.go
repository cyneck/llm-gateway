package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Gateway 网关核心
type Gateway struct {
	store  *ConfigStore
	client *http.Client
	logs   *logBuffer
}

func NewGateway(store *ConfigStore) *Gateway {
	return &Gateway{
		store: store,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logs: newLogBuffer(300),
	}
}

// handleOpenAI 处理 /v1/chat/completions（OpenAI 入口）
func (g *Gateway) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	g.forward(w, r, "openai")
}

// handleAnthropic 处理 /v1/messages（Anthropic 入口）
func (g *Gateway) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	g.forward(w, r, "anthropic")
}

// handleModels 处理 /v1/models
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
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

// forward 通用转发：读请求 -> 路由 -> 转换 -> 转发 -> 处理响应
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, entryProtocol string) {
	start := time.Now()
	rec := &reqRecord{
		time:     start,
		method:   r.Method,
		path:     r.URL.Path,
		entry:    entryProtocol,
	}
	defer func() {
		rec.duration = time.Since(start)
		g.logs.add(rec)
	}()

	// 读请求体
	body, err := io.ReadAll(r.Body)
	if err != nil {
		rec.status = 400
		rec.err = "读取请求体失败"
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid request body", "type": "invalid_request_error"}})
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		rec.status = 400
		rec.err = "JSON 解析失败"
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid JSON", "type": "invalid_request_error"}})
		return
	}

	model := orString(payload["model"])
	rec.model = model

	upstream := g.store.ResolveUpstream(model)
	if upstream == nil {
		rec.status = 404
		rec.err = "找不到模型对应的上游"
		writeJSON(w, 404, map[string]any{"error": map[string]any{
			"message": fmt.Sprintf("no upstream configured for model %q", model),
			"type":    "model_not_found",
		}})
		return
	}
	rec.upstream = upstream.Name

	// 判断是否需要协议转换
	needsConvert := entryProtocol != upstream.Protocol
	if needsConvert {
		if entryProtocol == "openai" && upstream.Protocol == "anthropic" {
			payload = convertReqOpenAIToAnthropic(payload)
			rec.convert = "openai→anthropic"
		} else if entryProtocol == "anthropic" && upstream.Protocol == "openai" {
			payload = convertReqAnthropicToOpenAI(payload)
			rec.convert = "anthropic→openai"
		}
	}

	// 拼目标 URL
	targetURL, targetPath := upstreamTarget(upstream, entryProtocol)
	reqBody, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, bytes.NewReader(reqBody))
	if err != nil {
		rec.status = 500
		rec.err = "构造上游请求失败"
		writeJSON(w, 500, map[string]any{"error": map[string]any{"message": "internal error", "type": "internal_error"}})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+upstream.APIKey)
	if upstream.Protocol == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	rec.upstreamURL = targetURL

	// 判断流式
	isStream := false
	if s, ok := payload["stream"].(bool); ok {
		isStream = s
	}

	resp, err := g.client.Do(req)
	if err != nil {
		rec.status = 502
		rec.err = "上游连接失败: " + err.Error()
		writeJSON(w, 502, map[string]any{"error": map[string]any{
			"message": "upstream unreachable: " + err.Error(), "type": "upstream_error",
		}})
		return
	}
	defer resp.Body.Close()
	rec.status = resp.StatusCode

	// 上游报错：透传
	if resp.StatusCode >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		rec.err = "上游返回 " + resp.Status
		return
	}

	if isStream {
		g.proxyStream(w, resp, entryProtocol, upstream.Protocol, needsConvert)
	} else {
		g.proxyNonStream(w, resp, entryProtocol, upstream.Protocol, needsConvert)
	}

	_ = targetPath
}

// proxyNonStream 非流式响应转发（含协议转换）
func (g *Gateway) proxyNonStream(w http.ResponseWriter, resp *http.Response, entry, upstreamProto string, needsConvert bool) {
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

	// 响应转换：按"上游返回的协议"决定用哪个转换函数
	var out map[string]any
	if upstreamProto == "anthropic" {
		out = convertRespAnthropicToOpenAI(respJSON) // 上游 Anthropic -> 客户端 OpenAI
	} else {
		out = convertRespOpenAIToAnthropic(respJSON) // 上游 OpenAI -> 客户端 Anthropic
	}
	writeJSON(w, resp.StatusCode, out)
}

// proxyStream 流式响应转发（含协议转换）
func (g *Gateway) proxyStream(w http.ResponseWriter, resp *http.Response, entry, upstreamProto string, needsConvert bool) {
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

	// 协议转换流：转换器按"上游协议"选择
	var converter streamConverter
	if upstreamProto == "anthropic" {
		converter = newAOStreamConverter() // 上游 Anthropic 事件 -> OpenAI chunk
	} else {
		converter = newOAStreamConverter() // 上游 OpenAI chunk -> Anthropic 事件
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var currentEvent string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			if upstreamProto == "anthropic" {
				// Anthropic 上游，事件名在 currentEvent
				events := converter.convertEvent(currentEvent, data)
				for _, ev := range events {
					writeSSE(w, flusher, ev)
				}
			} else {
				// OpenAI 上游，直接用 data 调用
				events := converter.convert(data)
				for _, ev := range events {
					writeSSE(w, flusher, ev)
				}
			}
		}
	}
	// 补发收尾
	for _, ev := range converter.finish() {
		writeSSE(w, flusher, ev)
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// streamConverter 流式转换器统一接口
type streamConverter interface {
	convert(data string) []sseEvent
	convertEvent(event, data string) []sseEvent
	finish() []sseEvent
}

// 为两个转换器补充 convertEvent 方法（Anthropic 上游时，事件名实际不影响转换，data 里含 type）

func (c *oaStreamConverter) convertEvent(event, data string) []sseEvent {
	// OpenAI->Anthropic 不会接收 Anthropic 事件，这里不会走到
	return c.convert(data)
}

func (c *aoStreamConverter) convertEvent(event, data string) []sseEvent {
	return c.convert(data)
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, ev sseEvent) {
	var sb strings.Builder
	if ev.event != "" {
		sb.WriteString("event: " + ev.event + "\n")
	}
	sb.WriteString("data: " + ev.data + "\n\n")
	_, _ = io.WriteString(w, sb.String())
	if flusher != nil {
		flusher.Flush()
	}
}

// upstreamTarget 计算目标 URL 和路径
func upstreamTarget(up *Upstream, entryProtocol string) (string, string) {
	base := strings.TrimRight(up.BaseURL, "/")
	switch up.Protocol {
	case "anthropic":
		return base + "/v1/messages", "/v1/messages"
	default: // openai
		return base + "/chat/completions", "/chat/completions"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ============================================================================
// 请求日志
// ============================================================================

type reqRecord struct {
	time        time.Time
	method      string
	path        string
	entry       string
	model       string
	upstream    string
	upstreamURL string
	convert     string
	status      int
	duration    time.Duration
	err         string
	stream      bool
}

func (r *reqRecord) toMap() map[string]any {
	m := map[string]any{
		"time":     r.time.Format("15:04:05"),
		"method":   r.method,
		"path":     r.path,
		"model":    r.model,
		"upstream": r.upstream,
		"convert":  r.convert,
		"status":   r.status,
		"duration": r.duration.Round(time.Millisecond).String(),
	}
	if r.err != "" {
		m["error"] = r.err
	}
	return m
}

// logBuffer 环形缓冲
type logBuffer struct {
	mu   chan struct{}
	buf  []map[string]any
	head int
	size int
}

func newLogBuffer(size int) *logBuffer {
	return &logBuffer{
		mu:   make(chan struct{}, 1),
		buf:  make([]map[string]any, 0, size),
		size: size,
	}
}

func (l *logBuffer) add(r *reqRecord) {
	l.mu <- struct{}{}
	defer func() { <-l.mu }()
	if len(l.buf) < l.size {
		l.buf = append(l.buf, r.toMap())
	} else {
		l.buf = append(l.buf, r.toMap())
		l.buf = l.buf[1:]
	}
}

func (l *logBuffer) list() []map[string]any {
	l.mu <- struct{}{}
	defer func() { <-l.mu }()
	out := make([]map[string]any, len(l.buf))
	for i, v := range l.buf {
		out[len(l.buf)-1-i] = v // 最新的在前
	}
	return out
}

var _ = log.Println
