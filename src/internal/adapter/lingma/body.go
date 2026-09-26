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
			"is_vl":        false, // 本渠道不发图（Images=false），恒 false
			"is_reasoning": isReasoningModel(model),
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

	// 工具定义透传（上游就是 OpenAI 形态的 tools 数组），但补齐国参考实现保证存在的字段
	// （见 projectTools）。ForwardTools 已处理「客户端显式 tool_choice:"none"」，不会静默丢工具。
	if tools := projectTools(req.ForwardTools()); len(tools) > 0 {
		payload["tools"] = tools
	}
	if req.ToolChoice != nil {
		payload["tool_choice"] = req.ToolChoice
	}

	return json.Marshal(payload)
}

// isReasoningModel 判定是否要向上游声明「本轮按思考模式跑」。
//
// 依据参考实现 remote/client.go:467-473 remoteReasoningEnabled：客户端显式给了
// reasoning_effort，**或**模型名里含 "thinking"，就把 model_config.is_reasoning 置真。
// 这里只实现了「模型名含 thinking」这一支 —— channel.ChatRequest 没有 reasoning_effort
// 字段（共享类型，本轮不改），那条判据取不到材料，见报告。
//
// 它只影响上游的思考意图，不改变响应形态：远端 SSE 没有独立 reasoning 块
// （参考实现 README:43、207），所以置真不会让正文变成我们解析不到的东西。
func isReasoningModel(model string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(model)), "thinking")
}

// projectTools 保证每个工具都带齐 name / description / parameters 三个字段。
//
// 依据参考实现 remote/client.go:604-628 projectTools：它按内部 ToolDef 结构**重建**每个工具，
// 于是 description 取不到时是空串、parameters 为空时兜底成 {"type":"object","properties":{}} ——
// 也就是说上游实测形态里这两个键**恒存在**。我们的 tools 是客户端 OpenAI 形态的 map 直接透传，
// 而 description 在 OpenAI 里是选填、parameters 也可能被客户端省掉，少键就与实测形态不一致。
//
// 只补缺失的键，其余（含 strict 等未知键）原样保留 —— 参考实现会丢掉未知键，我们保留，
// 避免削掉客户端的语义；上游对多出来的 JSON 键是宽容的。
func projectTools(tools []map[string]any) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			out = append(out, tool) // 非标准形态：不认识就不动它，原样透传
			continue
		}
		clone := make(map[string]any, len(tool))
		for k, v := range tool {
			clone[k] = v
		}
		newFn := make(map[string]any, len(fn)+2)
		for k, v := range fn {
			newFn[k] = v
		}
		if _, ok := newFn["description"]; !ok {
			newFn["description"] = ""
		}
		if params, ok := newFn["parameters"]; !ok || params == nil {
			newFn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		clone["function"] = newFn
		out = append(out, clone)
	}
	return out
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
