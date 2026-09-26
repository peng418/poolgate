package codebuddy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 发起授权：拿 state + authUrl，并把跳转链解析成最终地址给用户。
func TestStartLoginResolvesAuthURL(t *testing.T) {
	var hops []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			if r.Method != http.MethodPost {
				t.Errorf("auth/state 应为 POST，实际 %s", r.Method)
			}
			if got := r.URL.Query().Get("platform"); got != "CLI" {
				t.Errorf("platform 应为 CLI，实际 %q", got)
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-1","authUrl":"`+srvURL(r)+`/login"}}`)
		case "/login":
			hops = append(hops, "/login")
			w.Header().Set("Location", srvURL(r)+"/login/")
			w.WriteHeader(http.StatusMovedPermanently)
		case "/login/":
			hops = append(hops, "/login/")
			_, _ = io.WriteString(w, "<html>login page</html>")
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}
	defer s.Cancel()

	if len(hops) != 2 {
		t.Fatalf("应跟随一次 301 拿到最终地址，实际访问 %v", hops)
	}
	if s.AuthURL() != srv.URL+"/login/" {
		t.Fatalf("应给用户最终地址 %s，实际 %s", srv.URL+"/login/", s.AuthURL())
	}
}

// 主路（上游授权接口）不可用时**不报错**：退回「等粘贴」会话，兜底必须仍可用。
func TestStartLoginFallsBackToPaste(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `upstream down`)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("主路失败不应让发起授权整体失败（兜底要可用）: %v", err)
	}
	defer s.Cancel()
	if s.AuthURL() == "" {
		t.Fatal("兜底会话仍应给一个可打开的登录页地址")
	}
	// Hint 要如实写明主路不可用，并给出粘贴方式。
	hp, ok := s.(interface{ Hint() string })
	if !ok || hp.Hint() == "" {
		t.Fatal("会话必须实现 Hint 且内容非空")
	}
	if !strings.Contains(hp.Hint(), "方式二") {
		t.Fatalf("主路不可用时 Hint 应指向粘贴兜底，实际 %q", hp.Hint())
	}
	// 还没粘贴：Poll 是 pending，不是失败。
	if _, err := s.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("等待粘贴时应为 pending，实际 %v", err)
	}
}

// 未登录完：业务 code != 0（"login ing"）是 pending，不是错误。
func TestPollPendingOnBusinessCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-1","authUrl":"https://www.codebuddy.cn/login"}}`)
		case "/v2/plugin/auth/token":
			_, _ = io.WriteString(w, `{"code":11217,"msg":"login ing...","data":null}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}
	defer s.Cancel()

	if _, err := s.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("login ing 应视为 pending，实际 %v", err)
	}
}

// 200 但没有 token：继续等（既不是失败也不是成功）。
func TestPollPendingOnEmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/state" {
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-1","authUrl":"https://www.codebuddy.cn/login"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":""}}`)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()
	if _, err := s.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("空 token 应视为 pending，实际 %v", err)
	}
}

