// anthropic.go Anthropic Messages API（POST /v1/messages + /v1/messages/count_tokens）兼容层。
// 请求：Anthropic Messages → 内部 channel.ChatRequest；响应：Chat 结果 → Anthropic Messages/SSE。
// 客户端来源：Claude Code、各类 Anthropic SDK。鉴权用 x-api-key / Authorization 皆可。
// 移植自 wild-work internal/gateway/anthropic.go，适配 PoolGate 的简化内部模型。
package gateway

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/toolshim"
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

	// 工具调用：Anthropic tools → OpenAI tools（input_schema → function.parameters）。
	// 能力同样分四档：原生透传 / 网关代做模拟（toolshim）/ 忽略（纯文本入口）/ 明确拒绝。
	tools := anthropicToolsToOpenAI(body["tools"])
	useShim := false
	if len(tools) > 0 {
		switch {
		case ch.Spec().Tools:
		case ch.Spec().ToolsShim:
			useShim = true
		case ch.Spec().ToolsIgnore:
			// 与 /v1/chat/completions 同一条合同：本渠道没有工具位，丢掉 tools 转发 + 日志留痕。
			log.Printf("poolgate: 渠道 %s 本轮请求带 %d 个 tools，该渠道无工具位（已声明忽略），按纯文本转发",
				string(kind), len(tools))
			tools = nil
		default:
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
				"渠道 "+string(kind)+" 暂不支持工具调用（tools）：已明确拒绝而不是静默忽略")
			return
		}
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
	// tool_use_id → 函数名：Anthropic 的 tool_result 只带 id 不带名字，
	// 而多数上游要求 role=tool 的消息带 name，所以先扫一遍 assistant 的 tool_use。
	toolNames := map[string]string{}
	for _, m := range rawMsgs {
		mm, _ := m.(map[string]any)
		blocks, _ := mm["content"].([]any)
		for _, b := range blocks {
			bm, _ := b.(map[string]any)
			if bm == nil || !strings.EqualFold(asString(bm["type"]), "tool_use") {
				continue
			}
			toolNames[asString(bm["id"])] = asString(bm["name"])
		}
	}
	for _, m := range rawMsgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		role := normalizeAnthropicRole(strings.ToLower(strings.TrimSpace(asString(mm["role"]))))
		blocks, isBlocks := mm["content"].([]any)
		if !isBlocks {
			// 纯字符串 content（老式客户端）。
			msgs = append(msgs, channel.Message{Role: role, Content: flattenAnthropicContent(mm["content"])})
			continue
		}
		if role == "assistant" {
			var texts []string
			var calls []channel.ToolCall
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
				case "tool_use":
					id, name := asString(bm["id"]), asString(bm["name"])
					if name == "" {
						continue
					}
					args := "{}"
					if bm["input"] != nil {
						if raw, err := json.Marshal(bm["input"]); err == nil {
							args = string(raw)
						}
					}
					calls = append(calls, channel.ToolCall{
						ID: id, Type: "function",
						Function: channel.FunctionCall{Name: name, Arguments: args},
					})
				}
			}
			msgs = append(msgs, channel.Message{
				Role: role, Content: strings.Join(texts, "\n"), ToolCalls: calls,
			})
			continue
		}
		// user：文本块合成一条 user 消息；**每个 tool_result 单独成一条 tool 消息**
		// （OpenAI 侧工具结果必须独立成条，和 assistant 的 tool_calls 一一对应）。
		var texts []string
		var results []channel.Message
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
				if res == "" && bm["content"] != nil {
					if raw, err := json.Marshal(bm["content"]); err == nil {
						res = string(raw)
					}
				}
				id := asString(bm["tool_use_id"])
				results = append(results, channel.Message{
					Role: "tool", Content: res, ToolCallID: id, Name: toolNames[id],
				})
			}
		}
		if len(texts) > 0 || len(results) == 0 {
			msgs = append(msgs, channel.Message{Role: role, Content: strings.Join(texts, "\n")})
		}
		msgs = append(msgs, results...)
	}

	creq := channel.ChatRequest{Model: modelName, Messages: msgs, MaxTokens: maxTokens, Stream: asBool(body["stream"]),
		Tools: tools, ToolChoice: anthropicToolChoiceToOpenAI(body["tool_choice"])}
	if useShim {
		creq = toolshim.BuildRequest(creq)
	}

	// 网关侧最后一道防线（见 pairing.go）：发出上游前清理孤儿 tool 配对。
	// Anthropic 入口同样归一成 channel.ChatRequest，与 chat.completions 走同一套清理。
	creq.Messages = sanitizeToolPairing(creq.Messages)

	// 路由。
	res, err := s.router.Route(r.Context(), ch, kind, r.Header.Get("X-Poolgate-Session"), creq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	st := res.Stream
	if useShim {
		st = toolshim.Wrap(st)
	}
	defer st.Close()

	if creq.Stream {
		s.streamAnthropic(w, st, model)
		return
	}

	// 非流式：聚合。
	agg := aggregate(st, model)
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
	ToolCalls []channel.ToolCall
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
			a.ToolCalls = mergeToolCalls(a.ToolCalls, ch.Delta.ToolCalls)
			if ch.FinishReason != "" {
				a.Finish = ch.FinishReason
			}
		}
	}
	// 流被截断（finish_reason==length）时丢弃 arguments 残缺的工具调用（见 truncation.go）：
	// buildAnthropicMessage 会把它转成 input 非法 JSON 的 tool_use 块，客户端据此解析会卡死。
	if truncatedFinish(a.Finish) {
		a.ToolCalls = dropTruncatedToolCalls(a.ToolCalls)
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
	// 工具调用 → tool_use 块（Claude Code 靠它决定去执行哪个工具）。
	for _, tc := range a.ToolCalls {
		if strings.TrimSpace(tc.Function.Name) == "" {
			continue
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    anthropicToolUseID(tc.ID),
			"name":  tc.Function.Name,
			"input": parseToolArguments(tc.Function.Arguments),
		})
	}
	out := map[string]any{
		"id":          anthropicMessageID(a.ID),
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": anthropicStopReason(a.Finish, len(a.ToolCalls) > 0),
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

// anthropicToolsToOpenAI 把 Anthropic 的 tools 转成 OpenAI 形态：
// {name, description, input_schema} → {type:"function", function:{name, description, parameters}}。
// 带 type 的内置工具（如 computer_20241022）语义不同，不猜、跳过。
func anthropicToolsToOpenAI(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, it := range list {
		tm, _ := it.(map[string]any)
		if tm == nil {
			continue
		}
		if typ := asString(tm["type"]); typ != "" && !strings.HasPrefix(typ, "custom") && tm["input_schema"] == nil {
			continue
		}
		name := asString(tm["name"])
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if d := asString(tm["description"]); d != "" {
			fn["description"] = d
		}
		if schema, ok := tm["input_schema"].(map[string]any); ok {
			fn["parameters"] = schema
		} else {
			// 上游普遍要求 parameters 存在；缺省给空对象 schema，不编造字段。
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// anthropicToolChoiceToOpenAI 转换 tool_choice：auto / any / tool → auto / required / {function}。
func anthropicToolChoiceToOpenAI(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(asString(m["type"]))) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if name := asString(m["name"]); name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
		return "required"
	}
	return nil
}

// anthropicToolUseID 保留上游的调用 id（客户端要用它把 tool_result 对回来），
// 但按 Anthropic 的约束过滤成 ^[a-zA-Z0-9_-]+$；为空或过滤后为空则新生成一个。
func anthropicToolUseID(upstream string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(upstream) {
		if r == '_' || r == '-' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "toolu_" + randSuffix()
	}
	return b.String()
}

// parseToolArguments 把 OpenAI 的 arguments（JSON 字符串）解成 tool_use.input。
// 解不出来时不丢信息：用 _raw 包一层（与 wild-work 的处理一致）。
func parseToolArguments(args string) any {
	s := strings.TrimSpace(args)
	if s == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		if m, ok := v.(map[string]any); ok {
			return m
		}
		return map[string]any{"_raw": v}
	}
	return map[string]any{"_raw": s}
}

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
