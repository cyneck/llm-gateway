package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReqResponsesToOpenAI_StringInput(t *testing.T) {
	in := map[string]any{
		"model":             "deepseek-chat",
		"instructions":      "你是助手",
		"max_output_tokens": 100,
		"input":             "你好",
	}
	out := ReqResponsesToOpenAI(in)

	if out["model"] != "deepseek-chat" {
		t.Errorf("model 丢失")
	}
	if out["max_tokens"] != 100 {
		t.Errorf("max_output_tokens -> max_tokens 失败: %v", out["max_tokens"])
	}
	msgs := out["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("instructions 应转 system: %v", msgs[0])
	}
	if msgs[1].(map[string]any)["content"] != "你好" {
		t.Errorf("字符串 input 应转 user message: %v", msgs[1])
	}
}

func TestReqResponsesToOpenAI_ArrayInput(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "查天气"}},
			},
			map[string]any{
				"type": "function_call", "call_id": "call_1",
				"name": "get_weather", "arguments": `{"city":"北京"}`,
			},
			map[string]any{
				"type": "function_call_output", "call_id": "call_1",
				"output": map[string]any{"temp": 25},
			},
		},
		"tools": []any{
			map[string]any{
				"type": "function", "name": "get_weather",
				"description": "查天气", "parameters": map[string]any{"type": "object"},
			},
		},
	}
	out := ReqResponsesToOpenAI(in)

	msgs := out["messages"].([]any)
	// 1: user message
	if m := msgs[0].(map[string]any); m["role"] != "user" || m["content"] != "查天气" {
		t.Errorf("message item 转换失败: %v", m)
	}
	// 2: function_call -> assistant + tool_calls
	m1 := msgs[1].(map[string]any)
	if m1["role"] != "assistant" {
		t.Fatalf("function_call 应转 assistant: %v", m1["role"])
	}
	tcs := m1["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Errorf("tool_call id 应为 call_id: %v", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("function 转换失败: %v", fn)
	}
	// 3: function_call_output -> tool message（output 是对象，应 JSON 序列化）
	m2 := msgs[2].(map[string]any)
	if m2["role"] != "tool" || m2["tool_call_id"] != "call_1" {
		t.Errorf("tool 消息转换失败: %v", m2)
	}
	if !strings.Contains(m2["content"].(string), `"temp":25`) {
		t.Errorf("output 对象应 JSON 序列化: %v", m2["content"])
	}
	// tools 平铺 -> 嵌套
	tools := out["tools"].([]any)
	tm := tools[0].(map[string]any)
	nested := tm["function"].(map[string]any)
	if tm["type"] != "function" || nested["name"] != "get_weather" || nested["parameters"] == nil {
		t.Errorf("tools 平铺转嵌套失败: %v", tm)
	}
}

