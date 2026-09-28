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
