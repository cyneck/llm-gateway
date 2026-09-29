// OpenAI Responses API（/v1/responses，Codex wire_api=responses 依赖）与
// OpenAI Chat Completions 之间的转换。作为第三种入口协议，
// 内部先归一到 Chat Completions，再复用现有 OpenAI ↔ Anthropic 转换链路。
package convert

import (
	"encoding/json"
	"sort"
	"strings"
)

// ============================================================================
// 请求转换：OpenAI Responses -> OpenAI Chat Completions
// ============================================================================

// ReqResponsesToOpenAI 把 Responses 请求转成 Chat Completions 请求
func ReqResponsesToOpenAI(in map[string]any) map[string]any {
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
	if v, ok := in["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := in["stream"]; ok {
		out["stream"] = v
	}
	if v, ok := in["tool_choice"]; ok {
		out["tool_choice"] = v
	}

	var msgs []any

	// instructions -> system message
	if ins, ok := in["instructions"]; ok {
		if s := stringifyContent(ins); s != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": s})
		}
	}

	// input：字符串或 item 数组
	switch input := in["input"].(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": input})
	case []any:
		for _, item := range input {
			im, _ := item.(map[string]any)
			if im == nil {
				continue
			}
			switch im["type"] {
			case "message":
				// {"type":"message","role":"...","content":[{"type":"input_text"|"output_text"|"text","text":...}]}
				// 多 part 拼接为一条字符串 content 消息
				role, _ := im["role"].(string)
				if role == "" {
					role = "user"
				}
				var parts []string
				content, _ := im["content"].([]any)
				for _, c := range content {
					cm, _ := c.(map[string]any)
					if cm == nil {
						continue
					}
					switch cm["type"] {
					case "input_text", "output_text", "text":
						parts = append(parts, orEmpty(cm["text"]))
					}
				}
				msgs = append(msgs, map[string]any{"role": role, "content": strings.Join(parts, "")})

			case "function_call":
				// Responses 平铺的函数调用 -> assistant 消息 + tool_calls
				msgs = append(msgs, map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{
						map[string]any{
							"id":   orEmpty(im["call_id"]),
							"type": "function",
							"function": map[string]any{
								"name":      orEmpty(im["name"]),
								"arguments": orEmpty(im["arguments"]),
							},
						},
					},
				})

			case "function_call_output":
				// 函数执行结果 -> tool 消息；output 非字符串（对象）时 JSON 序列化
				var outStr string
				switch o := im["output"].(type) {
				case string:
					outStr = o
				case nil:
					outStr = ""
				default:
					outStr = mustJSON(o)
				}
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": orEmpty(im["call_id"]),
					"content":      outStr,
				})
			}
			// 未知 type 跳过
		}
	}

	out["messages"] = msgs

	// tools：Responses 平铺格式 -> OpenAI 嵌套格式
	if tools, ok := in["tools"].([]any); ok {
		var oaTools []any
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			if tm == nil || tm["type"] != "function" {
				continue
			}
			name := orEmpty(tm["name"])
			if name == "" {
				continue
			}
			oaTools = append(oaTools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": orEmpty(tm["description"]),
					"parameters":  orEmptyMap(tm["parameters"]),
				},
			})
		}
		if len(oaTools) > 0 {
			out["tools"] = oaTools
		}
	}

	return out
}

// ============================================================================
// 响应转换（非流式）：OpenAI Chat Completions -> OpenAI Responses
// ============================================================================

