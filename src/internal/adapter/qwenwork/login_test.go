package qwenwork

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// 适配器必须实现 channel.Authorizer，否则面板上「添加账号」对它没有授权按钮。
func TestImplementsAuthorizer(t *testing.T) {
	var _ channel.Authorizer = New()
}

// StartLogin 必须起本机回调并给出带 PKCE 的授权页地址。
// 没有本机回调就没法收 code，用户点完授权会卡住。
func TestStartLoginProvidesCallbackAndPKCE(t *testing.T) {
	a := New()
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("发起授权失败：%v", err)
	}
	defer s.Cancel()

	raw := s.AuthURL()
	if raw == "" {
		t.Fatal("必须给出授权页地址")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("client_id") != OAuthClientID {
		t.Fatalf("client_id 不对：%s", q.Get("client_id"))
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatal("必须带 PKCE S256 challenge")
	}
	if len(q.Get("state")) < 8 {
		t.Fatalf("state 太短（Hydra 要求 ≥8）：%q", q.Get("state"))
	}
	// 回调地址必须是本机随机端口，且能从它反查到监听中的 server。
	red := q.Get("redirect_uri")
	if !strings.HasPrefix(red, "http://127.0.0.1:") || !strings.HasSuffix(red, "/callback") {
		t.Fatalf("回调地址形态不对：%s", red)
	}
	resp, err := http.Get(red + "?code=x")
	if err != nil {
		t.Fatalf("本机回调 server 未监听：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("回调应返回落地页，实际 %d", resp.StatusCode)
	}
}

// 没回调时轮询必须返回 ErrPending（控制台据此显示「等待授权」而不是报错）。
func TestPollPendingBeforeCallback(t *testing.T) {
	a := New()
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cancel()

	if _, err := s.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("未回调时应返回 ErrPending，实际 %v", err)
	}
}

// 回调带 error 时，轮询必须把上游给的原因原样带出来（红线一：失败必带原因）。
func TestPollSurfacesCallbackError(t *testing.T) {
	a := New()
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cancel()

	red := redirectURIOf(t, s.AuthURL())
	// 模拟浏览器带着 error 回调。
	resp, err := http.Get(red + "?error=access_denied&error_description=user%20rejected")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	_, perr := s.Poll(context.Background())
	if perr == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(perr.Error(), "user rejected") {
		t.Fatalf("应带上游/浏览器给的原因，实际 %v", perr)
	}
}

// 回调缺少 code 要明确报错，不能静默当成 pending。
func TestPollMissingCode(t *testing.T) {
	a := New()
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cancel()
	red := redirectURIOf(t, s.AuthURL())
	resp, _ := http.Get(red) // 不带 code
	if resp != nil {
		resp.Body.Close()
	}
	_, perr := s.Poll(context.Background())
	if perr == nil || perr == channel.ErrPending {
		t.Fatalf("缺 code 应报错，实际 %v", perr)
	}
}

// Cancel 必须释放本机端口且可重复调用（控制台会先 Cancel 再轮询）。
func TestCancelIsIdempotentAndReleasesPort(t *testing.T) {
	a := New()
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	red := redirectURIOf(t, s.AuthURL())
	s.Cancel()
	s.Cancel() // 重复调用不应 panic
	if _, err := http.Get(red + "?code=x"); err == nil {
		t.Fatal("Cancel 后本机端口应已释放")
	}
}

// 兑换失败时必须带上上游原话（不写「请重试」）。
func TestPollExchangeFailureCarriesUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_grant",
			"error_description": "code expired",
		})
	}))
	defer srv.Close()

	a := New()
	a.tokenURL = srv.URL // 指向 mock，验证错误携带而不碰真实上游
	s := &qwSession{a: a, verifier: "v", redirectURI: "http://127.0.0.1:1/callback", code: "c"}

	_, err := a.exchangeToken(context.Background(), s)
	if err == nil {
		t.Fatal("兑换失败应报错")
	}
	if !strings.Contains(err.Error(), "code expired") {
		t.Fatalf("应带上游原话，实际 %v", err)
	}
	if k, _ := errs.KindOf(err); k == "" {
		t.Fatalf("应给出错误分类，实际 %v", err)
	}
}

