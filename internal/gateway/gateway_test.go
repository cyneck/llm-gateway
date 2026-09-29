package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-gateway/internal/config"
)

// 模拟 OpenAI 上游，验证网关的完整转换链路
func TestEndToEnd_AnthropicEntryToOpenAIUpstream(t *testing.T) {
	var receivedModel string
	var receivedIsOpenAI bool
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("mock 上游收到错误路径: %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		receivedModel = body["model"].(string)
		if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
			if m, ok := msgs[0].(map[string]any); ok {
				_, hasContent := m["content"]
				_, hasRole := m["role"]
				receivedIsOpenAI = hasContent && hasRole
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "chatcmpl-mock",
			"object": "chat.completion",
			"model":  "deepseek-chat",
			"choices": []any{
				map[string]any{
					"message":       map[string]any{"role": "assistant", "content": "你好，世界"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer mockUpstream.Close()

	store := &config.Store{}
	store.SetForTest(&config.Config{
		Listen: config.Listen{Host: "127.0.0.1", Port: 0},
		Upstreams: []config.Upstream{
			{Name: "mock", Protocol: "openai", BaseURL: mockUpstream.URL, APIKey: "sk-test", Models: []string{"deepseek-chat"}},
		},
		Routes: map[string]string{},
	})
	g := New(store)

	anthReq := `{"model":"deepseek-chat","max_tokens":100,"stream":false,"messages":[{"role":"user","content":"你好"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthReq))
	rec := httptest.NewRecorder()
	g.HandleAnthropic(rec, req)

	if receivedModel != "deepseek-chat" {
		t.Errorf("mock 收到的模型错误: %s", receivedModel)
	}
	if !receivedIsOpenAI {
		t.Errorf("mock 收到的不是 OpenAI 格式消息")
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp["type"] != "message" {
		t.Errorf("响应应为 Anthropic message 类型，实际: %v", resp["type"])
	}
	content := resp["content"].([]any)
	if content[0].(map[string]any)["text"] != "你好，世界" {
		t.Errorf("响应内容转换失败: %v", content)
	}
}

// 测试 OpenAI 入口透传到 OpenAI 上游（协议相同，不转换）
func TestEndToEnd_OpenAIEntryToOpenAIUpstream(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "chatcmpl-mock",
			"object": "chat.completion",
			"model":  "deepseek-chat",
			"choices": []any{
				map[string]any{
					"message":       map[string]any{"role": "assistant", "content": "直通"},
					"finish_reason": "stop",
				},
			},
		})
	}))
	defer mockUpstream.Close()

	store := &config.Store{}
	store.SetForTest(&config.Config{
		Listen: config.Listen{Host: "127.0.0.1", Port: 0},
		Upstreams: []config.Upstream{
			{Name: "mock", Protocol: "openai", BaseURL: mockUpstream.URL, APIKey: "sk-test", Models: []string{"deepseek-chat"}},
		},
		Routes: map[string]string{},
	})
	g := New(store)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	g.HandleOpenAI(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "chat.completion" {
		t.Errorf("透传失败，应为 chat.completion，实际: %v", resp["object"])
	}
}

// Responses 入口（Codex wire_api=responses）端到端：非流式
func TestEndToEnd_ResponsesEntryToOpenAIUpstream(t *testing.T) {
	var receivedIsChatFormat bool
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("mock 上游收到错误路径: %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
			if m, ok := msgs[0].(map[string]any); ok {
				_, hasContent := m["content"]
				_, hasRole := m["role"]
				receivedIsChatFormat = hasContent && hasRole
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "chatcmpl-mock",
			"object": "chat.completion",
			"model":  "deepseek-chat",
			"choices": []any{
				map[string]any{
					"message":       map[string]any{"role": "assistant", "content": "响应内容"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{"prompt_tokens": 8, "completion_tokens": 4},
		})
	}))
	defer mockUpstream.Close()

	store := &config.Store{}
	store.SetForTest(&config.Config{
		Listen: config.Listen{Host: "127.0.0.1", Port: 0},
		Upstreams: []config.Upstream{
			{Name: "mock", Protocol: "openai", BaseURL: mockUpstream.URL, APIKey: "sk-test", Models: []string{"deepseek-chat"}},
		},
		Routes: map[string]string{},
	})
	g := New(store)

	respReq := `{"model":"deepseek-chat","instructions":"你是助手","stream":false,"input":"你好"}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(respReq))
	rec := httptest.NewRecorder()
	g.HandleResponses(rec, req)

	if !receivedIsChatFormat {
		t.Errorf("mock 收到的不是 Chat Completions 格式")
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp["object"] != "response" {
		t.Errorf("响应应为 Responses 格式（object=response），实际: %v", resp["object"])
	}
	output := resp["output"].([]any)
	msg := output[0].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("output item 转换失败: %v", msg)
	}
	content := msg["content"].([]any)
	if content[0].(map[string]any)["text"] != "响应内容" {
		t.Errorf("文本转换失败: %v", content)
	}
}

// Responses 入口端到端：流式（SSE chunk -> Responses 事件流）
func TestEndToEnd_ResponsesEntryStream(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"cc1","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"流式"},"finish_reason":null}]}`,
			`{"id":"cc1","model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"内容"},"finish_reason":null}]}`,
			`{"id":"cc1","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
			`[DONE]`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			flusher.Flush()
		}
	}))
	defer mockUpstream.Close()

	store := &config.Store{}
	store.SetForTest(&config.Config{
		Listen: config.Listen{Host: "127.0.0.1", Port: 0},
		Upstreams: []config.Upstream{
			{Name: "mock", Protocol: "openai", BaseURL: mockUpstream.URL, APIKey: "sk-test", Models: []string{"deepseek-chat"}},
		},
		Routes: map[string]string{},
	})
	g := New(store)

	respReq := `{"model":"deepseek-chat","stream":true,"input":"你好"}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(respReq))
	rec := httptest.NewRecorder()
	g.HandleResponses(rec, req)

	body := rec.Body.String()
	var sawCreated, sawDelta, sawCompleted bool
	var textDeltas []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "event: ") {
			continue
		}
		name := strings.TrimPrefix(line, "event: ")
		switch name {
		case "response.created":
			sawCreated = true
		case "response.output_text.delta":
			sawDelta = true
		case "response.completed":
			sawCompleted = true
		}
	}
	// 提取 delta 文本
	for _, m := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(m, "\n") {
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"response.output_text.delta"`) {
				var ev map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil {
					textDeltas = append(textDeltas, ev["delta"].(string))
				}
			}
		}
	}
	if !sawCreated || !sawDelta || !sawCompleted {
		t.Errorf("缺少关键事件: created=%v delta=%v completed=%v\nbody:\n%s", sawCreated, sawDelta, sawCompleted, body)
	}
	if strings.Join(textDeltas, "") != "流式内容" {
		t.Errorf("流式文本聚合错误: %v", textDeltas)
	}
}
