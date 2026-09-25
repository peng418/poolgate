package antigravity

// convert.go 两个方向的格式转换。
//
// 请求（本渠道最容易写错的一层）：归一化的 channel.ChatRequest → Antigravity 的
//
//	{"project":…, "model":…, "request":{contents/systemInstruction/generationConfig/tools/toolConfig},
//	 "requestType":"agent", "userAgent":"antigravity", "requestId":…}
//
// 三个形态上的硬性要求（上游会 400，参考实现的 API 规格文档逐条验过）：
//   - contents 是 **Gemini 形态**（role 只有 user / model），Anthropic 的 messages 形态不认；
//   - systemInstruction 必须是 {parts:[…]} **对象**，写成一个字符串会 400；
//   - 工具是 functionDeclarations（原生），响应里的 functionCall 是**整包**到达（不是分片）。
//
// 响应：SSE 每帧外面还包了一层 `response`（`{"response":{"candidates":…,"usageMetadata":…}}`），
// 先剥壳再转 channel.ChatCompletionChunk。

import (
	"encoding/json"
	"strings"

	"poolgate/internal/channel"
)

// buildRequest 组装请求体。requestID 是上游用来串日志的请求号（形如 agent-<uuid>）。
func buildRequest(req channel.ChatRequest, project, requestID string) ([]byte, error) {
	var systemParts []map[string]any
	contents := make([]map[string]any, 0, len(req.Messages))

	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			// system 不进 contents：上游单独认 systemInstruction（形态见文件头注释）。
			if s := strings.TrimSpace(m.Content); s != "" {
				systemParts = append(systemParts, map[string]any{"text": s})
			}
			continue
		case "tool":
			// 工具结果：Gemini 形态是 role=user + functionResponse。
			// id 要带上 —— 上游靠它把结果对回具体那一次 functionCall。
			fr := map[string]any{
				"name":     firstNonEmpty(m.Name, "tool"),
				"response": functionResponseBody(m.Content),
			}
			if m.ToolCallID != "" {
				fr["id"] = m.ToolCallID
			}
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": []map[string]any{{"functionResponse": fr}},
			})
			continue
		}

		parts := make([]map[string]any, 0, 2)
		if s := m.Content; strings.TrimSpace(s) != "" {
			parts = append(parts, map[string]any{"text": s})
		}
		// assistant 的 tool_calls：转成 functionCall part（args 必须是对象，不是 JSON 串）。
		for _, tc := range m.ToolCalls {
			if strings.TrimSpace(tc.Function.Name) == "" {
				continue
			}
			fc := map[string]any{
				"name": tc.Function.Name,
				"args": parseArgs(tc.Function.Arguments),
			}
			// 回传时带上原 id：不带的话上游会认为是「一次新调用」，多轮里模型会重复调同一个工具。
			if tc.ID != "" {
				fc["id"] = tc.ID
			}
			parts = append(parts, map[string]any{"functionCall": fc})
		}
		if len(parts) == 0 {
			continue
		}
		gr := "user"
		if m.Role == "assistant" {
			gr = "model" // 上游只认 user / model
		}
		contents = append(contents, map[string]any{"role": gr, "parts": parts})
	}

	inner := map[string]any{
		"contents": contents,
		"generationConfig": map[string]any{
			"maxOutputTokens": maxOrDefault(req.MaxTokens),
			// includeThoughts 打开，思考型号才会把思考块单独吐出来（映射成 reasoning_content）。
			// 非思考型号忽略它，无害。
			"thinkingConfig": map[string]any{"includeThoughts": true},
		},
	}
	if req.Temperature != nil {
		inner["generationConfig"].(map[string]any)["temperature"] = *req.Temperature
	}
	if len(systemParts) > 0 {
		// 注意是对象形态（带 parts），不是字符串 —— 这条写错上游直接 400。
		inner["systemInstruction"] = map[string]any{"parts": systemParts}
	}
	if tools := req.ForwardTools(); len(tools) > 0 {
		if decls := functionDeclarations(tools); len(decls) > 0 {
			inner["tools"] = []map[string]any{{"functionDeclarations": decls}}
			if tc := toolConfig(req.ToolChoice); tc != nil {
				inner["toolConfig"] = map[string]any{"functionCallingConfig": tc}
			}
		}
	}

	// 外层信封。requestType/userAgent 是 Antigravity 模式专有，缺了上游按别的客户端处理。
	return json.Marshal(map[string]any{
		"project":     project,
		"model":       req.Model, // 全名原样发（Antigravity 模式不剥思考档后缀）
		"request":     inner,
		"requestType": "agent",
		"userAgent":   "antigravity",
		"requestId":   requestID,
	})
}

func maxOrDefault(n int) int {
	if n > 0 {
		return n
	}
	return 8192
}

// functionDeclarations 把 OpenAI 形态的 tools 转成 Gemini 的 functionDeclarations。
func functionDeclarations(tools []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		decl := map[string]any{"name": name}
		if d, _ := fn["description"].(string); d != "" {
			decl["description"] = d
		}
		if p, ok := fn["parameters"].(map[string]any); ok {
			decl["parameters"] = p
		} else {
			// 上游要求 parameters 是对象：没有就给个空 object，别省掉这个键。
			decl["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, decl)
	}
	return out
}

// toolConfig 把 tool_choice 转成 functionCallingConfig。
func toolConfig(choice any) map[string]any {
	switch v := choice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "auto":
			return map[string]any{"mode": "AUTO"}
		case "required":
			return map[string]any{"mode": "ANY"}
		case "none":
			return map[string]any{"mode": "NONE"}
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); strings.TrimSpace(name) != "" {
				return map[string]any{"mode": "ANY", "allowedFunctionNames": []string{name}}
			}
		}
	}
	return nil
}

