package anthropic

// body.go 构造发往 Anthropic Messages API 的请求体。
//
// 与 openaiup 的原则一致：客户端给什么就转发什么。这里做的只是**形态换算**
// （OpenAI 的 tool_calls ↔ Anthropic 的 tool_use/tool_result），不做内容改写 ——
// 任何自作聪明的改写都会变成新的静默失败源（红线一）。
//
// Anthropic 与 OpenAI 的消息形态有三处硬差别，也是本文件存在的全部理由：
//   - system 不在 messages 里，是顶层字段；
//   - 没有 role=tool：工具结果必须是 user 消息里的 tool_result 块；
//   - 工具结果与工具调用靠 tool_use_id 配对，OpenAI 那边靠 tool_call_id。

import (
	"encoding/json"
	"strings"

	"poolgate/internal/channel"
)

// defaultMaxTokens 是客户端没给 max_tokens 时的兜底。
// Anthropic 的 max_tokens 是**必填**，缺了直接 400 —— 与 OpenAI 不同。
const defaultMaxTokens = 4096

func buildBody(req channel.ChatRequest) []byte {
	system, msgs := packMessages(req.Messages)

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	out := map[string]any{
		"model":      req.Model,
		"max_tokens": maxTokens,
		"stream":     true, // 统一走流式；客户端要的非流式由网关聚合（Spec.SSEOnly）
		"messages":   msgs,
	}
	if system != "" {
		out["system"] = system
	}
	// 工具定义原样带上；客户端显式 tool_choice:"none" 时 ForwardTools 返回 nil（那是明确要求，不是丢弃）。
	if tools := req.ForwardTools(); len(tools) > 0 {
		out["tools"] = toAnthropicTools(tools)
		// tool_choice 只在带 tools 时才有意义：Anthropic 会拒绝「有 tool_choice 但没有 tools」。
		if tc := toAnthropicToolChoice(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}
	// temperature 原样透传。注意：聊天模型在**打开思考**时不允许带 temperature，
	// 而本适配器不主动打开思考（见 Spec 的说明），所以这里不会撞上那个冲突。
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	raw, _ := json.Marshal(out)
	return raw
}

// packMessages channel.Message → (system, Anthropic messages)。
//
// 两件事值得单独说：
//
//  1. 连续的 tool 消息**合并成同一条 user 消息里的多个 tool_result 块**。
//     Anthropic 要求同一轮的工具结果放在一条 user 消息里；拆成多条不仅不合规范，
//     还会让模型以为工具是分几轮调用的（并行调用会退化成串行）。
//
//  2. assistant 消息里的文本与 tool_use 块按顺序放进 content 数组 ——
//     tool_use 必须原样回传，否则模型看到的是「自己上一轮什么都没调用」，
//     于是重复调用同一个工具。
func packMessages(msgs []channel.Message) (string, []any) {
	var system strings.Builder
	out := make([]any, 0, len(msgs))
	// pending 是攒着的 tool_result 块，遇到第一条非 tool 消息时一次性落进 user 消息。
	var pending []any

	flushTools := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "user", "content": pending})
		pending = nil
	}

	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			// developer 一并按 system 处理：Anthropic 只认 system（与 qodercn 的实测要求一致）。
			if s := strings.TrimSpace(m.Content); s != "" {
				system.WriteString(s)
				system.WriteString("\n")
			}
		case "tool":
			pending = append(pending, map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			})
		case "assistant":
			flushTools()
			blocks := []any{}
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := map[string]any{}
				if s := strings.TrimSpace(tc.Function.Arguments); s != "" {
					// 参数解析失败就发空对象：宁让上游说「参数不对」，也不要丢掉这次调用，
					// 因为丢掉会让模型以为上一轮没调用过工具。
					_ = json.Unmarshal([]byte(s), &input)
				}
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
				})
			}
			// 既没文本也没工具调用的 assistant 消息直接跳过：空 content 数组会被上游拒。
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": blocks})
			}
		default: // user
			flushTools()
			if strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 {
				continue // 空 user 消息同样会被上游拒
			}
			out = append(out, map[string]any{"role": "user", "content": m.Content})
		}
	}
	flushTools() // 结尾就是工具结果的常见情形（客户端在等下一轮）

	return strings.TrimSpace(system.String()), out
}

// toAnthropicTools OpenAI tools → Anthropic tools（function.parameters → input_schema）。
func toAnthropicTools(tools []map[string]any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		item := map[string]any{"name": name}
		if d, ok := fn["description"].(string); ok && d != "" {
			item["description"] = d
		}
		// input_schema 必填：客户端没给参数定义时补一个空对象 schema，
		// 而不是省略它 —— 省略会被上游拒，整个请求都发不出去。
		if p, ok := fn["parameters"].(map[string]any); ok && len(p) > 0 {
			item["input_schema"] = p
		} else {
			item["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, item)
	}
	return out
}

// toAnthropicToolChoice OpenAI tool_choice → Anthropic tool_choice。
//
// 三处名字不一样，对错一眼看不出（发错了上游只回「参数不合法」）：
// auto → auto、required → **any**、{"type":"function"} → **{"type":"tool"}**。
func toAnthropicToolChoice(v any) any {
	switch t := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		}
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && strings.TrimSpace(name) != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
		if typ, ok := t["type"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(typ)) {
			case "required":
				return map[string]any{"type": "any"}
			case "auto", "none":
				return map[string]any{"type": strings.ToLower(strings.TrimSpace(typ))}
			}
		}
	}
	return nil
}
