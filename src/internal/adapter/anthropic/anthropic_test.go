package anthropic

// anthropic_test.go 用 mock 上游覆盖六件事（每一项都对应一个「错了就很难查」的地方）：
//
//  1. OAuth 授权码兑换：PKCE 的 verifier 真的对得上 challenge、client_id/redirect_uri 正确；
//  2. 对话请求带的是 **Bearer + oauth beta**（而不是 x-api-key）——混了只会得到 401；
//  3. tools 透传：OpenAI 的 function.parameters → Anthropic 的 input_schema，
//     tool_choice 的 required → any（名字不一样，发错了上游只回「参数不合法」）；
//  4. SSE 增量重组：正文逐片、**tool_use 参数只在块结束时一次性给出**、usage 把缓存计入；
//  5. 401 之后用 refresh token 换一次再试，且**只重试一次**；
//  6. Classify 表（403 的两种面孔必须分清，判反了会把好号冷却掉）。
//
// mock 上游用 httptest；适配器的 apiBase/tokenURL 是字段，测试直接指过去。

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// newTestAdapter 把适配器指向 mock server（授权页保留生产地址，测试只校验它的形态）。
func newTestAdapter(srv *httptest.Server) *Adapter {
	a := New()
	a.apiBase = srv.URL
	a.tokenURL = srv.URL + "/v1/oauth/token"
	return a
}

func stateOf(t *testing.T, authURL string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("授权地址不是合法 URL：%v", err)
	}
	st := u.Query().Get("state")
	if st == "" {
		t.Fatalf("授权地址里没有 state：%s", authURL)
	}
	return st
}

