package qwen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// fakeJWT 造一个 payload 里带 sub 的 JWT（不验签，只为 identityOf 能读出身份）。
func fakeJWT(sub string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sub":%q,"email":%q}`, sub, sub+"@example.test")))
	return "h." + payload + ".s"
}

func testAdapter(srv *httptest.Server) *Adapter {
	a := New()
	a.chatBase = srv.URL
	a.cliBase = srv.URL
	return a
}

// 设备码授权：先 pending，再拿到 token；身份从 JWT 里读出来。
func TestDeviceLoginFlow(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epDeviceCode:
			if r.Header.Get("User-Agent") == "" || strings.HasPrefix(r.Header.Get("User-Agent"), "Go-http") {
				t.Errorf("授权请求必须带客户端标识（无 UA 会被 WAF 拦）：%q", r.Header.Get("User-Agent"))
			}
			raw, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(raw))
			if form.Get("client_id") != clientID || form.Get("code_challenge_method") != "S256" ||
				form.Get("code_challenge") == "" {
				t.Errorf("设备码请求参数不对：%v", form)
			}
			if !strings.Contains(form.Get("scope"), "model.completion") {
				t.Errorf("scope 少了 model.completion：%q", form.Get("scope"))
			}
			fmt.Fprintf(w, `{"device_code":"dc1","user_code":"UC-1",
				"verification_uri":"https://chat.qwen.ai/device","verification_uri_complete":"https://chat.qwen.ai/device?user_code=UC-1",
				"expires_in":300,"interval":2}`)
		case epToken:
			raw, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(raw))
			if form.Get("grant_type") == deviceGrant {
				if form.Get("device_code") != "dc1" || form.Get("code_verifier") == "" {
					t.Errorf("换令牌参数不对：%v", form)
				}
				polls++
				if polls < 2 { // 第一次还没授权：按 RFC 8628 返回 pending
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":"authorization_pending"}`)
					return
				}
				fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rt1","expires_in":7200}`, fakeJWT("user-42"))
				return
			}
			t.Errorf("意外的 grant_type: %q", form.Get("grant_type"))
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	a := testAdapter(srv)
	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	if !strings.Contains(sess.AuthURL(), "user_code=UC-1") {
		t.Fatalf("授权地址应带上 user_code：%q", sess.AuthURL())
	}
	if _, err := sess.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("未授权时应返回 ErrPending，got %v", err)
	}
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("第二次 Poll 应成功：%v", err)
	}
	if cred.UID != "user-42" || cred.AccessToken == "" || cred.RefreshToken != "rt1" {
		t.Fatalf("凭证不对：%+v", cred)
	}
	if cred.ExpiresAt.IsZero() {
		t.Fatal("过期时间应当被记下（续期靠它）")
	}
}

// token 不是 JWT（上游换成不透明 token）时也要给出稳定 UID —— 否则控制台按「uid 为空」拒绝落盘。
func TestDeviceLoginWithoutJWTStillYieldsUID(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == epDeviceCode {
			fmt.Fprint(w, `{"device_code":"dc","user_code":"UC","verification_uri":"https://x/authorize","expires_in":900}`)
			return
		}
		if r.URL.Path == epToken {
			polls++
			if polls < 2 {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"authorization_pending"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"opaque-token","refresh_token":"rt","expires_in":7200}`)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	a := testAdapter(srv)
	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sess.Poll(context.Background()) // pending
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if cred.UID == "" {
		t.Fatal("不透明 token 也必须给出 UID（否则控制台拒绝落盘）")
	}
	if !strings.HasPrefix(cred.UID, "qwen-") {
		t.Fatalf("兜底 UID 形状不对：%q", cred.UID)
	}
}

// 续期：refresh_token 轮换后要存新的，否则重启后拿到的是作废的旧值。
func TestRefreshRotatesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt-old" {
			t.Errorf("续期参数不对：%v", form)
		}
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rt-new","expires_in":7200}`, fakeJWT("user-42"))
	}))
	defer srv.Close()

	a := testAdapter(srv)
	nc, err := a.Refresh(context.Background(), &channel.Credential{UID: "user-42", RefreshToken: "rt-old"})
	if err != nil {
		t.Fatalf("续期失败：%v", err)
	}
	if nc == nil || nc.RefreshToken != "rt-new" {
		t.Fatalf("轮换后的 refresh_token 必须带上：%+v", nc)
	}
	// 没有 refresh_token：明确表示刷不了（交给上层按 401 处理，不要假装成功）
	nc2, err2 := a.Refresh(context.Background(), &channel.Credential{UID: "u"})
	if err2 != nil || nc2 != nil {
		t.Fatalf("无 refresh_token 时应返回 (nil, nil)：%v %v", nc2, err2)
	}
}

