package antigravity

// antigravity_test.go 用 mock 上游端到端覆盖四件最容易出错的事：
//  1. 登录三段（授权码换令牌 → 认身份 → 开通）；
//  2. 开通（loadCodeAssist 有项目直接用；没有才 onboardUser，且**不能带 project**）；
//  3. 对话请求体（外层信封 + Gemini 形态 contents + 伪装头）与 SSE 解析（含 functionCall）；
//  4. 错误归一（含流内错误与 403 的两种含义）。
//
// 全程不打真实上游：所有断言都落在「发出去的字节」与「转出来的 chunk」上。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// testAdapter 把三处端点与两台基址都指向 mock server。
func testAdapter(srv *httptest.Server) *Adapter {
	a := New()
	a.chatBases = []string{srv.URL}
	a.loadBases = []string{srv.URL}
	a.tokenURL = srv.URL + "/token"
	a.userInfoURL = srv.URL + "/userinfo"
	return a
}

// ---------------------------------------------------------------------------
// 1. 登录：授权码 → 令牌 → 身份 → 开通
// ---------------------------------------------------------------------------

func TestOAuthFlowFindsExistingProject(t *testing.T) {
	var sawForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			raw, _ := io.ReadAll(r.Body)
			sawForm, _ = url.ParseQuery(string(raw))
			fmt.Fprint(w, `{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600}`)
		case "/userinfo":
			if got := r.Header.Get("Authorization"); got != "Bearer at-1" {
				t.Errorf("userinfo 应带 Bearer access token，得到 %q", got)
			}
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
	if !strings.Contains(q.Get("scope"), "cloud-platform") || !strings.Contains(q.Get("scope"), "cclog") {
		t.Fatalf("scope 必须含 cloud-platform 与 cclog：%q", q.Get("scope"))
	}
	// 粘贴引导语必须讲清「localhost 打不开是正常的」，否则用户会以为授权失败。
	hp, ok := sess.(interface{ Hint() string })
	if !ok || !strings.Contains(hp.Hint(), "localhost") {
		t.Fatalf("Hint 必须说明回调是 localhost（打不开正常）：%v", ok)
	}

	// 还没粘码：应当是「等待中」而不是错误
	if _, err := sess.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("未粘码时应返回 ErrPending，got %v", err)
	}
	// 粘一整条回调地址也要能抽出 code（含 %2F 编码）
	if err := sess.(channel.CallbackAcceptor).AcceptCallback(
		"http://localhost:51121/oauth-callback?code=4%2F0Aabcdefghijklmnop&scope=https://x"); err != nil {
		t.Fatalf("AcceptCallback 失败：%v", err)
	}
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if sawForm.Get("grant_type") != "authorization_code" ||
		sawForm.Get("code") != "4/0Aabcdefghijklmnop" ||
		sawForm.Get("code_verifier") == "" ||
		sawForm.Get("client_id") != clientID ||
		sawForm.Get("client_secret") != clientSecret ||
		sawForm.Get("redirect_uri") != redirectURI {
		t.Fatalf("换令牌参数不对：%v", sawForm)
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
	// 重复 Poll 不该再换一次令牌
	if _, err := sess.Poll(context.Background()); err == nil {
		t.Fatal("完成的会话再 Poll 应当报错，而不是再走一遍流程")
	}
}

