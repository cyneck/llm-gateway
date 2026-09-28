package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 测试 OpenAI → Anthropic 流式转换
func TestOAStreamConverter_TextAndTool(t *testing.T) {
	c := newOAStreamConverter()

	// 第一个 chunk：role + 内容
	events := c.convert(`{"id":"chatcmpl-1","model":"claude-x","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"},"finish_reason":null}]}`)
	if len(events) < 2 {
		t.Fatalf("首个 chunk 应产生 message_start + content_block_start + delta")
	}
	if events[0].event != "message_start" {
		t.Errorf("第一个事件应为 message_start，实际 %s", events[0].event)
	}

	// 文本增量
	events = c.convert(`{"choices":[{"index":0,"delta":{"content":"世界"},"finish_reason":null}]}`)
	if len(events) != 1 || events[0].event != "content_block_delta" {
		t.Errorf("文本增量事件错误: %v", events)
	}

	// 工具调用
	events = c.convert(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\""}}]},"finish_reason":null}]}`)
	events = c.convert(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"ls\"}"}}]},"finish_reason":null}]}`)

	// 结束
	events = c.convert(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	// 应包含 content_block_stop + tool_use blocks + message_delta + message_stop
	var hasToolUse, hasMessageStop bool
	for _, ev := range events {
		if ev.event == "content_block_start" {
			var m map[string]any
			_ = json.Unmarshal([]byte(ev.data), &m)
			if cb, ok := m["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
				hasToolUse = true
			}
		}
		if ev.event == "message_stop" {
			hasMessageStop = true
		}
	}
	if !hasToolUse {
		t.Errorf("缺少 tool_use block，事件: %v", events)
	}
	if !hasMessageStop {
		t.Errorf("缺少 message_stop")
	}
}

// 测试 Anthropic → OpenAI 流式转换
func TestAOStreamConverter_TextAndTool(t *testing.T) {
	c := newAOStreamConverter()

	var allText strings.Builder
	var sawToolCall, sawDone bool

	events := c.convert(`{"type":"message_start","message":{"id":"msg_1","role":"assistant","content":[]}}`)
	if len(events) != 1 {
		t.Fatalf("message_start 应产生 1 个 chunk")
	}

	events = c.convert(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	events = c.convert(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`)
	for _, ev := range events {
		if ev.data == "[DONE]" {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(ev.data), &m)
		choices := m["choices"].([]any)
		delta := choices[0].(map[string]any)["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok {
			allText.WriteString(s)
		}
	}

	events = c.convert(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}`)
	for _, ev := range events {
		var m map[string]any
		_ = json.Unmarshal([]byte(ev.data), &m)
		choices := m["choices"].([]any)
		delta := choices[0].(map[string]any)["delta"].(map[string]any)
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			sawToolCall = true
		}
	}

	c.convert(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`)
	events = c.convert(`{"type":"message_stop"}`)
	for _, ev := range events {
		if ev.data == "[DONE]" {
			sawDone = true
		}
	}

	if allText.String() != "你好" {
		t.Errorf("文本转换错误: %q", allText.String())
	}
	if !sawToolCall {
		t.Errorf("缺少 tool_calls chunk")
	}
	if !sawDone {
		t.Errorf("缺少 [DONE]")
	}
}

// 测试完成标记映射
func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]string{
		"tool_calls": "tool_use",
		"length":     "max_tokens",
		"stop":       "end_turn",
	}
	for oa, anth := range cases {
		if got := mapOpenAIFinishToAnthropic(oa); got != anth {
			t.Errorf("OpenAI %s -> Anthropic 期望 %s，实际 %s", oa, anth, got)
		}
	}
	rev := map[string]string{
		"tool_use":  "tool_calls",
		"max_tokens": "length",
		"end_turn":   "stop",
	}
	for anth, oa := range rev {
		if got := mapAnthropicStopToOpenAI(anth); got != oa {
			t.Errorf("Anthropic %s -> OpenAI 期望 %s，实际 %s", anth, oa, got)
		}
	}
}
