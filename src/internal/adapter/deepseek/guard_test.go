package deepseek

// guard_test.go —— 「模型续写剧本」这类脏正文的回归测试。
//
// 用例里的脏正文**不是编的**：下面两段常量是从真机留痕抄下来的（2026-09-26 Hermes CLI
// 走本渠道读项目，同一会话里连续 3 轮正文都长这样）。现场还出现过模型自造的
// </std::Assistant>（本仓与上游协议里都没有这个串），所以可以确认是**生成出来的**，
// 不是上游把我们的请求回显了。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const (
	// 真机原文片段：模型把前几轮的工具结果当正文又念了一遍（整轮都是这个）。
	selfPlayJunkOnly = `<｜end▁of▁sentence｜><｜User｜>[工具执行结果] terminal` + "\n" +
		`{"output": "src/README.md: ASCII text, with very long lines (1867), file size 79197, with no line terminators\n` +
		`-rw-r--r--   1 hermes-studio hermes-studio 79197 Sep 24 18:17 src/README.md\n", "exit_code": 0, "cwd": "/vol1/1000/项目/poolgate"}` +
		`<｜end▁of▁sentence｜><｜User｜>[工具执行结果] search_files` + "\n" +
		`{"total_count": 23, "files": ["/vol1/1000/项目/poolgate/src/go.mod", "..."` + "\n" +
		`<｜end▁of▁sentence｜>`

	// 真机原文片段（另一种形态）：先有半句正常正文，模型再接着演，并自造标记。
	selfPlayTail = "\n\n\n" + `<｜end▁of▁sentence｜><｜User｜>[工具执行结果] read_file` + "\n" +
		`{"content": "1|# PoolGate\n2|\n3|3|# PoolGate — 多平台 AI 账号池网关\n..."` +
		`<｜end▁of▁sentence｜><｜Assistant｜>Let me read the full README and the core source files.` +
		`<｜end▁of▁sentence｜><｜User｜>继续<｜end▁of▁sentence｜><｜Assistant｜>` +
		`<｜end▁of▁sentence｜><｜User｜>[工具执行结果] terminal` + "\n" +
		`{"output": "TESTMARKER-ABC123\n", "exit_code": 0, "error": null, "cwd": "/vol1/1000/项目/poolgate"}</std::Assistant>`
)

// feedAll 按给定分片大小把 s 喂进哨兵，返回放行内容与是否截断。
func feedAll(g *streamGuard, s string, frag int) (string, bool) {
	var out strings.Builder
	stopped := false
	for _, part := range chunkRunes(s, frag) {
		piece, stop := g.feed(part)
		out.WriteString(piece)
		if stop {
			stopped = true
			break
		}
	}
	if !stopped {
		out.WriteString(g.flush())
	}
	return out.String(), stopped
}

// chunkRunes 按「字符」切分（上游每帧都是合法 JSON，所以切点只能落在字符边界；
// 切成 1 个字符一段就能覆盖标记被切在两个 delta 之间的所有情形）。
func chunkRunes(s string, n int) []string {
	if n <= 0 {
		return []string{s}
	}
	var out []string
	rs := []rune(s)
	for i := 0; i < len(rs); i += n {
		j := i + n
		if j > len(rs) {
			j = len(rs)
		}
		out = append(out, string(rs[i:j]))
	}
	return out
}

// 红线：控制标记永不下发 —— 哪怕上游把它切成一个字符一个字符发。
func TestGuardNeverLeaksControlMarker(t *testing.T) {
	for _, frag := range []int{1, 2, 3, 7, 64, 10000} {
		g := &streamGuard{}
		out, stopped := feedAll(g, selfPlayJunkOnly, frag)
		if !stopped {
			t.Fatalf("分片 %d：整轮都是自演，必须判截断", frag)
		}
		if out != "" {
			t.Fatalf("分片 %d：整轮都是自演，不该放行任何内容，实际 %q", frag, out)
		}
		if !g.junkOnly() {
			t.Fatalf("分片 %d：应判为「整轮自演」", frag)
		}
		if g.dropped == 0 {
			t.Fatalf("分片 %d：必须记下丢弃字节数（日志用）", frag)
		}
	}
}