// 1. 授权码兑换：PKCE 对得上、参数齐全、凭证组装正确、当场验一次。
func TestLoginExchangesCodeWithPKCE(t *testing.T) {
	var exchangeCalls, modelsCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			atomic.AddInt32(&exchangeCalls, 1)
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("兑换请求不是 JSON：%v", err)
			}
			if body["grant_type"] != "authorization_code" {
				t.Errorf("grant_type 应为 authorization_code：%v", body["grant_type"])
			}
			if body["client_id"] != oauthClientID {
				t.Errorf("client_id 不对：%v", body["client_id"])
			}
			if body["redirect_uri"] != oauthRedirect {
				t.Errorf("redirect_uri 不对：%v", body["redirect_uri"])
			}
			if body["code"] != "auth-code-1" {
				t.Errorf("code 没有原样带上：%v", body["code"])
			}
			if v, _ := body["code_verifier"].(string); v == "" {
				t.Errorf("PKCE 的 code_verifier 缺失（缺了上游会拒，而且报文看不出原因）")
			}
			_, _ = w.Write([]byte(`{"access_token":"oat-1","refresh_token":"ort-1","expires_in":3600,` +
				`"account":{"email_address":"user@example.com","uuid":"acct-uuid-1"}}`))

		case "/v1/models":
			atomic.AddInt32(&modelsCalls, 1)
			if got := r.Header.Get("Authorization"); got != "Bearer oat-1" {
				t.Errorf("校验凭证应带 Bearer access token，得到 %q", got)
			}
			if got := r.Header.Get("anthropic-beta"); !strings.Contains(got, oauthBeta) {
				t.Errorf("校验凭证也要带 oauth beta 头，得到 %q", got)
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5","max_input_tokens":200000}]}`))

		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(srv)
	loginSess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	// 同包内直取具体类型：授权码要走 CallbackAcceptor（channel.LoginSession 不带它），
	// 顺带校验 PKCE 的 verifier 真的被保存下来了。
	sess := loginSess.(*session)

	// 授权地址：端点、参数、以及 scope 的冒号**保持原样**。
	authURL := sess.AuthURL()
	if !strings.HasPrefix(authURL, epAuthorize+"?") {
		t.Fatalf("授权地址应指向 %s，得到 %s", epAuthorize, authURL)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	if q.Get("client_id") != oauthClientID || q.Get("response_type") != "code" {
		t.Fatalf("授权参数不对：%v", q)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("缺 PKCE 参数：%v", q)
	}
	if q.Get("redirect_uri") != oauthRedirect {
		t.Fatalf("redirect_uri 应为手工回调地址，得到 %q", q.Get("redirect_uri"))
	}
	// scope 值本身要解得出原样（含冒号）；同时原始查询串里 scope 段不能出现 %3A
	// （redirect_uri 里的 %3A 是正常编码，不受影响）。
	if q.Get("scope") != oauthScopes {
		t.Fatalf("scope 不对：%q", q.Get("scope"))
	}
	scopeSeg := authURL[strings.Index(authURL, "&scope=")+len("&scope="):]
	if strings.Contains(scopeSeg, "%3A") {
		t.Fatalf("scope 的冒号不能被编码：%s", scopeSeg)
	}
	state := stateOf(t, authURL)

	// 还没粘码时 Poll 是「等待授权」，不是错误。
	if _, err := sess.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("未粘码时应返回 ErrPending，得到 %v", err)
	}
	// 错 state 必须被拒（CSRF）。
	if err := sess.AcceptCallback("auth-code-1#wrong-state"); err == nil {
		t.Fatalf("state 不匹配时应拒绝")
	}
	// 正确形态：授权页显示的 code#state。
	if err := sess.AcceptCallback("auth-code-1#" + state); err != nil {
		t.Fatalf("AcceptCallback 失败：%v", err)
	}

	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if cred.AccessToken != "oat-1" || cred.RefreshToken != "ort-1" {
		t.Fatalf("凭证组装不对：%+v", cred)
	}
	if cred.UID != "acct-uuid-1" || cred.Nickname != "user@example.com" {
		t.Fatalf("账号标识应取自令牌响应里的 account：uid=%q nick=%q", cred.UID, cred.Nickname)
	}
	if cred.ExpiresAt.Before(time.Now()) {
		t.Fatalf("过期时间应在未来：%v", cred.ExpiresAt)
	}
	if atomic.LoadInt32(&exchangeCalls) != 1 || atomic.LoadInt32(&modelsCalls) != 1 {
		t.Fatalf("调用次数不对：exchange=%d models=%d", exchangeCalls, modelsCalls)
	}
	// 再次 Poll 不应重复兑换。
	if _, err := sess.Poll(context.Background()); err == nil {
		t.Fatalf("登录完成后再次 Poll 应报错")
	}
	// Hint 必须引导用户去粘贴授权码（面板据此切换形态）。
	var h interface{ Hint() string } = sess
	if !strings.Contains(h.Hint(), "授权码") {
		t.Fatalf("Hint 应引导用户粘贴授权码，得到 %q", h.Hint())
	}

	// PKCE 的可验证性：授权地址里的 challenge 必须等于 verifier 的 S256。
	sum := sha256.Sum256([]byte(sess.verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); want != q.Get("code_challenge") {
		t.Fatalf("code_challenge 与 code_verifier 对不上（PKCE 会失败）：%s vs %s", q.Get("code_challenge"), want)
	}
}

// 2 + 3 + 4. 带 Bearer/oauth beta 头、tools 透传、SSE 增量与工具调用重组。
func TestChatHeadersToolsAndStream(t *testing.T) {
	var chatCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("未预期的路径：%s", r.URL.Path)
			return
		}
		atomic.AddInt32(&chatCalls, 1)

		// —— 鉴权头：必须是 Bearer + oauth beta，绝不能是 x-api-key。
		if got := r.Header.Get("Authorization"); got != "Bearer oat-live" {
			t.Errorf("缺少 Bearer 令牌：%q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "" {
			t.Errorf("OAuth 令牌不能用 x-api-key（混了只会 401）：%q", got)
		}
		if got := r.Header.Get("anthropic-beta"); !strings.Contains(got, oauthBeta) || !strings.Contains(got, cliBeta) {
			t.Errorf("anthropic-beta 应同时含 %q 与 %q（官方 CLI 两项都带），得到 %q", cliBeta, oauthBeta, got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicVersion {
			t.Errorf("anthropic-version 应为 %q，得到 %q", anthropicVersion, got)
		}
		if got := r.Header.Get("anthropic-dangerous-direct-browser-access"); got != "true" {
			t.Errorf("官方 SDK 会注入 anthropic-dangerous-direct-browser-access: true，得到 %q", got)
		}
		if got := r.Header.Get("X-App"); got != "cli" {
			t.Errorf("应伪装成官方 CLI（x-app: cli），得到 %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != cliUA {
			t.Errorf("User-Agent 应为 %q，得到 %q", cliUA, got)
		}

		// —— 请求体：tools → input_schema、tool_choice → any、max_tokens 兜底。
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model      string `json:"model"`
			MaxTokens  int    `json:"max_tokens"`
			Stream     bool   `json:"stream"`
			CountTools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"input_schema"`
			} `json:"tools"`
			ToolChoice map[string]any `json:"tool_choice"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("请求体不是 JSON：%v", err)
		}
		if body.Model != "claude-sonnet-5" || !body.Stream {
			t.Errorf("model/stream 不对：%+v", body)
		}
		if body.MaxTokens != defaultMaxTokens {
			t.Errorf("客户端没给 max_tokens 时应兜底 %d，得到 %d", defaultMaxTokens, body.MaxTokens)
		}
		if len(body.CountTools) != 1 || body.CountTools[0].Name != "get_weather" {
			t.Fatalf("tools 没透传过去：%+v", body.CountTools)
		}
		props, _ := body.CountTools[0].InputSchema["properties"].(map[string]any)
		if body.CountTools[0].InputSchema["type"] != "object" || props["location"] == nil {
			t.Errorf("function.parameters 应翻成 input_schema：%+v", body.CountTools[0].InputSchema)
		}
		if body.ToolChoice["type"] != "any" {
			t.Errorf("tool_choice=required 应翻成 any，得到 %v", body.ToolChoice)
		}

		// —— SSE：正文分片 + tool_use 参数在块结束时一次性给出。
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, frame := range []string{
			`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":2,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"世界"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"location\":"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"北京\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
			`{"type":"message_stop"}`,
		} {
			_, _ = w.Write([]byte("event: x\ndata: " + frame + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()

	a := newTestAdapter(srv)
	cred := &channel.Credential{UID: "anthropic-test", AccessToken: "oat-live", RefreshToken: "ort-live"}
	req := channel.ChatRequest{
		Model:    "claude-sonnet-5",
		Messages: []channel.Message{{Role: "user", Content: "北京天气"}},
		Tools: []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "查询天气",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"location": map[string]any{"type": "string"}},
					"required":   []any{"location"},
				},
			},
		}},
		ToolChoice: "required",
	}

	st, err := a.Chat(context.Background(), cred, req)
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	var content strings.Builder
	var toolCall chunkToolCall
	var finish string
	var usage map[string]any
	toolCallAt, lastTextAt, seq := -1, -1, 0
	for {
		c, err := st.Next()
		if err != nil {
			if err != io.EOF {
				t.Fatalf("读流出错：%v", err)
			}
			break
		}
		seq++
		if c.Usage != nil {
			usage = c.Usage
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
				lastTextAt = seq
			}
			if len(ch.Delta.ToolCalls) > 0 {
				tc := ch.Delta.ToolCalls[0]
				toolCall = chunkToolCall{tc.Function.Name, tc.Function.Arguments, tc.ID}
				if toolCallAt < 0 {
					toolCallAt = seq
				}
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}

	if content.String() != "你好世界" {
		t.Fatalf("正文增量重组不对：%q", content.String())
	}
	if toolCall.Name != "get_weather" || toolCall.ID != "toolu_1" {
		t.Fatalf("工具调用没重组成：%+v", toolCall)
	}
	if toolCall.Args != `{"location":"北京"}` {
		t.Fatalf("参数应在块结束时一次性给全，得到 %q", toolCall.Args)
	}
	if toolCallAt < lastTextAt {
		t.Fatalf("tool_use 参数不能早于块结束给出（中途半截 JSON 客户端解析不了）：tool=%d text=%d", toolCallAt, lastTextAt)
	}
	if finish != "tool_calls" {
		t.Fatalf("stop_reason=tool_use 应归一成 tool_calls，得到 %q", finish)
	}
	// usage：prompt_tokens 必须把缓存计入（10+5+2），否则客户端把上下文算小一大截。
	if usage == nil || usage["prompt_tokens"] != 17 || usage["completion_tokens"] != 7 || usage["total_tokens"] != 24 {
		t.Fatalf("usage 换算不对：%v", usage)
	}
	if d, _ := usage["prompt_tokens_details"].(map[string]any); d["cached_tokens"] != 5 {
		t.Fatalf("缓存命中数应透出：%v", usage)
	}
	if atomic.LoadInt32(&chatCalls) != 1 {
		t.Fatalf("对话只应发一次，实际 %d", chatCalls)
	}
}

type chunkToolCall struct {
	Name, Args, ID string
}

// 5. 401 之后刷新一次再试，且只重试一次。
func TestChatRefreshesOnceOn401(t *testing.T) {
	var chatCalls, refreshCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			atomic.AddInt32(&refreshCalls, 1)
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			if body["grant_type"] != "refresh_token" {
				t.Errorf("应是 refresh 调用：%v", body["grant_type"])
			}
			if body["refresh_token"] != "ort-old" {
				t.Errorf("应带手里的 refresh token：%v", body["refresh_token"])
			}
			// scope 是官方 CLI refresh 报文固定带的一项（services/oauth/client.ts）。
			if s, _ := body["scope"].(string); !strings.Contains(s, "user:inference") {
				t.Errorf("refresh 应带完整 scope（官方 CLI 会带）：%v", body["scope"])
			}
			_, _ = w.Write([]byte(`{"access_token":"oat-new","refresh_token":"ort-new","expires_in":3600}`))
			return
		}
		n := atomic.AddInt32(&chatCalls, 1)
		if n == 1 {
			// 第一次：令牌过期。
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"token expired"}}`))
			return
		}
		// 第二次：刷新后的新令牌必须被用上。
		if got := r.Header.Get("Authorization"); got != "Bearer oat-new" {
			t.Errorf("重试应带新令牌，得到 %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"type":"message_start","message":{"id":"m2"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好了"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"message_stop"}` + "\n\n"))
	}))
	defer srv.Close()

	a := newTestAdapter(srv)
	// ExpiresAt 留零值 = 未知，不触发「提前刷新」，确保走的是 401 重试路径。
	cred := &channel.Credential{UID: "u1", AccessToken: "oat-old", RefreshToken: "ort-old"}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "claude-sonnet-5", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("401 后应刷新重试成功，得到错误：%v", err)
	}
	defer st.Close()

	var content strings.Builder
	for {
		c, err := st.Next()
		if err != nil {
			if err != io.EOF {
				t.Fatalf("读流出错：%v", err)
			}
			break
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
		}
	}
	if content.String() != "好了" {
		t.Fatalf("重试后没拿到内容：%q", content.String())
	}
	if atomic.LoadInt32(&chatCalls) != 2 || atomic.LoadInt32(&refreshCalls) != 1 {
		t.Fatalf("应恰好重试一次：chat=%d refresh=%d", chatCalls, refreshCalls)
	}
}

// 5b. 重试也只有 401：只刷一次、直接报 SessionDead（不反复拿死 refresh token 去试）。
func TestChatRetriesOnlyOnce(t *testing.T) {
	var chatCalls, refreshCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			atomic.AddInt32(&refreshCalls, 1)
			_, _ = w.Write([]byte(`{"access_token":"oat-new","expires_in":3600}`))
			return
		}
		atomic.AddInt32(&chatCalls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid"}}`))
	}))
	defer srv.Close()

	a := newTestAdapter(srv)
	cred := &channel.Credential{UID: "u1", AccessToken: "oat-old", RefreshToken: "ort-old"}
	_, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "claude-sonnet-5", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if k, ok := errs.KindOf(err); !ok || k != errs.SessionDead {
		t.Fatalf("最终 401 应归一成 SessionDead，得到 %v（%v）", k, err)
	}
	if atomic.LoadInt32(&chatCalls) != 2 || atomic.LoadInt32(&refreshCalls) != 1 {
		t.Fatalf("只应重试一次：chat=%d refresh=%d", chatCalls, refreshCalls)
	}
}

