package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/toolshim"
)

// TestChatWithToolsThroughShim 是本渠道「工具调用与普通模型一致」的决定性验证：
// 客户端带 tools 发请求 → 上游（模拟真实 DeepSeek）回**原生 DSML 语法的文本** →
// 网关的 toolshim 把它解析成结构化 tool_calls → 客户端拿到的就是标准 tool_calls。
//
// 这条链路上每一环都用真实代码（adapter.Chat + toolshim.BuildRequest/Wrap），
// 只有上游是本机 mock —— 不需要真账号、不依赖外部网络，可重复跑。
func TestChatWithToolsThroughShim(t *testing.T) {
	bar := "\uff5c\uff5c" // 全角竖线：DSML 的真实形态
	var sawToolsInBody bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat_session/create"):
			fmt.Fprint(w, `{"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/create_pow_challenge"):
			fmt.Fprint(w, `{"data":{"biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"c1",`+
				`"salt":"s1","signature":"sig1","difficulty":144000,"expire_at":1775380966945,`+
				`"expire_after":300000,"target_path":"/api/v0/chat/completion"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completion"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			// 断言：上游收到的 prompt 里**没有** tools 字段（已被 shim 清掉），
			// 但工具说明确实写进了文本（这就是「网页模型也能用工具」的实现方式）。
			if _, ok := body["tools"]; ok {
				t.Errorf("发给上游的 body 不该含 tools 字段")
			}
			prompt, _ := body["prompt"].(string)
			if !strings.Contains(prompt, "Read") || !strings.Contains(prompt, "工具调用协议") {
				t.Errorf("工具说明应写进 prompt，实际：%.200s", prompt)
			}
			sawToolsInBody = true

			// 上游回「原生 DSML 语法」的文本 —— 模拟请求一大时模型的真实行为。
			dsml := bar + "DSML" + bar + " calls>\n" +
				bar + "DSML" + bar + ` invoke name="Read">` + "\n" +
				bar + "DSML" + bar + ` parameter name="file_path" string="true">/etc/hosts</` + bar + "DSML" + bar + " parameter>\n" +
				"</" + bar + "DSML" + bar + " invoke>\n</" + bar + "DSML" + bar + " calls>"

			w.Header().Set("Content-Type", "text/event-stream")
			frames := []string{
				`{"v":{"response":{"status":"WIP","fragments":[{"type":"RESPONSE","content":""}]}}}`,
				`{"p":"response/fragments/-1/content","o":"APPEND","v":` + jsonString(dsml) + `}`,
				`{"p":"response/status","v":"FINISHED"}`,
			}
			for _, f := range frames {
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", f)
			}
		case strings.HasSuffix(r.URL.Path, "/chat_session/delete"):
			fmt.Fprint(w, `{"code":0}`)
		default:
			t.Errorf("意外路径：%s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	a.SetSolver(&stubSolver{})
	cred := &channel.Credential{UID: "u1", AccessToken: "tok", Extra: map[string]string{"device_id": "dev-1"}}

	// 走网关同款路径：先 BuildRequest 改写，再 Chat，最后 Wrap 解析响应。
	req := channel.ChatRequest{
		Model:    "deepseek-chat",
		Messages: []channel.Message{{Role: "user", Content: "读一下 /etc/hosts"}},
		Tools: []map[string]any{
			{"type": "function", "function": map[string]any{
				"name": "Read", "description": "读取文件内容",
				"parameters": map[string]any{"type": "object",
					"properties": map[string]any{"file_path": map[string]any{"type": "string"}}},
			}},
		},
	}
	st, err := a.Chat(context.Background(), cred, toolshim.BuildRequest(req))
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	st = toolshim.Wrap(st)

	var content strings.Builder
	var calls []channel.ToolCall
	finish := ""
	for {
		c, err := st.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("读流失败：%v", err)
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
			calls = append(calls, ch.Delta.ToolCalls...)
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}

	if !sawToolsInBody {
		t.Fatal("mock 上游没有收到 completion 请求")
	}
	// 关键断言：DSML 原生语法被解析成了结构化 tool_calls，而不是留在正文里。
	if len(calls) != 1 {
		t.Fatalf("应解析出 1 个 tool_call，得到 %d（正文=%q）", len(calls), content.String())
	}
	if calls[0].Function.Name != "Read" {
		t.Fatalf("工具名应为 Read，得到 %q", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "/etc/hosts") {
		t.Fatalf("参数应含 /etc/hosts，得到 %q", calls[0].Function.Arguments)
	}
	if strings.Contains(content.String(), "DSML") {
		t.Fatalf("正文不该残留 DSML 标记：%q", content.String())
	}
	if finish != "tool_calls" {
		t.Fatalf("有工具调用时 finish_reason 应为 tool_calls，得到 %q", finish)
	}
}

// jsonString 把字符串编码成 JSON 字面量（放进 SSE 帧里）。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