// 没有现成项目时要走 onboardUser；free tier **不能**带 project（带了会 412/403）。
func TestOnboardWhenNoProject(t *testing.T) {
	var onboardBody map[string]any
	var loadBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt","expires_in":3600}`)
		case "/userinfo":
			fmt.Fprint(w, `{"email":"u@example.test"}`)
		case "/v1internal:loadCodeAssist":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &loadBody)
			fmt.Fprint(w, `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`)
		case "/v1internal:onboardUser":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &onboardBody)
			fmt.Fprint(w, `{"name":"operations/abc","done":true,"response":{"cloudaicompanionProject":{"id":"proj-new"}}}`)
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := testAdapter(srv)
	sess, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	_ = sess.(channel.CallbackAcceptor).AcceptCallback("code-abcdefghijklmnop")
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
		t.Fatal("free tier 不能带 cloudaicompanionProject（带了会 Precondition Failed）")
	}
	meta, _ := onboardBody["metadata"].(map[string]any)
	if meta["ideType"] != ideType || meta["pluginType"] != pluginType {
		t.Fatalf("开通元数据不对：%+v", meta)
	}
	// loadCodeAssist 也要带元数据（不带上游不认调用方）。
	lm, _ := loadBody["metadata"].(map[string]any)
	if lm["ideType"] != ideType {
		t.Fatalf("loadCodeAssist 缺 metadata：%+v", loadBody)
	}
}

// 账号资格有问题时，上游给的枚举原因必须原样透出来（不能吞成「授权失败」）。
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
	_ = sess.(channel.CallbackAcceptor).AcceptCallback("code-abcdefghijklmnop")
	_, err = sess.Poll(context.Background())
	if err == nil {
		t.Fatal("账号不可用时应报错，而不是给一份用不了的凭证")
	}
	if !strings.Contains(err.Error(), "UNSUPPORTED_LOCATION") {
		t.Fatalf("应把上游的枚举原因原样透出来：%v", err)
	}
}

