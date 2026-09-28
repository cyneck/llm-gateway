package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ============================================================================
// 请求转换：OpenAI Chat Completions <-> Anthropic Messages
// ============================================================================

// convertReqOpenAIToAnthropic 把 OpenAI 请求转成 Anthropic 请求
func convertReqOpenAIToAnthropic(in map[string]any) map[string]any {
	out := map[string]any{}

	if v, ok := in["model"]; ok {
		out["model"] = v
	}
	if v, ok := in["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := in["top_p"]; ok {
		out["top_p"] = v
	}
	// max_tokens：OpenAI 可能用 max_tokens 或 max_completion_tokens
	if v, ok := in["max_completion_tokens"]; ok {
		out["max_tokens"] = v
	} else if v, ok := in["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := in["stream"]; ok {
		out["stream"] = v
	}
	if v, ok := in["stop"]; ok {
		out["stop_sequences"] = v
	}

	// system 提示词提取
	var systemParts []string
	var anthMessages []any

	msgs, _ := in["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		role, _ := msg["role"].(string)
		content := msg["content"]

		switch role {
		case "system":
			systemParts = append(systemParts, stringifyContent(content))
			continue
		case "tool":
			// OpenAI tool 消息 -> Anthropic user + tool_result
			anthMessages = append(anthMessages, map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": msg["tool_call_id"],
						"content":     stringifyContent(content),
					},
				},
			})
			continue
		}

		// user / assistant
		blocks := convertContentOpenAIToAnthropic(role, content, msg["tool_calls"])
		anthMessages = append(anthMessages, map[string]any{"role": role, "content": blocks})
	}

	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	out["messages"] = anthMessages

	// tools 转换
	if tools, ok := in["tools"].([]any); ok {
		var anthTools []any
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			fn, _ := tm["function"].(map[string]any)
			if fn == nil {
				continue
			}
			anthTools = append(anthTools, map[string]any{
				"name":         fn["name"],
				"description":  orEmpty(fn["description"]),
				"input_schema": orEmptyMap(fn["parameters"]),
			})
		}
		if len(anthTools) > 0 {
			out["tools"] = anthTools
		}
	}

	// 移除不支持/多余的字段
	delete(out, "max_completion_tokens")

	return out
}

// convertContentOpenAIToAnthropic 转换单个消息的 content + tool_calls
func convertContentOpenAIToAnthropic(role string, content any, toolCallsAny any) any {
	// 若 content 本身已是数组（部分 OpenAI 兼容后端），直接透传基础文本
	if arr, ok := content.([]any); ok {
		blocks := make([]any, 0, len(arr))
		for _, b := range arr {
			bm, _ := b.(map[string]any)
			if bm != nil && bm["type"] == "text" {
				blocks = append(blocks, bm)
			}
		}
		blocks = append(blocks, toolCallsToBlocks(toolCallsAny)...)
		return blocks
	}

	blocks := make([]any, 0, 2)
	if s := stringifyContent(content); s != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": s})
	}
	blocks = append(blocks, toolCallsToBlocks(toolCallsAny)...)
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return blocks
}

// toolCallsToBlocks 把 OpenAI tool_calls 转成 Anthropic tool_use blocks
func toolCallsToBlocks(toolCallsAny any) []any {
	toolCalls, _ := toolCallsAny.([]any)
	blocks := make([]any, 0, len(toolCalls))
	for _, tc := range toolCalls {
		tcm, _ := tc.(map[string]any)
		fn, _ := tcm["function"].(map[string]any)
		if fn == nil {
			continue
		}
		input := map[string]any{}
		if argsStr, ok := fn["arguments"].(string); ok && argsStr != "" {
			_ = json.Unmarshal([]byte(argsStr), &input)
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    tcm["id"],
			"name":  fn["name"],
			"input": input,
		})
	}
	return blocks
}

// convertReqAnthropicToOpenAI 把 Anthropic 请求转成 OpenAI 请求
func convertReqAnthropicToOpenAI(in map[string]any) map[string]any {
	out := map[string]any{}

	if v, ok := in["model"]; ok {
		out["model"] = v
	}
	if v, ok := in["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := in["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := in["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := in["stream"]; ok {
		out["stream"] = v
	}
	if v, ok := in["stop_sequences"]; ok {
		out["stop"] = v
	}

	var openaiMsgs []any

	// system 字段 -> system message
	if sys, ok := in["system"]; ok {
		openaiMsgs = append(openaiMsgs, map[string]any{
			"role": "system", "content": stringifyContent(sys),
		})
	}

	msgs, _ := in["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		role, _ := msg["role"].(string)
		content := msg["content"]

		// content 是字符串
		if _, ok := content.(string); ok {
			openaiMsgs = append(openaiMsgs, map[string]any{"role": role, "content": content})
			continue
		}

		// content 是 blocks 数组
		blocks, _ := content.([]any)
		var textParts []string
		var toolCalls []any
		var toolResults []map[string]any

		for _, b := range blocks {
			bm, _ := b.(map[string]any)
			bt, _ := bm["type"].(string)
			switch bt {
			case "text":
				if s, ok := bm["text"].(string); ok {
					textParts = append(textParts, s)
				}
			case "tool_use":
				inputJSON, _ := json.Marshal(bm["input"])
				toolCalls = append(toolCalls, map[string]any{
					"id":   bm["id"],
					"type": "function",
					"function": map[string]any{
						"name":      bm["name"],
						"arguments": string(inputJSON),
					},
				})
			case "tool_result":
				toolResults = append(toolResults, map[string]any{
					"role":         "tool",
					"tool_call_id": bm["tool_use_id"],
					"content":      stringifyContent(bm["content"]),
				})
			}
		}

		if role == "assistant" && len(toolCalls) > 0 {
			msgObj := map[string]any{
				"role":       role,
				"content":    strings.Join(textParts, ""),
				"tool_calls": toolCalls,
			}
			openaiMsgs = append(openaiMsgs, msgObj)
		} else if len(textParts) > 0 {
			openaiMsgs = append(openaiMsgs, map[string]any{"role": role, "content": strings.Join(textParts, "")})
		}
		// tool_result 消息（Anthropic 里挂在 user role 下）
		for _, tr := range toolResults {
			openaiMsgs = append(openaiMsgs, tr)
		}
	}

	out["messages"] = openaiMsgs

	// tools 转换
	if tools, ok := in["tools"].([]any); ok {
		var openaiTools []any
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			openaiTools = append(openaiTools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tm["name"],
					"description": orEmpty(tm["description"]),
					"parameters":  orEmptyMap(tm["input_schema"]),
				},
			})
		}
		if len(openaiTools) > 0 {
			out["tools"] = openaiTools
		}
	}

	return out
}

