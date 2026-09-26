package toolshim

import (
	"strings"
	"testing"

	"poolgate/internal/channel"
)

// TestFullShimRoundTrip 端到端证明「网页模型 + toolshim」与「原生工具调用」对外合同一致：
//
//	① 客户端发 tools → BuildRequest 把工具定义翻进提示词、清掉 tools 字段；
//	② 上游回文本（三种形态：约定标记 / DSML 原生 / XML 原生）→ Wrap 解析出结构化 tool_calls；
//	③ 客户端回填 role=tool 结果 → BuildRequest 改写成可读文本；
//	④ 解析不出来时原文照发（红线一：绝不吞内容）。
func TestFullShimRoundTrip(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{
			"name":        "Read",
			"description": "读取文件内容",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string", "description": "文件路径"},
				},
				"required": []any{"file_path"},
			},
		}},
	}

	// ① 请求改写：tools 被清掉、工具说明进系统提示词、末尾带提醒。
	req := channel.ChatRequest{
		Model: "deepseek-chat",
		Messages: []channel.Message{
			{Role: "system", Content: "你是一个编程助手。"},
			{Role: "user", Content: "读一下 /etc/hosts"},
		},
		Tools: tools,
	}
	out := BuildRequest(req)
	if len(out.Tools) != 0 {
		t.Fatalf("tools 应被清掉，仍有 %d 个", len(out.Tools))
	}
	sys := out.Messages[0]
	if sys.Role != "system" {
		t.Fatalf("首条应为 system，得到 %q", sys.Role)
	}
	for _, want := range []string{"Read", "file_path", "工具调用协议", OpenTag} {
		if !strings.Contains(sys.Content, want) {
			t.Fatalf("系统提示词缺 %q：\n%s", want, sys.Content)
		}
	}
	last := out.Messages[len(out.Messages)-1]
	if !strings.Contains(last.Content, "工具调用格式提醒") {
		t.Fatalf("末尾应有格式提醒，得到：\n%s", last.Content)
	}

	// ② 三种上游输出形态都应被解析成结构化 tool_calls。
	bar := "\uff5c\uff5c" // 全角竖线 ×2（DSML 的真实形态）
	cases := []struct {
		name    string
		upstrm  string
		wantArg string
	}{
		{"约定标记", `<tool_call>{"name":"Read","arguments":{"file_path":"/etc/hosts"}}</tool_call>`, "/etc/hosts"},
		{"DSML 原生", bar + "DSML" + bar + " calls>\n" +
			bar + "DSML" + bar + " invoke name=\"Read\">\n" +
			bar + "DSML" + bar + " parameter name=\"file_path\" string=\"true\">/etc/hosts</" + bar + "DSML" + bar + " parameter>\n" +
			"</" + bar + "DSML" + bar + " invoke>\n</" + bar + "DSML" + bar + " calls>", "/etc/hosts"},
		{"XML 原生", `<function_calls><invoke name="Read"><parameter name="file_path">/etc/hosts</parameter></invoke></function_calls>`, "/etc/hosts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Parser{}
			text, calls := p.Feed(c.upstrm)
			text2, calls2 := p.Flush()
			all := append(calls, calls2...)
			if len(all) != 1 {
				t.Fatalf("应解析出 1 个 tool_call，得到 %d（正文=%q）", len(all), text+text2)
			}
			if all[0].Function.Name != "Read" {
				t.Fatalf("工具名应为 Read，得到 %q", all[0].Function.Name)
			}
			if !strings.Contains(all[0].Function.Arguments, c.wantArg) {
				t.Fatalf("参数应含 %q，得到 %q", c.wantArg, all[0].Function.Arguments)
			}
			if all[0].Type != "function" {
				t.Fatalf("type 应为 function，得到 %q", all[0].Type)
			}
			if all[0].ID == "" {
				t.Fatal("tool_call 必须有 id")
			}
		})
	}

	// ③ 客户端回填工具结果 → 改写成可读文本（上游没有 tool 角色）。
	withTool := channel.ChatRequest{
		Messages: []channel.Message{
			{Role: "user", Content: "读一下 /etc/hosts"},
			{Role: "assistant", ToolCalls: []channel.ToolCall{{
				ID: "c1", Type: "function",
				Function: channel.FunctionCall{Name: "Read", Arguments: `{"file_path":"/etc/hosts"}`},
			}}},
			{Role: "tool", Name: "Read", Content: "127.0.0.1 localhost"},
			{Role: "user", Content: "谢谢"},
		},
		Tools: tools,
	}
	rewritten := BuildRequest(withTool)
	var sawToolResult, sawAssistantCalls bool
	for _, m := range rewritten.Messages {
		if m.Role == "tool" {
			t.Fatalf("不应残留 role=tool 消息")
		}
		if strings.Contains(m.Content, "[工具执行结果] Read") {
			sawToolResult = true
		}
		if m.Role == "assistant" && strings.Contains(m.Content, OpenTag) {
			sawAssistantCalls = true
		}
	}
	if !sawToolResult {
		t.Fatal("工具结果应被改写成「[工具执行结果] …」文本")
	}
	if !sawAssistantCalls {
		t.Fatal("assistant 的 tool_calls 应被改写成标记文本（上游不认识 tool_calls 字段）")
	}

	// ④ 解析不出来时原文照发（红线一）。
	p := &Parser{}
	garbage := "模型胡说了一段完全不是工具标记的话。"
	txt, cs := p.Feed(garbage)
	txt2, cs2 := p.Flush()
	if len(cs)+len(cs2) != 0 {
		t.Fatalf("不该误判出工具调用：%+v", append(cs, cs2...))
	}
	if got := txt + txt2; !strings.Contains(got, "模型胡说") {
		t.Fatalf("解析失败时原文必须照发，得到 %q", got)
	}
}