// 没有 refresh_token 的授权结果不可用（只能用一小时），必须当场报错。
func TestExchangeWithoutRefreshTokenFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"at-only","expires_in":3600}`)
			return
		}
		t.Errorf("不该请求 %s", r.URL.Path)
	}))
	defer srv.Close()

	a := testAdapter(srv)
	sess, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	_ = sess.(channel.CallbackAcceptor).AcceptCallback("code-abcdefghijklmnop")
	if _, err := sess.Poll(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("缺 refresh_token 应报错并说清：%v", err)
	}
}

// farFuture 造一个「还没过期」的时间点（用于逼出「不靠有效期判断」的路径）。
func farFuture() time.Time { return time.Now().Add(24 * time.Hour) }

// ---------------------------------------------------------------------------
// 2. 对话：请求体（外层信封 + contents 形态 + 伪装头）与 SSE 解析
// ---------------------------------------------------------------------------

func TestChatEnvelopeHeadersAndSSE(t *testing.T) {
	var gotPath, gotQuery string
	var gotHeaders http.Header
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotHeaders = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			// 思考块（Gemini 系的标法是 thought:true，另带签名）
			`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"想一下","thought":true,"thoughtSignature":"ErADCq0D"}]}}]}}`,
			// 正文
			`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"好的"}]}}]}}`,
			// 原生工具调用（整包到达，带 id）
			`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"北京"},"id":"toolu_vrtx_1"}}]},"finishReason":"OTHER"}]}}`,
			// 收尾 + usage
			`{"response":{"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":9,"totalTokenCount":16,"thoughtsTokenCount":2}}}`,
		}
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
	}))
	defer srv.Close()

	a := testAdapter(srv)
	st, err := a.Chat(context.Background(), &channel.Credential{
		UID: "u-1", AccessToken: "at", RefreshToken: "rt",
		Extra: map[string]string{"project": "proj-123"},
	}, channel.ChatRequest{
		Model:     "gemini-3-pro-high",
		MaxTokens: 2048,
		Messages: []channel.Message{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "北京天气"},
			{Role: "assistant", ToolCalls: []channel.ToolCall{{ID: "call_x", Type: "function",
				Function: channel.FunctionCall{Name: "get_weather", Arguments: `{"city":"北京"}`}}}},
			{Role: "tool", Name: "get_weather", ToolCallID: "call_x", Content: `{"temp":26}`},
		},
		Tools: []map[string]any{{
			"type": "function",
			"function": map[string]any{"name": "get_weather", "description": "查天气",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		}},
		ToolChoice: "required",
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	// ---- 请求侧 ----
	if gotPath != "/v1internal:streamGenerateContent" || gotQuery != "alt=sse" {
		t.Fatalf("路径/查询不对：%s?%s", gotPath, gotQuery)
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer at" {
		t.Fatalf("Authorization 不对：%q", got)
	}
	if got := gotHeaders.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept 不对：%q", got)
	}
	if ua := gotHeaders.Get("User-Agent"); !strings.HasPrefix(ua, "antigravity/") {
		t.Fatalf("UA 必须伪装成 Antigravity 客户端：%q", ua)
	}
	// 这两个头必须不存在：带上会按项目级鉴权校验，直接 403。
	for _, h := range []string{"x-api-key", "x-goog-user-project"} {
		if v := gotHeaders.Get(h); v != "" {
			t.Fatalf("禁止发送 %s（会 403）：%q", h, v)
		}
	}

	// 外层信封
	if gotBody["project"] != "proj-123" || gotBody["model"] != "gemini-3-pro-high" ||
		gotBody["requestType"] != "agent" || gotBody["userAgent"] != "antigravity" {
		t.Fatalf("外层信封不对：%v", gotBody)
	}
	if rid, _ := gotBody["requestId"].(string); !strings.HasPrefix(rid, "agent-") || len(rid) < 10 {
		t.Fatalf("requestId 形态不对：%v", gotBody["requestId"])
	}
	inner, _ := gotBody["request"].(map[string]any)
	if inner == nil {
		t.Fatalf("缺 request 内层：%v", gotBody)
	}
	// systemInstruction 必须是对象（带 parts），写成字符串会 400。
	si, _ := inner["systemInstruction"].(map[string]any)
	if si == nil {
		t.Fatalf("system 没进 systemInstruction：%v", inner)
	}
	parts, _ := si["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "你是助手" {
		t.Fatalf("systemInstruction 形态不对：%v", si)
	}
	// contents 是 Gemini 形态：user / model / user，assistant 必须叫 model。
	contents, _ := inner["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents 条数不对：%d", len(contents))
	}
	if contents[0].(map[string]any)["role"] != "user" {
		t.Fatalf("第一条应 role=user：%v", contents[0])
	}
	m1 := contents[1].(map[string]any)
	if m1["role"] != "model" {
		t.Fatalf("assistant 应转成 role=model：%v", m1)
	}
	fc, _ := m1["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	if fc == nil || fc["name"] != "get_weather" || fc["id"] != "call_x" {
		t.Fatalf("assistant 的 tool_calls 应转成带 id 的 functionCall：%v", m1["parts"])
	}
	if args, _ := fc["args"].(map[string]any); args["city"] != "北京" {
		t.Fatalf("args 应被解析成对象：%v", fc["args"])
	}
	m2 := contents[2].(map[string]any)
	if m2["role"] != "user" {
		t.Fatalf("工具结果应转成 role=user：%v", m2)
	}
	fr, _ := m2["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "get_weather" || fr["id"] != "call_x" {
		t.Fatalf("functionResponse 缺 name/id：%v", fr)
	}
	// 工具定义与 tool_choice
	tools, _ := inner["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 没转成 functionDeclarations：%v", inner["tools"])
	}
	decls, _ := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if len(decls) != 1 || decls[0].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("functionDeclarations 不对：%v", tools[0])
	}
	tcfg, _ := inner["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
	if tcfg["mode"] != "ANY" {
		t.Fatalf("tool_choice=required 应映射成 ANY：%v", tcfg)
	}
	// generationConfig
	gc, _ := inner["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(2048) {
		t.Fatalf("maxOutputTokens 不对：%v", gc)
	}
	if tc, _ := gc["thinkingConfig"].(map[string]any); tc["includeThoughts"] != true {
		t.Fatalf("思考型号应打开 includeThoughts：%v", gc)
	}

	// ---- 响应侧 ----
	var content, reasoning strings.Builder
	var calls []channel.ToolCall
	var finishes []string
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
				finishes = append(finishes, ch.FinishReason)
			}
		}
	}
	if content.String() != "好的" || reasoning.String() != "想一下" {
		t.Fatalf("正文/思考分流不对：%q / %q", content.String(), reasoning.String())
	}
	if len(calls) != 1 || calls[0].Function.Name != "get_weather" ||
		calls[0].Function.Arguments != `{"city":"北京"}` || calls[0].Type != "function" ||
		calls[0].ID != "toolu_vrtx_1" {
		t.Fatalf("functionCall 没转成标准 tool_call（id 必须用上游的）：%+v", calls)
	}
	// OTHER 映射成 tool_calls，STOP 映射成 stop，两者都出现过。
	if len(finishes) != 2 || finishes[0] != "tool_calls" || finishes[1] != "stop" {
		t.Fatalf("finishReason 映射不对：%v", finishes)
	}
	if usage["prompt_tokens"] != 7 || usage["total_tokens"] != 16 {
		t.Fatalf("usage 没转成 OpenAI 叫法：%+v", usage)
	}
	det, _ := usage["completion_tokens_details"].(map[string]any)
	if det["reasoning_tokens"] != 2 {
		t.Fatalf("思考 token 应单独带出：%+v", usage)
	}
}

// 凭证里没有 project 时，对话前应当**就地开通**（而不是直接发一个会 412 的请求）。
func TestChatOnboardsWhenProjectMissing(t *testing.T) {
	var sawLoad bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			sawLoad = true
			fmt.Fprint(w, `{"cloudaicompanionProject":{"id":"proj-lazy"}}`)
		case "/v1internal:streamGenerateContent":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if body["project"] != "proj-lazy" {
				t.Errorf("就地开通拿到的 project 没进请求体：%v", body["project"])
			}
			fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}]}}\n\n")
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := testAdapter(srv)
	st, err := a.Chat(context.Background(), &channel.Credential{
		UID: "u-2", AccessToken: "at", ExpiresAt: farFuture(),
	}, channel.ChatRequest{Model: "claude-sonnet-4-6", Messages: []channel.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	for {
		if _, err := st.Next(); err != nil {
			break
		}
	}
	if !sawLoad {
		t.Fatal("凭证缺 project 时应先 loadCodeAssist")
	}
}

// 令牌过期时，**开通请求也必须用刷新后的令牌**（顺序错了会拿旧令牌去打，表现成莫名其妙的 401）。
func TestChatUsesRefreshedTokenForProjectSetup(t *testing.T) {
	var loadAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			fmt.Fprint(w, `{"access_token":"at-new","expires_in":3600}`)
		case "/v1internal:loadCodeAssist":
			loadAuth = r.Header.Get("Authorization")
			fmt.Fprint(w, `{"cloudaicompanionProject":"proj-refreshed"}`)
		case "/v1internal:streamGenerateContent":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if body["project"] != "proj-refreshed" {
				t.Errorf("project 不对：%v", body["project"])
			}
			fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}]}}\n\n")
		}
	}))
	defer srv.Close()

	a := testAdapter(srv)
	st, err := a.Chat(context.Background(), &channel.Credential{
		UID: "u-6", AccessToken: "at-old", RefreshToken: "rt-6",
		ExpiresAt: time.Now().Add(-time.Minute), // 已过期 → usableToken 必须先刷
	}, channel.ChatRequest{Model: "gemini-3-pro-high", Messages: []channel.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	for {
		if _, err := st.Next(); err != nil {
			break
		}
	}
	if loadAuth != "Bearer at-new" {
		t.Fatalf("开通请求必须用刷新后的令牌：%q", loadAuth)
	}
}

// access token 被上游拒（401）时，必须**真去换一次**令牌再重试，且只重试一次。
func TestChatRefreshesOnceOn401(t *testing.T) {
	var chats, refreshes int
	var secondAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			refreshes++
			raw, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(raw))
			if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt-1" {
				t.Errorf("刷新参数不对：%v", form)
			}
			fmt.Fprint(w, `{"access_token":"at-new","expires_in":3600}`)
		case "/v1internal:streamGenerateContent":
			chats++
			if chats == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"code":401,"message":"unauthenticated"}}`)
				return
			}
			secondAuth = r.Header.Get("Authorization")
			fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}]}}\n\n")
		}
	}))
	defer srv.Close()

	a := testAdapter(srv)
	st, err := a.Chat(context.Background(), &channel.Credential{
		UID: "u-3", AccessToken: "at-old", RefreshToken: "rt-1",
		// 故意设成还没过期：证明 401 之后走的是「强制刷新」，不是靠有效期判断。
		ExpiresAt: farFuture(),
		Extra:     map[string]string{"project": "p"},
	}, channel.ChatRequest{Model: "gemini-3-pro-high", Messages: []channel.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("401 之后应当换令牌重试成功，却失败了：%v", err)
	}
	defer st.Close()
	for {
		if _, err := st.Next(); err != nil {
			break
		}
	}
	if chats != 2 || refreshes != 1 {
		t.Fatalf("应重试恰好一次（对话 2 次 / 刷新 1 次），实际 chats=%d refreshes=%d", chats, refreshes)
	}
	if secondAuth != "Bearer at-new" {
		t.Fatalf("重试必须用新令牌：%q", secondAuth)
	}
}

