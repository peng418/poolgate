package workbuddy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 发起授权：拿 state + authUrl，并把跳转链解析成最终地址给用户。
func TestStartLoginResolvesAuthURL(t *testing.T) {
	var hops []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/plugin/auth/state":
			if r.Method != http.MethodPost {
				t.Errorf("auth/state 应为 POST，实际 %s", r.Method)
			}
			if got := r.URL.Query().Get("platform"); got != "CLI" {
				t.Errorf("platform 应为 CLI，实际 %q", got)
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-1","authUrl":"`+srvURL(r)+`/login"}}`)
		case r.URL.Path == "/login":
			hops = append(hops, "/login")
			w.Header().Set("Location", srvURL(r)+"/login/")
			w.WriteHeader(http.StatusMovedPermanently)
		case r.URL.Path == "/login/":
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

// 上游没给 state/authUrl：明确报错（不能给用户一个打不开的链接）。
func TestStartLoginRejectsIncompleteState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":""}}`)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	if _, err := a.StartLogin(context.Background(), channel.LoginOptions{}); err == nil {
		t.Fatal("缺 state/authUrl 必须报错")
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
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-1","authUrl":"https://www.codebuddy.cn/login"}}`)
		default:
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":""}}`)
		}
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
		switch {
		case r.URL.Path == "/v2/plugin/auth/state":
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-9","authUrl":"https://www.codebuddy.cn/login"}}`)
		case r.URL.Path == "/v2/plugin/auth/token":
			if got := r.URL.Query().Get("state"); got != "st-9" {
				t.Errorf("轮询 state 不对: %q", got)
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":7200,"domain":"www.codebuddy.cn"}}`)
		case r.URL.Path == "/v2/plugin/login/account":
			if got := r.Header.Get("Authorization"); got != "Bearer at-new" {
				t.Errorf("账号接口应带 Bearer，实际 %q", got)
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"uid":"uid-5","enterpriseId":"ent-5","nickname":"demo-workbuddy"}}`)
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
	if cred.UID != "uid-5" || cred.Nickname != "demo-workbuddy" || cred.AccessToken != "at-new" || cred.RefreshToken != "rt-new" {
		t.Fatalf("凭证不对: %+v", cred)
	}
	if cred.Extra["enterprise_id"] != "ent-5" {
		t.Fatalf("企业 ID 应保留: %+v", cred.Extra)
	}
	if cred.Extra["domain"] != "www.codebuddy.cn" {
		t.Fatalf("domain 应保留（刷新/区域判定要用）: %+v", cred.Extra)
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

// pending 判据：真正的 HTTP/解析失败不能被当成「还在登录中」。
func TestPendingHeuristic(t *testing.T) {
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

// 适配器必须实现 channel.Authorizer。
func TestWBImplementsAuthorizer(t *testing.T) {
	var _ channel.Authorizer = New()
}

// srvURL 取测试服务器的地址（handler 里拿不到 httptest.Server 时用 r.Host 拼）。
func srvURL(r *http.Request) string {
	return "http://" + r.Host
}

// 兜底断言：授权 URL 解析失败时不阻塞授权（拿不到重定向就用原始地址）。
func TestStartLoginFallsBackToRawURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/state" {
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"state":"st-1","authUrl":"http://127.0.0.1:1/nope"}}`)
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("发起授权不该因重定向解析失败而失败: %v", err)
	}
	defer s.Cancel()
	if !strings.Contains(s.AuthURL(), "127.0.0.1:1/nope") {
		t.Fatalf("解析失败时应回退原始地址，实际 %s", s.AuthURL())
	}
}
