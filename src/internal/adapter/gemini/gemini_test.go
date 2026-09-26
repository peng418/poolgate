package gemini

import (
	"context"
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

func testAdapter(srv *httptest.Server) *Adapter {
	a := New()
	a.codeAssistBase = srv.URL
	a.tokenURL = srv.URL + "/token"
	a.userInfoURL = srv.URL + "/userinfo"
	return a
}

// 长任务没做完时应当**重发同一个 onboardUser**，不是去 GET 操作名 ——
// 那个 GET 路径没有任何参考实现用过（geminicli2api auth.py:497-510 等三份都是重发）。
func TestOnboardPollsByReposting(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt","expires_in":3600}`)
		case "/userinfo":
			fmt.Fprint(w, `{"email":"u@example.test"}`)
		case "/v1internal:loadCodeAssist":
			fmt.Fprint(w, `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`)
		case "/v1internal:onboardUser":
			posts++
			if posts < 2 {
				fmt.Fprint(w, `{"done":false}`)
				return
			}
			fmt.Fprint(w, `{"done":true,"response":{"cloudaicompanionProject":{"id":"proj-late"}}}`)
		default:
			t.Errorf("不该请求这个路径：%s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	a := testAdapter(srv)
	sess, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	sess.(channel.CallbackAcceptor).AcceptCallback("code-abcdefghijklmnop")
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if cred.Extra["project"] != "proj-late" {
		t.Fatalf("重发轮询没拿到 project：%+v", cred.Extra)
	}
	if posts != 2 {
		t.Fatalf("应当重发 onboardUser 直到 done，实际 POST %d 次", posts)
	}
}

// 流内错误（HTTP 200 但帧里带 error 对象）必须归一成 errs.Error 并把上游原话传出来。
// 这是红线一：过去这一支被当成「没有 candidates 的普通帧」静默跳过，客户端只看到空回复。
func TestChatStreamErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"好的"}]}}]}}`+"\n\n")
		fmt.Fprint(w, "data: "+`{"error":{"code":429,"message":"Resource has been exhausted (quota).","status":"RESOURCE_EXHAUSTED"}}`+"\n\n")
	}))
	defer srv.Close()
	a := testAdapter(srv)
	st, err := a.Chat(context.Background(), &channel.Credential{
		UID: "u", AccessToken: "at", Extra: map[string]string{"project": "p"},
	}, channel.ChatRequest{Model: "gemini-2.5-pro", Messages: []channel.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
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
		t.Fatal("流内错误必须暴露给调用方，不能静默跳过")
	}
	var ee *errs.Error
	if !errors.As(gotErr, &ee) {
		t.Fatalf("流内错误必须是 errs.Error：%T", gotErr)
	}
	if !strings.Contains(ee.Upstream, "Resource has been exhausted") {
		t.Fatalf("上游原话必须留在 Upstream：%+v", ee)
	}
	if ee.Kind != errs.HardCredit {
		t.Fatalf("429 + 额度耗尽应归为 HardCredit，实际 %v", ee.Kind)
	}
}

// 授权流：生成的授权地址要带对参数；用户粘回码之后能换到凭证并完成「开通」。
func TestUserCodeOAuthFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			raw, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(raw))
			if form.Get("grant_type") != "authorization_code" ||
				form.Get("code") != "4/0Aabcdefghijklmnop" ||
				form.Get("code_verifier") == "" ||
				form.Get("redirect_uri") != redirectURI {
				t.Errorf("换令牌参数不对：%v", form)
			}
			fmt.Fprint(w, `{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600}`)
		case "/userinfo":
			fmt.Fprint(w, `{"email":"demo@example.test","name":"Demo"}`)
		case "/v1internal:loadCodeAssist":
			fmt.Fprint(w, `{"currentTier":{"id":"free-tier"},"cloudaicompanionProject":"proj-123"}`)
		default:
			t.Errorf("不该请求这个路径：%s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := testAdapter(srv)
	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	u, err := url.Parse(sess.AuthURL())
	if err != nil {
		t.Fatalf("授权地址解析失败：%v", err)
	}
	q := u.Query()
	if u.Host != "accounts.google.com" || q.Get("client_id") != clientID ||
		q.Get("redirect_uri") != redirectURI || q.Get("response_type") != "code" ||
		q.Get("access_type") != "offline" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Fatalf("授权地址参数不对：%s", sess.AuthURL())
	}
	if !strings.Contains(q.Get("scope"), "cloud-platform") {
		t.Fatalf("scope 必须含 cloud-platform：%q", q.Get("scope"))
	}

	// 还没粘码：应当是「等待中」而不是错误
	if _, err := sess.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("未粘码时应返回 ErrPending，got %v", err)
	}
	// 粘一整个回调地址也能识别出 code
	if err := sess.(channel.CallbackAcceptor).AcceptCallback("https://codeassist.google.com/authcode?code=4%2F0Aabcdefghijklmnop&scope=x"); err != nil {
		t.Fatalf("AcceptCallback 失败：%v", err)
	}
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if cred.AccessToken != "at-1" || cred.RefreshToken != "rt-1" {
		t.Fatalf("令牌不对：%+v", cred)
	}
	if cred.UID != "demo@example.test" || cred.Nickname != "Demo" {
		t.Fatalf("身份不对：%+v", cred)
	}
	if cred.Extra["project"] != "proj-123" {
		t.Fatalf("开通拿到的 project 没落进凭证：%+v", cred.Extra)
	}
	if cred.ExpiresAt.IsZero() {
		t.Fatal("过期时间应当记下（续期靠它）")
	}
}

// 没有现成档位时要走 onboardUser；free-tier 不能带 project（带了会 Precondition Failed）。
func TestOnboardWhenNoCurrentTier(t *testing.T) {
	var onboardBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt","expires_in":3600}`)
		case "/userinfo":
			fmt.Fprint(w, `{"email":"u@example.test"}`)
		case "/v1internal:loadCodeAssist":
			fmt.Fprint(w, `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`)
		case "/v1internal:onboardUser":
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &onboardBody)
			fmt.Fprint(w, `{"name":"operations/abc","done":true,"response":{"cloudaicompanionProject":{"id":"proj-new"}}}`)
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := testAdapter(srv)
	sess, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	sess.(channel.CallbackAcceptor).AcceptCallback("code-abcdefghijklmnop")
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if cred.Extra["project"] != "proj-new" {
		t.Fatalf("onboard 的 project 没拿到：%+v", cred.Extra)
	}
	if onboardBody["tierId"] != "free-tier" {
		t.Fatalf("tierId 不对：%+v", onboardBody)
	}
	if _, has := onboardBody["cloudaicompanionProject"]; has {
		t.Fatal("free-tier 不能带 cloudaicompanionProject（带了会 Precondition Failed）")
	}
}

