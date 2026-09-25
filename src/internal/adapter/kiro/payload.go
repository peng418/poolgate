package kiro

// payload.go 把内部契约（channel.ChatRequest）拼成 Kiro 要的 Amz-Json 请求体。
//
// 上游对消息形态有**硬约束**，违反了就 400 "Improperly formed request"（文案很模糊，
// 所以下面每一条约束都要在本地先满足）：
//   - history 里 user / assistant 必须**严格交替**；
//   - 第一条消息必须是 user；
//   - 有 toolResults 就必须同时有 tools（否则上游拒收）；
//   - 有 toolResults 的前面必须有一条带 toolUses 的 assistant。
//
// 参考实现为此写了一串「归一」步骤（merge_adjacent / ensure_first_message_is_user /
// ensure_alternating_roles / ensure_assistant_before_tool_results / strip_all_tool_content），
// 这里按同样的顺序照搬 —— 顺序是有意义的，换序会让某些客户端（Cline/Roo/Cursor 发的
// 截断会话）撞上 400。

import (
	"encoding/json"
	"fmt"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// placeholder 是上游要求的「非空内容」占位（照参考实现的原话，别改）。
const placeholder = "(empty placeholder)"

// resolveModel 把客户端给的档位名翻成 Kiro 认识的模型 id。
//
// 认不出的一律**原样透传**（Kiro API 才是最终裁判）—— 不猜、不悄悄换成默认档：
// 悄悄换档会让用户以为自己在用 A，实际在用 B。
func resolveModel(id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	s = strings.TrimSuffix(s, "[1m]") // 客户端会带这种上下文窗口标记，不是模型名的一部分
	s = strings.TrimSuffix(s, "[200k]")
	if v, ok := modelAliases[s]; ok {
		return v
	}
	return s
}

// unifiedMsg 是归一后的消息（只有 user / assistant 两种角色）。
type unifiedMsg struct {
	role        string
	content     string
	toolUses    []map[string]any // 仅 assistant
	toolResults []map[string]any // 仅 user
}

// buildPayload 组装请求体。
func buildPayload(req channel.ChatRequest, modelID, profileArn string) (map[string]any, error) {
	kiroTools, err := convertTools(req.ForwardTools())
	if err != nil {
		return nil, err
	}
	system, msgs := splitMessages(req.Messages)

	// 没有 tools 就必须把工具痕迹全部转成文本：上游见到「有 toolResults 没有 tools」直接 400。
	if len(kiroTools) == 0 {
		msgs = stripToolContent(msgs)
	} else {
		msgs = ensureAssistantBeforeToolResults(msgs)
	}
	msgs = mergeAdjacent(msgs)
	msgs = ensureFirstUser(msgs)
	msgs = ensureAlternating(msgs)
	if len(msgs) == 0 {
		return nil, errs.New(errs.Parse, "没有可发送的消息（对话为空）").
			WithChannel(string(channel.Kiro))
	}

	historyMsgs := msgs[:len(msgs)-1]
	cur := msgs[len(msgs)-1]

	history := make([]map[string]any, 0, len(historyMsgs))
	for _, m := range historyMsgs {
		history = append(history, toHistoryEntry(m, modelID))
	}

	// 最后一条若是 assistant（客户端没给新的 user 提问）：把它塞进 history，
	// 当前消息补一个空 user —— 上游只认「currentMessage 必须是 userInputMessage」。
	if cur.role == "assistant" {
		history = append(history, toHistoryEntry(cur, modelID))
		cur = unifiedMsg{role: "user"}
	}

	curContent := strings.TrimSpace(cur.content)
	if curContent == "" {
		curContent = placeholder
	}

	// system 提示词没有独立位置：拼到第一条 user（有 history 就是 history[0]，
	// 否则就是当前消息）。这是参考实现的做法，形态变了模型表现会跟着变。
	if strings.TrimSpace(system) != "" && len(history) > 0 {
		prependSystem(history[0], system)
	} else if strings.TrimSpace(system) != "" {
		curContent = system + "\n\n" + curContent
	}

	ctx := map[string]any{}
	if len(kiroTools) > 0 {
		ctx["tools"] = kiroTools
	}
	if len(cur.toolResults) > 0 {
		ctx["toolResults"] = cur.toolResults
	}

	userInput := map[string]any{
		"content": curContent,
		"modelId": modelID,
		"origin":  originAIEditor,
	}
	if len(ctx) > 0 {
		userInput["userInputMessageContext"] = ctx
	}

	payload := map[string]any{
		"conversationState": map[string]any{
			"chatTriggerType": chatTriggerManual,
			// conversationId 每次请求新生成：我们不做参考实现那套「按消息哈希取稳定 id」
			// 的截断恢复机制，稳定 id 只会让不同会话在上游侧看起来是同一个。
			"conversationId": randHex(16),
			"currentMessage": map[string]any{"userInputMessage": userInput},
		},
	}
	if len(history) > 0 {
		payload["conversationState"].(map[string]any)["history"] = history
	}
	if strings.TrimSpace(profileArn) != "" {
		payload["profileArn"] = profileArn
	}
	return payload, nil
}

// splitMessages 拆出 system 文本与归一后的消息列表。
func splitMessages(msgs []channel.Message) (string, []unifiedMsg) {
	var systemParts []string
	out := make([]unifiedMsg, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			if t := strings.TrimSpace(m.Content); t != "" {
				systemParts = append(systemParts, t)
			}
		case "assistant":
			u := unifiedMsg{role: "assistant", content: m.Content}
			for _, tc := range m.ToolCalls {
				name := strings.TrimSpace(tc.Function.Name)
				if name == "" {
					continue // 没有函数名的调用无法回传（上游按名字校验）
				}
				u.toolUses = append(u.toolUses, map[string]any{
					"name":      name,
					"input":     argumentsJSON(tc.Function.Arguments),
					"toolUseId": tc.ID,
				})
			}
			out = append(out, u)
		case "tool":
			// 上游没有 tool 角色：工具结果挂在一条 user 消息的 toolResults 上。
			text := strings.TrimSpace(m.Content)
			if text == "" {
				text = "(empty result)"
			}
			out = append(out, unifiedMsg{role: "user", toolResults: []map[string]any{{
				"content":   []any{map[string]any{"text": text}},
				"status":    "success",
				"toolUseId": m.ToolCallID,
			}}})
		default: // user 及其它未知角色一律按 user 处理（归一在下一步做）
			out = append(out, unifiedMsg{role: "user", content: m.Content})
		}
	}
	return strings.Join(systemParts, "\n\n"), out
}

