package toolshim

import (
	"strings"
	"testing"

	"poolgate/internal/channel"
)

// tag 造一个 DSML 标记（全角竖线 U+FF5C 在源码里写成转义，免得文件编码出岔子）。
func tag(op string) string { return "｜｜DSML｜｜ " + op + ">" }

// 真实抓包的形态（2026-09-26，deepseek 网页渠道，30 工具 + 2 万字系统提示词）：
// 模型不守 <tool_call> 约定，改用自己那套 DSML —— 必须翻译回结构化 tool_calls。
func TestParserNativeDSML(t *testing.T) {
	full := "我来读一下。\n" +
		tag("calls") + "\n" +
		tag(`invoke name="Read"`) + "\n" +
		tag(`parameter name="file_path" string="true"`) + "/tmp/a.txt</｜｜DSML｜｜ parameter>\n" +
		"</｜｜DSML｜｜ invoke>\n" +
		"</｜｜DSML｜｜ calls>\n" +
		"读完了。"

	// 一片一片喂（SSE 就是碎的），正文与调用都必须在。
	p := &Parser{}
	var text strings.Builder
	var calls []channel.ToolCall
	for i := 0; i < len(full); i += 7 {
		end := i + 7
		if end > len(full) {
			end = len(full)
		}
		tx, cs := p.Feed(full[i:end])
		text.WriteString(tx)
		calls = append(calls, cs...)
	}
	tx, cs := p.Flush()
	text.WriteString(tx)
	calls = append(calls, cs...)

	if len(calls) != 1 {
		t.Fatalf("DSML 应解析成 1 个调用，got %+v（正文 %q）", calls, text.String())
	}
	if calls[0].Function.Name != "Read" || calls[0].Function.Arguments != `{"file_path":"/tmp/a.txt"}` {
		t.Fatalf("DSML 抽取不对：%+v", calls[0])
	}
	if !strings.Contains(text.String(), "我来读一下。") || !strings.Contains(text.String(), "读完了。") {
		t.Fatalf("正文丢了：%q", text.String())
	}
	if strings.Contains(text.String(), "DSML") {
		t.Fatalf("标记泄漏到正文了：%q", text.String())
	}
}

// 没有 calls 那层包裹、模型也忘了收尾时，Flush 仍要抽出来，且不能把标记后面的正文当参数值。
func TestParserNativeDSMLUnwrapped(t *testing.T) {
	p := &Parser{}
	_, calls := p.Feed(tag(`invoke name="Bash"`) + "\n" + tag(`parameter name="command"`) + "ls -l")
	tx, cs := p.Flush()
	calls = append(calls, cs...)
	if len(calls) != 1 || calls[0].Function.Name != "Bash" || calls[0].Function.Arguments != `{"command":"ls -l"}` {
		t.Fatalf("未收尾的 DSML 抽取不对：%+v（正文 %q）", calls, tx)
	}
	if strings.TrimSpace(tx) != "" {
		t.Fatalf("不该有正文残留：%q", tx)
	}

	// 标记之后还有正文：正文要还出来，不能被塞进参数。
	p2 := &Parser{}
	_, _ = p2.Feed(tag(`invoke name="Bash"`) + "\n" + tag(`parameter name="command"`) + "ls</｜｜DSML｜｜ parameter>\n" +
		"</｜｜DSML｜｜ invoke>\n后面这句是给用户的。")
	tx2, cs2 := p2.Flush()
	if len(cs2) != 1 || cs2[0].Function.Arguments != `{"command":"ls"}` {
		t.Fatalf("参数里混进了尾部正文：%+v", cs2)
	}
	if !strings.Contains(tx2, "后面这句是给用户的。") {
		t.Fatalf("尾部正文被吞了：%q", tx2)
	}
}

// XML 家族（<function_calls><invoke>）走同一套抽取器；string="false" 的值按 JSON 解回原类型。
func TestParserNativeXMLFamily(t *testing.T) {
	in := `<function_calls><invoke name="Search"><parameter name="query">北京</parameter>` +
		`<parameter name="limit" string="false">10</parameter></invoke></function_calls>`
	p := &Parser{}
	_, calls := p.Feed(in)
	if len(calls) != 1 || calls[0].Function.Name != "Search" {
		t.Fatalf("XML 家族抽取不对：%+v", calls)
	}
	if calls[0].Function.Arguments != `{"limit":10,"query":"北京"}` {
		t.Fatalf("参数值类型不对：%s", calls[0].Function.Arguments)
	}
}

