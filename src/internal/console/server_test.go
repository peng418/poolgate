package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/registry"
	"poolgate/internal/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	registry.Reset()
	// 一家启用 + 一家暂停：验证暂停渠道可见但不下发。
	registry.RegisterSpec(channel.Spec{Kind: channel.QoderCN, DisplayName: "QoderCN", Status: channel.Active})
	registry.RegisterSpec(channel.Spec{Kind: "qwenwork", DisplayName: "千问办公", Status: channel.Paused})

	dir := t.TempDir()
	return New(store.NewAdminStore(dir), store.NewSessionStore(), Options{Version: "test"})
}

func post(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(http.MethodPost, path, nil)
	} else {
		raw, _ := json.Marshal(body)
		r = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// M0 验收主线：bootstrap 报未初始化 → 设置密码 → 拿到会话 → 渠道清单可见。

func TestBootstrapNotInitialized(t *testing.T) {
	srv := newTestServer(t)
	w := get(t, srv.Routes(), "/api/bootstrap")
	if w.Code != http.StatusOK {
		t.Fatalf("bootstrap 应可用，实际 %d", w.Code)
	}
	var got bootstrapResp
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Initialized {
		t.Fatal("未设置密码时 Initialized 应为 false")
	}
	if got.Version != "test" {
		t.Fatalf("版本未透出: %q", got.Version)
	}
}

func TestSetupRequiresConfirmation(t *testing.T) {
	srv := newTestServer(t)
	// 两次不一致必须明确指出原因，而不是笼统「请重试」。
	w := post(t, srv.Routes(), "/api/setup", setupReq{Password: "abc123456789", Confirm: "abc12345678"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("不一致应被拒，实际 %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("不一致")) {
		t.Fatalf("应说明哪一条不满足，实际 %s", w.Body.String())
	}
}

// 密码强度只提示、不拦截：这是自用单管理员系统，锁的是自己。
func TestSetupAcceptsWeakPassword(t *testing.T) {
	srv := newTestServer(t)
	w := post(t, srv.Routes(), "/api/setup", setupReq{Password: "123", Confirm: "123"})
	if w.Code != http.StatusOK {
		t.Fatalf("弱密码应被接受（仅提示），实际 %d %s", w.Code, w.Body.String())
	}
	if !srv.admin.Exists() {
		t.Fatal("密码应已设置")
	}
	ok, err := srv.admin.Verify("123")
	if err != nil || !ok {
		t.Fatalf("设置的弱密码应可登录，ok=%v err=%v", ok, err)
	}
}

func TestSetupThenChannelsVisible(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Routes()

	w := post(t, h, "/api/setup", setupReq{Password: "PooLGate-2026!", Confirm: "PooLGate-2026!"})
	if w.Code != http.StatusOK {
		t.Fatalf("设置应成功，实际 %d body=%s", w.Code, w.Body.String())
	}
	var sr sessionResp
	json.Unmarshal(w.Body.Bytes(), &sr)
	if !sr.OK || sr.Token == "" {
		t.Fatalf("设置后应直接签发会话: %+v", sr)
	}

	cookie := w.Header().Get("Set-Cookie")
	if !bytes.Contains([]byte(cookie), []byte("HttpOnly")) {
		t.Fatalf("会话 Cookie 应 HttpOnly，实际 %q", cookie)
	}

	// 会话可用 → 渠道清单可见。
	r := httptest.NewRequest(http.MethodGet, "/api/channels", nil)
	r.Header.Set("Cookie", "pg_session="+sr.Token)
	cw := httptest.NewRecorder()
	h.ServeHTTP(cw, r)
	if cw.Code != http.StatusOK {
		t.Fatalf("已登录应可读渠道，实际 %d", cw.Code)
	}
	var out struct {
		Channels []struct {
			Kind       string `json:"kind"`
			Downstream bool   `json:"downstream"`
		} `json:"channels"`
	}
	json.Unmarshal(cw.Body.Bytes(), &out)
	if len(out.Channels) != 2 {
		t.Fatalf("暂停渠道也应可见（写明恢复条件），实际 %d 个", len(out.Channels))
	}
	for _, c := range out.Channels {
		if c.Kind == "qwenwork" && c.Downstream {
			t.Fatal("暂停渠道不得下发模型")
		}
		if c.Kind == "qodercn" && !c.Downstream {
			t.Fatal("启用渠道应下发模型")
		}
	}
}

func TestSetupClosedAfterInitialized(t *testing.T) {
	// 设置接口只能成功一次，之后永久关闭。
	srv := newTestServer(t)
	h := srv.Routes()
	post(t, h, "/api/setup", setupReq{Password: "PooLGate-2026!", Confirm: "PooLGate-2026!"})

	w := post(t, h, "/api/setup", setupReq{Password: "Another-2026!", Confirm: "Another-2026!"})
	if w.Code != http.StatusConflict {
		t.Fatalf("已初始化后设置接口应关闭，实际 %d", w.Code)
	}
}

func TestChannelsRequiresAuth(t *testing.T) {
	srv := newTestServer(t)
	w := get(t, srv.Routes(), "/api/channels")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w.Code)
	}
	// 401 也要带 kind —— 客户端不该自己去猜。
	if !bytes.Contains(w.Body.Bytes(), []byte("AuthFailed")) {
		t.Fatalf("错误响应应带 kind，实际 %s", w.Body.String())
	}
}

func TestLoginFailureCounters(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Routes()
	post(t, h, "/api/setup", setupReq{Password: "PooLGate-2026!", Confirm: "PooLGate-2026!"})

	// 第一次失败要说还剩几次，不能只说「密码错误」。
	w := post(t, h, "/api/session", loginReq{Password: "wrong"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错误密码应 401，实际 %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("还可尝试")) {
		t.Fatalf("应提示剩余次数，实际 %s", w.Body.String())
	}
}

func TestLoginLocksAfterMaxFailures(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Routes()
	post(t, h, "/api/setup", setupReq{Password: "PooLGate-2026!", Confirm: "PooLGate-2026!"})

	var last int
	for i := 0; i < 6; i++ {
		w := post(t, h, "/api/session", loginReq{Password: "wrong"})
		last = w.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("超过上限应锁定(429)，实际 %d", last)
	}
}

func TestLoginSuccessClearsFailures(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Routes()
	post(t, h, "/api/setup", setupReq{Password: "PooLGate-2026!", Confirm: "PooLGate-2026!"})

	for i := 0; i < 3; i++ {
		post(t, h, "/api/session", loginReq{Password: "wrong"})
	}
	w := post(t, h, "/api/session", loginReq{Password: "PooLGate-2026!"})
	if w.Code != http.StatusOK {
		t.Fatalf("正确密码应登录成功，实际 %d body=%s", w.Code, w.Body.String())
	}
	// 失败计数清零，再错一次应回到「还可尝试 4 次」而不是立刻锁定。
	w2 := post(t, h, "/api/session", loginReq{Password: "wrong"})
	if bytes.Contains(w2.Body.Bytes(), []byte("锁定")) {
		t.Fatal("登录成功后失败计数应清零")
	}
}

func TestSessionLogout(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Routes()
	w := post(t, h, "/api/setup", setupReq{Password: "PooLGate-2026!", Confirm: "PooLGate-2026!"})
	var sr sessionResp
	json.Unmarshal(w.Body.Bytes(), &sr)

	// 登出
	r := httptest.NewRequest(http.MethodDelete, "/api/session", nil)
	r.Header.Set("Cookie", "pg_session="+sr.Token)
	h.ServeHTTP(httptest.NewRecorder(), r)

	g := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	g.Header.Set("Cookie", "pg_session="+sr.Token)
	gw := httptest.NewRecorder()
	h.ServeHTTP(gw, g)
	if gw.Code != http.StatusUnauthorized {
		t.Fatalf("登出后会话应失效，实际 %d", gw.Code)
	}
}

// 强度结论用于前端提示文案，必须给出可读的中文结论。
func TestPasswordStrength(t *testing.T) {
	cases := []struct {
		pw      string
		wantMin int
	}{
		{"abc", 0},
		{"abcdefgh", 1},
		{"Abcdefghijkl", 2},
		{"PooLGate-2026!", 3},
	}
	for _, c := range cases {
		n, note := passwordStrength(c.pw)
		if n < c.wantMin {
			t.Errorf("%q: got %d want >= %d", c.pw, n, c.wantMin)
		}
		if note == "" {
			t.Errorf("%q: 应给出文字结论", c.pw)
		}
	}
}
