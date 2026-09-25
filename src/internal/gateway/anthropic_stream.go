// anthropic_stream.go Anthropic Messages 流式（SSE）转换：Chat chunk → Anthropic 事件。
// 移植自 wild-work internal/gateway/anthropic_stream.go，适配 PoolGate 内部模型。
package gateway

import (
	"net/http"

	"poolgate/internal/channel"
)

// streamAnthropic 把 channel.Stream 转成 Anthropic SSE 写回客户端。
func (s *Server) streamAnthropic(w http.ResponseWriter, st channel.Stream, clientModel string) {
	flush := sseHeader(w)
	msgID := "msg_" + randSuffix()

	const (
		idxThinking = 0
		idxText     = 1
	)
	var (
		thinkingOpen bool
		textOpen     bool
		finish       string
		usage        map[string]any
	)

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
		return emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": idxText,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	closeText := func() error {
		if !textOpen {
			return nil
		}
		textOpen = false
		return emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idxText})
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
				_ = emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": idxText,
					"delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content},
				})
			}
		}
	}

	_ = closeThinking()
	_ = closeText()

	stopReason := anthropicStopReason(finish, false)
	delta := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}}
	if u := anthropicUsage(usage); u != nil {
		delta["usage"] = u
	}
	_ = emit("message_delta", delta)
	_ = emit("message_stop", map[string]any{"type": "message_stop"})
}
