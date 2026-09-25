// responses.go OpenAI Responses API（POST /v1/responses）兼容层，供 Codex 直连。
// 请求：Responses → 内部 channel.ChatRequest；响应：Chat 结果 → Responses 格式。
// Responses API 的 messages 形态与 Chat 几乎一致（role/content），核心是响应结构
// 差异（output 数组 + response 顶层字段）。文本对话为最小可用子集。
package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// handleResponses 处理 POST /v1/responses。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, ok := parseJSONBody(w, r)
	if !ok {
		return
	}
	model := asString(body["model"])
	kind, modelName := parseModel(model)
	ch, ok := registryGet(kind)
	if !ok || !ch.Spec().Downstream() {
		writeErr(w, http.StatusBadRequest, errs.New(errs.ModelUnavailable, "模型不可用或渠道已暂停: "+model))
		return
	}

	// input 可以是字符串或数组。
	var msgs []channel.Message
	if instr, ok := body["instructions"].(string); ok && strings.TrimSpace(instr) != "" {
		msgs = append(msgs, channel.Message{Role: "system", Content: instr})
	}
	if inputs, ok := body["input"].([]any); ok {
		for _, it := range inputs {
			im, _ := it.(map[string]any)
			if im == nil {
				continue
			}
			role := strings.ToLower(strings.TrimSpace(asString(im["role"])))
			content := responsesContent(im["content"])
			if role == "" {
				role = "user"
			}
			msgs = append(msgs, channel.Message{Role: role, Content: content})
		}
	} else if ins, ok := body["input"].(string); ok {
		msgs = append(msgs, channel.Message{Role: "user", Content: ins})
	}
	if len(msgs) == 0 {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "缺少 input"))
		return
	}

	maxTokens := 4096
	if n, has := asInt(body["max_output_tokens"]); has && n > 0 {
		maxTokens = n
	}
	stream := asBool(body["stream"])

	creq := channel.ChatRequest{Model: modelName, Messages: msgs, MaxTokens: maxTokens, Stream: stream}
	res, err := s.router.Route(r.Context(), ch, kind, r.Header.Get("X-Poolgate-Session"), creq)
	if err != nil {
		writeErrFromErr(w, err)
		return
	}
	defer res.Stream.Close()

	if stream {
		s.streamResponses(w, res.Stream, model)
		return
	}

	agg := aggregate(res.Stream, model)
	writeJSON(w, http.StatusOK, buildResponses(agg, model))
}

func responsesContent(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	if blocks, ok := content.([]any); ok {
		var out []string
		for _, b := range blocks {
			if bm, ok := b.(map[string]any); ok {
				if t := asString(bm["text"]); t != "" {
					out = append(out, t)
				}
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

// buildResponses 组装 Responses API 响应。
func buildResponses(a aggMessage, model string) map[string]any {
	output := []any{}
	if rc := strings.TrimSpace(a.Reasoning); rc != "" {
		output = append(output, map[string]any{
			"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": rc}},
		})
	}
	output = append(output, map[string]any{
		"type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": a.Content}},
	})
	out := map[string]any{
		"id":     a.ID,
		"object": "response",
		"model":  model,
		"status": "completed",
		"output": output,
	}
	if u := anthropicUsage(a.Usage); u != nil {
		out["usage"] = map[string]any{
			"input_tokens":  u["input_tokens"],
			"output_tokens": u["output_tokens"],
			"total_tokens":  u["input_tokens"].(int) + u["output_tokens"].(int),
		}
	}
	return out
}

// streamResponses 把 channel.Stream 转成 Responses API SSE。
func (s *Server) streamResponses(w http.ResponseWriter, st channel.Stream, model string) {
	flush := sseHeader(w)
	respID := "resp_" + randSuffix()

	emit := func(event string, payload map[string]any) {
		raw, _ := json.Marshal(payload)
		_, _ = w.Write([]byte("event: " + event + "\ndata: " + string(raw) + "\n\n"))
		flush()
	}

	emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{
		"id": respID, "object": "response", "model": model, "status": "in_progress", "output": []any{},
	}})
	itemID := "msg_" + randSuffix()

	for {
		c, err := st.Next()
		if err != nil {
			break
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != "" {
				emit("response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "item_id": itemID,
					"output_index": 0, "content_index": 0, "delta": ch.Delta.Content,
				})
			}
		}
	}

	emit("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{
		"id": respID, "object": "response", "model": model, "status": "completed",
	}})
}
