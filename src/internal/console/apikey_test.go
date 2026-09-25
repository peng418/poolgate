package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/registry"
	"poolgate/internal/store"
)

// newKeyServer 起一个「已登录 + 有 Key 存储 + 有设置」的面板。
func newKeyServer(t *testing.T) (*Server, *store.APIKeyStore, string) {
	t.Helper()
	registry.Reset()
	dir := t.TempDir()
	admin := store.NewAdminStore(dir)
	if err := admin.Set("test-password-1234"); err != nil {
		t.Fatal(err)
	}
	keys := store.NewAPIKeyStore(dir)
	if err := keys.Ensure(); err != nil {
		t.Fatal(err)
	}
	srv := New(admin, store.NewSessionStore(), Options{
		Version:  "test",
		Keys:     keys,
		Settings: store.NewSettingsStore(dir),
		DataDir:  dir,
	})
	return srv, keys, loginToken(t, srv)
}

func authGet(t *testing.T, srv *Server, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		r.Header.Set("Cookie", "pg_session="+token)
	}
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	return w
}

func TestAPIKeyMasked(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"pg-short", "••••••"}, // 短到没法安全打码 → 整条打掉
		{"pg-0123456789abcdef0123456789abcdef0123456789abcdef", "pg-0…cdef"},
	}
	for _, c := range cases {
		if got := apiKeyMasked(c.in); got != c.want {
			t.Errorf("apiKeyMasked(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 默认展示态是掩码：页面加载不该顺带把明文带出来。
func TestAPIKeyGetReturnsMaskedOnly(t *testing.T) {
	srv, keys, token := newKeyServer(t)

	if w := authGet(t, srv, "", "/api/apikey"); w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录必须 401，实际 %d", w.Code)
	}

	w := authGet(t, srv, token, "/api/apikey")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/apikey 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	plain, _, _ := keys.Current()
	if bytes.Contains(w.Body.Bytes(), []byte(plain)) {
		t.Fatal("默认响应里不能出现 key 明文")
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["exists"] != true {
		t.Fatalf("exists 应为 true，实际 %v", got["exists"])
	}
	masked, _ := got["masked"].(string)
	if !strings.HasPrefix(masked, "pg-") || !strings.Contains(masked, "…") {
		t.Fatalf("掩码形态不对: %q", masked)
	}
	if got["path"] == "" {
		t.Fatal("应透出 key 存放路径（面板要提示它只存在服务器上）")
	}
}

// 显示明文要走显式的 POST，且回的就是那个能用的 key。
func TestAPIKeyRevealMatchesVerifier(t *testing.T) {
	srv, keys, token := newKeyServer(t)
	w := authPost(t, srv, token, "/api/apikey/reveal", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reveal 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !keys.Verify(got["key"]) {
		t.Fatalf("回显的 key 必须能用，实际 %q", got["key"])
	}
	// GET 出来的掩码必须能对上这条明文（否则掩码是假的）。
	w = authGet(t, srv, token, "/api/apikey")
	var meta map[string]any
	json.Unmarshal(w.Body.Bytes(), &meta)
	if meta["masked"] != apiKeyMasked(got["key"]) {
		t.Fatalf("掩码与明文不匹配: %v vs %q", meta["masked"], apiKeyMasked(got["key"]))
	}
}

// 轮换：新 key 立刻可用，旧 key 立刻失效，明文只在这一个响应里出现。
func TestAPIKeyRotateFlow(t *testing.T) {
	srv, keys, token := newKeyServer(t)
	old, _, _ := keys.Current()

	w := authPost(t, srv, token, "/api/apikey/rotate", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	newKey, _ := got["key"].(string)
	if newKey == "" || newKey == old {
		t.Fatalf("rotate 必须换出不同的 key，实际 %q", newKey)
	}
	if got["rotated"] != true {
		t.Fatal("应标明这是一次轮换（前端要提示客户端同步更新）")
	}
	if !keys.Verify(newKey) {
		t.Fatal("新 key 应立即可用")
	}
	if keys.Verify(old) {
		t.Fatal("旧 key 应立即失效")
	}
	// 再 GET 一次也不该带出明文。
	if w := authGet(t, srv, token, "/api/apikey"); bytes.Contains(w.Body.Bytes(), []byte(newKey)) {
		t.Fatal("轮换后 GET 仍不能出现明文")
	}
}

// 没装配 Key 存储时要明确报「未装配」，不是空 key。
func TestAPIKeyMissingStore(t *testing.T) {
	registry.Reset()
	dir := t.TempDir()
	admin := store.NewAdminStore(dir)
	if err := admin.Set("test-password-1234"); err != nil {
		t.Fatal(err)
	}
	srv := New(admin, store.NewSessionStore(), Options{Version: "test"})
	token := loginToken(t, srv)
	w := authGet(t, srv, token, "/api/apikey")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503，实际 %d", w.Code)
	}
}

// 对外基址探测：客户端要填的地址不该让用户自己拼。
func TestDetectClientBase(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		fwdProto string
		listen   string
		setPort  int
		want     string
	}{
		{"带端口访问", "192.0.2.10:15124", "", "0.0.0.0:5014", 5014, "http://192.0.2.10:5014/v1"},
		{"不带端口", "nas.local", "", "0.0.0.0:5014", 5014, "http://nas.local:5014/v1"},
		{"反代给了 https", "nas.local", "https", "0.0.0.0:5014", 5014, "https://nas.local:5014/v1"},
		// 实际监听绑了具体 IP 时以它为准：比可被反代改写的 Host 头可信。
		{"监听具体 IP 时以它为准", "gw.local:80", "", "192.0.2.10:5014", 5014, "http://192.0.2.10:5014/v1"},
		{"监听 :: 视为无信息", "nas.local:80", "", "[::]:5014", 5014, "http://nas.local:5014/v1"},
		// 端口必须取实际监听值：安装包的启动脚本显式给 -addr，设置里的端口不生效。
		{"端口取实际监听值", "nas.local:80", "", "0.0.0.0:6001", 5014, "http://nas.local:6001/v1"},
		{"IPv6 字面量加方括号", "[fd00::1]:80", "", "0.0.0.0:5014", 5014, "http://[fd00::1]:5014/v1"},
		// 实际监听未知（测试/非常规启动）时退回设置端口。
		{"监听未知时退回设置端口", "nas.local", "", "", 6002, "http://nas.local:6002/v1"},
	}
	for _, c := range cases {
		srv, _, _ := newKeyServer(t)
		srv.opts.ListenAddr = c.listen
		if err := srv.opts.Settings.Update(func(st *store.Settings) error {
			st.ListenPort = c.setPort
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
		r.Host = c.host
		if c.fwdProto != "" {
			r.Header.Set("X-Forwarded-Proto", c.fwdProto)
		}
		if got := srv.detectClientBase(r); got != c.want {
			t.Errorf("%s: detectClientBase = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// 设置接口要把探测到的基址一起给前端（面板据此显示可复制的地址）。
func TestSettingsExposesDetectedBase(t *testing.T) {
	srv, _, token := newKeyServer(t)
	srv.opts.ListenAddr = "0.0.0.0:5014"
	r := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	r.Host = "192.0.2.10:15124"
	r.Header.Set("Cookie", "pg_session="+token)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("settings 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Detected        map[string]string `json:"detected"`
		ListenEffective string            `json:"listen_effective"`
		ListenPinned    bool              `json:"listen_pinned"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Detected["base_url"] != "http://192.0.2.10:5014/v1" {
		t.Fatalf("detected.base_url 不对: %q", got.Detected["base_url"])
	}
	// 面板也要能看到「实际监听」，否则用户改了端口会以为生效（安装包下并不生效）。
	if got.ListenEffective != "0.0.0.0:5014" {
		t.Fatalf("listen_effective 应透出实际监听地址，实际 %q", got.ListenEffective)
	}
	// 这份测试里启动参数没被显式指定 → 设置里的监听地址是生效的，不该吓唬用户。
	if got.ListenPinned {
		t.Fatal("未显式指定 -addr 时 listen_pinned 应为 false（设置里的值生效）")
	}

	// 显式指定（安装包启动脚本就是这样）时必须置 true，面板要据此提示「以启动参数为准」。
	srv.opts.ListenPinned = true
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	r.Host = "192.0.2.10:15124"
	r.Header.Set("Cookie", "pg_session="+token)
	srv.Routes().ServeHTTP(w, r)
	var pinned struct {
		ListenPinned bool `json:"listen_pinned"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pinned); err != nil {
		t.Fatal(err)
	}
	if !pinned.ListenPinned {
		t.Fatal("启动参数指定了 -addr 时 listen_pinned 应为 true")
	}
}
