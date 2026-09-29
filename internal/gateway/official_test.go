package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-gateway/internal/config"
)

// 测试官方模型透传：路由到 openai-official 上游，保留原始凭证，不做协议转换
func TestEndToEnd_OfficialPassthrough(t *testing.T) {
	var receivedAuth string
	var receivedPath string
	var receivedBody map[string]any

	mockOfficial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		// 官方返回 responses 格式
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_mock",
			"object": "response",
			"model":  "gpt-5.6-luna",
			"status": "completed",
			"output": []any{
				map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "官方回复"}},
				},
			},
		})
	}))
	defer mockOfficial.Close()

	store := &config.Store{}
	store.SetForTest(&config.Config{
		Listen: config.Listen{Host: "127.0.0.1", Port: 0},
		Upstreams: []config.Upstream{
			{Name: "official", Protocol: "openai-official", BaseURL: mockOfficial.URL, APIKey: "should-not-be-used", Models: []string{}},
			{Name: "volcengine", Protocol: "openai", BaseURL: "https://ark.example.com/api/v3", APIKey: "ark-test", Models: []string{"deepseek-v4-1-flash-260910"}},
		},
		Routes: map[string]string{},
	})
	g := New(store)

	// 官方模型请求，带原始订阅凭证
	reqBody := `{"model":"gpt-5.6-luna","input":"hi","stream":false}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer original-subscription-token")
	rec := httptest.NewRecorder()
	g.HandleResponses(rec, req)

	// 1. 凭证透传：mock 官方收到的是原始凭证，不是 api key
	if receivedAuth != "Bearer original-subscription-token" {
		t.Errorf("官方凭证未透传，收到: %q", receivedAuth)
	}

	// 2. 路径正确
	if receivedPath != "/responses" {
		t.Errorf("官方端点路径错误: %q", receivedPath)
	}

	// 3. 请求体透传（responses 格式，未转成 chat）
	if _, hasMessages := receivedBody["messages"]; hasMessages {
		t.Errorf("官方透传不应转成 chat 格式，body: %v", receivedBody)
	}
	if receivedBody["model"] != "gpt-5.6-luna" {
		t.Errorf("模型名透传错误: %v", receivedBody["model"])
	}

	// 4. 响应透传（responses 格式）
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "response" {
		t.Errorf("响应应透传 responses 格式，实际: %v", resp["object"])
	}
}

// 测试第三方模型仍走普通上游（官方透传不影响第三方路由）
func TestEndToEnd_ThirdPartyUnaffected(t *testing.T) {
	var receivedModel string
	mockVolc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		receivedModel = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_mock", "object": "response", "model": "deepseek-v4-1-flash-260910",
			"status": "completed", "output": []any{},
		})
	}))
	defer mockVolc.Close()

	store := &config.Store{}
	store.SetForTest(&config.Config{
		Listen: config.Listen{Host: "127.0.0.1", Port: 0},
		Upstreams: []config.Upstream{
			{Name: "official", Protocol: "openai-official", BaseURL: "https://chatgpt.example.com/backend-api/codex", APIKey: "", Models: []string{}},
			{Name: "volcengine", Protocol: "openai", BaseURL: mockVolc.URL, APIKey: "ark-test", Models: []string{"deepseek-v4-1-flash-260910"}},
		},
		Routes: map[string]string{},
	})
	g := New(store)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"deepseek-v4-1-flash-260910","input":"hi","stream":false}`))
	req.Header.Set("Authorization", "Bearer some-token")
	rec := httptest.NewRecorder()
	g.HandleResponses(rec, req)

	if receivedModel != "deepseek-v4-1-flash-260910" {
		t.Errorf("第三方模型路由错误: %q", receivedModel)
	}
}
