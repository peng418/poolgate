// body.go 构造 agent_chat_generation 请求体。
//
// 形态以参考实现 remote/client.go buildBody 为准（它是实测跑通的版本），逐字段对齐：
// 上游要的是一份**富结构**的 body，不是标准 OpenAI 请求 —— 但里面嵌的 messages 与 tools
// 又是 OpenAI 形态，所以工具调用可以原生透传，不需要走 toolshim。
//
// 两个刻意的「不做」：
//   - **不发 max_tokens**：参考实现的 parameters 里只有 temperature。凭据复用这条路本就
//     在风控边上，少发一个上游可能不认的字段比多发一个安全。
//   - **不伪造 usage**：响应流里上游不返回真实 token 数，我们也不在请求里塞假的。
package lingma

import (
	"encoding/json"
	"strings"
	"time"

	"poolgate/internal/channel"
)

// defaultTemperature 是上游模板默认值（参考实现 buildBody 的兜底）。
const defaultTemperature = 0.1

// buildChatBody 组装上游请求体，返回**将要发送的字节**。
//
// 返回值必须是最终字节：调用方拿它去算签名（签名覆盖 body 原文），
// 再把这个字节原样发出去 —— 中间任何一次重新 marshal（哪怕只是 map 顺序变了）都会让签名失效。
func buildChatBody(req channel.ChatRequest, modelKey string) ([]byte, error) {
	requestID := hexID()

	temperature := defaultTemperature
	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	model := strings.TrimSpace(modelKey)
	if strings.EqualFold(model, "auto") {
		model = "" // "auto" 在上游表示「不给指定模型」，空串才是它的表达
	}

	payload := map[string]any{
		"request_id":       requestID,
		"request_set_id":   "",
		"chat_record_id":   requestID,
		"stream":           true,
		"image_urls":       nil,
		"is_reply":         false,
		"is_retry":         false,
		"session_id":       "",
		"code_language":    "",
		"source":           0,
		"version":          "3",
		"chat_prompt":      "",
		"parameters":       map[string]float64{"temperature": temperature},
		"aliyun_user_type": "personal_standard",
		"agent_id":         "agent_common",
		"task_id":          "question_refine",
		"model_config": map[string]any{
			"key":          model,
			"display_name": "",
			"model":        model,
			"format":       "",
			"is_vl":        false,
			"is_reasoning": false,
			"api_key":      "",
			"url":          "",
			"source":       "",
			"enable":       false,
		},
		"messages": projectMessages(req.Messages),
		"business": map[string]any{
			"product":  "jb_plugin",
			"version":  cosyVersion,
			"type":     "memory",
			"id":       uuid4(),
			"begin_at": time.Now().UnixMilli(),
			"stage":    "start",
			"name":     "memory_intent_recognition_" + requestID,
		},
	}

	// 工具定义原样透传（上游就是 OpenAI 形态的 tools 数组）。
	// ForwardTools 已处理「客户端显式 tool_choice:"none"」这种情况，不会静默丢工具。
	if tools := req.ForwardTools(); len(tools) > 0 {
		payload["tools"] = tools
	}
	if req.ToolChoice != nil {
		payload["tool_choice"] = req.ToolChoice
	}

	return json.Marshal(payload)
}

// projectMessages 把内部消息列表转成上游形态。
//
// 三条要点（缺一条 agent 就会「忘记自己刚调用过工具」）：
//   - developer 角色改写成 system；
//   - assistant 的 tool_calls 必须原样带上；
//   - role=tool 的消息必须带 tool_call_id（与 name，若客户端给了）。
//
// response_meta 与 reasoning_content_signature 是上游消息结构里的固定槽位
// （参考实现对每条消息都塞了空/零值），照抄不省 —— 上游可能有非空校验。
func projectMessages(msgs []channel.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := strings.TrimSpace(m.Role)
		if role == "" {
			continue
		}
		if role == "developer" {
			role = "system"
		}
		item := map[string]any{
			"role":    role,
			"content": m.Content,
			"response_meta": map[string]any{
				"id": "",
				"usage": map[string]int{
					"prompt_tokens":     0,
					"completion_tokens": 0,
					"total_tokens":      0,
				},
			},
			"reasoning_content_signature": "",
		}
		if m.Name != "" {
			item["name"] = m.Name
		}
		if m.ToolCallID != "" {
			item["tool_call_id"] = m.ToolCallID
		}
		if role == "assistant" && len(m.ToolCalls) > 0 {
			item["tool_calls"] = m.ToolCalls
		}
		out = append(out, item)
	}
	return out
}