// ============================================================================
// 响应转换（非流式）
// ============================================================================

// convertRespOpenAIToAnthropic 把 OpenAI 响应转成 Anthropic 响应
func convertRespOpenAIToAnthropic(in map[string]any) map[string]any {
	out := map[string]any{
		"id":    in["id"],
		"type":  "message",
		"role":  "assistant",
		"model": in["model"],
	}

	var content []any
	finishReason := "end_turn"

	choices, _ := in["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		msg, _ := choice["message"].(map[string]any)
		if fr, ok := choice["finish_reason"].(string); ok {
			finishReason = mapOpenAIFinishToAnthropic(fr)
		}

		if c, ok := msg["content"].(string); ok && c != "" {
			content = append(content, map[string]any{"type": "text", "text": c})
		}
		// reasoning 内容（DeepSeek R1 等）
		if r, ok := msg["reasoning_content"].(string); ok && r != "" {
			content = append([]any{map[string]any{"type": "thinking", "thinking": r}}, content...)
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tcm, _ := tc.(map[string]any)
				fn, _ := tcm["function"].(map[string]any)
				input := map[string]any{}
				if argsStr, ok := fn["arguments"].(string); ok && argsStr != "" {
					_ = json.Unmarshal([]byte(argsStr), &input)
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    tcm["id"],
					"name":  fn["name"],
					"input": input,
				})
			}
		}
	}

	out["content"] = content
	out["stop_reason"] = finishReason
	out["stop_sequence"] = nil

	if usage, ok := in["usage"].(map[string]any); ok {
		out["usage"] = map[string]any{
			"input_tokens":  orInt(usage["prompt_tokens"]),
			"output_tokens": orInt(usage["completion_tokens"]),
		}
	}
	return out
}

// convertRespAnthropicToOpenAI 把 Anthropic 响应转成 OpenAI 响应
func convertRespAnthropicToOpenAI(in map[string]any) map[string]any {
	out := map[string]any{
		"id":     in["id"],
		"object": "chat.completion",
		"model":  in["model"],
	}

	var textParts []string
	var toolCalls []any

	content, _ := in["content"].([]any)
	for _, b := range content {
		bm, _ := b.(map[string]any)
		bt, _ := bm["type"].(string)
		switch bt {
		case "text":
			if s, ok := bm["text"].(string); ok {
				textParts = append(textParts, s)
			}
		case "tool_use":
			inputJSON, _ := json.Marshal(bm["input"])
			toolCalls = append(toolCalls, map[string]any{
				"id":   bm["id"],
				"type": "function",
				"function": map[string]any{
					"name":      bm["name"],
					"arguments": string(inputJSON),
				},
			})
		}
	}

	msg := map[string]any{"role": "assistant", "content": strings.Join(textParts, "")}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	out["choices"] = []any{
		map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": mapAnthropicStopToOpenAI(orString(in["stop_reason"])),
		},
	}

	if usage, ok := in["usage"].(map[string]any); ok {
		out["usage"] = map[string]any{
			"prompt_tokens":     orInt(usage["input_tokens"]),
			"completion_tokens": orInt(usage["output_tokens"]),
			"total_tokens":      orInt(usage["input_tokens"]) + orInt(usage["output_tokens"]),
		}
	}
	return out
}

// ============================================================================
// 工具函数
// ============================================================================

func stringifyContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case nil:
		return ""
	case []any:
		var sb strings.Builder
		for _, b := range c {
			bm, _ := b.(map[string]any)
			if bm != nil && bm["type"] == "text" {
				if s, ok := bm["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		return sb.String()
	default:
		data, _ := json.Marshal(v)
		return string(data)
	}
}

func orEmpty(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func orEmptyMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func orInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func orString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func mapOpenAIFinishToAnthropic(fr string) string {
	switch fr {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func mapAnthropicStopToOpenAI(sr string) string {
	switch sr {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

// mustJSON 序列化（内部用，忽略错误）
func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(data)
}
