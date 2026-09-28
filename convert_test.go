package main

import (
	"encoding/json"
	"testing"
)

func mustParse(t *testing.T, s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	return m
}

func TestConvertReqOpenAIToAnthropic_Basic(t *testing.T) {
	in := mustParse(t, `{
		"model": "claude-x",
		"messages": [
			{"role": "system", "content": "你是助手"},
			{"role": "user", "content": "你好"}
		],
		"max_tokens": 100,
		"stream": false
	}`)
	out := convertReqOpenAIToAnthropic(in)

	if out["system"] != "你是助手" {
		t.Errorf("system 提取失败: %v", out["system"])
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("消息数量错误: %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("role 错误: %v", first["role"])
	}
	blocks := first["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "你好" {
		t.Errorf("content 转换错误: %v", blocks)
	}
}

func TestConvertReqOpenAIToAnthropic_ToolCalls(t *testing.T) {
	in := mustParse(t, `{
		"model": "claude-x",
		"messages": [
			{"role": "user", "content": "列目录"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "bash", "arguments": "{\"command\":\"ls\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "file1.txt"}
		],
		"tools": [
			{"type": "function", "function": {"name": "bash", "description": "运行命令", "parameters": {"type":"object"}}}
		]
	}`)
	out := convertReqOpenAIToAnthropic(in)

	msgs := out["messages"].([]any)
	// user + assistant + tool_result
	if len(msgs) != 3 {
		t.Fatalf("消息数量错误: %d", len(msgs))
	}

	assistant := msgs[1].(map[string]any)
	blocks := assistant["content"].([]any)
	toolUse := blocks[0].(map[string]any)
	if toolUse["type"] != "tool_use" {
		t.Fatalf("tool_use 转换失败: %v", toolUse)
	}
	if toolUse["name"] != "bash" {
		t.Errorf("工具名错误: %v", toolUse["name"])
	}
	input := toolUse["input"].(map[string]any)
	if input["command"] != "ls" {
		t.Errorf("工具参数错误: %v", input)
	}

	toolResult := msgs[2].(map[string]any)
	if toolResult["role"] != "user" {
		t.Errorf("tool_result role 错误: %v", toolResult["role"])
	}

	tools := out["tools"].([]any)
	firstTool := tools[0].(map[string]any)
	if firstTool["input_schema"] == nil {
		t.Errorf("tools input_schema 转换失败")
	}
}

func TestConvertReqAnthropicToOpenAI(t *testing.T) {
	in := mustParse(t, `{
		"model": "deepseek-chat",
		"system": "你是助手",
		"messages": [
			{"role": "user", "content": "列目录"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "好的"},
				{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": {"command": "ls"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "file1.txt"}
			]}
		],
		"max_tokens": 100
	}`)
	out := convertReqAnthropicToOpenAI(in)

	msgs := out["messages"].([]any)
	// system + user + assistant + tool
	if len(msgs) != 4 {
		t.Fatalf("消息数量错误: %d (实际 %v)", len(msgs), msgs)
	}

	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "你是助手" {
		t.Errorf("system 转换失败")
	}

	assistant := msgs[2].(map[string]any)
	toolCalls := assistant["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls 数量错误: %d", len(toolCalls))
	}
	fn := toolCalls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Errorf("工具名错误")
	}

	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" {
		t.Errorf("tool 消息转换失败: %v", tool)
	}
}

func TestConvertRespOpenAIToAnthropic(t *testing.T) {
	in := mustParse(t, `{
		"id": "chatcmpl-1",
		"model": "claude-x",
		"choices": [{"message": {"role": "assistant", "content": "你好"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5}
	}`)
	out := convertRespOpenAIToAnthropic(in)

	if out["type"] != "message" || out["role"] != "assistant" {
		t.Errorf("响应结构错误: %v", out)
	}
	content := out["content"].([]any)
	if content[0].(map[string]any)["text"] != "你好" {
		t.Errorf("content 转换失败")
	}
	if out["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason 错误: %v", out["stop_reason"])
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 10 || usage["output_tokens"] != 5 {
		t.Errorf("usage 转换失败: %v", usage)
	}
}

func TestConvertRespAnthropicToOpenAI(t *testing.T) {
	in := mustParse(t, `{
		"id": "msg_1",
		"model": "deepseek-chat",
		"content": [
			{"type": "text", "text": "你好"},
			{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": {"command": "ls"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`)
	out := convertRespAnthropicToOpenAI(in)

	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content 转换失败: %v", msg["content"])
	}
	toolCalls := msg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls 数量错误")
	}
	finish := choices[0].(map[string]any)["finish_reason"]
	if finish != "tool_calls" {
		t.Errorf("finish_reason 错误: %v", finish)
	}
}