// 首选基址不可达时应回落到下一台（daily 沙箱挂了不影响生产）。
func TestChatFallsBackToSecondBase(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}}\n\n")
	}))
	defer srv.Close()

	a := testAdapter(srv)
	// 第一台指向一个必然连不上的地址（端口 1）。
	a.chatBases = []string{"http://127.0.0.1:1", srv.URL}

	st, err := a.Chat(context.Background(), &channel.Credential{
		UID: "u-4", AccessToken: "at", ExpiresAt: farFuture(), Extra: map[string]string{"project": "p"},
	}, channel.ChatRequest{Model: "gemini-3-pro-high", Messages: []channel.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("应回落到第二台基址：%v", err)
	}
	defer st.Close()
	var got strings.Builder
	for {
		c, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("流读取失败：%v", err)
		}
		for _, ch := range c.Choices {
			got.WriteString(ch.Delta.Content)
		}
	}
	if got.String() != "ok" || hits != 1 {
		t.Fatalf("回落没成功：content=%q hits=%d", got.String(), hits)
	}
}

// 流内错误：token 类应归一成 SessionDead（提示重新授权），而不是上游故障。
func TestFrameErrorIsSessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"error\":{\"code\":401,\"message\":\"invalid token\"}}\n\n")
	}))
	defer srv.Close()

	a := testAdapter(srv)
	st, err := a.Chat(context.Background(), &channel.Credential{
		UID: "u-5", AccessToken: "at", ExpiresAt: farFuture(), Extra: map[string]string{"project": "p"},
	}, channel.ChatRequest{Model: "gemini-3-pro-high", Messages: []channel.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat 应成功建立流（错误在流里）：%v", err)
	}
	defer st.Close()
	var got error
	for {
		_, err := st.Next()
		if err != nil {
			got = err
			break
		}
	}
	if k, ok := errs.KindOf(got); !ok || k != errs.SessionDead {
		t.Fatalf("帧内 token 错误应归一成 SessionDead，得到 %v（%v）", k, got)
	}
}

