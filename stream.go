package main

import (
	"encoding/json"
	"strings"
)

// sseEvent 一个 SSE 事件：event 名 + data 内容
type sseEvent struct {
	event string
	data  string
}

// ============================================================================
// 流式转换：OpenAI Chat Completions stream -> Anthropic Messages stream
// ============================================================================

type oaStreamConverter struct {
	started    bool
	finished   bool
	textOpen   bool
	nextBlock  int // Anthropic content block index
	tools      []*oaTool
	toolByName map[int]*oaTool // openai tool index -> accum
}

type oaTool struct {
	openaiIndex int
	id          string
	name        string
	args        string
	blockIndex  int
	started     bool
}

func newOAStreamConverter() *oaStreamConverter {
	return &oaStreamConverter{toolByName: map[int]*oaTool{}}
}

// convert 输入一行 OpenAI chunk 的 data（JSON 字符串，或 "[DONE]"），返回 Anthropic 事件列表
func (c *oaStreamConverter) convert(data string) []sseEvent {
	if data == "[DONE]" {
		c.finished = true
		return c.finish()
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil
	}

	var out []sseEvent

	// message_start（首个 chunk）
	if !c.started {
		c.started = true
		msgID := orString(chunk["id"])
		if msgID == "" {
			msgID = "msg_" + randID()
		}
		out = append(out, sseEvent{event: "message_start", data: mustJSON(map[string]any{
			"type":    "message_start",
			"message": map[string]any{"id": msgID, "role": "assistant", "content": []any{}, "model": chunk["model"]},
		})})
	}

	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return out
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)

	// 文本增量
	if content, ok := delta["content"].(string); ok && content != "" {
		if !c.textOpen {
			c.textOpen = true
			out = append(out, sseEvent{event: "content_block_start", data: mustJSON(map[string]any{
				"type": "content_block_start", "index": c.nextBlock,
				"content_block": map[string]any{"type": "text", "text": ""},
			})})
			c.nextBlock++
		}
		out = append(out, sseEvent{event: "content_block_delta", data: mustJSON(map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": content},
		})})
	}

	// 工具调用增量
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			tcm, _ := tc.(map[string]any)
			idx := orInt(tcm["index"])
			tool := c.toolByName[idx]
			if tool == nil {
				fn, _ := tcm["function"].(map[string]any)
				tool = &oaTool{openaiIndex: idx, id: orString(tcm["id"]), name: orString(fn["name"])}
				c.toolByName[idx] = tool
				c.tools = append(c.tools, tool)
			}
			if fn, ok := tcm["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" && tool.name == "" {
					tool.name = name
				}
				if args, ok := fn["arguments"].(string); ok {
					tool.args += args
				}
			}
		}
	}

	// 结束
	if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
		out = append(out, c.finish()...)
		c.finished = true
		return out
	}

	return out
}

// finish 补发所有未关闭的 block 和收尾事件
func (c *oaStreamConverter) finish() []sseEvent {
	if c.finished {
		return nil
	}
	c.finished = true

	var out []sseEvent
	if c.textOpen {
		out = append(out, sseEvent{event: "content_block_stop", data: mustJSON(map[string]any{
			"type": "content_block_stop", "index": 0,
		})})
		c.textOpen = false
	}
	// 逐个工具块：start -> input_json_delta -> stop
	for _, tool := range c.tools {
		blockIdx := c.nextBlock
		c.nextBlock++
		out = append(out, sseEvent{event: "content_block_start", data: mustJSON(map[string]any{
			"type": "content_block_start", "index": blockIdx,
			"content_block": map[string]any{"type": "tool_use", "id": tool.id, "name": tool.name, "input": map[string]any{}},
		})})
		args := tool.args
		if args == "" {
			args = "{}"
		}
		out = append(out, sseEvent{event: "content_block_delta", data: mustJSON(map[string]any{
			"type": "content_block_delta", "index": blockIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})})
		out = append(out, sseEvent{event: "content_block_stop", data: mustJSON(map[string]any{
			"type": "content_block_stop", "index": blockIdx,
		})})
	}

	stopReason := "end_turn"
	if len(c.tools) > 0 {
		stopReason = "tool_use"
	}
	out = append(out, sseEvent{event: "message_delta", data: mustJSON(map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
	})})
	out = append(out, sseEvent{event: "message_stop", data: mustJSON(map[string]any{"type": "message_stop"})})
	return out
}