// 401 但手里没有 refresh token：不去刷新（刷不了就别假装能刷）。
func TestChatNoRefreshTokenSkipsRefresh(t *testing.T) {
	var refreshCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			atomic.AddInt32(&refreshCalls, 1)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid"}}`))
	}))
	defer srv.Close()

	a := newTestAdapter(srv)
	cred := &channel.Credential{UID: "u1", AccessToken: "oat-old"}
	_, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "claude-sonnet-5", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if k, _ := errs.KindOf(err); k != errs.SessionDead {
		t.Fatalf("应报 SessionDead，得到 %v", k)
	}
	if atomic.LoadInt32(&refreshCalls) != 0 {
		t.Fatalf("没有 refresh token 时不应调令牌端点，实际 %d 次", refreshCalls)
	}
}

// 6. Classify 表。
func TestClassify(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{"type":"error","error":{"type":"authentication_error"}}`, errs.SessionDead},
		// 403 的两副面孔：权限类判死账号，出口 IP 被拦算上游故障（判反会误杀好号）。
		{403, `{"type":"error","error":{"type":"permission_error","message":"no access"}}`, errs.SessionDead},
		// 令牌被撤销：官方 CLI 对这个 403 报文同样强制刷新（utils/http.ts:112-129），
		// 说明上游有时用 403 而非 401 表达撤销 —— 必须判死，别当成出口 IP 问题。
		{403, `{"type":"error","error":{"message":"OAuth token has been revoked"}}`, errs.SessionDead},
		{403, `{"type":"forbidden","message":"Request not allowed"}`, errs.UpstreamFault},
		{429, `{"message":"rate limited"}`, errs.SoftRate},
		{429, `{"message":"your credit balance is too low"}`, errs.HardCredit},
		{400, `{"message":"prompt is too long: 300000 tokens"}`, errs.PromptTooLong},
		{400, `{"message":"credit balance too low"}`, errs.HardCredit},
		{400, `{"message":"model: claude-x not found"}`, errs.ModelUnavailable},
		{400, `{"message":"unexpected parameter"}`, errs.Parse},
		{413, `{}`, errs.PromptTooLong},
		{404, `{"type":"not_found_error"}`, errs.ModelUnavailable},
		{500, `{"type":"api_error"}`, errs.UpstreamFault},
		{529, `{"type":"overloaded_error"}`, errs.UpstreamFault},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("status=%d body=%s：期望 %v 得到 %v", c.status, c.body, c.want, got)
		}
	}
}

// Models：上游目录可用时照实返回（Source=upstream）；凭证被拒时报错而不是拿本地清单冒充。
func TestModelsAndCredentialVerdict(t *testing.T) {
	var status int32 = 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer oat-live" {
			t.Errorf("模型目录要带 Bearer：%q", got)
		}
		switch atomic.LoadInt32(&status) {
		case 401:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error"}}`))
			return
		case 500:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"type":"api_error","message":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5","max_input_tokens":200000}]}`))
	}))
	defer srv.Close()

	a := newTestAdapter(srv)
	cred := &channel.Credential{UID: "u1", AccessToken: "oat-live"}
	models, err := a.Models(context.Background(), cred)
	if err != nil {
		t.Fatalf("Models 失败：%v", err)
	}
	if len(models) != 1 || models[0].ID != "claude-sonnet-5" || models[0].Source != channel.SourceUpstream {
		t.Fatalf("模型目录不对：%+v", models)
	}
	if models[0].Tools != channel.CapYes || models[0].ContextWindow != 200000 {
		t.Fatalf("能力位/上下文不对：%+v", models[0])
	}

	// 上游 5xx：回落到本地清单（来源必须标 local，不冒充上游）。
	atomic.StoreInt32(&status, 500)
	models, err = a.Models(context.Background(), cred)
	if err != nil {
		t.Fatalf("上游 5xx 时不应让目录整体不可用：%v", err)
	}
	if len(models) == 0 || models[0].Source != channel.SourceLocal {
		t.Fatalf("回落清单必须标 SourceLocal：%+v", models)
	}

	// 401：这是凭证的判决，必须报错，不能拿本地清单糊过去。
	atomic.StoreInt32(&status, 401)
	if _, err = a.Models(context.Background(), cred); err == nil {
		t.Fatalf("凭证被拒时 Models 应报错")
	} else if k, _ := errs.KindOf(err); k != errs.SessionDead {
		t.Fatalf("应归一成 SessionDead，得到 %v", k)
	}
}