// JWT 解析：uid 优先 user_id，回退 sub；昵称回退 email。
func TestParseJWTIdentity(t *testing.T) {
	mk := func(claims map[string]any) string {
		h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
		p, _ := json.Marshal(claims)
		enc := base64.RawURLEncoding.EncodeToString
		return enc(h) + "." + enc(p) + ".sig"
	}
	uid, nick := parseJWTIdentity(mk(map[string]any{"user_id": "u1", "username": "demo-qwen"}))
	if uid != "u1" || nick != "demo-qwen" {
		t.Fatalf("解析不对：uid=%q nick=%q", uid, nick)
	}
	uid, nick = parseJWTIdentity(mk(map[string]any{"sub": "u2", "email": "a@b.c"}))
	if uid != "u2" || nick != "a@b.c" {
		t.Fatalf("回退解析不对：uid=%q nick=%q", uid, nick)
	}
	if uid, _ := parseJWTIdentity("garbage"); uid != "" {
		t.Fatal("坏 token 应返回空")
	}
	if uid, _ := parseJWTIdentity(""); uid != "" {
		t.Fatal("空 token 应返回空")
	}
}

// 落地页文案：成功时不能写「登录成功」（此时还没兑换 token）。
func TestCallbackPageWording(t *testing.T) {
	ok := callbackPage(true, "")
	if !strings.Contains(ok, "已收到授权") {
		t.Fatal("成功页应说「已收到授权」而非「登录成功」")
	}
	bad := callbackPage(false, "用户拒绝")
	if !strings.Contains(bad, "用户拒绝") {
		t.Fatal("失败页应显示原因")
	}
}

func redirectURIOf(t *testing.T, authURL string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	red := u.Query().Get("redirect_uri")
	if red == "" {
		t.Fatal("授权地址里没有 redirect_uri")
	}
	return red
}

// 授权码兑换之后必须再换一次 device_token —— 这是「千问重新登录了还是一样报错」的正解。
//
// OAuth 兑换出来的 access token 网页域不认（实测 401 invalid-credential），
// wild-work 的做法就是拿 refresh_token 去 /api/v1/deviceToken/refresh 换 device_token。
func TestLoginExchangesDeviceToken(t *testing.T) {
	oauthJWT := func() string {
		h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
		p, _ := json.Marshal(map[string]any{"user_id": "u-700f", "email": "x@y.z"})
		enc := base64.RawURLEncoding.EncodeToString
		return enc(h) + "." + enc(p) + ".sig"
	}
	var deviceCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": oauthJWT(), "refresh_token": "ory-rt-1", "expires_in": 3600,
			})
		default:
			deviceCalls++
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), `"refresh_token":"ory-rt-1"`) {
				t.Errorf("应拿 OAuth 的 refresh_token 去换 device_token，实际 body %s", b)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_token": "device-jwt", "refresh_token": "rotated-rt", "expires_in": 86400 * 1000,
			})
		}
	}))
	defer srv.Close()

	a := New()
	a.tokenURL = srv.URL + "/oauth2/token"
	a.deviceURL = srv.URL + epDeviceToken
	s := &qwSession{a: a, verifier: "v", redirectURI: "http://127.0.0.1:1/callback", code: "c"}

	cred, err := a.exchangeToken(context.Background(), s)
	if err != nil {
		t.Fatalf("兑换失败：%v", err)
	}
	if deviceCalls != 1 {
		t.Fatalf("应恰好调用一次 deviceToken 端点，实际 %d", deviceCalls)
	}
	if cred.AccessToken != "device-jwt" {
		t.Fatalf("落盘的必须是 device_token（网页域才认），实际 %q", cred.AccessToken)
	}
	if cred.RefreshToken != "rotated-rt" {
		t.Fatalf("应保存轮换后的 refresh_token，实际 %q", cred.RefreshToken)
	}
	if cred.UID != "u-700f" {
		t.Fatalf("uid 应从 OAuth 的 JWT 里解出来，实际 %q", cred.UID)
	}
	if time.Until(cred.ExpiresAt) < time.Hour {
		t.Fatalf("到期时间应按 device 响应的 expires_in 生效，实际 %v", cred.ExpiresAt)
	}
}

// 换不到 device_token 就不能算登录成功：宁可报错，也别把「网页域不认的凭证」塞进池子
// （那正是用户遇到的状态：面板显示授权成功，之后每次操作都 401，重新登录多少次都一样）。
func TestLoginFailsWhenDeviceExchangeFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
			p, _ := json.Marshal(map[string]any{"user_id": "u1"})
			enc := base64.RawURLEncoding.EncodeToString
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": enc(h) + "." + enc(p) + ".sig", "refresh_token": "rt", "expires_in": 3600,
			})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"errorCode": "INVALID_REFRESH_TOKEN", "errorMessage": "refresh token is invalid"})
	}))
	defer srv.Close()

	a := New()
	a.tokenURL = srv.URL + "/oauth2/token"
	a.deviceURL = srv.URL + epDeviceToken
	s := &qwSession{a: a, verifier: "v", redirectURI: "http://127.0.0.1:1/callback", code: "c"}

	cred, err := a.exchangeToken(context.Background(), s)
	if err == nil || cred != nil {
		t.Fatalf("换不到 device_token 时不该返回凭证，got %v %v", cred, err)
	}
	if !strings.Contains(err.Error(), "重新授权") {
		t.Fatalf("要说清下一步，实际 %q", err.Error())
	}
}

