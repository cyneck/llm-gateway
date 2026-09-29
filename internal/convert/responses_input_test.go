package convert

import "testing"

// msgsOf 取出转换结果里的 messages 数组
func msgsOf(t *testing.T, out map[string]any) []any {
	t.Helper()
	raw, ok := out["messages"]
	if !ok {
		t.Fatalf("转换结果没有 messages 字段: %#v", out)
	}
	msgs, ok := raw.([]any)
	if !ok {
		t.Fatalf("messages 不是数组: %#v", raw)
	}
	return msgs
}

// TestReqResponsesToOpenAI_InputWithoutType 简写写法（input 项不带 type）不能被整条丢弃，
// 否则 messages 会被清空，上游报"缺少 messages"。
func TestReqResponsesToOpenAI_InputWithoutType(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"role": "user", "content": "你好"},
		},
	}
	msgs := msgsOf(t, ReqResponsesToOpenAI(in))
	if len(msgs) != 1 {
		t.Fatalf("期望 1 条消息，实际 %d 条: %#v", len(msgs), msgs)
	}
	m, _ := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "你好" {
		t.Errorf("消息内容不对: %#v", m)
	}
}

// TestReqResponsesToOpenAI_InputStandard 标准写法（带 type=message、content 为 part 数组）行为不变。
func TestReqResponsesToOpenAI_InputStandard(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "你好"},
					map[string]any{"type": "input_text", "text": "世界"},
				},
			},
		},
	}
	msgs := msgsOf(t, ReqResponsesToOpenAI(in))
	if len(msgs) != 1 {
		t.Fatalf("期望 1 条消息，实际 %d 条", len(msgs))
	}
	m, _ := msgs[0].(map[string]any)
	if m["content"] != "你好世界" {
		t.Errorf("多 part 未正确拼接: %#v", m)
	}
}

// TestReqResponsesToOpenAI_UnknownTypeSkipped 真正未知的 type 仍然要跳过，不能被误当成普通消息。
func TestReqResponsesToOpenAI_UnknownTypeSkipped(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "reasoning", "summary": "思考中"},
		},
	}
	msgs := msgsOf(t, ReqResponsesToOpenAI(in))
	if len(msgs) != 0 {
		t.Errorf("未知 type 应被跳过，实际得到 %d 条: %#v", len(msgs), msgs)
	}
}

// TestReqResponsesToOpenAI_MixedInput 简写消息与函数调用混排时顺序和条数都要对。
func TestReqResponsesToOpenAI_MixedInput(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"role": "user", "content": "查天气"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "get_weather", "arguments": "{}"},
			map[string]any{"role": "user", "content": "再查一次"},
		},
	}
	msgs := msgsOf(t, ReqResponsesToOpenAI(in))
	if len(msgs) != 3 {
		t.Fatalf("期望 3 条消息，实际 %d 条: %#v", len(msgs), msgs)
	}
	first, _ := msgs[0].(map[string]any)
	second, _ := msgs[1].(map[string]any)
	third, _ := msgs[2].(map[string]any)
	if first["role"] != "user" || first["content"] != "查天气" {
		t.Errorf("第 1 条不对: %#v", first)
	}
	if second["role"] != "assistant" {
		t.Errorf("第 2 条应为 assistant 工具调用: %#v", second)
	}
	if third["content"] != "再查一次" {
		t.Errorf("第 3 条不对: %#v", third)
	}
}