// 认不出来的原生标记：原文（连同标记）必须交出去，不能吞（红线一）。
func TestParserNativeUnknownKept(t *testing.T) {
	p := &Parser{}
	in := "说明：" + tag("calls") + "\n" + tag("whatever") + "\n</｜｜DSML｜｜ calls>"
	tx, calls := p.Feed(in)
	tx2, cs := p.Flush()
	tx += tx2
	calls = append(calls, cs...)
	if len(calls) != 0 {
		t.Fatalf("不该解析出调用：%+v", calls)
	}
	for _, want := range []string{"说明：", "DSML", "whatever"} {
		if !strings.Contains(tx, want) {
			t.Fatalf("原文被吞了（缺 %q）：%q", want, tx)
		}
	}
}

// 复数标记与「复数外壳里嵌多个单数调用」都要认（参考实现里两种写法都在用，
// 不认就是标记泄漏成正文 —— 正是用户看到的乱码形态）。
func TestParserAcceptsPluralAndNestedTags(t *testing.T) {
	p := &Parser{}
	if _, calls := p.Feed(`<tool_calls>{"name":"a","arguments":{"x":1}}</tool_calls>`); len(calls) != 1 ||
		calls[0].Function.Name != "a" || calls[0].Function.Arguments != `{"x":1}` {
		t.Fatalf("复数标记没被认出来：%+v", calls)
	}

	p2 := &Parser{}
	_, calls2 := p2.Feed(`<tool_calls><tool_call>{"name":"a","arguments":{"x":1}}</tool_call>` +
		`<tool_call>{"name":"b","arguments":{}}</tool_call></tool_calls>`)
	if len(calls2) != 2 || calls2[0].Function.Name != "a" || calls2[1].Function.Name != "b" ||
		calls2[1].Index != 1 {
		t.Fatalf("复数外壳里的多个调用没被拆出来：%+v", calls2)
	}

	// 复数标记里不是合法调用：原文（连标记）照还，不吞。
	p3 := &Parser{}
	tx, calls3 := p3.Feed("前<tool_calls>这不是 JSON</tool_calls>后")
	if len(calls3) != 0 {
		t.Fatalf("不该解析出调用：%+v", calls3)
	}
	for _, want := range []string{"前", "这不是 JSON", "后"} {
		if !strings.Contains(tx, want) {
			t.Fatalf("原文被吞了（缺 %q）：%q", want, tx)
		}
	}
}

// 格式提醒必须落在**对话末尾**（最后一条消息里），而不是只写在系统提示词里。
func TestBuildRequestPutsReminderAtTail(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{
		"name": "get_weather", "description": "查天气",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}

	out := BuildRequest(channel.ChatRequest{
		Messages: []channel.Message{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "北京天气"},
		},
		Tools: tools,
	})
	if len(out.Messages) != 2 {
		t.Fatalf("不该新增消息：%d", len(out.Messages))
	}
	last := out.Messages[len(out.Messages)-1]
	if !strings.Contains(last.Content, ReminderSuffix) || !strings.HasPrefix(last.Content, "北京天气") {
		t.Fatalf("提醒没接在最后一条消息末尾：%q", last.Content)
	}
	if !strings.Contains(out.Messages[0].Content, OpenTag) {
		t.Fatalf("系统提示词里那份完整说明也该在：%q", out.Messages[0].Content)
	}

	// 末尾是 assistant（少见）时补一条 user，而不是塞进 assistant 的正文。
	out2 := BuildRequest(channel.ChatRequest{
		Messages: []channel.Message{
			{Role: "system", Content: "你是助手"},
			{Role: "assistant", Content: "好的"},
		},
		Tools: tools,
	})
	if n := len(out2.Messages); n != 3 || out2.Messages[n-1].Role != "user" ||
		!strings.Contains(out2.Messages[n-1].Content, strings.TrimSpace(ReminderSuffix)) {
		t.Fatalf("末尾不是用户消息时应补一条：%+v", out2.Messages)
	}
}
