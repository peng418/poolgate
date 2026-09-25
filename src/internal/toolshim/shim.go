// Package toolshim 是「工具调用模拟层」：让**只会聊天的上游**也能给 coding agent 用。
//
// 背景：扫码登录来的网页聊天模型（豆包/DeepSeek/Kimi 这类）没有工具调用协议 ——
// 它们的接口只有「发消息 → 回文本」，没有放 tools 的位置。硬要它给 Studio / Claude Code
// 当后端，模型只能把「我要调用工具」写成文本，客户端拿不到结构化 tool_calls，
// 表现就是「不回复 / 回一堆乱码」（0.4.2 修的是我们**自己**丢 tools 的那个 bug，
// 这一层修的是**上游本来就没有**的那种情况）。
//
// 做法（业界叫 prompt-based tool calling / tool-call shim）：
//
//	客户端发来 tools
//	  ↓ 我们把工具定义 + 输出格式写进系统提示词
//	上游回文本
//	  ↓ 我们把 <tool_call>{"name":…,"arguments":…}</tool_call> 解析回结构化 tool_calls
//	客户端执行工具、把结果回传
//	  ↓ 我们把 role=tool 改写成「[工具执行结果] …」的普通消息
//	循环
//
// 与「原生工具调用」的差别必须对用户透明：可靠性依赖模型是否守格式，因此
// 解析层对格式变体尽量宽容（代码围栏、arguments 是字符串、标记被分片切开都要吃下），
// 而**解析不出来时绝不丢内容** —— 一律当正文交给客户端（红线一）。
package toolshim

import (
	"encoding/json"
	"fmt"
	"strings"

	"poolgate/internal/channel"
)

// OpenTag / CloseTag 是约定的工具调用标记。
//
// 选这两个标记的理由：模型见过它们（各家 agent 框架都用类似形状），出现概率高；
// 又是 XML 风格的独立标签，不容易与正常正文撞车。
const (
	OpenTag  = "<tool_call>"
	CloseTag = "</tool_call>"
)

// ToolPrompt 生成写进系统提示词的工具说明。
//
// 三条硬要求：① 工具定义原样给出（参数 schema 不简化，否则模型编参数）；
// ② 输出格式写死并且**要求只输出标记**（多写解释会让解析层和用户都困惑）；
// ③ 告诉它工具结果长什么样，否则第二轮它不认识回传的结果。
func ToolPrompt(tools []map[string]any) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("你可以调用工具。可用工具如下（JSON Schema）：\n\n")
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		if d, _ := fn["description"].(string); d != "" {
			fmt.Fprintf(&b, "- %s：%s\n", name, d)
		} else {
			fmt.Fprintf(&b, "- %s\n", name)
		}
		params := fn["parameters"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		raw, _ := json.Marshal(params)
		fmt.Fprintf(&b, "  参数格式：%s\n", string(raw))
	}
	b.WriteString("\n当你需要调用工具时，**只输出**下面这种标记（不要写额外的解释、不要用代码围栏）：\n")
	b.WriteString(OpenTag + `{"name":"工具名","arguments":{"参数名":值}}` + CloseTag + "\n")
	b.WriteString("需要同时调用多个工具，就并排写多个这样的标记。arguments 必须是合法 JSON 对象。\n")
	b.WriteString("不需要调用工具时，正常用自然语言回答即可。\n")
	b.WriteString("工具执行结果会以「[工具执行结果]」开头回给你，请据此继续回答用户。\n")
	return b.String()
}

// BuildRequest 把「带工具定义的请求」改写成「只会聊天的上游能吃的请求」。
//
// 三件事：① 工具说明并进系统提示词；② 清掉 tools/tool_choice（上游没有这个位置，
// 留着有些上游会 400）；③ 把 assistant 的 tool_calls 与 role=tool 的历史改写成
// 纯文本 —— 否则上游看到不认识的字段/角色会报错，或者把工具结果当成用户的话。
func BuildRequest(req channel.ChatRequest) channel.ChatRequest {
	if len(req.Tools) == 0 {
		return req
	}
	out := req
	out.Tools = nil
	out.ToolChoice = nil

	// 1) 把既有 system 消息合并成一条，再接上工具说明
	var systems []string
	rest := make([]channel.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		if role == "system" {
			if s := strings.TrimSpace(m.Content); s != "" {
				systems = append(systems, s)
			}
			continue
		}
		m.Role = role
		rest = append(rest, rewriteMessage(m))
	}
	if p := ToolPrompt(req.Tools); p != "" {
		systems = append(systems, p)
	}
	msgs := make([]channel.Message, 0, len(rest)+1)
	if len(systems) > 0 {
		msgs = append(msgs, channel.Message{Role: "system", Content: strings.Join(systems, "\n\n")})
	}
	msgs = append(msgs, rest...)
	out.Messages = msgs
	return out
}

// rewriteMessage 把工具语义的消息改写成纯文本形态。
func rewriteMessage(m channel.Message) channel.Message {
	if m.Role == "assistant" && len(m.ToolCalls) > 0 {
		var b strings.Builder
		if s := strings.TrimSpace(m.Content); s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
		for _, tc := range m.ToolCalls {
			args := strings.TrimSpace(tc.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			fmt.Fprintf(&b, "%s{\"name\":%q,\"arguments\":%s}%s",
				OpenTag, tc.Function.Name, args, CloseTag)
		}
		return channel.Message{Role: "assistant", Content: b.String()}
	}
	if m.Role == "tool" {
		// 上游没有 tool 角色：改写成一条「可读的工具结果」消息。
		head := "[工具执行结果]"
		if m.Name != "" {
			head += " " + m.Name
		}
		return channel.Message{Role: "user", Content: head + "\n" + m.Content}
	}
	return m
}