// RespOpenAIToResponses 把 OpenAI Chat 响应转成 Responses 响应
func RespOpenAIToResponses(in map[string]any) map[string]any {
	id := "resp_" + orString(in["id"])
	if id == "resp_" {
		id = "resp_" + newID()
	}

	out := map[string]any{
		"id":     id,
		"object": "response",
		"model":  in["model"],
		"status": "completed",
	}

	var output []any
	choices, _ := in["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		msg, _ := choice["message"].(map[string]any)

		// 文本内容 -> message item
		if c, ok := msg["content"].(string); ok && c != "" {
			output = append(output, map[string]any{
				"type": "message",
				"id":   "msg_" + newID(),
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": c, "annotations": []any{}},
				},
			})
		}
		// tool_calls -> function_call items
		if tcs, ok := msg["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tcm, _ := tc.(map[string]any)
				fn, _ := tcm["function"].(map[string]any)
				if fn == nil {
					continue
				}
				callID := orEmpty(tcm["id"])
				output = append(output, map[string]any{
					"type":      "function_call",
					"id":        "fc_" + callID,
					"call_id":   callID,
					"name":      fn["name"],
					"arguments": orEmpty(fn["arguments"]),
					"status":    "completed",
				})
			}
		}
		// reasoning_content 忽略（Codex 不依赖）
	}
	if output == nil {
		output = []any{}
	}
	out["output"] = output

	if usage, ok := in["usage"].(map[string]any); ok {
		pt, ct := orInt(usage["prompt_tokens"]), orInt(usage["completion_tokens"])
		total := orInt(usage["total_tokens"])
		if total == 0 {
			total = pt + ct
		}
		out["usage"] = map[string]any{
			"input_tokens":  pt,
			"output_tokens": ct,
			"total_tokens":  total,
		}
	} else {
		out["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	return out
}

// ============================================================================
// 流式转换：OpenAI Chat Completions stream -> OpenAI Responses SSE 事件流
// ============================================================================

// respMsgItem 文本 message item 的流式状态
type respMsgItem struct {
	outputIndex int
	id          string
}

// respToolItem 单个 function_call item 的流式状态
type respToolItem struct {
	outputIndex int
	id          string // "fc_" 前缀 id
	callID      string // Responses call_id（即 OpenAI tool_call id）
	name        string
	args        strings.Builder
	added       bool // 是否已发出 output_item.added
}

// chatToResponsesStream 把 OpenAI chat chunk 流转成 Responses SSE 事件流。
// 事件顺序严格对齐 Codex 依赖：response.created -> output_item.added ->
// content_part.added -> output_text.delta* -> (.done 系列) -> response.completed
type chatToResponsesStream struct {
	started bool
	finished bool
	respID  string
	model   string
	usage   map[string]any

	msgItem *respMsgItem
	textBuf strings.Builder

	tools         []*respToolItem
	toolByOAIndex map[int]*respToolItem // openai tool_call index -> item

	nextOutIdx int // 下一个可用的 output_index
}

// NewChatToResponsesStream 创建 chat chunk -> Responses 事件 的流式转换器
func NewChatToResponsesStream() *chatToResponsesStream {
	return &chatToResponsesStream{toolByOAIndex: map[int]*respToolItem{}}
}

// nextOutputIndex 分配下一个 output_index
func (c *chatToResponsesStream) nextOutputIndex() int {
	idx := c.nextOutIdx
	c.nextOutIdx++
	return idx
}

// Convert 输入一行 OpenAI chunk 的 data（JSON 字符串，或 "[DONE]"），返回 Responses 事件列表
func (c *chatToResponsesStream) Convert(data string) []SSEEvent {
	if data == "[DONE]" {
		return c.Finish()
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil
	}

	var out []SSEEvent

	// 首个 chunk：response.created
	if !c.started {
		c.started = true
		id := orString(chunk["id"])
		if id == "" {
			id = newID()
		}
		c.respID = "resp_" + id
		c.model = orString(chunk["model"])
		out = append(out, SSEEvent{Event: "response.created", Data: mustJSON(map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id": c.respID, "object": "response", "model": c.model, "output": []any{},
			},
		})})
	}

	// 最后一个 chunk 可能携带 usage
	if u, ok := chunk["usage"].(map[string]any); ok {
		pt, ct := orInt(u["prompt_tokens"]), orInt(u["completion_tokens"])
		total := orInt(u["total_tokens"])
		if total == 0 {
			total = pt + ct
		}
		c.usage = map[string]any{"input_tokens": pt, "output_tokens": ct, "total_tokens": total}
	}

	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return out
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)

	// 文本增量
	if content, ok := delta["content"].(string); ok && content != "" {
		if c.msgItem == nil {
			c.msgItem = &respMsgItem{outputIndex: c.nextOutputIndex(), id: "msg_" + newID()}
			out = append(out,
				SSEEvent{Event: "response.output_item.added", Data: mustJSON(map[string]any{
					"type":         "response.output_item.added",
					"output_index": c.msgItem.outputIndex,
					"item":         map[string]any{"type": "message", "id": c.msgItem.id, "role": "assistant", "content": []any{}},
				})},
				SSEEvent{Event: "response.content_part.added", Data: mustJSON(map[string]any{
					"type":          "response.content_part.added",
					"item_id":       c.msgItem.id,
					"output_index":  c.msgItem.outputIndex,
					"content_index": 0,
					"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				})},
			)
		}
		c.textBuf.WriteString(content)
		out = append(out, SSEEvent{Event: "response.output_text.delta", Data: mustJSON(map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       c.msgItem.id,
			"output_index":  c.msgItem.outputIndex,
			"content_index": 0,
			"delta":         content,
		})})
	}

	// 工具调用增量
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			tcm, _ := tc.(map[string]any)
			oaIdx := orInt(tcm["index"])
			tool := c.toolByOAIndex[oaIdx]
			if tool == nil {
				tool = &respToolItem{outputIndex: c.nextOutputIndex()}
				c.toolByOAIndex[oaIdx] = tool
				c.tools = append(c.tools, tool)
			}
			fn, _ := tcm["function"].(map[string]any)

			// 首个 delta 携带 id 与 name：发 output_item.added
			if id, ok := tcm["id"].(string); ok && id != "" && tool.callID == "" {
				tool.callID = id
			}
			if fn != nil {
				if name, ok := fn["name"].(string); ok && name != "" && tool.name == "" {
					tool.name = name
				}
			}
			if !tool.added && (tool.callID != "" || tool.name != "") {
				tool.added = true
				if tool.callID == "" {
					tool.callID = "call_" + newID()
				}
				tool.id = "fc_" + tool.callID
				out = append(out, SSEEvent{Event: "response.output_item.added", Data: mustJSON(map[string]any{
					"type":         "response.output_item.added",
					"output_index": tool.outputIndex,
					"item": map[string]any{
						"type": "function_call", "id": tool.id, "call_id": tool.callID,
						"name": tool.name, "arguments": "",
					},
				})})
			}
			// 参数增量（added 前先缓冲，避免乱序丢内容）
			if fn != nil {
				if args, ok := fn["arguments"].(string); ok && args != "" {
					tool.args.WriteString(args)
					if tool.added {
						out = append(out, SSEEvent{Event: "response.function_call_arguments.delta", Data: mustJSON(map[string]any{
							"type":         "response.function_call_arguments.delta",
							"item_id":      tool.id,
							"output_index": tool.outputIndex,
							"delta":        args,
						})})
					}
				}
			}
		}
	}

	// 结束
	if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
		out = append(out, c.Finish()...)
		return out
	}

	return out
}