// 授权成功：token 轮换 + 账号信息（uid/昵称/企业）补齐。
func TestPollSuccessFillsCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-9","authUrl":"https://www.codebuddy.cn/login"}}`)
		case "/v2/plugin/auth/token":
			if got := r.URL.Query().Get("state"); got != "st-9" {
				t.Errorf("轮询 state 不对: %q", got)
			}
			if got := r.Header.Get("User-Agent"); got != clientUA {
				t.Errorf("轮询应带 CodeBuddy 身份 UA，实际 %q", got)
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":7200,"domain":"www.codebuddy.cn"}}`)
		case "/v2/plugin/login/account":
			if got := r.Header.Get("Authorization"); got != "Bearer at-new" {
				t.Errorf("账号接口应带 Bearer，实际 %q", got)
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"uid":"uid-5","enterpriseId":"ent-5","nickname":"demo-codebuddy"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()

	cred, err := s.Poll(context.Background())
	if err != nil {
		t.Fatalf("授权应成功: %v", err)
	}
	if cred.UID != "uid-5" || cred.Nickname != "demo-codebuddy" || cred.AccessToken != "at-new" || cred.RefreshToken != "rt-new" {
		t.Fatalf("凭证不对: %+v", cred)
	}
	if cred.Extra["enterprise_id"] != "ent-5" {
		t.Fatalf("企业 ID 应保留: %+v", cred.Extra)
	}
	if cred.Extra["domain"] != "www.codebuddy.cn" {
		t.Fatalf("domain 应保留: %+v", cred.Extra)
	}
	if cred.ExpiresAt.IsZero() {
		t.Fatal("expiresIn 应换算成过期时间")
	}
}

// 有 token 但拿不到 uid：必须报错（不能落一份没有主键的凭证）。
func TestPollWithoutAccountFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-1","authUrl":"https://www.codebuddy.cn/login"}}`)
		case "/v2/plugin/auth/token":
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"at"}}`)
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()

	_, err := s.Poll(context.Background())
	if err == nil {
		t.Fatal("拿不到 uid 必须报错")
	}
	if k, _ := errs.KindOf(err); k != errs.Parse {
		t.Fatalf("期望 Parse，实际 %s", k)
	}
}

// login/account 拿不到 uid 时，回落到令牌自带的 sub（参考实现以 sub 为权威 uid）。
func TestPollFallsBackToJWTSubForUID(t *testing.T) {
	tok := fakeJWT(t, map[string]any{"sub": "uid-sub-1", "exp": float64(time.Now().Add(time.Hour).Unix())})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-1","authUrl":"https://www.codebuddy.cn/login"}}`)
		case "/v2/plugin/auth/token":
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"`+tok+`","refreshToken":"rt-1","expiresIn":3600}}`)
		default:
			// login/account 挂了：不能再让整次授权失败。
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()

	cred, err := s.Poll(context.Background())
	if err != nil {
		t.Fatalf("login/account 失败时应回落到令牌 sub，实际报错: %v", err)
	}
	if cred.UID != "uid-sub-1" {
		t.Fatalf("uid 应取自 JWT 的 sub，实际 %q", cred.UID)
	}
}

// 取消后不再轮询上游。
func TestPollAfterCancel(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/state" {
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-1","authUrl":"https://www.codebuddy.cn/login"}}`)
			return
		}
		polls++
		w.WriteHeader(404)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	s.Cancel()

	if _, err := s.Poll(context.Background()); err == nil || err == channel.ErrPending {
		t.Fatalf("取消后应报「已取消」，实际 %v", err)
	}
	if polls != 0 {
		t.Fatalf("取消后不应再请求上游，实际 %d 次", polls)
	}
}

// 粘贴 auth/*.info 整段 JSON：有 refreshToken → 刷新即验证，换来新 access token。
func TestPasteInfoJSONThenRefresh(t *testing.T) {
	var refreshHdr http.Header
	var refreshCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-1","authUrl":"https://www.codebuddy.cn/login"}}`)
		case "/v2/plugin/auth/token/refresh":
			refreshCalls++
			refreshHdr = r.Header.Clone()
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"at-fresh","refreshToken":"rt-fresh","expiresIn":3600}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}
	defer s.Cancel()

	info := `{"auth":{"accessToken":"at-old-1234567890123456","refreshToken":"rt-old-1234567890123456","expiresAt":4102444800000},` +
		`"account":{"uid":"uid-paste","enterpriseId":"ent-paste","nickname":"粘来的号"}}`
	acc, ok := s.(channel.CallbackAcceptor)
	if !ok {
		t.Fatal("会话必须实现 channel.CallbackAcceptor")
	}
	if err := acc.AcceptCallback(info); err != nil {
		t.Fatalf("整段 .info JSON 应被接受: %v", err)
	}

	cred, err := s.Poll(context.Background())
	if err != nil {
		t.Fatalf("粘贴后的轮询应完成授权: %v", err)
	}
	if refreshCalls != 1 {
		t.Fatalf("有 refreshToken 时应刷新一次当验证，实际 %d 次", refreshCalls)
	}
	if got := refreshHdr.Get("X-Auth-Refresh-Source"); got != "plugin" {
		t.Fatalf("CodeBuddy 刷新来源应为 plugin，实际 %q", got)
	}
	if got := refreshHdr.Get("X-Refresh-Token"); got != "rt-old-1234567890123456" {
		t.Fatalf("应带粘贴进来的 refresh token，实际 %q", got)
	}
	if cred.UID != "uid-paste" || cred.AccessToken != "at-fresh" || cred.RefreshToken != "rt-fresh" {
		t.Fatalf("凭证不对: %+v", cred)
	}
	if cred.Extra["enterprise_id"] != "ent-paste" {
		t.Fatalf("企业 ID 应从 .info 的 account 带过来: %+v", cred.Extra)
	}
}