// ---------------------------------------------------------------------------
// 3. 续期、能力声明、错误归一
// ---------------------------------------------------------------------------

func TestRefreshPersistsRotatedRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Errorf("意外路径 %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt-old" {
			t.Errorf("刷新参数不对：%v", form)
		}
		// 上游轮换了 refresh token
		fmt.Fprint(w, `{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`)
	}))
	defer srv.Close()

	a := testAdapter(srv)
	nc, err := a.Refresh(context.Background(), &channel.Credential{
		UID: "u", AccessToken: "at-old", RefreshToken: "rt-old",
	})
	if err != nil {
		t.Fatalf("Refresh 失败：%v", err)
	}
	if nc.AccessToken != "at-new" || nc.RefreshToken != "rt-new" {
		t.Fatalf("轮换后的 refresh token 必须落盘：%+v", nc)
	}
	if nc.ExpiresAt.IsZero() {
		t.Fatal("过期时间必须更新")
	}
	// 没有 refresh token 的凭证：刷不了就是刷不了，返回 (nil, nil) 让上层按 401 处理。
	nc2, err := a.Refresh(context.Background(), &channel.Credential{UID: "u", AccessToken: "at"})
	if err != nil || nc2 != nil {
		t.Fatalf("无可续期凭证时应返回 (nil,nil)，得到 %v %v", nc2, err)
	}
}

