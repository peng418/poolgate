package qwen

// body.go 构造发往 CLI 端点的请求体。
//
// 与「原样透传」的通用适配器不同，通义的 CLI 端点有两个**硬要求**，不满足会被上游拒或行为漂移：
//  1. 模型别名要重定向（qwen3.5-plus → coder-model）；
//  2. 消息数组首条必须是 system，且它的 content 数组第一项是**空的 text part +
//     cache_control: ephemeral**（上游用它表达「这段是可缓存的系统前缀」）。
//     不带这一项，参考实现实测会出问题，所以照做。
//
// 其余字段（model / messages / tools / tool_choice / max_tokens / temperature / stream）
// 原样送上去 —— 工具调用是原生 OpenAI 形态，不需要任何转换。

import (
	"encoding/json"

	"poolgate/internal/channel"
)

func buildBody(req channel.ChatRequest) ([]byte, error) {
	model := req.Model
	if m, ok := modelRedirect[model]; ok {
		model = m
	}

	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	for _, m := range req.Messages {
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		msg := map[string]any{"role": role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			msg["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		msgs = append(msgs, msg)
	}
	msgs = withCacheAnchor(msgs)

	obj := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   true, // 统一走流式；非流式由网关聚合（Spec.SSEOnly）
	}
	if req.MaxTokens > 0 {
		obj["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	if tools := req.ForwardTools(); len(tools) > 0 {
		obj["tools"] = tools
		if req.ToolChoice != nil {
			obj["tool_choice"] = req.ToolChoice
		}
	}
	return json.Marshal(obj)
}

// withCacheAnchor 保证首条是 system，且其 content 以「空 text part + cache_control: ephemeral」开头。
func withCacheAnchor(msgs []map[string]any) []map[string]any {
	anchor := map[string]any{"type": "text", "text": "", "cache_control": map[string]any{"type": "ephemeral"}}
	if len(msgs) > 0 {
		if role, _ := msgs[0]["role"].(string); role == "system" {
			parts := []any{anchor}
			switch c := msgs[0]["content"].(type) {
			case string:
				if c != "" {
					parts = append(parts, map[string]any{"type": "text", "text": c})
				}
			case []any:
				parts = append(parts, c...)
			}
			msgs[0]["content"] = parts
			return msgs
		}
	}
	out := make([]map[string]any, 0, len(msgs)+1)
	out = append(out, map[string]any{"role": "system", "content": []any{anchor}})
	return append(out, msgs...)
}