// ============================================================================
// 流式转换：Anthropic Messages stream -> OpenAI Chat Completions stream
// ============================================================================

type aoStreamConverter struct {
	started        bool
	finished       bool
	stopReason     string
	textOpen       bool
	toolIndexCount int
	anthToOATool   map[int]int // anthropic block index -> openai tool index
	nextIndex      int
}

func newAOStreamConverter() *aoStreamConverter {
	return &aoStreamConverter{anthToOATool: map[int]int{}}
}

// convert 输入一个 Anthropic 事件的 data JSON，返回 OpenAI chunk 事件列表
func (c *aoStreamConverter) convert(data string) []sseEvent {
	var ev map[string]any
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil
	}
	typ, _ := ev["type"].(string)

	var out []sseEvent

	switch typ {
	case "message_start":
		if !c.started {
			c.started = true
			msg, _ := ev["message"].(map[string]any)
			out = append(out, c.chunk(map[string]any{"role": "assistant", "content": ""}, ""))
			_ = msg
		}

	case "content_block_start":
		cb, _ := ev["content_block"].(map[string]any)
		idx := orInt(ev["index"])
		switch cb["type"] {
		case "text":
			c.textOpen = true
		case "tool_use":
			oaIdx := c.nextIndex
			c.nextIndex++
			c.anthToOATool[idx] = oaIdx
			out = append(out, c.chunk(map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": oaIdx, "id": cb["id"], "type": "function",
						"function": map[string]any{"name": cb["name"], "arguments": ""},
					},
				},
			}, ""))
		}

	case "content_block_delta":
		delta, _ := ev["delta"].(map[string]any)
		idx := orInt(ev["index"])
		switch delta["type"] {
		case "text_delta":
			if t, ok := delta["text"].(string); ok && t != "" {
				out = append(out, c.chunk(map[string]any{"content": t}, ""))
			}
		case "input_json_delta":
			if pj, ok := delta["partial_json"].(string); ok && pj != "" {
				if oaIdx, found := c.anthToOATool[idx]; found {
					out = append(out, c.chunk(map[string]any{
						"tool_calls": []any{
							map[string]any{"index": oaIdx, "function": map[string]any{"arguments": pj}},
						},
					}, ""))
				}
			}
		case "thinking_delta":
			// 思维链暂不映射到 OpenAI 标准字段，忽略
		}

	case "content_block_stop":
		idx := orInt(ev["index"])
		if _, isTool := c.anthToOATool[idx]; !isTool {
			c.textOpen = false
		}

	case "message_delta":
		if d, ok := ev["delta"].(map[string]any); ok {
			c.stopReason = orString(d["stop_reason"])
		}

	case "message_stop":
		fr := mapAnthropicStopToOpenAI(c.stopReason)
		out = append(out, c.chunk(map[string]any{}, fr))
		out = append(out, sseEvent{data: "[DONE]"})
		c.finished = true
	}

	return out
}

// chunk 构造一个 OpenAI chat.completion.chunk
func (c *aoStreamConverter) chunk(delta map[string]any, finishReason string) sseEvent {
	obj := map[string]any{
		"id":      "chatcmpl-" + randID(),
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   "",
		"choices": []any{
			map[string]any{"index": 0, "delta": delta, "finish_reason": nil},
		},
	}
	if finishReason != "" {
		obj["choices"] = []any{
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason},
		}
	}
	return sseEvent{data: mustJSON(obj)}
}

// finish 若上游异常中断，补发收尾
func (c *aoStreamConverter) finish() []sseEvent {
	if c.finished {
		return nil
	}
	c.finished = true
	fr := mapAnthropicStopToOpenAI(c.stopReason)
	return []sseEvent{
		c.chunk(map[string]any{}, fr),
		sseEvent{data: "[DONE]"},
	}
}

// randID 生成简单随机 ID（避免引入额外依赖）
func randID() string {
	return strings.TrimPrefix(newID(), "")
}