func TestSpecAndModels(t *testing.T) {
	a := New()
	sp := a.Spec()
	if !sp.Tools || sp.ToolsShim {
		t.Fatal("Antigravity 是原生工具调用，不该走模拟档")
	}
	if !sp.SSEOnly || !sp.Reasoning || sp.Images {
		t.Fatalf("能力位不对：%+v", sp)
	}
	if sp.Category != channel.CategoryChat || sp.DefaultMinIntervalSec <= 0 {
		t.Fatalf("分类/最小间隔不对：%+v", sp)
	}
	if sp.Status != channel.Active {
		t.Fatalf("状态不对：%+v", sp)
	}
	// Docs 必须写明封号风险（这是硬性要求，不是文案偏好）。
	for _, kw := range []string{"条款", "封", "shadow", "风险"} {
		if !strings.Contains(sp.Docs, kw) {
			t.Fatalf("Spec.Docs 必须写明 ToS/封号风险，缺关键词 %q：%s", kw, sp.Docs)
		}
	}

	ms, err := a.Models(context.Background(), &channel.Credential{UID: "u"})
	if err != nil || len(ms) == 0 {
		t.Fatalf("模型表不对：%v %+v", err, ms)
	}
	for _, m := range ms {
		if m.Source != channel.SourceLocal || m.Tools != channel.CapYes {
			t.Fatalf("模型标注不对：%+v", m)
		}
		if m.Reasoning == channel.CapUnknown && m.ContextWindow != 0 {
			t.Fatalf("未知能力的模型不应编造上下文长度：%+v", m)
		}
	}
	// Claude 非思考型号必须如实标「不吐思考块」（不能整条渠道一刀切说支持）。
	var sonnet *channel.ModelInfo
	for i := range ms {
		if ms[i].ID == "claude-sonnet-4-6" {
			sonnet = &ms[i]
		}
	}
	if sonnet == nil || sonnet.Reasoning != channel.CapNo {
		t.Fatalf("claude-sonnet-4-6 应标 Reasoning=CapNo：%+v", sonnet)
	}

	if _, err := a.Login(context.Background()); err == nil || !strings.Contains(err.Error(), "授权") {
		t.Fatalf("Login 应提示走面板授权：%v", err)
	}
	if b, _ := a.Balance(context.Background(), nil); b.Known {
		t.Fatal("余额未知时不能假装知道")
	}
	if r, _ := a.Checkin(context.Background(), nil); !r.NoActivity {
		t.Fatal("无签到活动应标 NoActivity")
	}
}