// 兜底只粘一个 accessToken（JWT）：本地从 sub 取 uid、从 exp 判过期，不发上游请求。
func TestPasteBareAccessToken(t *testing.T) {
	var refreshCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/state" {
			w.WriteHeader(503) // 主路不可用，纯靠粘贴
			return
		}
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			refreshCalls++
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	tok := fakeJWT(t, map[string]any{"sub": "uid-777", "exp": float64(time.Now().Add(time.Hour).Unix())})

	a := NewWithBase(srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()
	acc := s.(channel.CallbackAcceptor)
	if err := acc.AcceptCallback(tok); err != nil {
		t.Fatalf("裸 accessToken 应被接受: %v", err)
	}

	cred, err := s.Poll(context.Background())
	if err != nil {
		t.Fatalf("裸 accessToken 应完成授权: %v", err)
	}
	if cred.UID != "uid-777" || cred.AccessToken != tok {
		t.Fatalf("uid 应从 JWT sub 解出，实际 %+v", cred)
	}
	if cred.Extra["access_token_only"] != "1" {
		t.Fatalf("应标记只有 access token（无法自动续期）: %+v", cred.Extra)
	}
	if refreshCalls != 0 {
		t.Fatalf("只有 access token 时不应调刷新，实际 %d 次", refreshCalls)
	}
}

// 已过期的 accessToken（且无 refresh）必须拒绝，不能落一份过期凭证。
func TestPasteExpiredAccessTokenRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	tok := fakeJWT(t, map[string]any{"sub": "uid-x", "exp": float64(time.Now().Add(-time.Hour).Unix())})
	a := NewWithBase(srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()
	if err := s.(channel.CallbackAcceptor).AcceptCallback(tok); err != nil {
		t.Fatalf("接受不应失败: %v", err)
	}
	_, err := s.Poll(context.Background())
	if err == nil {
		t.Fatal("过期令牌必须报错")
	}
	if k, _ := errs.KindOf(err); k != errs.SessionDead {
		t.Fatalf("期望 SessionDead，实际 %s", k)
	}
}

// 解不出 uid 的不透明 accessToken：明确报错，绝不编造 UID。
func TestPasteOpaqueTokenWithoutUIDRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()
	if err := s.(channel.CallbackAcceptor).AcceptCallback("opaque-token-1234567890"); err != nil {
		t.Fatalf("接受不应失败: %v", err)
	}
	if _, err := s.Poll(context.Background()); err == nil {
		t.Fatal("解不出 uid 必须报错")
	}
}

// 明显不是凭据的内容要被挡掉。
func TestAcceptRejectsGarbage(t *testing.T) {
	a := NewWithBase("http://127.0.0.1:1", nil)
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()
	if err := s.(channel.CallbackAcceptor).AcceptCallback("这是一段中文，包含空格，不是令牌"); err == nil {
		t.Fatal("非凭据文本必须被拒绝")
	}
	if err := s.(channel.CallbackAcceptor).AcceptCallback("{}"); err == nil {
		t.Fatal("空 JSON 必须被拒绝")
	}
}

