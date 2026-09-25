// anthropic_stream.go Anthropic Messages 流式（SSE）转换：Chat chunk → Anthropic 事件。
// 移植自 wild-work internal/gateway/anthropic_stream.go，适配 PoolGate 内部模型。
//
// 工具调用会变成 tool_use 内容块 + input_json_delta —— Claude Code 靠它执行工具，
// 少了这一段，客户端只会看到一段「模型在说它要调用工具」的文本（coding agent 不可用）。
package gateway

import (
	"net/http"
	"strings"

	"poolgate/internal/channel"
)

// streamAnthropic 把 channel.Stream 转成 Anthropic SSE 写回客户端。
func (s *Server) streamAnthropic(w http.ResponseWriter, st channel.Stream, clientModel string) {
	flush := sseHeader(w)
	msgID := "msg_" + randSuffix()

	const idxThinking = 0
	// 内容块序号按出现顺序递增：thinking 固定 0，其余（文本/工具）依次往后排。
	// 这样「工具在前、文本在后」的上游也不会出现块序号回退（Anthropic 要求递增）。
	var (
		thinkingOpen bool
		textOpen     bool
		textIdx      = 1
		nextIdx      = 2
		finish       string
		usage        map[string]any
		sawToolUse   bool
	)

	type toolBlock struct {
		idx  int
		id   string
		name string
		buf  strings.Builder
		open bool
	}
	tools := map[int]*toolBlock{} // key: OpenAI 的 tool_calls index
	var toolOrder []int

	emit := func(event string, payload map[string]any) error {
		return writeSSEEvent(w, flush, event, payload)
	}

	startPayload := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant",
			"model": clientModel, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}
	_ = emit("message_start", startPayload)

	openThinking := func() error {
		if thinkingOpen {
			return nil
		}
		thinkingOpen = true
		return emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": idxThinking,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		})
	}
	closeThinking := func() error {
		if !thinkingOpen {
			return nil
		}
		thinkingOpen = false
		return emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idxThinking})
	}
	openText := func() error {
		if textOpen {
			return nil
		}
		_ = closeThinking()
		textOpen = true
		textIdx = nextIdx
		nextIdx++
		return emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": textIdx,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	closeText := func() error {
		if !textOpen {
			return nil
		}
		textOpen = false
		return emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": textIdx})
	}
	// openTool 开一个 tool_use 块，并把此前攒下的参数片段补发出去。
	// Anthropic 要求 content_block_start 里就有 id/name，而 OpenAI 的分片是先段
	// 带 id/name、后段才是 arguments，所以这里要等拿到函数名才真正开块。
	openTool := func(tb *toolBlock) error {
		if tb.open || tb.name == "" {
			return nil
		}
		_ = closeThinking()
		_ = closeText()
		tb.open = true
		sawToolUse = true
		if err := emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": tb.idx,
			"content_block": map[string]any{
				"type": "tool_use", "id": tb.id, "name": tb.name,
				"input": map[string]any{},
			},
		}); err != nil {
			return err
		}
		if tb.buf.Len() > 0 {
			partial := tb.buf.String()
			tb.buf.Reset()
			return emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": tb.idx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
			})
		}
		return nil
	}

	for {
		c, err := st.Next()
		if err != nil {
			break
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		for _, ch := range c.Choices {
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
			if ch.Delta.ReasoningContent != "" && !textOpen {
				_ = openThinking()
				if thinkingOpen {
					_ = emit("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": idxThinking,
						"delta": map[string]any{"type": "thinking_delta", "thinking": ch.Delta.ReasoningContent},
					})
				}
			}
			if ch.Delta.Content != "" {
				_ = openText()
				if textOpen {
					_ = emit("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": textIdx,
						"delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content},
					})
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				tb, ok := tools[tc.Index]
				if !ok {
					tb = &toolBlock{idx: nextIdx}
					nextIdx++
					tools[tc.Index] = tb
					toolOrder = append(toolOrder, tc.Index)
				}
				if tc.ID != "" {
					tb.id = anthropicToolUseID(tc.ID)
				}
				if tb.id == "" {
					tb.id = anthropicToolUseID("")
				}
				if tc.Function.Name != "" {
					tb.name = tc.Function.Name
				}
				_ = openTool(tb)
				if tc.Function.Arguments == "" {
					continue
				}
				if tb.open {
					_ = emit("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": tb.idx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
					})
				} else {
					// 还没拿到函数名，先攒着，开块时一次补发。
					tb.buf.WriteString(tc.Function.Arguments)
				}
			}
		}
	}

	_ = closeThinking()
	_ = closeText()
	for _, k := range toolOrder {
		tb := tools[k]
		if !tb.open {
			continue
		}
		_ = emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": tb.idx})
	}

	stopReason := anthropicStopReason(finish, sawToolUse)
	delta := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}}
	if u := anthropicUsage(usage); u != nil {
		delta["usage"] = u
	}
	_ = emit("message_delta", delta)
	_ = emit("message_stop", map[string]any{"type": "message_stop"})
}