func TestClassify(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{"error":{"message":"unauthenticated"}}`, errs.SessionDead},
		{403, `{"error":{"status":"PERMISSION_DENIED","message":"invalid token"}}`, errs.SessionDead},
		{403, `{"error":{"message":"PERMISSION_DENIED: UNSUPPORTED_LOCATION"}}`, errs.AuthFailed},
		{403, `{"error":"account not onboarded"}`, errs.AuthFailed},
		{412, `{"error":"precondition failed"}`, errs.AuthFailed},
		{429, `{"error":{"message":"rate limited"}}`, errs.SoftRate},
		{429, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"quota exceeded"}}`, errs.HardCredit},
		{400, `{"error":"context too long"}`, errs.PromptTooLong},
		{400, `{"error":"safety blocked"}`, errs.ContentBlocked},
		{404, `not found`, errs.ModelUnavailable},
		{500, `boom`, errs.UpstreamFault},
		{503, `unavailable`, errs.UpstreamFault},
		{418, `teapot`, errs.Parse},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Errorf("Classify(%d, %q) = %v，want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestExtractCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"4/0Aabcdefghijklmnop", "4/0Aabcdefghijklmnop"},
		{"http://localhost:51121/oauth-callback?code=4%2F0Aabcdefghijklmnop&scope=x", "4/0Aabcdefghijklmnop"},
		{"http://localhost:51121/oauth-callback?code=4/0Aabcdefghijklmnop&scope=x", "4/0Aabcdefghijklmnop"},
		{"  4/0Aabcdefghijklmnop  ", "4/0Aabcdefghijklmnop"},
		{"", ""},
		{"too short", ""},
		{"含 空格 的串不许通过xxxxxxxx", ""},
	}
	for _, c := range cases {
		if got := extractCode(c.in); got != c.want {
			t.Errorf("extractCode(%q) = %q，want %q", c.in, got, c.want)
		}
	}
}

// 指纹必须按账号稳定：同账号两次派生一致，不同账号应当能分出来（否则风控会当成一台机器轮流用）。
func TestFingerprintIsStablePerAccount(t *testing.T) {
	f1, f2 := fingerprintOf("user-a"), fingerprintOf("user-a")
	if f1 != f2 {
		t.Fatalf("同账号指纹必须稳定：%+v vs %+v", f1, f2)
	}
	if !strings.HasPrefix(f1.ua(), "antigravity/"+antigravityVersion+" ") {
		t.Fatalf("UA 形态不对：%q", f1.ua())
	}
	if f1.metaPlatform != "WINDOWS" && f1.metaPlatform != "MACOS" {
		t.Fatalf("平台标识不对：%+v", f1)
	}
	seen := map[fingerprint]bool{}
	for _, seed := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		seen[fingerprintOf(seed)] = true
	}
	if len(seen) < 2 {
		t.Fatal("不同账号应当能派生出不同指纹（全都一样说明派生没起作用）")
	}
}

// ---------------------------------------------------------------------------
// 4. 请求体的思考配置与 systemInstruction 形态（按参考实现逐字段对齐）
// ---------------------------------------------------------------------------