// parseArgs 把 arguments（JSON 字符串）转成对象；解不出来就原样包一层，不丢信息。
func parseArgs(s string) map[string]any {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) == nil && m != nil {
		return m
	}
	return map[string]any{"_raw": s}
}

// functionResponseBody 工具结果必须是对象：能解成对象就解，否则包一层。
func functionResponseBody(content string) map[string]any {
	s := strings.TrimSpace(content)
	if s == "" {
		return map[string]any{"content": ""}
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) == nil && m != nil {
		return m
	}
	return map[string]any{"content": s}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// chunksFromFrame 把一个 SSE 帧（**已剥掉外层 response**）转成 chunk。
//
// 返回 ok=false 表示这帧没有可用增量（例如只有 traceId、空 candidates）。
func chunksFromFrame(resp map[string]any, model string, toolSeq *int) (channel.ChatCompletionChunk, bool) {
	if resp == nil {
		return channel.ChatCompletionChunk{}, false
	}
	c := channel.ChatCompletionChunk{Model: model}
	if u, ok := resp["usageMetadata"].(map[string]any); ok {
		c.Usage = normalizeUsage(u)
	}
	cands, _ := resp["candidates"].([]any)
	if len(cands) == 0 {
		// 只有 usage 的收尾帧也算「有用」，否则客户端拿不到 token 统计。
		if c.Usage != nil {
			return c, true
		}
		return c, false
	}

	used := false
	for _, ci := range cands {
		cm, _ := ci.(map[string]any)
		if cm == nil {
			continue
		}
		ch := channel.ChunkChoice{}
		if idx, ok := cm["index"].(float64); ok {
			ch.Index = int(idx)
		}
		if fr, ok := cm["finishReason"].(string); ok && fr != "" {
			ch.FinishReason = mapFinish(fr)
			used = true
		}
		content, _ := cm["content"].(map[string]any)
		parts, _ := content["parts"].([]any)

		var text, think strings.Builder
		var calls []channel.ToolCall
		for _, pi := range parts {
			pm, _ := pi.(map[string]any)
			if pm == nil {
				continue
			}
			if t, ok := pm["text"].(string); ok && t != "" {
				// thought:true 是思考块（Gemini 系与 Claude 思考系都这么标）。
				// Gemini 的思考块通常还带 thoughtSignature —— 我们只取文本，
				// 签名不回传（见 client.go 里「已知局限」的说明）。
				if thought, _ := pm["thought"].(bool); thought {
					think.WriteString(t)
				} else {
					text.WriteString(t)
				}
			}
			if fc, ok := pm["functionCall"].(map[string]any); ok {
				name, _ := fc["name"].(string)
				if strings.TrimSpace(name) == "" {
					continue
				}
				args := "{}"
				if raw, err := json.Marshal(fc["args"]); err == nil && len(raw) > 0 && string(raw) != "null" {
					args = string(raw)
				}
				*toolSeq++
				// 上游给了 id 就用它的（Anthropic 后端是 toolu_vrtx_…，Gemini 后端可能不给）；
				// 没给才自己造一个 —— 客户端要靠 id 把工具结果对回来。
				id, _ := fc["id"].(string)
				if strings.TrimSpace(id) == "" {
					id = "call_ag_" + name + "_" + itoa(*toolSeq)
				}
				calls = append(calls, channel.ToolCall{
					Index: *toolSeq - 1,
					ID:    id,
					Type:  "function",
					Function: channel.FunctionCall{
						Name: name, Arguments: args,
					},
				})
			}
		}
		ch.Delta.Content = text.String()
		ch.Delta.ReasoningContent = think.String()
		ch.Delta.ToolCalls = calls
		if ch.Delta.Content != "" || ch.Delta.ReasoningContent != "" || len(calls) > 0 {
			used = true
		}
		c.Choices = append(c.Choices, ch)
	}
	return c, used
}

// normalizeUsage 把 usageMetadata 转成 OpenAI 叫法（客户端只认这个）。
func normalizeUsage(u map[string]any) map[string]any {
	in := intOf(u["promptTokenCount"])
	out := intOf(u["candidatesTokenCount"])
	total := intOf(u["totalTokenCount"])
	if total == 0 {
		total = in + out
	}
	m := map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      total,
	}
	// 思考 token 单独一档（Gemini 后端会给），带上便于面板统计，缺就不写。
	if t := intOf(u["thoughtsTokenCount"]); t > 0 {
		m["completion_tokens_details"] = map[string]any{"reasoning_tokens": t}
	}
	return m
}

func intOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// mapFinish 把上游的 finishReason 映射成 OpenAI 的叫法。
func mapFinish(fr string) string {
	switch strings.ToUpper(fr) {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	case "OTHER":
		// OTHER 在工具调用轮次里是常态（上游用它表示「这一轮吐出的是 functionCall」）。
		// 映射成 tool_calls 而不是 stop：客户端据此知道还有工具要执行。
		// 纯文本轮次偶发 OTHER 也不至于让客户端卡住 —— 它在 tool_calls 为空时会正常收尾。
		return "tool_calls"
	}
	return strings.ToLower(fr)
}

// itoa 是个不带 strconv 的小工具（避免为一个整数转换引入依赖）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// containsFold 是不区分大小写的子串判断（模型名大小写上游不保证）。
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