// argumentsJSON 把工具调用的参数串解成 JSON 值（解不出就给空对象，别把坏 JSON 发给上游）。
func argumentsJSON(args string) any {
	args = strings.TrimSpace(args)
	if args == "" {
		return map[string]any{}
	}
	var v any
	if json.Unmarshal([]byte(args), &v) != nil {
		return map[string]any{}
	}
	return v
}

// toHistoryEntry 把一条历史消息翻成上游形态。
func toHistoryEntry(m unifiedMsg, modelID string) map[string]any {
	if m.role == "assistant" {
		content := strings.TrimSpace(m.content)
		if content == "" {
			content = placeholder // 上游要求非空
		}
		entry := map[string]any{"assistantResponseMessage": map[string]any{"content": content}}
		if len(m.toolUses) > 0 {
			entry["assistantResponseMessage"].(map[string]any)["toolUses"] = m.toolUses
		}
		return entry
	}
	content := strings.TrimSpace(m.content)
	if content == "" {
		content = placeholder
	}
	inner := map[string]any{"content": content, "modelId": modelID, "origin": originAIEditor}
	if len(m.toolResults) > 0 {
		inner["userInputMessageContext"] = map[string]any{"toolResults": m.toolResults}
	}
	return map[string]any{"userInputMessage": inner}
}

// prependSystem 把 system 文本塞到一条历史 user 消息的最前面。
func prependSystem(entry map[string]any, system string) {
	inner, ok := entry["userInputMessage"].(map[string]any)
	if !ok {
		return
	}
	cur, _ := inner["content"].(string)
	if strings.TrimSpace(cur) == "" {
		inner["content"] = system
		return
	}
	inner["content"] = system + "\n\n" + cur
}

// ---------------------------------------------------------------------------
// 归一流水线
// ---------------------------------------------------------------------------

// stripToolContent 把工具调用/工具结果降级成文本。
//
// 用在「客户端没定义任何 tools，但历史里有工具痕迹」的场合。为什么不直接删掉：
// 删了模型就看不到自己上一轮做了什么，会重复调用同一个工具；转成文本至少保住了上下文。
func stripToolContent(msgs []unifiedMsg) []unifiedMsg {
	out := make([]unifiedMsg, 0, len(msgs))
	for _, m := range msgs {
		if len(m.toolUses) == 0 && len(m.toolResults) == 0 {
			out = append(out, m)
			continue
		}
		parts := make([]string, 0, 3)
		if t := strings.TrimSpace(m.content); t != "" {
			parts = append(parts, t)
		}
		for _, tu := range m.toolUses {
			name, _ := tu["name"].(string)
			args, _ := json.Marshal(tu["input"])
			id, _ := tu["toolUseId"].(string)
			parts = append(parts, "[Tool: "+name+" ("+id+")]\n"+string(args))
		}
		for _, tr := range m.toolResults {
			id, _ := tr["toolUseId"].(string)
			parts = append(parts, "[Tool Result ("+id+")]\n"+toolResultText(tr))
		}
		if len(parts) == 0 {
			parts = append(parts, placeholder)
		}
		m.toolUses, m.toolResults = nil, nil
		m.content = strings.Join(parts, "\n\n")
		out = append(out, m)
	}
	return out
}

// toolResultText 从 toolResults 项里取回文本（我们自己构造的形态：content[0].text）。
func toolResultText(tr map[string]any) string {
	if arr, ok := tr["content"].([]any); ok && len(arr) > 0 {
		if m, ok := arr[0].(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				return s
			}
		}
	}
	return ""
}

