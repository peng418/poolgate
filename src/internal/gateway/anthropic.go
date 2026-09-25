// anthropic.go Anthropic Messages API（POST /v1/messages + /v1/messages/count_tokens）兼容层。
// 请求：Anthropic Messages → 内部 channel.ChatRequest；响应：Chat 结果 → Anthropic Messages/SSE。
// 客户端来源：Claude Code、各类 Anthropic SDK。鉴权用 x-api-key / Authorization 皆可。
// 移植自 wild-work internal/gateway/anthropic.go，适配 PoolGate 的简化内部模型。
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"poolgate/internal/channel"
)

// handleAnthropicMessages 处理 POST /v1/messages。
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, ok := parseJSONBody(w, r)
	if !ok {
		return
	}
	model := asString(body["model"])
	kind, modelName := parseModel(model)
	ch, ok := registryGet(kind)
	if !ok || !ch.Spec().Downstream() {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "模型不可用或渠道已暂停: "+model)
		return
	}

	// Anthropic 要求 max_tokens 必填；缺失给保守默认。
	maxTokens := 4096
	if n, has := asInt(body["max_tokens"]); has && n > 0 {
		maxTokens = n
	}

	// system（string 或 block 数组）→ 首条 system 消息。
	var msgs []channel.Message
	if sys := flattenText(body["system"]); strings.TrimSpace(sys) != "" {
		msgs = append(msgs, channel.Message{Role: "system", Content: sys})
	}
	rawMsgs, ok := body["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages is required and must be non-empty")
		return
	}
	for _, m := range rawMsgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(asString(mm["role"])))
		role = normalizeAnthropicRole(role)
		content := flattenAnthropicContent(mm["content"])
		msgs = append(msgs, channel.Message{Role: role, Content: content})
	}

	creq := channel.ChatRequest{Model: modelName, Messages: msgs, MaxTokens: maxTokens, Stream: asBool(body["stream"])}

	// 路由。
	res, err := s.router.Route(r.Context(), ch, kind, r.Header.Get("X-Poolgate-Session"), creq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer res.Stream.Close()

	if creq.Stream {
		s.streamAnthropic(w, res.Stream, model)
		return
	}

	// 非流式：聚合。
	agg := aggregate(res.Stream, model)
	writeJSON(w, http.StatusOK, buildAnthropicMessage(agg, model))
}

// handleAnthropicCountTokens 处理 POST /v1/messages/count_tokens。
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, ok := parseJSONBody(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateAnthropicTokens(body)})
}

// ---------------------------------------------------------------------------
// 聚合 + 响应转换
// ---------------------------------------------------------------------------

// aggMessage 是非流式聚合后的中间结果。
type aggMessage struct {
	Content   string
	Reasoning string
	ID        string
	Finish    string
	Usage     map[string]any
}

func aggregate(st channel.Stream, model string) aggMessage {
	var a aggMessage
	for {
		c, err := st.Next()
		if err != nil {
			break
		}
		if c.ID != "" {
			a.ID = c.ID
		}
		if c.Usage != nil {
			a.Usage = c.Usage
		}
		for _, ch := range c.Choices {
			a.Content += ch.Delta.Content
			a.Reasoning += ch.Delta.ReasoningContent
			if ch.FinishReason != "" {
				a.Finish = ch.FinishReason
			}
		}
	}
	return a
}

// buildAnthropicMessage 组装 Anthropic Messages 响应。
func buildAnthropicMessage(a aggMessage, model string) map[string]any {
	content := []any{}
	if rc := strings.TrimSpace(a.Reasoning); rc != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": rc, "signature": ""})
	}
	if t := strings.TrimSpace(a.Content); t != "" {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	out := map[string]any{
		"id":          anthropicMessageID(a.ID),
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": anthropicStopReason(a.Finish, false),
	}
	if u := anthropicUsage(a.Usage); u != nil {
		out["usage"] = u
	}
	return out
}

func anthropicStopReason(finish string, hasToolCalls bool) string {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	}
	if hasToolCalls {
		return "tool_use"
	}
	return "end_turn"
}

func anthropicUsage(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	in := usageInt(usage, "prompt_tokens", "input_tokens")
	out := usageInt(usage, "completion_tokens", "output_tokens")
	if in == 0 && out == 0 {
		return nil
	}
	return map[string]any{"input_tokens": in, "output_tokens": out}
}

func anthropicMessageID(upstreamID string) string {
	if s := strings.TrimSpace(upstreamID); s != "" {
		if i := strings.Index(s, "-"); i >= 0 && i+1 < len(s) {
			return "msg_" + s[i+1:]
		}
		return "msg_" + s
	}
	return "msg_" + randSuffix()
}

// ---------------------------------------------------------------------------
// 请求转换辅助
// ---------------------------------------------------------------------------

func normalizeAnthropicRole(role string) string {
	if strings.EqualFold(role, "assistant") {
		return "assistant"
	}
	return "user"
}

// flattenAnthropicContent 把 Anthropic content（string 或 block 数组）压成纯文本。
func flattenAnthropicContent(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		if bm == nil {
			continue
		}
		switch strings.ToLower(asString(bm["type"])) {
		case "text":
			if t := asString(bm["text"]); t != "" {
				texts = append(texts, t)
			}
		case "tool_result":
			res := flattenText(bm["content"])
			if res == "" {
				raw, _ := json.Marshal(bm["content"])
				res = string(raw)
			}
			texts = append(texts, res)
		}
	}
	return strings.Join(texts, "\n")
}

// ---------------------------------------------------------------------------
// 错误输出
// ---------------------------------------------------------------------------

func writeAnthropicError(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// estimateAnthropicTokens 估算输入 token 数（中文 1 字≈1 token，其余 4 字符≈1）。
func estimateAnthropicTokens(body map[string]any) int {
	var cjk, other, blocks int
	countText := func(s string) {
		for _, r := range s {
			if r > 0x2E80 {
				cjk++
			} else {
				other++
			}
		}
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			countText(t)
		case []any:
			for _, x := range t {
				walk(x)
			}
		case map[string]any:
			if typ, has := t["type"]; has {
				if s, ok := typ.(string); ok && (s == "text" || s == "tool_result") {
					blocks++
				}
			}
			for k, x := range t {
				switch k {
				case "text", "content", "input", "system":
					walk(x)
				}
			}
		}
	}
	walk(body["system"])
	walk(body["messages"])
	tokens := cjk + other/4 + 1
	if min := blocks * 2; tokens < min {
		tokens = min
	}
	if msgs, ok := body["messages"].([]any); ok {
		if min := len(msgs) * 2; tokens < min {
			tokens = min
		}
	}
	return tokens
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func asString(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func asBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true")
	}
	return false
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

func flattenText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if blocks, ok := v.([]any); ok {
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

func usageInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k].(float64); ok {
			return int(v)
		}
		if v, ok := m[k].(int); ok {
			return v
		}
	}
	return 0
}

func randSuffix() string {
	return fmt.Sprintf("%08x", time.Now().UnixNano())
}
