package convert

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// SSEEvent 一个 SSE 事件：event 名 + data 内容
type SSEEvent struct {
	Event string
	Data  string
}

// ============================================================================
// 流式转换：OpenAI Chat Completions stream -> Anthropic Messages stream
// ============================================================================

type oaStreamConverter struct {
	started      bool
	finished    bool
	hasThinking bool // 是否已开启 thinking block
	thinkingBlk int  // thinking 块的 Anthropic index
	textBlock   int  // 文本块的 Anthropic index，-1 表示尚未开启
	nextBlk     int  // 下一个可用的块索引
	tools       []*oaTool
	toolByIndex map[int]*oaTool // openai tool index -> accum
}

type oaTool struct {
	openaiIndex int
	id          string
	name        string
	args        strings.Builder
	blockIndex  int  // Anthropic block index
	nameSent    bool // 是否已发出 content_block_start
}

// nextBlockIndex 分配下一个 Anthropic 内容块索引
func (c *oaStreamConverter) nextBlockIndex() int {
	idx := c.nextBlk
	c.nextBlk++
	return idx
}

// NewOAStreamConverter 创建 OpenAI chunk -> Anthropic 事件 的流式转换器
func NewOAStreamConverter() *oaStreamConverter {
	return &oaStreamConverter{
		toolByIndex: map[int]*oaTool{},
		textBlock:   -1,
		thinkingBlk: -1,
	}
}

