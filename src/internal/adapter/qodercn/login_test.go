package qodercn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 发起授权：给用户的 URL 必须带齐设备流四个参数，且每次都是新的 PKCE。
func TestStartLoginBuildsDeviceURL(t *testing.T) {
	a := New()
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}
	url := s.AuthURL()
	for _, want := range []string{"/device/selectAccounts", "nonce=", "challenge=", "challenge_method=S256", "client_id=" + OAuthClientID} {
		if !strings.Contains(url, want) {
			t.Fatalf("授权 URL 缺少 %q：%s", want, url)
		}
	}
	s2, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	if s2.AuthURL() == url {
		t.Fatal("每次授权都应生成新的 nonce/challenge（复用会串号）")
	}
}

// 轮询：404/202 是「还没授权」，不是错误。
func TestPollTreatsStatusAsPending(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusAccepted} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		a := NewWithBase(srv.URL, srv.URL, srv.Client())
		s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
		_, err := s.Poll(context.Background())
		srv.Close()
		if !errors.Is(err, channel.ErrPending) {
			t.Fatalf("HTTP %d 应视为 pending，实际 %v", code, err)
		}
	}
}

// 授权成功：拿到 dt-/drt-、补齐机器指纹（否则后续 COSY 签名过不了）、过期时间换算正确。
func TestPollSuccessFillsCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, EpDTPoll):
			// 轮询参数必须带 nonce/verifier/challenge_method
			if r.URL.Query().Get("nonce") == "" || r.URL.Query().Get("verifier") == "" ||
				r.URL.Query().Get("challenge_method") != "S256" {
				t.Errorf("轮询参数不全: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"token":"dt-new","refresh_token":"drt-new","user_id":"u-1","expires_in":2592000}`))
		case r.URL.Path == EpUserInfo:
			if r.Header.Get("Authorization") != "Bearer dt-new" {
				t.Errorf("userinfo 应带新 token: %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"name":"阿绮","userType":"personal_pro"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	cred, err := s.Poll(context.Background())
	if err != nil {
		t.Fatalf("授权应成功: %v", err)
	}
	if cred.UID != "u-1" || cred.AccessToken != "dt-new" || cred.RefreshToken != "drt-new" {
		t.Fatalf("凭证字段不对: %+v", cred)
	}
	if cred.Nickname != "阿绮" {
		t.Fatalf("昵称应由 userinfo 补齐，实际 %q", cred.Nickname)
	}
	for _, k := range []string{"machine_id", "machine_token", "machine_type"} {
		if cred.Extra[k] == "" {
			t.Fatalf("机器指纹 %s 必须补齐（COSY 签名依赖）", k)
		}
	}
	if cred.ExpiresAt.IsZero() {
		t.Fatal("expires_in=2592000 应换算成过期时间")
	}
	if ut := a.userTypeOf(cred); ut != "personal_pro" {
		t.Fatalf("userType 应缓存供推理签名用，实际 %q", ut)
	}
}

// 200 但没有 token：还在处理，继续等（不是失败，也不能当成功）。
func TestPoll200WithoutTokenIsPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"processing"}`))
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	if _, err := s.Poll(context.Background()); !errors.Is(err, channel.ErrPending) {
		t.Fatalf("无 token 应视为 pending，实际 %v", err)
	}
}

// 有 token 但没有 user_id：不能落一份没主键的凭证，必须报错。
func TestPollWithoutUIDFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token":"dt","refresh_token":"drt"}`))
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	_, err := s.Poll(context.Background())
	if err == nil {
		t.Fatal("缺 user_id 必须报错，不能当成功")
	}
	if k, _ := errs.KindOf(err); k != errs.Parse {
		t.Fatalf("期望 Parse，实际 %s", k)
	}
}

// 上游拒绝（如授权被撤销）：错误必须带状态分类与上游原话。
func TestPollUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	_, err := s.Poll(context.Background())
	if err == nil {
		t.Fatal("403 必须报错")
	}
	ee, ok := err.(*errs.Error)
	if !ok {
		t.Fatalf("必须是结构化错误，实际 %T", err)
	}
	if ee.Upstream == "" {
		t.Fatal("必须带上游原话，便于定位")
	}
}

// 取消后不再轮询（用户点了取消，就不该继续问上游）。
func TestPollAfterCancel(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(404)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	s.Cancel()
	if _, err := s.Poll(context.Background()); err == nil || errors.Is(err, channel.ErrPending) {
		t.Fatalf("取消后应报「已取消」，实际 %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("取消后不应再请求上游，实际请求 %d 次", n)
	}
}

// 适配器必须实现 channel.Authorizer —— 控制台靠这个接口发现「该渠道能面板授权」。
func TestAdapterImplementsAuthorizer(t *testing.T) {
	var _ channel.Authorizer = New()
}

// 授权链接的 JSON 结构（前端要展示）——确认 URL 里没有多余参数，
// 尤其不能带 redirect_uri/machine_id（CN 设备流不需要，带上反而可能被拒）。
func TestAuthorizeURLShape(t *testing.T) {
	url := AuthorizeURL("n1", "c1")
	var q map[string]any
	_ = json.Unmarshal([]byte(`{}`), &q)
	if strings.Contains(url, "redirect_uri") || strings.Contains(url, "machine_id") {
		t.Fatalf("设备流 URL 不应带 redirect_uri/machine_id：%s", url)
	}
	if !strings.HasPrefix(url, OAuthWebsite+"/device/selectAccounts?") {
		t.Fatalf("授权页地址不对：%s", url)
	}
}
