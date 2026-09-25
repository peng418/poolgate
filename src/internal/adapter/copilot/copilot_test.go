package copilot

// copilot_test.go 用 httptest 把整条链路走一遍，重点覆盖四件最容易出错的事：
//  1. 设备码流程真的能走完（申请 → 轮询 pending → 拿到 githubToken → 验一次 → 出凭证）；
//  2. 令牌链路的鉴权头对不对（换 copilotToken 用 `token <githubToken>`，对话用 `Bearer <copilotToken>`）；
//  3. **伪装头**有没有带上（缺一个上游就当成第三方调用拒绝，而且报错信息不会告诉你缺了哪个头）；
//  4. 增量文本解析与错误归一（401/403/429 这几档最容易分错，分错会让用户做错事）。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// newTestAdapter 把三个端点基址都指到测试服务器上，并返回适配器。
func newTestAdapter(url string) *Adapter {
	a := New()
	a.githubBase = url
	a.githubAPIBase = url
	a.copilotBase = url
	return a
}

// writeSSE 按 SSE 形态写一串 data 帧。故意拆成小块 flush，模拟真实网络的分片。
func writeSSE(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, f := range frames {
		for i := 0; i < len(f); i += 11 {
			end := i + 11
			if end > len(f) {
				end = len(f)
			}
			_, _ = w.Write([]byte(f[i:end]))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// readAll 把一条流读完，返回正文、思考、结束原因。
func readAll(t *testing.T, st channel.Stream) (content, reasoning, finish string) {
	t.Helper()
	defer st.Close()
	var c, r strings.Builder
	for {
		chunk, err := st.Next()
		if err != nil {
			break
		}
		for _, ch := range chunk.Choices {
			c.WriteString(ch.Delta.Content)
			r.WriteString(ch.Delta.ReasoningContent)
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	return c.String(), r.String(), finish
}

// ---------------------------------------------------------------------------
// 1. 设备码登录端到端
// ---------------------------------------------------------------------------

func TestDeviceLoginEndToEnd(t *testing.T) {
	var deviceCalls, tokenCalls, copilotTokenCalls int32
	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epDeviceCode:
			atomic.AddInt32(&deviceCalls, 1)
			if r.Method != http.MethodPost {
				t.Errorf("设备码应 POST，得到 %s", r.Method)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("设备码请求应为 JSON，Content-Type=%q", ct)
			}
			if acc := r.Header.Get("Accept"); acc != "application/json" {
				t.Errorf("Accept 必须是 application/json（否则令牌接口回 form-encoded），得到 %q", acc)
			}
			var body struct {
				ClientID string `json:"client_id"`
				Scope    string `json:"scope"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if body.ClientID != clientID {
				t.Errorf("client_id 不对：%q", body.ClientID)
			}
			if body.Scope != scope {
				t.Errorf("scope 不对：%q（应只申请 read:user）", body.Scope)
			}
			_, _ = w.Write([]byte(`{"device_code":"dev-1","user_code":"ABCD-1234",` +
				`"verification_uri":"` + srv.URL + `/login/device","expires_in":900,"interval":5}`))

		case epAccessToken:
			n := atomic.AddInt32(&tokenCalls, 1)
			var body struct {
				ClientID   string `json:"client_id"`
				DeviceCode string `json:"device_code"`
				GrantType  string `json:"grant_type"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if body.DeviceCode != "dev-1" {
				t.Errorf("轮询应带上设备码，得到 %q", body.DeviceCode)
			}
			if body.GrantType != deviceGrant {
				t.Errorf("grant_type 应为 %q，得到 %q", deviceGrant, body.GrantType)
			}
			if n == 1 {
				// 第一次：用户还没在浏览器点完。注意这是 HTTP 200 + error 字段。
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"gho_test_token","token_type":"bearer","scope":"read:user"}`))

		case epCopilotToken:
			atomic.AddInt32(&copilotTokenCalls, 1)
			if got := r.Header.Get("Authorization"); got != "token gho_test_token" {
				t.Errorf("换 copilotToken 必须用 `token <githubToken>`，得到 %q", got)
			}
			if got := r.Header.Get("Editor-Version"); got != "vscode/"+vsCodeVersion {
				t.Errorf("换 copilotToken 也要带编辑器版本头，得到 %q", got)
			}
			_, _ = w.Write([]byte(`{"token":"copilot-tok","expires_at":1799999999,"refresh_in":1500}`))

		case epUser:
			if got := r.Header.Get("Authorization"); got != "token gho_test_token" {
				t.Errorf("读 /user 应带 githubToken，得到 %q", got)
			}
			_, _ = w.Write([]byte(`{"login":"octocat"}`))

		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	if got, want := sess.AuthURL(), srv.URL+"/login/device"; got != want {
		t.Fatalf("AuthURL 不对：%q want %q", got, want)
	}
	// 面板能展示 user_code 的唯一通道就是 Hint —— 少了它用户到了授权页不知道输什么。
	hint := sess.(interface{ Hint() string }).Hint()
	if !strings.Contains(hint, "ABCD-1234") || !strings.Contains(hint, sess.AuthURL()) {
		t.Fatalf("Hint 必须同时含设备码与授权地址：%q", hint)
	}

	// 第一次轮询：还没授权完 → ErrPending（不是错误）。
	if _, err := sess.Poll(context.Background()); !errors.Is(err, channel.ErrPending) {
		t.Fatalf("未完成时应返回 ErrPending，得到 %v", err)
	}
	// 第二次：拿到 githubToken，并当场换成 copilotToken 验一把。
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("第二次轮询应成功：%v", err)
	}
	if cred.UID != "gh-octocat" {
		t.Errorf("UID 应为 gh-octocat（登录名），得到 %q", cred.UID)
	}
	if cred.AccessToken != "gho_test_token" {
		t.Errorf("凭证里应存 githubToken，得到 %q", cred.AccessToken)
	}
	if cred.Extra["github_login"] != "octocat" {
		t.Errorf("Extra 应记下登录名，得到 %v", cred.Extra)
	}
	if atomic.LoadInt32(&deviceCalls) != 1 || atomic.LoadInt32(&tokenCalls) != 2 {
		t.Errorf("调用次数不对：device=%d token=%d（应为 1 / 2）",
			deviceCalls, tokenCalls)
	}
	if atomic.LoadInt32(&copilotTokenCalls) != 1 {
		t.Errorf("登录时应换一次 copilotToken 做校验，实际 %d 次", copilotTokenCalls)
	}
	// 完成后再问一次：必须明确报错，不能返回同一份凭证（否则控制台会重复入池）。
	if _, err := sess.Poll(context.Background()); err == nil {
		t.Fatal("已完成的会话再次 Poll 应报错")
	}
}

// 设备码被拒绝（用户点拒绝）时应归成 AuthFailed，而不是让面板一直转到超时。
func TestDeviceLoginAccessDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epDeviceCode:
			_, _ = w.Write([]byte(`{"device_code":"d","user_code":"U-1","verification_uri":"https://x/y","expires_in":900}`))
		case epAccessToken:
			_, _ = w.Write([]byte(`{"error":"access_denied","error_description":"user denied"}`))
		}
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	_, err = sess.Poll(context.Background())
	k, ok := errs.KindOf(err)
	if !ok || k != errs.AuthFailed {
		t.Fatalf("拒绝授权应归 AuthFailed，得到 %v（%v）", k, err)
	}
}

// 兜底路径：用户直接粘 GitHub token；而把设备码本身粘进来必须被挡住。
func TestLoginPasteFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epDeviceCode:
			_, _ = w.Write([]byte(`{"device_code":"d","user_code":"ABCD-1234","verification_uri":"https://x/y"}`))
		case epCopilotToken:
			if got := r.Header.Get("Authorization"); got != "token ghp_pasted_token_value" {
				t.Errorf("粘回来的 token 应被当作 githubToken，得到 %q", got)
			}
			_, _ = w.Write([]byte(`{"token":"copilot-tok","refresh_in":1500}`))
		case epUser:
			_, _ = w.Write([]byte(`{"login":"pasted-user"}`))
		}
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	acc := sess.(channel.CallbackAcceptor)

	// 设备码（短且带连字符）不是 token，必须明确拒绝 —— 否则用户会把码粘进来然后一头雾水。
	if err := acc.AcceptCallback("ABCD-1234"); err == nil {
		t.Fatal("把设备码当 token 粘进来应报错")
	}
	if err := acc.AcceptCallback(`{"access_token":"ghp_pasted_token_value"}`); err != nil {
		t.Fatalf("合法 token 应被接受：%v", err)
	}
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("粘贴后 Poll 应成功：%v", err)
	}
	if cred.UID != "gh-pasted-user" || cred.AccessToken != "ghp_pasted_token_value" {
		t.Fatalf("凭证不对：%+v", cred)
	}
}

// 换了 200 却没有 token：绝不能当成功（否则后面每个请求都 401，看起来像别的问题）。
func TestEmptyCopilotTokenIsSessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"refresh_in":1500}`))
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	_, err := a.exchangeCopilotToken(context.Background(), nil, "gho_x")
	k, ok := errs.KindOf(err)
	if !ok || k != errs.SessionDead {
		t.Fatalf("空 token 应归 SessionDead，得到 %v（%v）", k, err)
	}
}

// ---------------------------------------------------------------------------
// 2. 对话：伪装头、X-Initiator、增量文本
// ---------------------------------------------------------------------------

func TestChatHeadersAndIncrementalText(t *testing.T) {
	var copilotTokenCalls, chatCalls int32
	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epCopilotToken:
			atomic.AddInt32(&copilotTokenCalls, 1)
			_, _ = w.Write([]byte(`{"token":"copilot-tok","refresh_in":1500}`))

		case "/chat/completions":
			n := atomic.AddInt32(&chatCalls, 1)
			if got := r.Header.Get("Authorization"); got != "Bearer copilot-tok" {
				t.Errorf("对话必须用 Bearer <copilotToken>，得到 %q", got)
			}
			// 伪装头逐个点名：缺任何一个上游都可能判成第三方调用，而报错不会告诉你是哪个。
			want := map[string]string{
				"Copilot-Version":                     copilotVersion,
				"Copilot-Integration-Id":              "vscode-chat",
				"Editor-Version":                      "vscode/" + vsCodeVersion,
				"Editor-Plugin-Version":               "copilot-chat/" + copilotVersion,
				"User-Agent":                          "GitHubCopilotChat/" + copilotVersion,
				"Openai-Intent":                       "conversation-panel",
				"X-GitHub-Api-Version":                githubAPIVersion,
				"X-Vscode-User-Agent-Library-Version": "electron-fetch",
			}
			for k, v := range want {
				if got := r.Header.Get(k); got != v {
					t.Errorf("伪装头 %s 不对：%q want %q", k, got, v)
				}
			}
			if got := r.Header.Get("X-Request-Id"); len(got) != 36 {
				t.Errorf("x-request-id 应为 UUID，得到 %q", got)
			}
			// 第一轮只有 user → "user"；第二轮含 assistant → "agent"。
			wantInitiator := map[int32]string{1: "user", 2: "agent"}[n]
			if got := r.Header.Get("X-Initiator"); got != wantInitiator {
				t.Errorf("第 %d 次 X-Initiator 应为 %s，得到 %q", n, wantInitiator, got)
			}
			var body struct {
				Model    string           `json:"model"`
				Stream   bool             `json:"stream"`
				Tools    []map[string]any `json:"tools"`
				Messages []map[string]any `json:"messages"`
			}
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("请求体不是 JSON：%v", err)
			}
			if body.Model != "gpt-4.1" || !body.Stream {
				t.Errorf("请求体不对：model=%q stream=%v", body.Model, body.Stream)
			}
			if get := atomic.LoadInt32(&chatCalls); get == 1 && len(body.Tools) != 1 {
				t.Errorf("原生工具调用应原样转发，得到 %v", body.Tools)
			}
			writeSSE(w,
				`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`+"\n\n",
				`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"你"}}]}`+"\n\n",
				`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"好"}}]}`+"\n\n",
				`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n",
				"data: [DONE]\n\n",
			)

		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	cred := &channel.Credential{UID: "gh-octocat", AccessToken: "gho_test_token"}
	tools := []map[string]any{{
		"type":     "function",
		"function": map[string]any{"name": "get_weather", "parameters": map[string]any{}},
	}}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model:    "gpt-4.1",
		Messages: []channel.Message{{Role: "user", Content: "你好"}},
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	content, reasoning, finish := readAll(t, st)
	if content != "你好" {
		t.Fatalf("增量文本拼接不对：%q（期望 你好）", content)
	}
	if reasoning != "" {
		t.Fatalf("上游没有思考字段，不应产生 reasoning：%q", reasoning)
	}
	if finish != "stop" {
		t.Fatalf("结束原因不对：%q", finish)
	}

	// 第二轮：带 assistant 历史 → X-Initiator 必须是 agent；且 copilotToken 应走缓存（不再换一次）。
	st2, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "gpt-4.1",
		Messages: []channel.Message{
			{Role: "user", Content: "天气"},
			{Role: "assistant", Content: "", ToolCalls: []channel.ToolCall{{ID: "call_1", Type: "function",
				Function: channel.FunctionCall{Name: "get_weather", Arguments: "{}"}}}},
			{Role: "tool", ToolCallID: "call_1", Content: "晴"},
		},
	})
	if err != nil {
		t.Fatalf("第二轮 Chat 失败：%v", err)
	}
	readAll(t, st2)

	if got := atomic.LoadInt32(&chatCalls); got != 2 {
		t.Fatalf("应打两次对话接口，实际 %d", got)
	}
	if got := atomic.LoadInt32(&copilotTokenCalls); got != 1 {
		t.Fatalf("copilotToken 必须缓存复用（只换一次），实际 %d 次", got)
	}
}

// copilotToken 过期（401）时应丢缓存再换一次并重试，且只重试一次。
func TestChatRefreshesOnceOn401(t *testing.T) {
	var exchanges, chats int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epCopilotToken:
			n := atomic.AddInt32(&exchanges, 1)
			// 每次换出来的 token 不同，便于确认重试用的是新 token。
			_, _ = w.Write([]byte(`{"token":"copilot-` + string(rune('0'+n)) + `","refresh_in":1500}`))
		case "/chat/completions":
			n := atomic.AddInt32(&chats, 1)
			if n == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer copilot-2" {
				t.Errorf("重试应用新换的 copilotToken，得到 %q", got)
			}
			writeSSE(w, "data: [DONE]\n\n")
		}
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	cred := &channel.Credential{UID: "gh-octocat", AccessToken: "gho_test_token"}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "gpt-4.1", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("401 之后应换 token 重试成功，却失败：%v", err)
	}
	st.Close()
	if got := atomic.LoadInt32(&chats); got != 2 {
		t.Fatalf("应重试恰好一次（共 2 次），实际 %d", got)
	}
	if got := atomic.LoadInt32(&exchanges); got != 2 {
		t.Fatalf("401 后必须丢缓存再换一次 token（共 2 次），实际 %d", got)
	}
}

// ---------------------------------------------------------------------------
// 3. 目录与余额
// ---------------------------------------------------------------------------

func TestModelsFiltersNonPickerAndMarksSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epCopilotToken:
			_, _ = w.Write([]byte(`{"token":"copilot-tok","refresh_in":1500}`))
		case "/models":
			if got := r.Header.Get("Copilot-Integration-Id"); got != "vscode-chat" {
				t.Errorf("目录请求也要伪装头，得到 %q", got)
			}
			_, _ = w.Write([]byte(`{"data":[
				{"id":"gpt-4.1","name":"GPT-4.1","model_picker_enabled":true,
				 "capabilities":{"limits":{"max_context_window_tokens":128000},
				                 "supports":{"tool_calls":true}}},
				{"id":"text-embedding-3-small","name":"Embeddings","model_picker_enabled":false,
				 "capabilities":{"supports":{"tool_calls":false}}}
			]}`))
		}
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	got, err := a.Models(context.Background(), &channel.Credential{UID: "gh-octocat", AccessToken: "gho_test_token"})
	if err != nil {
		t.Fatalf("Models 失败：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("非对话模型（model_picker_enabled=false）应被过滤，得到 %d 个：%+v", len(got), got)
	}
	m := got[0]
	if m.ID != "gpt-4.1" || m.ContextWindow != 128000 || m.Source != channel.SourceUpstream {
		t.Fatalf("模型信息不对：%+v", m)
	}
	if m.Tools != channel.CapYes {
		t.Fatalf("上游说支持工具调用，Tools 应为 CapYes：%+v", m)
	}
	if m.Reasoning != channel.CapUnknown || m.Images != channel.CapUnknown {
		t.Fatalf("上游没给思考/图片能力位，应为 CapUnknown（不猜）：%+v", m)
	}
}

func TestBalanceReadsQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != epUsage {
			t.Errorf("未预期路径：%s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token gho_test_token" {
			t.Errorf("配额接口应带 githubToken，得到 %q", got)
		}
		_, _ = w.Write([]byte(`{"quota_snapshots":{"premium_interactions":{"remaining":123,"unlimited":false}}}`))
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	bal, err := a.Balance(context.Background(), &channel.Credential{UID: "u", AccessToken: "gho_test_token"})
	if err != nil {
		t.Fatalf("Balance 失败：%v", err)
	}
	if !bal.Known || bal.Credits != 123 {
		t.Fatalf("余额不对：%+v", bal)
	}
}

// ---------------------------------------------------------------------------
// 4. 错误归一与 X-Initiator
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		// 401：copilotToken 过期或 githubToken 失效 —— 重新登录。
		{401, `{"error":{"message":"unauthorized"}}`, errs.SessionDead},
		// 403 两副面孔：一般 403 = 身份不被接受（重新登录）；说了「没有订阅/权益」则是账号权益问题。
		{403, `{"error":{"message":"forbidden"}}`, errs.SessionDead},
		{403, `{"error":{"message":"This account has no Copilot subscription"}}`, errs.HardCredit},
		{403, `{"error":{"message":"Copilot is not enabled for this user"}}`, errs.HardCredit},
		{402, `payment required`, errs.HardCredit},
		// 429 两副面孔：限流 vs 额度耗尽。
		{429, `{"error":{"message":"rate limited"}}`, errs.SoftRate},
		{429, `{"error":{"message":"You have exceeded your premium request quota"}}`, errs.HardCredit},
		{400, `{"error":{"message":"context length exceeded"}}`, errs.PromptTooLong},
		{400, `{"error":{"message":"content policy violation"}}`, errs.ContentBlocked},
		{400, `{"error":{"message":"model gpt-9 is not supported"}}`, errs.ModelUnavailable},
		{404, `{"error":{"message":"not found"}}`, errs.ModelUnavailable},
		{503, `{"error":{"message":"upstream boom"}}`, errs.UpstreamFault},
		{418, `teapot`, errs.Parse},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Errorf("Classify(%d, %q) = %v，want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestInitiatorOf(t *testing.T) {
	cases := []struct {
		name string
		msgs []channel.Message
		want string
	}{
		{"只有用户消息", []channel.Message{{Role: "system"}, {Role: "user"}}, "user"},
		{"含 assistant（agent 多轮）", []channel.Message{{Role: "user"}, {Role: "assistant"}}, "agent"},
		{"含 tool 结果", []channel.Message{{Role: "user"}, {Role: "tool"}}, "agent"},
		{"含 developer（改写后的 system）", []channel.Message{{Role: "user"}, {Role: "developer"}}, "user"},
	}
	for _, c := range cases {
		if got := initiatorOf(c.msgs); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// Spec 的能力位必须说实话：原生工具调用 → Tools=true / ToolsShim=false；思考字段上游没有 → Reasoning=false。
func TestSpecIsHonest(t *testing.T) {
	s := New().Spec()
	if s.Kind != channel.Copilot || s.Category != channel.CategoryCoding {
		t.Fatalf("Kind/Category 不对：%+v", s)
	}
	if !s.Tools || s.ToolsShim {
		t.Fatalf("Copilot 原生支持工具调用：Tools=true 且 ToolsShim=false，得到 %+v", s)
	}
	if s.Reasoning {
		t.Fatalf("上游没有思考字段，不得声明 Reasoning=true：%+v", s)
	}
	if !s.SSEOnly {
		t.Fatal("统一向上游要流式，SSEOnly 应为 true")
	}
	if s.DefaultMinIntervalSec <= 0 {
		t.Fatalf("应有出厂默认最小间隔，得到 %d", s.DefaultMinIntervalSec)
	}
}