// extractCredential 各种粘贴形态。
func TestExtractCredentialVariants(t *testing.T) {
	access := "at-12345678901234567890"
	refresh := "rt-12345678901234567890"
	cases := []struct {
		name              string
		in                string
		wantAccess        string
		wantRefresh       string
		wantUID           string
		wantEnt           string
		wantExpiryNotZero bool
	}{
		{
			name: "info-nested",
			in: `{"auth":{"accessToken":"` + access + `","refreshToken":"` + refresh + `","expiresAt":4102444800000},` +
				`"account":{"uid":"u9","enterpriseId":"e9","nickname":"n"}}`,
			wantAccess: access, wantRefresh: refresh, wantUID: "u9", wantEnt: "e9", wantExpiryNotZero: true,
		},
		{
			name:       "data-wrapped",
			in:         `{"data":{"auth":{"accessToken":"` + access + `"},"account":{"uid":"u3"}}}`,
			wantAccess: access, wantUID: "u3",
		},
		{
			name:       "flat",
			in:         `{"access_token":"` + access + `","refresh_token":"` + refresh + `","uid":"u2"}`,
			wantAccess: access, wantRefresh: refresh, wantUID: "u2",
		},
		{name: "bearer-prefix", in: "Bearer " + access, wantAccess: access},
		{name: "key-equals", in: "accessToken=" + access, wantAccess: access},
		{name: "spaced-equals", in: "token = " + access, wantAccess: access},
		{name: "bare-quoted", in: `"` + access + `"`, wantAccess: access},
		{name: "garbage", in: "not a token at all", wantAccess: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pc := extractCredential(c.in)
			if c.wantAccess == "" {
				if pc != nil && (pc.access != "" || pc.refresh != "") {
					t.Fatalf("应解析不出令牌，实际 %+v", pc)
				}
				return
			}
			if pc == nil {
				t.Fatalf("应解析出凭据，实际 nil（输入 %q）", c.in)
			}
			if pc.access != c.wantAccess {
				t.Errorf("access 期望 %q，实际 %q", c.wantAccess, pc.access)
			}
			if c.wantRefresh != "" && pc.refresh != c.wantRefresh {
				t.Errorf("refresh 期望 %q，实际 %q", c.wantRefresh, pc.refresh)
			}
			if c.wantUID != "" && pc.uid != c.wantUID {
				t.Errorf("uid 期望 %q，实际 %q", c.wantUID, pc.uid)
			}
			if c.wantEnt != "" && pc.enterprise != c.wantEnt {
				t.Errorf("enterprise 期望 %q，实际 %q", c.wantEnt, pc.enterprise)
			}
			if c.wantExpiryNotZero && pc.expiresAt.IsZero() {
				t.Error("expiresAt 应被解析")
			}
		})
	}
}

// pending 判据：真正的 HTTP/解析失败不能被当成「还在登录中」。
func TestLoginPendingHeuristic(t *testing.T) {
	if !isLoginPending(errs.New(errs.Parse, "code=11217 msg=login ing...")) {
		t.Fatal("业务码错误应判为 pending")
	}
	if isLoginPending(errs.New(errs.Transport, "upstream http 502: bad gateway")) {
		t.Fatal("HTTP 失败不能判成 pending")
	}
	if isLoginPending(errs.New(errs.Parse, "parse failed: unexpected end")) {
		t.Fatal("解析失败不能判成 pending")
	}
}

// 适配器与授权会话的契约实现（编译期断言之外的显式检查）。
func TestImplementsContracts(t *testing.T) {
	var _ channel.Channel = New()
	var _ channel.Authorizer = New()
	s, _ := NewWithBase("http://127.0.0.1:1", nil).StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()
	var _ channel.LoginSession = s
	if _, ok := s.(channel.CallbackAcceptor); !ok {
		t.Fatal("会话应实现 CallbackAcceptor（粘贴兜底）")
	}
	if _, ok := s.(interface{ Hint() string }); !ok {
		t.Fatal("会话应实现 Hint（面板引导语）")
	}
}

// srvURL 取测试服务器的地址。
func srvURL(r *http.Request) string {
	return "http://" + r.Host
}

// fakeJWT 造一个不校验签名的 JWT（只用于验证本地解析 sub/exp）。
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + ".sig"
}
