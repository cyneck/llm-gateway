// Package convert 实现 OpenAI Chat Completions 与 Anthropic Messages
// 两种协议间的请求/响应双向转换。全部为纯函数，无外部状态。
package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ============================================================================
// 请求转换：OpenAI Chat Completions -> Anthropic Messages
// ============================================================================

// ReqOpenAIToAnthropic 把 OpenAI 请求转成 Anthropic 请求
func ReqOpenAIToAnthropic(in map[string]any) map[string]any {
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
		blocks := contentOpenAIToAnthropicBlocks(role, content, msg["tool_calls"])
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

	return out
}

// contentOpenAIToAnthropicBlocks 转换单个消息的 content + tool_calls 为 Anthropic blocks
func contentOpenAIToAnthropicBlocks(role string, content any, toolCallsAny any) any {
	// content 为数组（多模态：text / image_url 混排）
	if arr, ok := content.([]any); ok {
		blocks := make([]any, 0, len(arr))
		for _, b := range arr {
			bm, _ := b.(map[string]any)
			if bm == nil {
				continue
			}
			switch bm["type"] {
			case "text":
				blocks = append(blocks, map[string]any{
					"type": "text", "text": orEmpty(bm["text"]),
				})
			case "image_url":
				if ib := openAIImageToAnthropic(bm); ib != nil {
					blocks = append(blocks, ib)
				}
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

// openAIImageToAnthropic 把 OpenAI image_url part 转成 Anthropic image block
func openAIImageToAnthropic(bm map[string]any) map[string]any {
	iu, _ := bm["image_url"].(map[string]any)
	if iu == nil {
		return nil
	}
	url := orEmpty(iu["url"])
	if url == "" {
		return nil
	}
	// data URL: data:image/png;base64,xxxx -> base64 source block
	if mediaType, data, ok := splitDataURL(url); ok {
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mediaType,
				"data":       data,
			},
		}
	}
	// 远程 URL
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": url},
	}
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

// ============================================================================
// 请求转换：Anthropic Messages -> OpenAI Chat Completions
// ============================================================================

// ReqAnthropicToOpenAI 把 Anthropic 请求转成 OpenAI 请求
func ReqAnthropicToOpenAI(in map[string]any) map[string]any {
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
		var imageParts []any
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
			case "image":
				if part := anthropicImageToOpenAI(bm); part != nil {
					imageParts = append(imageParts, part)
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
		} else if len(imageParts) > 0 {
			// 多模态：text + image_url 混排
			parts := make([]any, 0, len(textParts)+len(imageParts))
			if len(textParts) > 0 {
				parts = append(parts, map[string]any{
					"type": "text", "text": strings.Join(textParts, ""),
				})
			}
			parts = append(parts, imageParts...)
			openaiMsgs = append(openaiMsgs, map[string]any{"role": role, "content": parts})
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

// anthropicImageToOpenAI 把 Anthropic image block 转成 OpenAI image_url part
func anthropicImageToOpenAI(bm map[string]any) map[string]any {
	src, _ := bm["source"].(map[string]any)
	if src == nil {
		return nil
	}
	switch src["type"] {
	case "base64":
		mediaType := orEmpty(src["media_type"])
		if mediaType == "" {
			mediaType = "image/png"
		}
		url := "data:" + mediaType + ";base64," + orEmpty(src["data"])
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": url},
		}
	case "url":
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": orEmpty(src["url"])},
		}
	}
	return nil
}

// ============================================================================
// 响应转换（非流式）
// ============================================================================

// RespOpenAIToAnthropic 把 OpenAI 响应转成 Anthropic 响应
func RespOpenAIToAnthropic(in map[string]any) map[string]any {
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
			finishReason = MapOpenAIFinishToAnthropic(fr)
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

// RespAnthropicToOpenAI 把 Anthropic 响应转成 OpenAI 响应
func RespAnthropicToOpenAI(in map[string]any) map[string]any {
	out := map[string]any{
		"id":     in["id"],
		"object": "chat.completion",
		"model":  in["model"],
	}

	var textParts []string
	var reasoning string
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
		case "thinking":
			if s, ok := bm["thinking"].(string); ok {
				reasoning = s
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
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	out["choices"] = []any{
		map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": MapAnthropicStopToOpenAI(orString(in["stop_reason"])),
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

// splitDataURL 解析 data URL：data:image/png;base64,xxxx
// 返回 (mediaType, data, ok)
func splitDataURL(url string) (string, string, bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "", "", false
	}
	rest := url[len(prefix):]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	meta := rest[:comma]
	data := rest[comma+1:]
	mediaType := "image/png"
	if i := strings.Index(meta, ";"); i >= 0 {
		mediaType = meta[:i]
	} else if meta != "" {
		mediaType = meta
	}
	return mediaType, data, true
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

// MapOpenAIFinishToAnthropic finish_reason 映射
func MapOpenAIFinishToAnthropic(fr string) string {
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

// MapAnthropicStopToOpenAI stop_reason 映射
func MapAnthropicStopToOpenAI(sr string) string {
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
