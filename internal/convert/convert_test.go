package convert

import (
	"encoding/json"
	"testing"
)

func TestReqOpenAIToAnthropic_Basic(t *testing.T) {
	in := map[string]any{
		"model":    "claude-sonnet-4",
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
	}
	out := ReqOpenAIToAnthropic(in)

	if out["model"] != "claude-sonnet-4" {
		t.Errorf("model 丢失: %v", out["model"])
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("消息数错误: %d", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" {
		t.Errorf("role 错误: %v", m["role"])
	}
	blocks := m["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "text" {
		t.Errorf("应为 text block: %v", blocks)
	}
}

func TestReqOpenAIToAnthropic_SystemAndTools(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "user", "content": "hi"},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "get_weather",
					"description": "查天气",
					"parameters":  map[string]any{"type": "object"},
				},
			},
		},
	}
	out := ReqOpenAIToAnthropic(in)

	if out["system"] != "你是助手" {
		t.Errorf("system 提取失败: %v", out["system"])
	}
	tools := out["tools"].([]any)
	tm := tools[0].(map[string]any)
	if tm["name"] != "get_weather" || tm["input_schema"] == nil {
		t.Errorf("tools 转换失败: %v", tm)
	}
}

func TestReqOpenAIToAnthropic_ToolCalls(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{
				"role":    "assistant",
				"content": "我来查一下",
				"tool_calls": []any{
					map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "get_weather",
							"arguments": `{"city":"北京"}`,
						},
					},
				},
			},
			map[string]any{
				"role":         "tool",
				"tool_call_id": "call_1",
				"content":      "晴",
			},
		},
	}
	out := ReqOpenAIToAnthropic(in)

	msgs := out["messages"].([]any)
	// 第一条：assistant text + tool_use
	m0 := msgs[0].(map[string]any)
	blocks := m0["content"].([]any)
	var hasToolUse bool
	for _, b := range blocks {
		bm := b.(map[string]any)
		if bm["type"] == "tool_use" {
			hasToolUse = true
			if bm["id"] != "call_1" || bm["name"] != "get_weather" {
				t.Errorf("tool_use 转换失败: %v", bm)
			}
			input := bm["input"].(map[string]any)
			if input["city"] != "北京" {
				t.Errorf("tool_use input 转换失败: %v", input)
			}
		}
	}
	if !hasToolUse {
		t.Errorf("未找到 tool_use block: %v", blocks)
	}
	// 第二条：tool -> user + tool_result
	m1 := msgs[1].(map[string]any)
	if m1["role"] != "user" {
		t.Errorf("tool 消息应转为 user: %v", m1["role"])
	}
	rb := m1["content"].([]any)
	tr := rb[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" {
		t.Errorf("tool_result 转换失败: %v", tr)
	}
}

func TestReqOpenAIToAnthropic_ImageURL(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "这是什么"},
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:image/png;base64,AAAA"},
					},
				},
			},
		},
	}
	out := ReqOpenAIToAnthropic(in)
	msgs := out["messages"].([]any)
	blocks := msgs[0].(map[string]any)["content"].([]any)

	var img map[string]any
	for _, b := range blocks {
		if bm, ok := b.(map[string]any); ok && bm["type"] == "image" {
			img = bm
		}
	}
	if img == nil {
		t.Fatalf("image block 丢失: %v", blocks)
	}
	src := img["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "AAAA" {
		t.Errorf("image source 转换失败: %v", src)
	}
}

func TestReqAnthropicToOpenAI_ImageBlock(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "看图"},
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/jpeg",
							"data":       "BBBB",
						},
					},
				},
			},
		},
	}
	out := ReqAnthropicToOpenAI(in)
	msgs := out["messages"].([]any)
	parts := msgs[0].(map[string]any)["content"].([]any)

	var imgPart map[string]any
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok && pm["type"] == "image_url" {
			imgPart = pm
		}
	}
	if imgPart == nil {
		t.Fatalf("image_url part 丢失: %v", parts)
	}
	url := imgPart["image_url"].(map[string]any)["url"].(string)
	if url != "data:image/jpeg;base64,BBBB" {
		t.Errorf("data URL 构造失败: %s", url)
	}
}

func TestReqAnthropicToOpenAI(t *testing.T) {
	in := map[string]any{
		"model":      "deepseek-chat",
		"max_tokens": 100,
		"system":     "你是助手",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "你好"},
			}},
		},
	}
	out := ReqAnthropicToOpenAI(in)

	if out["max_tokens"] != 100 {
		t.Errorf("max_tokens 丢失: %v", out["max_tokens"])
	}
	msgs := out["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("system 应为首条消息: %v", msgs[0])
	}
	if msgs[1].(map[string]any)["content"] != "你好" {
		t.Errorf("文本转换失败: %v", msgs[1])
	}
}

func TestRespOpenAIToAnthropic_Reasoning(t *testing.T) {
	in := map[string]any{
		"id":    "chatcmpl-1",
		"model": "deepseek-reasoner",
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role":              "assistant",
					"reasoning_content": "让我想想",
					"content":           "答案是42",
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	}
	out := RespOpenAIToAnthropic(in)

	content := out["content"].([]any)
	first := content[0].(map[string]any)
	if first["type"] != "thinking" || first["thinking"] != "让我想想" {
		t.Errorf("thinking 转换失败: %v", first)
	}
	second := content[1].(map[string]any)
	if second["text"] != "答案是42" {
		t.Errorf("文本转换失败: %v", second)
	}
}

func TestRespAnthropicToOpenAI_Reasoning(t *testing.T) {
	in := map[string]any{
		"id":    "msg_1",
		"model": "claude",
		"content": []any{
			map[string]any{"type": "thinking", "thinking": "思考中"},
			map[string]any{"type": "text", "text": "答案"},
		},
		"stop_reason": "end_turn",
	}
	out := RespAnthropicToOpenAI(in)

	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "思考中" {
		t.Errorf("reasoning_content 转换失败: %v", msg)
	}
	if msg["content"] != "答案" {
		t.Errorf("文本转换失败: %v", msg)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]string{
		"tool_calls":     "tool_use",
		"length":         "max_tokens",
		"stop":           "end_turn",
		"content_filter": "refusal",
	}
	for oa, anth := range cases {
		if MapOpenAIFinishToAnthropic(oa) != anth {
			t.Errorf("finish 映射错误: %s -> %s (期望 %s)", oa, MapOpenAIFinishToAnthropic(oa), anth)
		}
	}
}

func TestSplitDataURL(t *testing.T) {
	mt, data, ok := splitDataURL("data:image/webp;base64,XYZ")
	if !ok || mt != "image/webp" || data != "XYZ" {
		t.Errorf("splitDataURL 失败: %s %s %v", mt, data, ok)
	}
	if _, _, ok := splitDataURL("https://example.com/a.png"); ok {
		t.Errorf("http URL 不应被识别为 data URL")
	}
}

var _ = json.Marshal