// 对话：请求头/请求体按 CLI 端要求构造，响应按标准 OpenAI SSE 解析（含原生工具调用）。
func TestChatSendsCLIShapeAndParsesTools(t *testing.T) {
	var gotHeader http.Header
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range []string{
			`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"想想"}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"content":"好的"}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}`,
			`{"id":"c1","choices":[{"index":0,"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":9}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := testAdapter(srv)
	st, err := a.Chat(context.Background(), &channel.Credential{UID: "u", AccessToken: "at-1"}, channel.ChatRequest{
		Model: "qwen3.5-plus", // 应被重定向到 coder-model
		Messages: []channel.Message{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "北京天气"},
		},
		MaxTokens: 256,
		Tools: []map[string]any{{
			"type": "function",
			"function": map[string]any{"name": "get_weather", "description": "查天气",
				"parameters": map[string]any{"type": "object"}},
		}},
		ToolChoice: "auto",
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	if gotHeader.Get("Authorization") != "Bearer at-1" {
		t.Fatalf("鉴权头不对：%q", gotHeader.Get("Authorization"))
	}
	if !strings.HasPrefix(gotHeader.Get("User-Agent"), "QwenCode/") {
		t.Fatalf("缺少 CLI 客户端标识：%q", gotHeader.Get("User-Agent"))
	}
	if gotHeader.Get("X-Dashscope-Authtype") != "qwen-oauth" {
		t.Fatalf("缺少 X-Dashscope-Authtype：%q", gotHeader.Get("X-Dashscope-Authtype"))
	}
	if gotBody["model"] != "coder-model" {
		t.Fatalf("模型别名应重定向到 coder-model，got %v", gotBody["model"])
	}
	if gotBody["tools"] == nil || gotBody["tool_choice"] != "auto" {
		t.Fatalf("工具定义应原样透传：%v %v", gotBody["tools"], gotBody["tool_choice"])
	}
	// 流式必须带 stream_options.include_usage，否则上游不回 usage
	// （来源：qwen-code-oai-proxy src/qwen/api.ts 流式 payload）。
	so, _ := gotBody["stream_options"].(map[string]any)
	if so == nil || so["include_usage"] != true {
		t.Fatalf("流式请求应带 stream_options.include_usage=true：%v", gotBody["stream_options"])
	}
	// system 的 content 必须是「带 cache_control: ephemeral 的 text part 数组」，
	// 且只在客户端给了 system 时才发（不凭空造一条）——来源：qwen-code-oai-proxy
	// src/qwen/api.ts transformMessagesForPortal。
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("消息数组应原样对应（不插空 system）：%v", gotBody["messages"])
	}
	head, _ := msgs[0].(map[string]any)
	parts, _ := head["content"].([]any)
	if head["role"] != "system" || len(parts) != 1 {
		t.Fatalf("首条应为 system 且 content 是单元素数组：%v", head)
	}
	p0, _ := parts[0].(map[string]any)
	cc, _ := p0["cache_control"].(map[string]any)
	if p0["type"] != "text" || p0["text"] != "你是助手" || cc["type"] != "ephemeral" {
		t.Fatalf("system 的缓存锚点不对：%v", p0)
	}
	if u, _ := msgs[1].(map[string]any); u["role"] != "user" || u["content"] != "北京天气" {
		t.Fatalf("user 消息应保持扁平字符串：%v", u)
	}

	var content strings.Builder
	var reasoning strings.Builder
	var calls []channel.ToolCall
	finish := ""
	usageSeen := false
	for {
		c, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("流读取失败：%v", err)
		}
		if c.Usage != nil {
			usageSeen = true
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
			reasoning.WriteString(ch.Delta.ReasoningContent)
			calls = mergeCalls(calls, ch.Delta.ToolCalls)
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	if content.String() != "好的" || reasoning.String() != "想想" {
		t.Fatalf("正文/推理解析不对：%q / %q", content.String(), reasoning.String())
	}
	if len(calls) != 1 || calls[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("工具调用分片没拼回来：%+v", calls)
	}
	if finish != "tool_calls" || !usageSeen {
		t.Fatalf("finish_reason/usage 不对：%q %v", finish, usageSeen)
	}
}

// 流内错误帧必须归一成 *errs.Error（带 Kind + 上游原话），不能被当成空帧丢掉。
// 教训：TraeWork 就是因为流内错误没归一，上游原话被网关换成通用文案（红线一）。
func TestChatSurfacesInStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"好"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"error":{"message":"free allocated quota exceeded","type":"insufficient_quota"}}`+"\n\n")
	}))
	defer srv.Close()

	a := testAdapter(srv)
	st, err := a.Chat(context.Background(), &channel.Credential{UID: "u", AccessToken: "at"},
		channel.ChatRequest{Model: "coder-model", Messages: []channel.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	var got error
	for {
		_, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			got = err
			break
		}
	}
	if got == nil {
		t.Fatal("流内错误必须被归一上报，不能静默结束成空流")
	}
	var ee *errs.Error
	if !errors.As(got, &ee) {
		t.Fatalf("必须是 *errs.Error（普通 error 会被网关换掉原话），got %T: %v", got, got)
	}
	if ee.Kind != errs.UpstreamFault || !strings.Contains(ee.Upstream, "quota exceeded") {
		t.Fatalf("错误应带 Kind 与上游原话：%+v", ee)
	}
}

