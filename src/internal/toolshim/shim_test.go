package toolshim

import (
	"errors"
	"io"
	"strings"
	"testing"

	"poolgate/internal/channel"
)

func names(calls []channel.ToolCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Function.Name)
	}
	return out
}

// 标记被分片切开也要解析出来 —— SSE 真会把 `<tool_` 和 `call>` 分两片送。
func TestParserHandlesSplitMarkers(t *testing.T) {
	p := &Parser{}
	var text strings.Builder
	var calls []channel.ToolCall
	pieces := []string{"我来查一下。", "<tool", "_call", ">{\"name\":\"get_weather\",", "\"arguments\":{\"city\":", "\"北京\"}}<", "/tool_call>", "稍等。"}
	for _, s := range pieces {
		tx, cs := p.Feed(s)
		text.WriteString(tx)
		calls = append(calls, cs...)
	}
	tx, cs := p.Flush()
	text.WriteString(tx)
	calls = append(calls, cs...)

	if got := text.String(); got != "我来查一下。稍等。" {
		t.Fatalf("正文不该被吃掉也不该混入标记，got %q", got)
	}
	if len(calls) != 1 || calls[0].Function.Name != "get_weather" ||
		calls[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("分片标记解析失败：%+v", calls)
	}
}

// 没有标记的普通对话要原样透传（包括以 < 开头的正文）。
func TestParserKeepsPlainText(t *testing.T) {
	p := &Parser{}
	var out strings.Builder
	for _, s := range []string{"2 小于 3 是", "对的，", "而 <tool 只是", "一半的标签。"} {
		tx, cs := p.Feed(s)
		out.WriteString(tx)
		if len(cs) != 0 {
			t.Fatalf("不该解析出调用：%+v", cs)
		}
	}
	tx, _ := p.Flush()
	out.WriteString(tx)
	if !strings.Contains(out.String(), "对的，") || !strings.Contains(out.String(), "一半的标签。") {
		t.Fatalf("正文丢了：%q", out.String())
	}
}

// 模型手滑的几种形态：代码围栏、arguments 是字符串、多个调用并排。
func TestParserTolerantForms(t *testing.T) {
	cases := []struct{ in, name, args string }{
		{"```json\n<tool_call>{\"name\":\"a\",\"arguments\":{}}</tool_call>\n```", "a", "{}"},
		{"<tool_call>{\"name\":\"b\",\"arguments\":\"{\\\"x\\\":1}\"}</tool_call>", "b", `{"x":1}`},
		{"<tool_call>\n  {\"name\":\"c\",\"arguments\":null}\n</tool_call>", "c", "{}"},
	}
	for _, c := range cases {
		p := &Parser{}
		_, calls := p.Feed(c.in)
		if len(calls) != 1 || calls[0].Function.Name != c.name || calls[0].Function.Arguments != c.args {
			t.Errorf("输入 %q → %+v，want name=%s args=%s", c.in, calls, c.name, c.args)
		}
	}

	// 并排两个调用 → index 递增，便于客户端按 index 归并
	p := &Parser{}
	_, calls := p.Feed("<tool_call>{\"name\":\"a\",\"arguments\":{}}</tool_call><tool_call>{\"name\":\"b\",\"arguments\":{}}</tool_call>")
	if len(calls) != 2 || calls[0].Index != 0 || calls[1].Index != 1 || calls[1].ID == calls[0].ID {
		t.Fatalf("并排调用解析不对：%+v", calls)
	}
}

// 解析不出来时**绝不丢内容**：标记体不合法就原样当正文还回去（红线一）。
func TestParserNeverLosesContent(t *testing.T) {
	p := &Parser{}
	text, calls := p.Feed("说明：<tool_call>这不是 JSON</tool_call>结束")
	if len(calls) != 0 {
		t.Fatalf("不该解析出调用：%+v", calls)
	}
	if !strings.Contains(text, "这不是 JSON") || !strings.Contains(text, "说明：") || !strings.Contains(text, "结束") {
		t.Fatalf("解析失败的内容被吞了：%q", text)
	}
	// 未闭合的标记（模型忘了收尾）也要尽力解析
	p2 := &Parser{}
	_, _ = p2.Feed("<tool_call>{\"name\":\"x\",\"arguments\":{\"a\":1}}")
	_, calls2 := p2.Flush()
	if len(calls2) != 1 || calls2[0].Function.Name != "x" {
		t.Fatalf("未闭合标记应尽力解析：%+v", calls2)
	}
}