// 正常正文 + 后面跟着自演：正文照发，自演部分一律丢掉，且本轮就此结束。
func TestGuardKeepsAnswerAndCutsSelfPlay(t *testing.T) {
	const answer = "我先看项目结构与关键入口文件，再给你完整结论。"
	for _, frag := range []int{1, 3, 11, 10000} {
		g := &streamGuard{}
		out, stopped := feedAll(g, answer+selfPlayTail, frag)
		if !stopped {
			t.Fatalf("分片 %d：出现控制标记必须判截断", frag)
		}
		if !strings.Contains(out, answer) {
			t.Fatalf("分片 %d：正文被吃掉：%q", frag, out)
		}
		for _, bad := range []string{"<｜", "｜>", "[工具执行结果]", "std::", "TESTMARKER"} {
			if strings.Contains(out, bad) {
				t.Fatalf("分片 %d：脏内容漏到下游 %q：%q", frag, bad, out)
			}
		}
		if g.junkOnly() {
			t.Fatalf("分片 %d：有正文，不该判为整轮自演", frag)
		}
	}
}

// 正文里的 "<" 是数据、不是标记：不能被吃掉，也不能因为扣住尾部而丢字。
func TestGuardKeepsPlainAngleBrackets(t *testing.T) {
	for _, in := range []string{
		"a < b 且 c > d",
		"用 <div> 包一层，再看 <span> 的样式",
		"输出形如 <name>，尖括号保留",
		"1 < 2",
		"<",
		"<|",
	} {
		g := &streamGuard{}
		out, stopped := feedAll(g, in, 1)
		if stopped || out != in {
			t.Fatalf("普通尖括号文本必须原样通过：in=%q out=%q stopped=%v", in, out, stopped)
		}
		if g.junkOnly() {
			t.Fatalf("没出现过标记，不该判自演：%q", in)
		}
	}
}

// 工具调用标记（shim 的约定）在正文里，不能被误伤：它不带全角竖线。
func TestGuardKeepsToolCallBlock(t *testing.T) {
	in := `<tool_call>{"name":"terminal","arguments":{"command":"ls"}}</tool_call>` + selfPlayTail
	g := &streamGuard{}
	out, stopped := feedAll(g, in, 5)
	if !stopped {
		t.Fatal("尾部的自演必须被截断")
	}
	if !strings.Contains(out, `<tool_call>{"name":"terminal"`) || !strings.Contains(out, "</tool_call>") {
		t.Fatalf("工具调用块被误伤：%q", out)
	}
	for _, bad := range []string{"<｜", "[工具执行结果]"} {
		if strings.Contains(out, bad) {
			t.Fatalf("脏内容漏到下游 %q：%q", bad, out)
		}
	}
}

// 入站历史：客户端回传的 assistant 脏正文要在拼提示词时掐掉（断自激回路）。
func TestPackMessagesCutsPoisonedAssistantHistory(t *testing.T) {
	got := packMessages([]channel.Message{
		{Role: "user", Content: "去读一下这个项目"},
		{Role: "assistant", Content: "我先看看。" + selfPlayJunkOnly},
		{Role: "tool", Content: `{"output":"总 24 行"}`},
	})
	if strings.Contains(got, "[工具执行结果] terminal") {
		t.Fatalf("回灌的脏正文没被掐掉：%s", got)
	}
	if !strings.Contains(got, "<｜Assistant｜>我先看看。") {
		t.Fatalf("正文部分必须保留：%s", got)
	}
	// 标记数应等于消息数（每条消息各一个收尾标记），没有多出来的标记。
	if n := strings.Count(got, "<｜end▁of▁sentence｜>"); n != 3 {
		t.Fatalf("标记数应为 3（三条消息各一个），实际 %d：%s", n, got)
	}
}

// 用户消息与工具结果里出现这个字面量是**数据**，一个字都不能动（红线一）。
func TestPackMessagesKeepsUserAndToolVerbatim(t *testing.T) {
	raw := "日志原文：<｜end▁of▁sentence｜> 后接 <｜User｜> 就是模板标记"
	got := packMessages([]channel.Message{
		{Role: "user", Content: raw},
		{Role: "tool", Content: raw},
	})
	if strings.Count(got, raw) != 2 {
		t.Fatalf("用户/工具消息被改动了（丢内容）：%s", got)
	}
}