// 客户端没给 system 就不凭空造一条；max_tokens 超过档位上限要本地夹住
// （来源：qwen-code-oai-proxy src/qwen/api.ts transformMessagesForPortal / MODEL_LIMITS）。
func TestChatKeepsMessagesAndClampsMaxTokens(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := testAdapter(srv)
	st, err := a.Chat(context.Background(), &channel.Credential{UID: "u", AccessToken: "at"}, channel.ChatRequest{
		Model: "coder-model", Messages: []channel.Message{{Role: "user", Content: "hi"}}, MaxTokens: 999999,
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("没有 system 时不该插入 system：%v", gotBody["messages"])
	}
	if m, _ := msgs[0].(map[string]any); m["content"] != "hi" {
		t.Fatalf("user 消息应保持扁平字符串：%v", m)
	}
	if gotBody["max_tokens"] != float64(65536) {
		t.Fatalf("超过档位上限的 max_tokens 应被夹到 65536，got %v", gotBody["max_tokens"])
	}
}

// 认证失败要归一成 SessionDead（= 需要重新授权），而不是笼统的 Parse。
func TestClassify(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{"error":"invalid token"}`, errs.SessionDead},
		{403, `forbidden`, errs.SessionDead},
		{402, `payment required`, errs.HardCredit},
		{429, `rate limited`, errs.SoftRate},
		{429, `{"message":"quota exceeded"}`, errs.HardCredit},
		{400, `{"message":"maximum context length exceeded"}`, errs.PromptTooLong},
		{400, `{"message":"unexpected field","details":"model x"}`, errs.ModelUnavailable},
		{404, `not found`, errs.ModelUnavailable},
		{500, `boom`, errs.UpstreamFault},
		{418, `teapot`, errs.Parse},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Errorf("Classify(%d, %q) = %v，want %v", c.status, c.body, got, c.want)
		}
	}
}

// 能力声明与模型表：工具调用是原生（不是模拟），模型来自本地清单（如实标注）。
func TestSpecAndModels(t *testing.T) {
	a := New()
	sp := a.Spec()
	if !sp.Tools || sp.ToolsShim {
		t.Fatal("通义 CLI 端原生支持工具调用，不该走模拟档")
	}
	if !sp.SSEOnly || sp.DefaultMinIntervalSec <= 0 {
		t.Fatalf("SSEOnly/最小间隔不对：%+v", sp)
	}
	ms, err := a.Models(context.Background(), &channel.Credential{UID: "u"})
	if err != nil || len(ms) == 0 {
		t.Fatalf("模型表不对：%v %+v", err, ms)
	}
	for _, m := range ms {
		if m.Source != channel.SourceLocal {
			t.Fatalf("模型来源应标注为本地清单：%+v", m)
		}
		if m.Tools != channel.CapYes {
			t.Fatalf("CLI 端模型应声明支持工具：%+v", m)
		}
	}
	// 非交互登录必须明确报错，指向面板授权
	if _, err := a.Login(context.Background()); err == nil || !strings.Contains(err.Error(), "授权") {
		t.Fatalf("Login 应提示走面板授权：%v", err)
	}
}

// mergeCalls 是测试用的最小归并（与网关 mergeToolCalls 同语义）。
func mergeCalls(acc []channel.ToolCall, delta []channel.ToolCall) []channel.ToolCall {
	for _, d := range delta {
		idx := -1
		for i := range acc {
			if d.ID != "" && acc[i].ID == d.ID {
				idx = i
				break
			}
			if d.ID == "" && acc[i].Index == d.Index {
				idx = i
				break
			}
		}
		if idx < 0 {
			acc = append(acc, d)
			continue
		}
		if d.ID != "" {
			acc[idx].ID = d.ID
		}
		if d.Function.Name != "" {
			acc[idx].Function.Name = d.Function.Name
		}
		acc[idx].Function.Arguments += d.Function.Arguments
	}
	return acc
}
