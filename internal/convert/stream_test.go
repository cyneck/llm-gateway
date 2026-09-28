package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// runOAStream 模拟把一组 OpenAI chunk data 喂给转换器，收集所有 Anthropic 事件
func runOAStream(datas []string) []SSEEvent {
	c := NewOAStreamConverter()
	var events []SSEEvent
	for _, d := range datas {
		events = append(events, c.Convert(d)...)
	}
	events = append(events, c.Finish()...)
	return events
}

func TestOAStreamConverter_TextAndTool(t *testing.T) {
	datas := []string{
		`{"id":"1","model":"m","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`,
		`{"id":"1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`{"id":"1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]},"finish_reason":null}]}`,
		`{"id":"1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	}
	events := runOAStream(datas)

	// 校验事件序列完整性
	var sawMessageStart, sawMessageStop, sawToolBlockStart bool
	var toolArgs strings.Builder
	for _, ev := range events {
		if ev.Event == "message_start" {
			sawMessageStart = true
		}
		if ev.Event == "content_block_start" && strings.Contains(ev.Data, "tool_use") {
			sawToolBlockStart = true
		}
		if ev.Event == "content_block_delta" && strings.Contains(ev.Data, "input_json_delta") {
			var m map[string]any
			_ = json.Unmarshal([]byte(ev.Data), &m)
			delta := m["delta"].(map[string]any)
			toolArgs.WriteString(delta["partial_json"].(string))
		}
		if ev.Event == "message_stop" {
			sawMessageStop = true
		}
	}
	if !sawMessageStart || !sawMessageStop {
		t.Errorf("缺少 message_start/stop: start=%v stop=%v", sawMessageStart, sawMessageStop)
	}
	if !sawToolBlockStart {
		t.Errorf("缺少 tool_use content_block_start")
	}
	if toolArgs.String() != `{"city":"北京"}` {
		t.Errorf("tool 参数聚合错误: %s", toolArgs.String())
	}
}

func TestOAStreamConverter_ToolIndexTracking(t *testing.T) {
	// 工具先于文本出现：文本块 index 必须正确（不能错位）
	datas := []string{
		`{"id":"1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"id":"1","model":"m","choices":[{"index":0,"delta":{"content":"后到的文本"},"finish_reason":null}]}`,
		`{"id":"1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	events := runOAStream(datas)

	var textStartIdx, toolStartIdx int = -1, -1
	for _, ev := range events {
		if ev.Event == "content_block_start" {
			var m map[string]any
			_ = json.Unmarshal([]byte(ev.Data), &m)
			cb := m["content_block"].(map[string]any)
			if cb["type"] == "text" {
				textStartIdx = int(m["index"].(float64))
			}
			if cb["type"] == "tool_use" {
				toolStartIdx = int(m["index"].(float64))
			}
		}
		// 文本 delta 的 index 必须与文本块 start 一致
		if ev.Event == "content_block_delta" && strings.Contains(ev.Data, "text_delta") {
			var m map[string]any
			_ = json.Unmarshal([]byte(ev.Data), &m)
			if int(m["index"].(float64)) != textStartIdx {
				t.Errorf("text delta index 与 text block start 不一致: delta=%v textStart=%d", m["index"], textStartIdx)
			}
		}
	}
	if textStartIdx < 0 || toolStartIdx < 0 {
		t.Fatalf("缺少文本块或工具块: text=%d tool=%d", textStartIdx, toolStartIdx)
	}
	if textStartIdx == toolStartIdx {
		t.Errorf("文本块与工具块 index 冲突: %d", textStartIdx)
	}
}

func TestOAStreamConverter_Reasoning(t *testing.T) {
	datas := []string{
		`{"id":"1","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"思考"},"finish_reason":null}]}`,
		`{"id":"1","model":"m","choices":[{"index":0,"delta":{"content":"答案"},"finish_reason":null}]}`,
		`{"id":"1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	events := runOAStream(datas)

	var sawThinkingStart, sawThinkingDelta bool
	for _, ev := range events {
		if ev.Event == "content_block_start" && strings.Contains(ev.Data, "thinking") {
			sawThinkingStart = true
		}
		if ev.Event == "content_block_delta" && strings.Contains(ev.Data, "thinking_delta") {
			sawThinkingDelta = true
		}
	}
	if !sawThinkingStart || !sawThinkingDelta {
		t.Errorf("thinking 透传失败: start=%v delta=%v", sawThinkingStart, sawThinkingDelta)
	}
}

// runAOStream 模拟把一组 Anthropic 事件 data 喂给转换器，收集所有 OpenAI chunk
func runAOStream(datas []string) []SSEEvent {
	c := NewAOStreamConverter()
	var events []SSEEvent
	for _, d := range datas {
		events = append(events, c.Convert(d)...)
	}
	events = append(events, c.Finish()...)
	return events
}

func TestAOStreamConverter_TextAndTool(t *testing.T) {
	datas := []string{
		`{"type":"message_start","message":{"id":"msg_1","role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"get_weather","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"北京\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
	}
	events := runAOStream(datas)

	var sawRole, sawDone, sawToolCall bool
	var toolArgs strings.Builder
	var finishReason string
	for _, ev := range events {
		if ev.Data == "[DONE]" {
			sawDone = true
			continue
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(ev.Data), &m)
		choices := m["choices"].([]any)
		ch := choices[0].(map[string]any)
		delta := ch["delta"].(map[string]any)
		if _, ok := delta["role"]; ok {
			sawRole = true
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tcm := tc.(map[string]any)
				fn := tcm["function"].(map[string]any)
				if args, ok := fn["arguments"].(string); ok {
					sawToolCall = true
					toolArgs.WriteString(args)
				}
			}
		}
		if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
			finishReason = fr
		}
	}
	if !sawRole || !sawDone {
		t.Errorf("缺少 role chunk 或 [DONE]: role=%v done=%v", sawRole, sawDone)
	}
	if !sawToolCall {
		t.Errorf("缺少 tool_calls chunk")
	}
	if toolArgs.String() != `{"city":"北京"}` {
		t.Errorf("tool 参数聚合错误: %s", toolArgs.String())
	}
	if finishReason != "tool_calls" {
		t.Errorf("finish_reason 映射错误: %s", finishReason)
	}
}

func TestAOStreamConverter_ThinkingDelta(t *testing.T) {
	datas := []string{
		`{"type":"message_start","message":{"id":"m","role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"推理中"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`{"type":"message_stop"}`,
	}
	events := runAOStream(datas)

	var sawReasoning bool
	for _, ev := range events {
		if ev.Data == "[DONE]" {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(ev.Data), &m)
		delta := m["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if rc, ok := delta["reasoning_content"].(string); ok && rc == "推理中" {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Errorf("thinking_delta 未透传为 reasoning_content")
	}
}