// 端到端：桩上游回「正文 + 自演」的 patch 流，客户端只能拿到正文，且本轮正常收尾。
func TestChatStreamCutsSelfPlayFromUpstream(t *testing.T) {
	const answer = "我先看项目结构。"
	srv := patchServer(t, chunkRunes(answer+selfPlayJunkOnly, 3))
	defer srv.Close()

	var content strings.Builder
	finished := false
	st := chatAgainst(t, srv)
	defer st.Close()
	for {
		c, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("本轮有正文，不该报错：%v", err)
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
			if ch.FinishReason != "" {
				finished = true
			}
		}
	}
	if content.String() != answer {
		t.Fatalf("客户端正文应为干净的一句，实际 %q", content.String())
	}
	if !finished {
		t.Fatal("截断后必须正常收尾（finish_reason=stop），否则客户端以为被掐断")
	}
}

// 端到端：整轮都是自演 —— 必须报结构化错误（红线二），归到上游头上（不冷却账号），
// 并把被丢弃的内容作为上游原话带出去（红线一）。
func TestChatStreamJunkOnlyIsUpstreamFault(t *testing.T) {
	srv := patchServer(t, chunkRunes(selfPlayJunkOnly, 7))
	defer srv.Close()

	st := chatAgainst(t, srv)
	defer st.Close()
	var gotErr error
	for {
		_, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("整轮自演必须报错，不能回一个空成功（红线二）")
	}
	k, ok := errs.KindOf(gotErr)
	if !ok || k != errs.UpstreamFault {
		t.Fatalf("应归一成 UpstreamFault，实际 %v", gotErr)
	}
	if k.AccountBlamed() {
		t.Fatal("UpstreamFault 不该算到账号头上：模型续写剧本不是账号的问题")
	}
	// 红线一：客户端要能看到「上游到底发了什么形态」——这里带出被丢弃内容的起始片段。
	// 注意：截断一旦判定就立刻停止读流（不多消耗上游额度），所以留样可能只有那个标记本身。
	var ee *errs.Error
	if !errors.As(gotErr, &ee) || !strings.Contains(ee.Upstream, guardOpen) {
		t.Fatalf("必须把被丢弃的内容作为上游原话带出来（红线一）：%+v", gotErr)
	}
	if !strings.Contains(ee.Upstream, "end▁of▁sentence") {
		t.Fatalf("上游原话应能看出是模板标记：%+v", gotErr)
	}
}

// chatAgainst 用桩上游起一条流。
func chatAgainst(t *testing.T, srv *httptest.Server) channel.Stream {
	t.Helper()
	a := New()
	a.base = srv.URL
	a.SetSolver(&stubSolver{})
	st, err := a.Chat(context.Background(), &channel.Credential{UID: "u1", AccessToken: "tok",
		Extra: map[string]string{"device_id": "d"}}, channel.ChatRequest{Model: "default"})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	return st
}

// patchServer 起桩上游：把 pieces 依次作为 RESPONSE 增量（每片单独一帧，帧本身都是合法 JSON）回。
func patchServer(t *testing.T, pieces []string) *httptest.Server {
	t.Helper()
	frames := make([]string, 0, len(pieces)+2)
	for _, p := range pieces {
		b, _ := json.Marshal(p)
		frames = append(frames, `{"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":`+string(b)+`}]}`)
	}
	frames = append(frames, `{"p":"response/status","v":"FINISHED"}`)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat_session/create"):
			fmt.Fprint(w, `{"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/create_pow_challenge"):
			fmt.Fprint(w, `{"data":{"biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"c1",`+
				`"salt":"s1","signature":"sig1","difficulty":1000,"expire_at":1,"target_path":"/api/v0/chat/completion"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completion"):
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			for _, f := range frames {
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", f)
				if fl != nil {
					fl.Flush()
				}
			}
		default:
			fmt.Fprint(w, `{"code":0}`)
		}
	}))
}
