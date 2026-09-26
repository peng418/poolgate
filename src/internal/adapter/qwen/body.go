package qwen

// body.go 构造发往 CLI 端点的请求体。
//
// 与「原样透传」的通用适配器不同，通义的 CLI 端点有两个**硬要求**，不满足会被上游拒或行为漂移：
//  1. 模型别名要重定向（qwen3.5-plus / qwen3.6-plus → coder-model）；
//  2. **system 消息的 content 必须写成「带 cache_control: ephemeral 的 text part 数组」**，
//     直接给字符串会被上游回 400（上游用它表达「这段是可缓存的系统前缀」）；
//     上游**不会**自己补一条 system，客户端没给就不发（参考实现原样透传客户端消息）。
//
// 来源：qwen-code-oai-proxy src/qwen/api.ts 的 transformMessagesForPortal：
//   - system 且 content 是字符串 → content 变成 [{type:"text", text:content, cache_control:{type:"ephemeral"}}]；
//   - user 且 content 是 Anthropic 形态的块数组 → 摊平成字符串（portal 只接受扁平字符串）。
// 我们的 channel.Message.Content 本来就是 string（网关已归一），所以第二条天然满足；
// 第一条是必须做的转换。
//
// 交叉验证（第二份 CLI 实现 AIClient2API src/providers/openai/qwen-core.js:123-179
// ensureQwenSystemMessage）：它**总是注入**一条 system（首部 part 为 "You are Qwen Code." +
// cache_control，再把客户端 system 文本作为后续 part 合并进来）。两家的共同点是「system 的
// content 是带 cache_control 的 text part 数组」，分歧在「没有 system 时是否注入」。我们采信
// 前者（不注入）：注入会塞进用户没要求的系统提示词、改变模型行为，而 qwen-code-oai-proxy 的
// 用法证明「客户端没给 system 也能通」。这点已记入交接报告，不在此处盲改。
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

	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		msg := map[string]any{"role": role, "content": portalContent(role, m.Content)}
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
		"model":    model,
		"messages": msgs,
		"stream":   true, // 统一走流式；非流式由网关聚合（Spec.SSEOnly）
		// 上游只在 stream_options.include_usage=true 时才在流尾回 usage；
		// 不带它就没有 token 计数（来源：qwen-code-oai-proxy src/qwen/api.ts 流式 payload）。
		"stream_options": map[string]any{"include_usage": true},
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		if lim, ok := modelMaxTokens[model]; ok && mt > lim {
			mt = lim // 超过档位上限会被上游拒；参考实现的 clampMaxTokens 也是这么夹的
		}
		obj["max_tokens"] = mt
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

// portalContent 按上游要求整形一条消息的 content。
//
// 只有 system 需要特殊处理（包成带 cache_control 的 text part 数组）；且 content 为空时
// 保持空串 —— 参考实现里 `if (!message.content) return message;` 就是跳过空内容。
func portalContent(role, content string) any {
	if role != "system" || content == "" {
		return content
	}
	return []any{map[string]any{
		"type":          "text",
		"text":          content,
		"cache_control": map[string]any{"type": "ephemeral"},
	}}
}