// Convert 输入一行 OpenAI chunk 的 data（JSON 字符串，或 "[DONE]"），返回 Anthropic 事件列表
func (c *oaStreamConverter) Convert(data string) []SSEEvent {
	if data == "[DONE]" {
		return c.Finish()
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil
	}

	var out []SSEEvent

	// message_start（首个 chunk）
	if !c.started {
		c.started = true
		msgID := orString(chunk["id"])
		if msgID == "" {
			msgID = "msg_" + newID()
		}
		out = append(out, SSEEvent{Event: "message_start", Data: mustJSON(map[string]any{
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

	// 思维链增量（DeepSeek R1 / o 系列的 reasoning_content）
	if r, ok := delta["reasoning_content"].(string); ok && r != "" {
		out = append(out, c.thinkingDelta(r)...)
	}

	// 文本增量
	if content, ok := delta["content"].(string); ok && content != "" {
		if c.textBlock < 0 {
			c.textBlock = c.nextBlockIndex()
			out = append(out, SSEEvent{Event: "content_block_start", Data: mustJSON(map[string]any{
				"type": "content_block_start", "index": c.textBlock,
				"content_block": map[string]any{"type": "text", "text": ""},
			})})
		}
		out = append(out, SSEEvent{Event: "content_block_delta", Data: mustJSON(map[string]any{
			"type": "content_block_delta", "index": c.textBlock,
			"delta": map[string]any{"type": "text_delta", "text": content},
		})})
	}

	// 工具调用增量
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			tcm, _ := tc.(map[string]any)
			idx := orInt(tcm["index"])
			tool := c.toolByIndex[idx]
			if tool == nil {
				tool = &oaTool{openaiIndex: idx, blockIndex: c.nextBlockIndex()}
				c.toolByIndex[idx] = tool
				c.tools = append(c.tools, tool)
			}
			fn, _ := tcm["function"].(map[string]any)
			if fn != nil {
				if name, ok := fn["name"].(string); ok && name != "" {
					// 收到函数名时立即发出 content_block_start（真流式）
					if !tool.nameSent {
						tool.nameSent = true
						tool.id = orString(tcm["id"])
						tool.name = name
						out = append(out, SSEEvent{Event: "content_block_start", Data: mustJSON(map[string]any{
							"type": "content_block_start", "index": tool.blockIndex,
							"content_block": map[string]any{"type": "tool_use", "id": tool.id, "name": name, "input": map[string]any{}},
						})})
					}
				}
				if id, ok := tcm["id"].(string); ok && id != "" {
					tool.id = id
				}
				if args, ok := fn["arguments"].(string); ok && args != "" {
					tool.args.WriteString(args)
					// 参数增量直接透传（真流式 input_json_delta）
					if tool.nameSent {
						out = append(out, SSEEvent{Event: "content_block_delta", Data: mustJSON(map[string]any{
							"type": "content_block_delta", "index": tool.blockIndex,
							"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
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

// thinkingDelta 发出/续写 thinking block
func (c *oaStreamConverter) thinkingDelta(text string) []SSEEvent {
	if !c.hasThinking {
		c.hasThinking = true
		c.thinkingBlk = c.nextBlockIndex()
		return []SSEEvent{
			{Event: "content_block_start", Data: mustJSON(map[string]any{
				"type": "content_block_start", "index": c.thinkingBlk,
				"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
			})},
			{Event: "content_block_delta", Data: mustJSON(map[string]any{
				"type": "content_block_delta", "index": c.thinkingBlk,
				"delta": map[string]any{"type": "thinking_delta", "thinking": text},
			})},
		}
	}
	return []SSEEvent{{Event: "content_block_delta", Data: mustJSON(map[string]any{
		"type": "content_block_delta", "index": c.thinkingBlk,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})}}
}

// Finish 补发所有未关闭的 block 和收尾事件
func (c *oaStreamConverter) Finish() []SSEEvent {
	if c.finished {
		return nil
	}
	c.finished = true

	var out []SSEEvent
	if c.hasThinking {
		out = append(out, SSEEvent{Event: "content_block_stop", Data: mustJSON(map[string]any{
			"type": "content_block_stop", "index": c.thinkingBlk,
		})})
		c.hasThinking = false
	}
	if c.textBlock >= 0 {
		out = append(out, SSEEvent{Event: "content_block_stop", Data: mustJSON(map[string]any{
			"type": "content_block_stop", "index": c.textBlock,
		})})
		c.textBlock = -1
	}
	for _, tool := range c.tools {
		// 极端情况：直到结束都没收到函数名（上游异常），补发一个空块避免客户端悬挂
		if !tool.nameSent {
			tool.nameSent = true
			out = append(out, SSEEvent{Event: "content_block_start", Data: mustJSON(map[string]any{
				"type": "content_block_start", "index": tool.blockIndex,
				"content_block": map[string]any{"type": "tool_use", "id": tool.id, "name": tool.name, "input": map[string]any{}},
			})})
			args := tool.args.String()
			if args == "" {
				args = "{}"
			}
			out = append(out, SSEEvent{Event: "content_block_delta", Data: mustJSON(map[string]any{
				"type": "content_block_delta", "index": tool.blockIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			})})
		}
		out = append(out, SSEEvent{Event: "content_block_stop", Data: mustJSON(map[string]any{
			"type": "content_block_stop", "index": tool.blockIndex,
		})})
	}

	stopReason := "end_turn"
	if len(c.tools) > 0 {
		stopReason = "tool_use"
	}
	out = append(out, SSEEvent{Event: "message_delta", Data: mustJSON(map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
	})})
	out = append(out, SSEEvent{Event: "message_stop", Data: mustJSON(map[string]any{"type": "message_stop"})})
	return out
}

// ============================================================================
// 流式转换：Anthropic Messages stream -> OpenAI Chat Completions stream
// ============================================================================

type aoStreamConverter struct {
	started      bool
	finished     bool
	stopReason   string
	textOpen     bool
	anthToOATool map[int]int // anthropic block index -> openai tool index
	nextIndex    int
}

// NewAOStreamConverter 创建 Anthropic 事件 -> OpenAI chunk 的流式转换器
func NewAOStreamConverter() *aoStreamConverter {
	return &aoStreamConverter{anthToOATool: map[int]int{}}
}

// Convert 输入一个 Anthropic 事件的 data JSON，返回 OpenAI chunk 事件列表
func (c *aoStreamConverter) Convert(data string) []SSEEvent {
	var ev map[string]any
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil
	}
	typ, _ := ev["type"].(string)

	var out []SSEEvent

	switch typ {
	case "message_start":
		if !c.started {
			c.started = true
			out = append(out, c.chunk(map[string]any{"role": "assistant", "content": ""}, ""))
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
			// 思维链透传：映射为 OpenAI 侧广泛使用的 reasoning_content 增量
			if t, ok := delta["thinking"].(string); ok && t != "" {
				out = append(out, c.chunk(map[string]any{"reasoning_content": t}, ""))
			}
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
		fr := MapAnthropicStopToOpenAI(c.stopReason)
		out = append(out, c.chunk(map[string]any{}, fr))
		out = append(out, SSEEvent{Data: "[DONE]"})
		c.finished = true
	}

	return out
}

// chunk 构造一个 OpenAI chat.completion.chunk
func (c *aoStreamConverter) chunk(delta map[string]any, finishReason string) SSEEvent {
	obj := map[string]any{
		"id":      "chatcmpl-" + newID(),
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
	return SSEEvent{Data: mustJSON(obj)}
}

// Finish 若上游异常中断，补发收尾
func (c *aoStreamConverter) Finish() []SSEEvent {
	if c.finished {
		return nil
	}
	c.finished = true
	fr := MapAnthropicStopToOpenAI(c.stopReason)
	return []SSEEvent{
		c.chunk(map[string]any{}, fr),
		{Data: "[DONE]"},
	}
}

// newID 生成简单随机 ID（避免引入额外依赖）
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