// thinkingConfig 必须按型号分流：Gemini 3 用字符串 thinkingLevel、Claude 思考系用数字
// thinkingBudget（且 maxOutputTokens 必须大于它），非思考型号**完全不带** thinkingConfig。
// 旧实现不分型号一律塞 includeThoughts:true，与两份参考实现都不符。
func TestThinkingConfigPerModel(t *testing.T) {
	genCfg := func(model string, maxTok int) map[string]any {
		raw, err := buildRequest(channel.ChatRequest{
			Model: model, MaxTokens: maxTok,
			Messages: []channel.Message{{Role: "user", Content: "hi"}},
		}, "proj", "agent-x")
		if err != nil {
			t.Fatalf("buildRequest(%s) 失败：%v", model, err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("请求体不是 JSON：%v", err)
		}
		inner, _ := body["request"].(map[string]any)
		gc, _ := inner["generationConfig"].(map[string]any)
		if gc == nil {
			t.Fatalf("%s 缺 generationConfig：%v", model, body)
		}
		return gc
	}

	// Gemini 3 Pro：thinkingLevel 跟着型号后缀走，且不带数字 budget。
	for _, c := range []struct{ model, level string }{
		{"gemini-3-pro-high", "high"},
		{"gemini-3-pro-low", "low"},
	} {
		tc, _ := genCfg(c.model, 4096)["thinkingConfig"].(map[string]any)
		if tc == nil || tc["includeThoughts"] != true || tc["thinkingLevel"] != c.level {
			t.Fatalf("%s 的 thinkingConfig 应为 thinkingLevel=%s：%v", c.model, c.level, tc)
		}
		if _, has := tc["thinkingBudget"]; has {
			t.Fatalf("Gemini 3 不该带数字 thinkingBudget：%v", tc)
		}
	}

	// Claude 思考系：数字 thinkingBudget，且 maxOutputTokens 必须大于它。
	gc := genCfg("claude-opus-4-6-thinking", 2048)
	tc, _ := gc["thinkingConfig"].(map[string]any)
	if tc == nil || tc["thinkingBudget"] != float64(claudeThinkingBudget) {
		t.Fatalf("Claude 思考系应带 thinkingBudget：%v", tc)
	}
	if mo, _ := gc["maxOutputTokens"].(float64); int(mo) <= claudeThinkingBudget {
		t.Fatalf("maxOutputTokens 必须大于 thinkingBudget，得到 %v", gc["maxOutputTokens"])
	}

	// 非思考型号：一个 thinkingConfig 都不许带。
	for _, model := range []string{"claude-sonnet-4-6", "gpt-oss-120b-medium"} {
		if tc, _ := genCfg(model, 2048)["thinkingConfig"].(map[string]any); tc != nil {
			t.Fatalf("%s 是非思考型号，不应带 thinkingConfig：%v", model, tc)
		}
	}
}

// systemInstruction 必须是带 role:"user" 的对象（参考实现都显式带 role），parts 形态不变。
func TestSystemInstructionHasRole(t *testing.T) {
	raw, err := buildRequest(channel.ChatRequest{
		Model: "gemini-3-pro-high",
		Messages: []channel.Message{
			{Role: "system", Content: "s"},
			{Role: "user", Content: "u"},
		},
	}, "p", "agent-x")
	if err != nil {
		t.Fatalf("buildRequest 失败：%v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("请求体不是 JSON：%v", err)
	}
	inner, _ := body["request"].(map[string]any)
	si, _ := inner["systemInstruction"].(map[string]any)
	if si == nil || si["role"] != "user" {
		t.Fatalf("systemInstruction 必须是带 role=user 的对象：%v", si)
	}
	parts, _ := si["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "s" {
		t.Fatalf("systemInstruction parts 不对：%v", si)
	}
}

// Claude 系的 toolConfig 必须用 VALIDATED（opencode 与 AIClient2API 两份独立实现一致），
// Gemini 系仍保留标准 AUTO/ANY/NONE。
func TestClaudeToolConfigUsesValidated(t *testing.T) {
	fc := func(model, choice string) map[string]any {
		raw, err := buildRequest(channel.ChatRequest{
			Model: model, ToolChoice: choice,
			Messages: []channel.Message{{Role: "user", Content: "hi"}},
			Tools: []map[string]any{{
				"type": "function",
				"function": map[string]any{"name": "f",
					"parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
			}},
		}, "p", "agent-x")
		if err != nil {
			t.Fatalf("buildRequest 失败：%v", err)
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		inner, _ := body["request"].(map[string]any)
		cfg, _ := inner["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
		return cfg
	}

	if cfg := fc("claude-opus-4-6-thinking", "auto"); cfg == nil || cfg["mode"] != "VALIDATED" {
		t.Fatalf("Claude 系 toolConfig 应为 VALIDATED：%v", cfg)
	}
	// 客户端没给 tool_choice 时，Claude 系也要补出 VALIDATED（opencode/ACP 都这么做）。
	if cfg := fc("claude-sonnet-4-6", ""); cfg == nil || cfg["mode"] != "VALIDATED" {
		t.Fatalf("Claude 系即使没给 tool_choice 也应带 VALIDATED：%v", cfg)
	}
	if cfg := fc("gemini-3-pro-high", "required"); cfg == nil || cfg["mode"] != "ANY" {
		t.Fatalf("Gemini 系应保留标准 ANY：%v", cfg)
	}
}