// Finish 关闭所有未完成的 item 并补发 response.completed（上游异常中断时亦由此兜底）
func (c *chatToResponsesStream) Finish() []SSEEvent {
	if c.finished {
		return nil
	}
	c.finished = true

	var out []SSEEvent

	// 上游中断且从未收到任何 chunk：仍补发 created，保证客户端状态机完整
	if !c.started {
		c.started = true
		c.respID = "resp_" + newID()
		out = append(out, SSEEvent{Event: "response.created", Data: mustJSON(map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": c.respID, "object": "response", "model": "", "output": []any{}},
		})})
	}

	// 关闭文本 item：output_text.done -> content_part.done -> output_item.done
	if c.msgItem != nil {
		full := c.textBuf.String()
		out = append(out,
			SSEEvent{Event: "response.output_text.done", Data: mustJSON(map[string]any{
				"type":          "response.output_text.done",
				"item_id":       c.msgItem.id,
				"output_index":  c.msgItem.outputIndex,
				"content_index": 0,
				"text":          full,
			})},
			SSEEvent{Event: "response.content_part.done", Data: mustJSON(map[string]any{
				"type":          "response.content_part.done",
				"item_id":       c.msgItem.id,
				"output_index":  c.msgItem.outputIndex,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
			})},
			SSEEvent{Event: "response.output_item.done", Data: mustJSON(map[string]any{
				"type":         "response.output_item.done",
				"output_index": c.msgItem.outputIndex,
				"item": map[string]any{
					"type": "message", "id": c.msgItem.id, "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": full, "annotations": []any{}}},
				},
			})},
		)
	}

	// 关闭工具 items：function_call_arguments.done -> output_item.done
	for _, tool := range c.tools {
		if !tool.added {
			continue // 从未收到 id/name 的残缺工具，跳过
		}
		args := tool.args.String()
		out = append(out,
			SSEEvent{Event: "response.function_call_arguments.done", Data: mustJSON(map[string]any{
				"type":         "response.function_call_arguments.done",
				"item_id":      tool.id,
				"output_index": tool.outputIndex,
				"arguments":    args,
			})},
			SSEEvent{Event: "response.output_item.done", Data: mustJSON(map[string]any{
				"type":         "response.output_item.done",
				"output_index": tool.outputIndex,
				"item": map[string]any{
					"type": "function_call", "id": tool.id, "call_id": tool.callID,
					"name": tool.name, "arguments": args,
				},
			})},
		)
	}

	// completed 的 output 数组：按 output_index 升序（文本与工具交错出现时顺序仍正确）
	type indexed struct {
		idx  int
		item map[string]any
	}
	var items []indexed
	if c.msgItem != nil {
		items = append(items, indexed{c.msgItem.outputIndex, map[string]any{
			"type": "message", "id": c.msgItem.id, "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": c.textBuf.String(), "annotations": []any{}}},
		}})
	}
	for _, tool := range c.tools {
		if !tool.added {
			continue
		}
		items = append(items, indexed{tool.outputIndex, map[string]any{
			"type": "function_call", "id": tool.id, "call_id": tool.callID,
			"name": tool.name, "arguments": tool.args.String(),
		}})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].idx < items[j].idx })
	output := make([]any, 0, len(items))
	for _, it := range items {
		output = append(output, it.item)
	}

	if c.usage == nil {
		c.usage = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}

	out = append(out, SSEEvent{Event: "response.completed", Data: mustJSON(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": c.respID, "object": "response", "status": "completed",
			"model": c.model, "output": output, "usage": c.usage,
		},
	})})
	return out
}