func TestRespOpenAIToResponses(t *testing.T) {
	in := map[string]any{
		"id":    "chatcmpl-1",
		"model": "deepseek-chat",
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "答案",
					"tool_calls": []any{
						map[string]any{
							"id": "call_1", "type": "function",
							"function": map[string]any{"name": "get_weather", "arguments": `{"city":"北京"}`},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	}
	out := RespOpenAIToResponses(in)

	if out["object"] != "response" || out["status"] != "completed" {
		t.Errorf("顶层字段错误: %v", out)
	}
	output := out["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output 应含 message + function_call: %v", output)
	}
	msg := output[0].(map[string]any)
	if msg["type"] != "message" {
		t.Errorf("第一项应为 message: %v", msg)
	}
	content := msg["content"].([]any)
	if content[0].(map[string]any)["text"] != "答案" {
		t.Errorf("文本转换失败: %v", content)
	}
	fc := output[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "get_weather" {
		t.Errorf("function_call 转换失败: %v", fc)
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 10 || usage["output_tokens"] != 5 {
		t.Errorf("usage 转换失败: %v", usage)
	}
}

// runRespStream 把一组 chat chunk data 喂给 chatToResponsesStream
func runRespStream(datas []string) []SSEEvent {
	c := NewChatToResponsesStream()
	var events []SSEEvent
	for _, d := range datas {
		events = append(events, c.Convert(d)...)
	}
	events = append(events, c.Finish()...)
	return events
}

func TestChatToResponsesStream_TextFlow(t *testing.T) {
	datas := []string{
		`{"id":"cc1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"你"},"finish_reason":null}]}`,
		`{"id":"cc1","model":"m","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":null}]}`,
		`{"id":"cc1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
	}
	events := runRespStream(datas)

	var eventNames []string
	var textDeltas strings.Builder
	var completedData string
	for _, ev := range events {
		eventNames = append(eventNames, ev.Event)
		if ev.Event == "response.output_text.delta" {
			var m map[string]any
			_ = json.Unmarshal([]byte(ev.Data), &m)
			textDeltas.WriteString(m["delta"].(string))
		}
		if ev.Event == "response.completed" {
			completedData = ev.Data
		}
	}

	// 事件顺序校验：created 在最前，completed 在最后
	if eventNames[0] != "response.created" {
		t.Errorf("首事件应为 response.created: %v", eventNames)
	}
	if eventNames[len(eventNames)-1] != "response.completed" {
		t.Errorf("末事件应为 response.completed: %v", eventNames)
	}
	// 关键事件齐全
	need := map[string]bool{
		"response.created": false, "response.output_item.added": false,
		"response.content_part.added": false, "response.output_text.delta": false,
		"response.output_text.done": false, "response.content_part.done": false,
		"response.output_item.done": false, "response.completed": false,
	}
	for _, n := range eventNames {
		if _, ok := need[n]; ok {
			need[n] = true
		}
	}
	for n, saw := range need {
		if !saw {
			t.Errorf("缺少事件: %s", n)
		}
	}
	if textDeltas.String() != "你好" {
		t.Errorf("文本增量聚合错误: %s", textDeltas.String())
	}
	// completed 内含 usage
	var comp map[string]any
	_ = json.Unmarshal([]byte(completedData), &comp)
	resp := comp["response"].(map[string]any)
	usage := resp["usage"].(map[string]any)
	if orInt(usage["input_tokens"]) != 3 || orInt(usage["output_tokens"]) != 2 {
		t.Errorf("usage 透传失败: %v", usage)
	}
}

func TestChatToResponsesStream_ToolFlow(t *testing.T) {
	datas := []string{
		`{"id":"cc1","model":"m","choices":[{"index":0,"delta":{"content":"我来查"},"finish_reason":null}]}`,
		`{"id":"cc1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`{"id":"cc1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]},"finish_reason":null}]}`,
		`{"id":"cc1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	events := runRespStream(datas)

	var argsDeltas strings.Builder
	var funcItemAdded, argsDone, itemDone bool
	for _, ev := range events {
		switch ev.Event {
		case "response.output_item.added":
			if strings.Contains(ev.Data, "function_call") {
				funcItemAdded = true
			}
		case "response.function_call_arguments.delta":
			var m map[string]any
			_ = json.Unmarshal([]byte(ev.Data), &m)
			argsDeltas.WriteString(m["delta"].(string))
		case "response.function_call_arguments.done":
			var m map[string]any
			_ = json.Unmarshal([]byte(ev.Data), &m)
			if m["arguments"] == `{"city":"北京"}` {
				argsDone = true
			}
		case "response.output_item.done":
			if strings.Contains(ev.Data, "function_call") {
				itemDone = true
			}
		}
	}
	if !funcItemAdded {
		t.Errorf("缺少 function_call 的 output_item.added")
	}
	if argsDeltas.String() != `{"city":"北京"}` {
		t.Errorf("参数增量聚合错误: %s", argsDeltas.String())
	}
	if !argsDone || !itemDone {
		t.Errorf("缺少 function_call_arguments.done 或 output_item.done: argsDone=%v itemDone=%v", argsDone, itemDone)
	}
}

func TestChatToResponsesStream_AbruptFinish(t *testing.T) {
	// 上游中断（无 finish_reason、无 [DONE]）：Finish() 应补发完整收尾
	c := NewChatToResponsesStream()
	var events []SSEEvent
	events = append(events, c.Convert(`{"id":"cc1","model":"m","choices":[{"index":0,"delta":{"content":"部分"},"finish_reason":null}]}`)...)
	events = append(events, c.Finish()...)

	var sawCompleted bool
	for _, ev := range events {
		if ev.Event == "response.completed" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Errorf("异常中断未补发 response.completed")
	}
}