// ensureAssistantBeforeToolResults 给「孤儿工具结果」补一条 assistant，或者降级成文本。
//
// 为什么是降级而不是补：补一条 assistantResponseMessage 需要知道工具名与参数（上游会校验），
// 而孤儿结果里没有这些信息 —— 编一个假名字比降级成文本更危险。
func ensureAssistantBeforeToolResults(msgs []unifiedMsg) []unifiedMsg {
	out := make([]unifiedMsg, 0, len(msgs))
	for _, m := range msgs {
		if len(m.toolResults) > 0 {
			ok := len(out) > 0 && out[len(out)-1].role == "assistant" && len(out[len(out)-1].toolUses) > 0
			if !ok {
				text := strings.TrimSpace(m.content)
				var parts []string
				if text != "" {
					parts = append(parts, text)
				}
				for _, tr := range m.toolResults {
					id, _ := tr["toolUseId"].(string)
					parts = append(parts, "[Tool Result ("+id+")]\n"+toolResultText(tr))
				}
				if len(parts) == 0 {
					parts = append(parts, placeholder)
				}
				m.toolResults = nil
				m.content = strings.Join(parts, "\n\n")
			}
		}
		out = append(out, m)
	}
	return out
}

// mergeAdjacent 合并相邻的同角色消息（上游不接受连续两条同角色）。
func mergeAdjacent(msgs []unifiedMsg) []unifiedMsg {
	out := make([]unifiedMsg, 0, len(msgs))
	for _, m := range msgs {
		if len(out) == 0 || out[len(out)-1].role != m.role {
			out = append(out, m)
			continue
		}
		last := &out[len(out)-1]
		if a, b := strings.TrimSpace(last.content), strings.TrimSpace(m.content); a != "" && b != "" {
			last.content = a + "\n" + b
		} else if b != "" {
			last.content = b
		}
		last.toolUses = append(last.toolUses, m.toolUses...)
		last.toolResults = append(last.toolResults, m.toolResults...)
	}
	return out
}

// ensureFirstUser 保证首条是 user（上游要求，否则 400）。
func ensureFirstUser(msgs []unifiedMsg) []unifiedMsg {
	if len(msgs) == 0 || msgs[0].role == "user" {
		return msgs
	}
	return append([]unifiedMsg{{role: "user", content: placeholder}}, msgs...)
}

// ensureAlternating 在连续两条 user 之间插一条空的 assistant（上游要求严格交替）。
func ensureAlternating(msgs []unifiedMsg) []unifiedMsg {
	if len(msgs) < 2 {
		return msgs
	}
	out := make([]unifiedMsg, 0, len(msgs)+2)
	out = append(out, msgs[0])
	for _, m := range msgs[1:] {
		if m.role == "user" && out[len(out)-1].role == "user" {
			out = append(out, unifiedMsg{role: "assistant", content: placeholder})
		}
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------------
// 工具定义
// ---------------------------------------------------------------------------

// convertTools 把 OpenAI 形态的工具定义翻成上游要的 toolSpecification。
func convertTools(tools []map[string]any) ([]map[string]any, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		name, desc, params := openAIToolFn(t)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if len(name) > maxToolNameLen {
			// 上游硬限制 64 字符；本地拦下来才能给出可操作的提示
			//（上游只会回一句 "Improperly formed request"）。
			return nil, errs.New(errs.Parse, fmt.Sprintf(
				"工具名 %q 有 %d 个字符，超过 Kiro 上游的 %d 字符上限，请换短名",
				name, len(name), maxToolNameLen)).WithChannel(string(channel.Kiro))
		}
		if strings.TrimSpace(desc) == "" {
			desc = "Tool: " + name // 上游要求描述非空
		}
		out = append(out, map[string]any{
			"toolSpecification": map[string]any{
				"name":        name,
				"description": desc,
				"inputSchema": map[string]any{"json": sanitizeJSONSchema(params)},
			},
		})
	}
	return out, nil
}

// openAIToolFn 从一条工具定义里取出（名字、描述、JSON Schema）。
// 标准形态是 {"type":"function","function":{…}}；也兼容直接给 {name,parameters} 的裸形态。
func openAIToolFn(t map[string]any) (name, desc string, params map[string]any) {
	fn, _ := t["function"].(map[string]any)
	if fn == nil {
		fn = t
	}
	name, _ = fn["name"].(string)
	desc, _ = fn["description"].(string)
	if p, ok := fn["parameters"].(map[string]any); ok {
		params = p
	} else if p, ok := fn["input_schema"].(map[string]any); ok {
		params = p
	}
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return name, desc, params
}

// sanitizeJSONSchema 递归去掉上游不认的字段。
//
// 上游对 JSON Schema 的校验很严：`additionalProperties` 和**空的** `required: []`
// 都会直接 400 "Improperly formed request"（参考实现实测）。客户端生成的 schema
// 经常带这两个，所以在本地先洗掉。
func sanitizeJSONSchema(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "additionalProperties" {
				continue
			}
			if k == "required" {
				if arr, ok := val.([]any); ok && len(arr) == 0 {
					continue
				}
			}
			out[k] = sanitizeJSONSchema(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, it := range x {
			out = append(out, sanitizeJSONSchema(it))
		}
		return out
	default:
		return v
	}
}