// 手工回填回调地址：千问的回调被上游锁死在本机（实测 redirect_uri 必须命中预注册清单），
// 远端浏览器跳回 127.0.0.1 打不开时，粘贴是唯一不改回调地址的完成方式。
func TestQwenworkAcceptCallback(t *testing.T) {
	a := New()
	mk := func() *qwSession {
		return &qwSession{a: a, verifier: "v", redirectURI: "http://127.0.0.1:1/callback"}
	}
	// ① 整串回调 URL
	s := mk()
	if err := s.AcceptCallback("http://127.0.0.1:54321/callback?code=abc&state=xyz"); err != nil {
		t.Fatalf("整串 URL 应可解析：%v", err)
	}
	if s.code != "abc" {
		t.Fatalf("应取出 code，实际 %q", s.code)
	}
	// ② 只有 query 的一段
	s2 := mk()
	if err := s2.AcceptCallback("?code=def"); err != nil {
		t.Fatalf("query 片段应可解析：%v", err)
	}
	if s2.code != "def" {
		t.Fatalf("应取出 code，实际 %q", s2.code)
	}
	// ③ 只贴 code
	s3 := mk()
	if err := s3.AcceptCallback("ghi"); err != nil {
		t.Fatalf("裸 code 应可解析：%v", err)
	}
	if s3.code != "ghi" {
		t.Fatalf("应取出 code，实际 %q", s3.code)
	}
	// ④ 上游回了错误 → 记成失败原因（界面据此显示，不当成成功）
	s4 := mk()
	if err := s4.AcceptCallback("http://127.0.0.1:1/callback?error=access_denied&error_description=%E7%94%A8%E6%88%B7%E6%8B%92%E7%BB%9D"); err != nil {
		t.Fatalf("错误回调也应被接受（交给界面显示）：%v", err)
	}
	if s4.failed == "" || s4.code != "" {
		t.Fatalf("应记失败原因而不是 code，实际 failed=%q code=%q", s4.failed, s4.code)
	}
	// ⑤ 什么都没给 → 明确报错
	s5 := mk()
	if err := s5.AcceptCallback("https://qwenwork.cn/oauth2/fallbacks/error"); err == nil {
		t.Fatal("没有 code 时应报错")
	}
	if err := s5.AcceptCallback("  "); err == nil {
		t.Fatal("空内容应报错")
	}
}

// 回填之后 Poll 必须真的用这个 code 去兑换（而不是还在等回调）。
func TestQwenworkPollUsesPastedCode(t *testing.T) {
	var gotCode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			_ = r.ParseForm()
			gotCode = r.Form.Get("code")
			h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
			p, _ := json.Marshal(map[string]any{"user_id": "u-demo"})
			enc := base64.RawURLEncoding.EncodeToString
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": enc(h) + "." + enc(p) + ".sig", "refresh_token": "ory-rt", "expires_in": 3600,
			})
			return
		}
		// deviceToken 换发（登录路径必须走它）
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_token": "device-jwt", "refresh_token": "rotated", "expires_in": 86400 * 1000,
		})
	}))
	defer srv.Close()

	a := New()
	a.tokenURL = srv.URL + "/oauth2/token"
	a.deviceURL = srv.URL + "/device"
	s := &qwSession{a: a, verifier: "v", redirectURI: "http://127.0.0.1:1/callback"}
	if err := s.AcceptCallback("http://127.0.0.1:1/callback?code=pasted-code"); err != nil {
		t.Fatal(err)
	}
	cred, err := s.Poll(context.Background())
	if err != nil {
		t.Fatalf("回填后 Poll 应完成兑换：%v", err)
	}
	if gotCode != "pasted-code" {
		t.Fatalf("兑换应使用粘进来的 code，实际 %q", gotCode)
	}
	if cred == nil || cred.AccessToken != "device-jwt" {
		t.Fatalf("兑换结果应为 device_token，实际 %+v", cred)
	}
}
