package gemini

// convert.go 两个方向的格式转换。
//
// 请求：归一化的 channel.ChatRequest → Code Assist 的
// `{"model":…,"project":…,"request":{contents/systemInstruction/tools/generationConfig}}`。
// 响应：SSE 帧（**外面还包了一层 `response`**）→ channel.ChatCompletionChunk。
//
// 工具调用是原生的：请求用标准 `functionDeclarations`，响应里的 `functionCall` 整包到达
// （不是 OpenAI 那种 arguments 分片），所以这里要把它合成一个标准 tool_call。

import (
	"encoding/json"
	"strings"

	"poolgate/internal/channel"
)

// buildRequest 组装请求体。
func buildRequest(req channel.ChatRequest, project, sessionID string) ([]byte, error) {
	var systemParts []map[string]any
	contents := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := m.Role
		switch role {
		case "system", "developer":
			if s := strings.TrimSpace(m.Content); s != "" {
				systemParts = append(systemParts, map[string]any{"text": s})
			}
			continue
		case "tool":
			// 工具结果：Gemini 用 role=user + functionResponse 表达
			contents = append(contents, map[string]any{
				"role": "user",
				"parts": []map[string]any{{
					"functionResponse": map[string]any{
						"name":     firstNonEmpty(m.Name, "tool"),
						"response": functionResponseBody(m.Content),
					},
				}},
			})
			continue
		}

		parts := make([]map[string]any, 0, 2)
		if s := m.Content; strings.TrimSpace(s) != "" {
			parts = append(parts, map[string]any{"text": s})
		}
		// assistant 的工具调用：转成 functionCall part（args 必须是对象）
		for _, tc := range m.ToolCalls {
			if strings.TrimSpace(tc.Function.Name) == "" {
				continue
			}
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{
					"name": tc.Function.Name,
					"args": parseArgs(tc.Function.Arguments),
				},
			})
		}
		if len(parts) == 0 {
			continue
		}
		gr := "user"
		if role == "assistant" {
			gr = "model"
		}
		contents = append(contents, map[string]any{"role": gr, "parts": parts})
	}

	inner := map[string]any{
		"contents": contents,
		"generationConfig": map[string]any{
			"maxOutputTokens": maxOrDefault(req.MaxTokens),
			"thinkingConfig":  map[string]any{"includeThoughts": true},
		},
	}
	if req.Temperature != nil {
		inner["generationConfig"].(map[string]any)["temperature"] = *req.Temperature
	}
	if len(systemParts) > 0 {
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
	if sessionID != "" {
		inner["session_id"] = sessionID
	}
	return json.Marshal(map[string]any{
		"model":          req.Model, // 裸名，不带 models/ 前缀
		"project":        project,
		"user_prompt_id": sessionID,
		"request":        inner,
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

// parseArgs 把 arguments（JSON 字符串）转成对象；解不出来就给 {"_raw": ...}，不丢信息。
func parseArgs(s string) map[string]any {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) == nil {
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
	if json.Unmarshal([]byte(s), &m) == nil {
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

// chunksFromFrame 把一个 SSE 帧（已解包外层 response）转成 chunk。
//
// 返回 ok=false 表示这帧没有可用增量（例如只有 traceId / 空 candidates）。
// finish 由调用方决定什么时候写（通常是最后那帧带 finishReason 的时候）。
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
				calls = append(calls, channel.ToolCall{
					Index: *toolSeq - 1,
					ID:    "call_gemini_" + name + "_" + itoa(*toolSeq),
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

// normalizeUsage 把 Gemini 的 usageMetadata 转成 OpenAI 叫法（客户端认这个）。
func normalizeUsage(u map[string]any) map[string]any {
	in := intOf(u["promptTokenCount"])
	out := intOf(u["candidatesTokenCount"])
	total := intOf(u["totalTokenCount"])
	if total == 0 {
		total = in + out
	}
	return map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      total,
	}
}

func intOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// mapFinish 把 Gemini 的 finishReason 映射成 OpenAI 的叫法。
func mapFinish(fr string) string {
	switch strings.ToUpper(fr) {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	}
	return strings.ToLower(fr)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