// extractAuthCode：三种粘贴形态都要认（用户怎么粘是没法规定的）。
func TestExtractAuthCode(t *testing.T) {
	cases := []struct {
		in        string
		wantCode  string
		wantState string
	}{
		{"https://platform.claude.com/oauth/code/callback?code=abc123&state=st9", "abc123", "st9"},
		{"abc123#st9", "abc123", "st9"},
		{"abc123", "abc123", "default-state"},
		{"  授权码 abc  ", "", ""}, // 含空白：挡掉（明显误粘）
		{"", "", ""},
	}
	for _, c := range cases {
		code, st := extractAuthCode(c.in, "default-state")
		if code != c.wantCode || st != c.wantState {
			t.Fatalf("extractAuthCode(%q) = (%q,%q)，期望 (%q,%q)", c.in, code, st, c.wantCode, c.wantState)
		}
	}
}

// 空凭证：明确报 SessionDead，不发无用请求。
func TestUsableTokenEmptyIsSessionDead(t *testing.T) {
	a := New()
	_, err := a.Chat(context.Background(), &channel.Credential{UID: "u1"}, channel.ChatRequest{
		Model: "claude-sonnet-5", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if k, ok := errs.KindOf(err); !ok || k != errs.SessionDead {
		t.Fatalf("空凭证应报 SessionDead，得到 %v（%v）", k, err)
	}
	// Login 走面板通道，不该被当成可用登录入口。
	if _, err := a.Login(context.Background()); err == nil {
		t.Fatalf("Login 应提示去面板授权")
	}
}
