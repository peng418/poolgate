package openaiup

// body.go 构造发往上游的 OpenAI 兼容请求体。
//
// 原则：客户端给什么就转发什么。这个适配器的价值是「原样透传 + 归一化」，
// 任何自作聪明的改写都会变成新的静默失败源（红线一）。

import (
	"encoding/json"

	"poolgate/internal/channel"
)

func buildBody(req channel.ChatRequest) []byte {
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		// content 恒为字符串：多数上游不接受缺字段，assistant 带 tool_calls 时给空串最稳。
		msg := map[string]any{"role": m.Role, "content": m.Content}
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

	obj := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   true, // 统一走流式；客户端要的非流式由网关聚合（Spec.SSEOnly）
	}
	if req.MaxTokens > 0 {
		obj["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	// 工具定义原样带上；客户端显式 tool_choice:"none" 时 ForwardTools 返回 nil（那是明确要求，不是丢弃）。
	if tools := req.ForwardTools(); len(tools) > 0 {
		obj["tools"] = tools
		if req.ToolChoice != nil {
			obj["tool_choice"] = req.ToolChoice
		}
	}
	raw, _ := json.Marshal(obj)
	return raw
}