// BuildRequest：工具说明进系统提示词、tools 清空、工具消息改写成纯文本。
func TestBuildRequestRewrites(t *testing.T) {
	req := channel.ChatRequest{
		Model: "doubao-pro",
		Messages: []channel.Message{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "北京天气"},
			{Role: "assistant", ToolCalls: []channel.ToolCall{{ID: "c1", Function: channel.FunctionCall{
				Name: "get_weather", Arguments: `{"city":"北京"}`}}}},
			{Role: "tool", Name: "get_weather", ToolCallID: "c1", Content: "晴 26℃"},
		},
		Tools: []map[string]any{{
			"type": "function",
			"function": map[string]any{"name": "get_weather", "description": "查天气",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		}},
		ToolChoice: "auto",
	}
	out := BuildRequest(req)
	if out.Tools != nil || out.ToolChoice != nil {
		t.Fatal("适配后不该还带 tools/tool_choice（上游没有这个位置）")
	}
	if len(out.Messages) != 4 {
		t.Fatalf("消息条数不该变（系统消息合并成一条）：%d", len(out.Messages))
	}
	sys := out.Messages[0]
	if sys.Role != "system" || !strings.Contains(sys.Content, "你是助手") ||
		!strings.Contains(sys.Content, "get_weather") || !strings.Contains(sys.Content, OpenTag) {
		t.Fatalf("系统提示词没并进工具说明：%q", sys.Content)
	}
	if a := out.Messages[2]; !strings.Contains(a.Content, `<tool_call>`) ||
		!strings.Contains(a.Content, `"name":"get_weather"`) {
		t.Fatalf("assistant 的 tool_calls 应改写成标记：%q", a.Content)
	}
	if tr := out.Messages[3]; tr.Role != "user" || !strings.Contains(tr.Content, "[工具执行结果]") ||
		!strings.Contains(tr.Content, "晴 26℃") {
		t.Fatalf("role=tool 应改写成可读的消息：%+v", tr)
	}
}

// sliceStream 是测试用的内存流。
type sliceStream struct {
	chunks []channel.ChatCompletionChunk
	i      int
}

func (s *sliceStream) Next() (channel.ChatCompletionChunk, error) {
	if s.i >= len(s.chunks) {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}
func (s *sliceStream) Close() error { return nil }

func textChunk(s string) channel.ChatCompletionChunk {
	c := channel.ChatCompletionChunk{ID: "c1", Model: "m"}
	ch := channel.ChunkChoice{}
	ch.Delta.Role = "assistant"
	ch.Delta.Content = s
	c.Choices = append(c.Choices, ch)
	return c
}

// Wrap 端到端：文本流 → 结构化 tool_calls + finish_reason=tool_calls，且正文不丢。
func TestWrapEndToEnd(t *testing.T) {
	st := Wrap(&sliceStream{chunks: []channel.ChatCompletionChunk{
		textChunk("查一下："),
		textChunk(`<tool_call>{"name":"get_weather","arguments":{"city":"北京"}}`),
		textChunk("</tool_call>"),
		textChunk("好了。"),
	}})
	defer st.Close()

	var text strings.Builder
	var calls []channel.ToolCall
	finish := ""
	for {
		c, err := st.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("流读取失败：%v", err)
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
			calls = append(calls, ch.Delta.ToolCalls...)
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	if got := text.String(); got != "查一下：好了。" {
		t.Fatalf("正文不对：%q", got)
	}
	if len(calls) != 1 || calls[0].Function.Name != "get_weather" || calls[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("工具调用不对：%+v", calls)
	}
	if finish != "tool_calls" {
		t.Fatalf("有工具调用时 finish_reason 应为 tool_calls，got %q", finish)
	}
	if n := len(names(calls)); n != 1 {
		t.Fatalf("调用数不对：%d", n)
	}
}

// TestToolPromptListsNamesUpFrontAndDedupes 校验工具说明的两条新性质：
// ① 工具名清单出现在开头（长请求下模型能先扫到有哪些工具）；
// ② 同名工具去重（与 OpenAI 语义一致，后出现的覆盖先出现的）。
func TestToolPromptListsNamesUpFrontAndDedupes(t *testing.T) {
	tools := []map[string]any{
		{"function": map[string]any{"name": "Read", "description": "读文件",
			"parameters": map[string]any{"type": "object"}}},
		{"function": map[string]any{"name": "Bash", "description": "执行命令",
			"parameters": map[string]any{"type": "object"}}},
		{"function": map[string]any{"name": "Read", "description": "读文件（新版）",
			"parameters": map[string]any{"type": "object"}}},
	}
	got := ToolPrompt(tools)
	if !strings.Contains(got, "共 2 个") {
		t.Fatalf("应去重为 2 个工具，得到：\n%s", got)
	}
	if !strings.Contains(got, "Read / Bash") {
		t.Fatalf("开头应有工具名清单，得到：\n%s", got)
	}
	// 说明取后出现的那个（覆盖语义）。
	if !strings.Contains(got, "读文件（新版）") {
		t.Fatalf("同名工具应取后者，得到：\n%s", got)
	}
	// 名称清单必须出现在正文前 300 字符内（证明它真的在开头）。
	idx := strings.Index(got, "Read / Bash")
	if idx < 0 || idx > 300 {
		t.Fatalf("工具名清单应在开头 300 字符内，实际位置 %d", idx)
	}
}

// TestToolPromptHasPositiveAndNegativeExample 校验正例/反例都在，
// 这是实测里压住「模型用自然语言描述」的关键措辞。
func TestToolPromptHasPositiveAndNegativeExample(t *testing.T) {
	got := ToolPrompt([]map[string]any{
		{"function": map[string]any{"name": "Read", "description": "读文件"}},
	})
	for _, want := range []string{"正例：", "反例（禁止）", OpenTag, CloseTag} {
		if !strings.Contains(got, want) {
			t.Fatalf("工具说明缺 %q：\n%s", want, got)
		}
	}
}
