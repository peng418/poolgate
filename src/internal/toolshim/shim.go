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
	"strconv"
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
	// 先把有效工具挑出来（同名去重：后面的覆盖前面的，与 OpenAI 语义一致）。
	type toolDef struct {
		name, desc string
		params     any
	}
	var defs []toolDef
	seen := map[string]int{}
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		desc, _ := fn["description"].(string)
		params := fn["parameters"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if i, ok := seen[name]; ok {
			defs[i] = toolDef{name: name, desc: desc, params: params}
			continue
		}
		seen[name] = len(defs)
		defs = append(defs, toolDef{name: name, desc: desc, params: params})
	}
	if len(defs) == 0 {
		return ""
	}

	var b strings.Builder
	// 开头就把「最重要的一件事」放在生成位置最近处能看见的地方：必须用标记、不许用自然语言描述。
	b.WriteString("【工具调用协议 · 必须遵守】\n")
	// 工具名索引紧跟其后（实测位置敏感：越靠前越不会被长请求淹没）。
	b.WriteString("可用工具（共 ")
	b.WriteString(strconv.Itoa(len(defs)))
	b.WriteString(" 个）：")
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.name)
	}
	b.WriteString(strings.Join(names, " / "))
	b.WriteString("\n")
	b.WriteString("**需要动作时（读文件 / 写文件 / 执行命令 / 搜索 / 打开网页等），")
	b.WriteString("你必须先真正发出工具调用，而不是用自然语言描述你打算怎么做，也不是凭已有知识直接作答。**\n\n")

	// 输出格式写死，并给出「正例 / 反例」——实测反例能显著降低模型写成自然语言或 markdown 的概率。
	b.WriteString("调用格式（**只输出这个标记本身，不要写任何解释、不要用 ``` 代码围栏、不要加前缀**）：\n")
	b.WriteString(OpenTag + `{"name":"工具名","arguments":{"参数名":值}}` + CloseTag + "\n")
	b.WriteString("多个工具并排写多个标记。arguments 必须是合法 JSON 对象（键值用双引号）。\n")
	b.WriteString("正例：" + OpenTag + `{"name":"Read","arguments":{"file_path":"/tmp/a.txt"}}` + CloseTag + "\n")
	b.WriteString("反例（禁止）：先写「我来读一下文件」再写标记；写成 ```json 代码块；把参数写成非 JSON。\n\n")

	// 完整 schema 逐条给（不简化 —— 简化会让模型编参数）。
	b.WriteString("各工具参数 schema：\n")
	for _, d := range defs {
		if d.desc != "" {
			fmt.Fprintf(&b, "\n● %s —— %s\n", d.name, d.desc)
		} else {
			fmt.Fprintf(&b, "\n● %s\n", d.name)
		}
		raw, _ := json.Marshal(d.params)
		fmt.Fprintf(&b, "  %s\n", string(raw))
	}

	b.WriteString("\n不需要调用工具时，正常用自然语言回答即可。\n")
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
	out.Messages = withReminder(msgs, ReminderSuffix)
	return out
}

// ReminderSuffix 是接在**对话末尾**的格式提醒（不是写进系统提示词的那份）。
//
// 为什么末尾还要再说一遍：实测（2026-09-26，豆包网页渠道，30 个工具 + 2 万字 agent 系统
// 提示词、真实口吻的请求）只把约定放在系统提示词里时，4/4 次都直接用自然语言回答、
// 一个工具都不调；同一份请求只在最后一条消息末尾补这句，4/4 次都正确调用工具
// （doubao 另一个档位 2/3）。系统提示词一长，那里的约定就被淹没了，
// 而紧挨着生成位置的那句话才起作用。所以两份都留着：系统里那份给完整说明，末尾这份保命中率。
//
// 措辞要点（照实测调过）：把「必须先用工具」和「不要用自然语言描述」两句都写死，
// 并给出可直接照抄的标记形状 —— 只写「你可以调用工具」实测无效。
const ReminderSuffix = "\n\n[工具调用格式提醒] 只要这个任务需要动作（读文件、写文件、查日志、执行命令、搜索等），" +
	"你必须**先调用工具**再作答：只输出 " + OpenTag + `{"name":"工具名","arguments":{…}}` + CloseTag + "，" +
	"不要用自然语言描述你打算怎么做，也不要凭已有知识直接回答。可用工具见系统提示词。"

// withReminder 把提醒接在最后一条消息末尾；末尾不是用户侧消息时补一条。
//
// 接在末尾而不是新建一条：agent 循环里最后一条通常就是刚回填的工具结果，
// 那句话本来就该紧跟着它；新建一条又会把上游的「单轮」结构撑成两轮。
func withReminder(msgs []channel.Message, reminder string) []channel.Message {
	if reminder == "" || len(msgs) == 0 {
		return msgs
	}
	last := &msgs[len(msgs)-1]
	if last.Role == "user" {
		last.Content = strings.TrimRight(last.Content, "\n") + reminder
		return msgs
	}
	return append(msgs, channel.Message{Role: "user", Content: strings.TrimLeft(reminder, "\n")})
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