// 账号不可用时上游给的是枚举原因（地区/年龄/需验证）——必须原样透给用户，不能吞。
func TestIneligibleTierSurfacesReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt","expires_in":3600}`)
		case "/userinfo":
			fmt.Fprint(w, `{"email":"u@example.test"}`)
		case "/v1internal:loadCodeAssist":
			fmt.Fprint(w, `{"ineligibleTiers":[{"reasonCode":"UNSUPPORTED_LOCATION","reasonMessage":"not available in your region"}]}`)
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	a := testAdapter(srv)
	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	if err := sess.(channel.CallbackAcceptor).AcceptCallback("code-abcdefghijklmnop"); err != nil {
		t.Fatalf("AcceptCallback 失败：%v", err)
	}
	if _, err := sess.Poll(context.Background()); err == nil {
		t.Fatal("账号不可用时应报错，而不是给一份用不了的凭证")
	} else if !strings.Contains(err.Error(), "UNSUPPORTED_LOCATION") {
		t.Fatalf("应把上游的枚举原因原样透出来：%v", err)
	}
}

// 对话：原生工具调用（functionCall 整包）+ 思考分片 + usage，全部要转对。
func TestChatNativeTools(t *testing.T) {
	var gotAuth, gotUA string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:streamGenerateContent" {
			t.Errorf("路径不对：%s", r.URL.Path)
		}
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("缺 alt=sse：%s", r.URL.RawQuery)
		}
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"想一下","thought":true}]}}]}}`,
			`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"好的"}]}}]}}`,
			`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"北京"}}}]}}]}}`,
			`{"response":{"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":9,"totalTokenCount":16}}}`,
		}
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
	}))
	defer srv.Close()

	a := testAdapter(srv)
	st, err := a.Chat(context.Background(), &channel.Credential{
		UID: "u", AccessToken: "at", Extra: map[string]string{"project": "proj-123"},
	}, channel.ChatRequest{
		Model:     "gemini-3-pro-preview",
		Messages:  []channel.Message{{Role: "user", Content: "北京天气"}},
		MaxTokens: 2048,
		Tools: []map[string]any{{
			"type": "function",
			"function": map[string]any{"name": "get_weather", "description": "查天气",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		}},
		ToolChoice: "auto",
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	if gotAuth != "Bearer at" || !strings.HasPrefix(gotUA, "GeminiCLI/") {
		t.Fatalf("头不对：%q %q", gotAuth, gotUA)
	}
	if gotBody["model"] != "gemini-3-pro-preview" || gotBody["project"] != "proj-123" {
		t.Fatalf("外层封装不对：%v", gotBody)
	}
	inner, _ := gotBody["request"].(map[string]any)
	if inner == nil {
		t.Fatalf("缺 request 内层：%v", gotBody)
	}
	tools, _ := inner["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 没转成 functionDeclarations：%v", inner["tools"])
	}
	decls, _ := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if len(decls) != 1 {
		t.Fatalf("functionDeclarations 不对：%v", tools[0])
	}

	var content, reasoning strings.Builder
	var calls []channel.ToolCall
	finish := ""
	var usage map[string]any
	for {
		c, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("流读取失败：%v", err)
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
			reasoning.WriteString(ch.Delta.ReasoningContent)
			calls = append(calls, ch.Delta.ToolCalls...)
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	if content.String() != "好的" || reasoning.String() != "想一下" {
		t.Fatalf("正文/思考不对：%q / %q", content.String(), reasoning.String())
	}
	if len(calls) != 1 || calls[0].Function.Name != "get_weather" ||
		calls[0].Function.Arguments != `{"city":"北京"}` || calls[0].Type != "function" || calls[0].ID == "" {
		t.Fatalf("functionCall 没转成标准 tool_call：%+v", calls)
	}
	if finish != "stop" {
		t.Fatalf("finishReason 映射不对：%q", finish)
	}
	if usage["prompt_tokens"] != float64(7) && usage["prompt_tokens"] != 7 {
		t.Fatalf("usage 没转成 OpenAI 叫法：%+v", usage)
	}
}

// 请求体方向：system / 工具结果 / assistant 的 tool_calls 都要转对。
func TestBuildRequestConversion(t *testing.T) {
	req := channel.ChatRequest{
		Model: "gemini-2.5-pro",
		Messages: []channel.Message{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "北京天气"},
			{Role: "assistant", ToolCalls: []channel.ToolCall{{ID: "c1", Function: channel.FunctionCall{
				Name: "get_weather", Arguments: `{"city":"北京"}`}}}},
			{Role: "tool", Name: "get_weather", ToolCallID: "c1", Content: `{"temp":26}`},
		},
		Tools: []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}},
		}},
		ToolChoice: "required",
	}
	raw, err := buildRequest(req, "proj")
	if err != nil {
		t.Fatalf("buildRequest 失败：%v", err)
	}
	var obj map[string]any
	json.Unmarshal(raw, &obj)
	// 外层信封只允许 {model, project, request} —— 参考实现三份都只有这三个，
	// 多发 Google 会按未知字段 400。
	for k := range obj {
		if k != "model" && k != "project" && k != "request" {
			t.Fatalf("外层信封出现参考实现没有的字段 %q：%v", k, obj)
		}
	}
	inner, _ := obj["request"].(map[string]any)
	if inner == nil {
		t.Fatalf("缺内层：%v", obj)
	}
	if _, has := inner["session_id"]; has {
		t.Fatal("内层不应带 session_id（参考实现没有此字段）")
	}
	// system 走 systemInstruction，不进 contents
	if si, _ := inner["systemInstruction"].(map[string]any); si == nil {
		t.Fatalf("system 没进 systemInstruction：%v", inner)
	}
	contents, _ := inner["contents"].([]any)
	if len(contents) != 3 { // user / model(工具调用) / user(工具结果)
		t.Fatalf("contents 条数不对：%d", len(contents))
	}
	m1, _ := contents[1].(map[string]any)
	if m1["role"] != "model" {
		t.Fatalf("assistant 应转成 role=model：%v", m1)
	}
	parts1, _ := m1["parts"].([]any)
	if _, ok := parts1[0].(map[string]any)["functionCall"]; !ok {
		t.Fatalf("assistant 的 tool_calls 应转成 functionCall：%v", parts1[0])
	}
	args, _ := parts1[0].(map[string]any)["functionCall"].(map[string]any)["args"].(map[string]any)
	if args["city"] != "北京" {
		t.Fatalf("args 应当被解析成对象：%v", args)
	}
	m2, _ := contents[2].(map[string]any)
	if m2["role"] != "user" {
		t.Fatalf("工具结果应转成 role=user：%v", m2)
	}
	fr, _ := m2["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "get_weather" {
		t.Fatalf("functionResponse 缺 name：%v", fr)
	}
	// tool_choice=required → ANY
	tc, _ := inner["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
	if tc["mode"] != "ANY" {
		t.Fatalf("tool_choice 映射不对：%v", tc)
	}
}

func TestExtractCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"4/0Aabcdefghijklmnop", "4/0Aabcdefghijklmnop"},
		{"https://codeassist.google.com/authcode?code=4%2F0Aabcdefghijklmnop&scope=x", "4/0Aabcdefghijklmnop"},
		{"  4/0Aabcdefghijklmnop  ", "4/0Aabcdefghijklmnop"},
		{"", ""},
		{"too short", ""},
	}
	for _, c := range cases {
		if got := extractCode(c.in); got != c.want {
			t.Errorf("extractCode(%q) = %q，want %q", c.in, got, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{"error":"invalid token"}`, errs.SessionDead},
		{403, `{"error":{"status":"PERMISSION_DENIED","details":"UNSUPPORTED_LOCATION"}}`, errs.AuthFailed},
		{412, `{"error":"precondition failed"}`, errs.AuthFailed},
		{429, `{"error":"rate limited"}`, errs.SoftRate},
		{429, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"quota exceeded"}}`, errs.HardCredit},
		{400, `{"error":"model not found"}`, errs.ModelUnavailable},
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

func TestSpecAndModels(t *testing.T) {
	a := New()
	sp := a.Spec()
	if !sp.Tools || sp.ToolsShim {
		t.Fatal("Gemini 是原生工具调用，不该走模拟档")
	}
	if !sp.SSEOnly || sp.DefaultMinIntervalSec <= 0 {
		t.Fatalf("SSEOnly/最小间隔不对：%+v", sp)
	}
	if sp.Category != channel.CategoryChat {
		t.Fatalf("登录式渠道应归 chat 类：%+v", sp)
	}
	ms, err := a.Models(context.Background(), &channel.Credential{UID: "u"})
	if err != nil || len(ms) == 0 {
		t.Fatalf("模型表不对：%v %+v", err, ms)
	}
	for _, m := range ms {
		if m.Source != channel.SourceLocal || m.Tools != channel.CapYes {
			t.Fatalf("模型标注不对：%+v", m)
		}
	}
	if _, err := a.Login(context.Background()); err == nil || !strings.Contains(err.Error(), "授权") {
		t.Fatalf("Login 应提示走面板授权：%v", err)
	}
}
