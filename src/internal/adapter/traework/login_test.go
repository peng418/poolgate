package traework

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// callbackURLOf 从授权 URL 里取出本机回调地址（测试要拿它去模拟浏览器回调）。
func callbackURLOf(t *testing.T, authURL string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("授权 URL 无法解析: %v", err)
	}
	cb := u.Query().Get("auth_callback_url")
	if cb == "" {
		t.Fatal("授权 URL 必须带 auth_callback_url，否则浏览器回不来")
	}
	if !strings.HasPrefix(cb, "http://127.0.0.1:") {
		t.Fatalf("回调必须只监听本机，实际 %s", cb)
	}
	return cb
}

// 发起授权：URL 参数齐全、回调服务真的起着。
func TestStartLoginStartsLocalCallback(t *testing.T) {
	a := New()
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}
	defer s.Cancel()

	u, _ := url.Parse(s.AuthURL())
	if !strings.HasPrefix(s.AuthURL(), ConsoleHost+"/authorization?") {
		t.Fatalf("授权页地址不对: %s", s.AuthURL())
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id":             ClientID,
		"code_challenge_method": "S256",
		"auth_type":             "local",
	} {
		if q.Get(k) != want {
			t.Fatalf("授权 URL 参数 %s 期望 %q，实际 %q", k, want, q.Get(k))
		}
	}
	for _, k := range []string{"machine_id", "device_id", "code_challenge", "auth_callback_url", "login_trace_id"} {
		if q.Get(k) == "" {
			t.Fatalf("授权 URL 缺少参数 %s", k)
		}
	}

	// 回调服务确实在监听（浏览器要能打进来）。
	cb := callbackURLOf(t, s.AuthURL())
	resp, err := http.Get(cb + "?refreshToken=rt-x&host=https://example.invalid")
	if err != nil {
		t.Fatalf("回调服务未监听: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "授权成功") {
		t.Fatalf("回调页应报成功，实际 %d %s", resp.StatusCode, body)
	}
}

// 浏览器还没回调：Poll 必须是 pending，不是错误也不是成功。
func TestPollPendingBeforeCallback(t *testing.T) {
	a := New()
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()

	if _, err := s.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("未回调时应返回 ErrPending，实际 %v", err)
	}
}

// 标准路径：回调带 refreshToken → 换 access token → 取 uid/昵称 → 落成凭证。
func TestCallbackRefreshTokenPath(t *testing.T) {
	var sawExchange, sawUserInfo bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpExchange:
			sawExchange = true
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"RefreshToken":"rt-from-browser"`) {
				t.Errorf("兑换请求应带回调里的 refreshToken: %s", body)
			}
			_, _ = io.WriteString(w, `{"Result":{"Token":"jwt-1","RefreshToken":"rt-2","TokenExpireAt":1790241734}}`)
		case EpUserInfo:
			sawUserInfo = true
			if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT jwt-1" {
				t.Errorf("GetUserInfo 应带新 token，实际 %q", got)
			}
			_, _ = io.WriteString(w, `{"Result":{"UserID":"u-77","ScreenName":"阿绮","EnterpriseID":"ent-1"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.URL, srv.Client())
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}
	defer s.Cancel()

	cb := callbackURLOf(t, s.AuthURL())
	resp, err := http.Get(cb + "?refreshToken=rt-from-browser&host=" + url.QueryEscape(srv.URL))
	if err != nil {
		t.Fatalf("模拟回调失败: %v", err)
	}
	resp.Body.Close()

	cred, err := s.Poll(context.Background())
	if err != nil {
		t.Fatalf("回调后应能拿到凭证: %v", err)
	}
	if !sawExchange || !sawUserInfo {
		t.Fatalf("兑换与取账号信息都必须发生（exchange=%v userinfo=%v）", sawExchange, sawUserInfo)
	}
	if cred.UID != "u-77" || cred.Nickname != "阿绮" {
		t.Fatalf("账号信息不对: %+v", cred)
	}
	if cred.AccessToken != "jwt-1" || cred.RefreshToken != "rt-2" {
		t.Fatalf("token 未按兑换结果更新: %+v", cred)
	}
	if cred.Extra["machine_id"] == "" || cred.Extra["device_id"] == "" || cred.Extra["api_host"] == "" {
		t.Fatalf("凭证必须带上机器指纹与 api_host（后续请求要它们）：%+v", cred.Extra)
	}
	if cred.ExpiresAt.IsZero() {
		t.Fatal("TokenExpireAt 应换算成过期时间")
	}
}

// PKCE 新流程：回调只给 authCode → 用 verifier + 设备公钥兑换。
func TestCallbackAuthCodePath(t *testing.T) {
	var gotVerifier, gotPubKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpAuthCodeExch:
			raw, _ := io.ReadAll(r.Body)
			body := string(raw)
			if strings.Contains(body, `"CodeVerifier":"`) && !strings.Contains(body, `"CodeVerifier":""`) {
				gotVerifier = true
			}
			if strings.Contains(body, "BEGIN PUBLIC KEY") {
				gotPubKey = true
			}
			_, _ = io.WriteString(w, `{"Result":{"AccessToken":"jwt-ac","RefreshToken":"rt-ac"}}`)
		case EpUserInfo:
			_, _ = io.WriteString(w, `{"Result":{"UserID":"u-ac","ScreenName":"AC 用户"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.URL, srv.Client())
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()

	cb := callbackURLOf(t, s.AuthURL())
	// authCodeInfo 是 URL 编码的 JSON（上游形态之一）。
	info := url.QueryEscape(`{"AuthCode":"ac-123"}`)
	resp, _ := http.Get(cb + "?authCodeInfo=" + info + "&host=" + url.QueryEscape(srv.URL))
	if resp != nil {
		resp.Body.Close()
	}

	cred, err := s.Poll(context.Background())
	if err != nil {
		t.Fatalf("authCode 兑换应成功: %v", err)
	}
	if !gotVerifier || !gotPubKey {
		t.Fatalf("PKCE 兑换必须带 codeVerifier 与设备公钥（verifier=%v pubkey=%v）", gotVerifier, gotPubKey)
	}
	if cred.AccessToken != "jwt-ac" || cred.UID != "u-ac" {
		t.Fatalf("凭证不对: %+v", cred)
	}
}

// 回调没带任何凭证：页面必须说明原因，Poll 必须报错（不能当成功）。
func TestCallbackWithoutCredentialFails(t *testing.T) {
	a := New()
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer s.Cancel()

	cb := callbackURLOf(t, s.AuthURL())
	resp, err := http.Get(cb + "?somethingElse=1")
	if err != nil {
		t.Fatalf("回调失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "授权未完成") || !strings.Contains(string(body), "没有携带任何凭证") {
		t.Fatalf("回调页应说明缺什么，实际 %s", body)
	}

	_, err = s.Poll(context.Background())
	if err == nil {
		t.Fatal("没有凭证必须报错")
	}
	if k, _ := errs.KindOf(err); k != errs.AuthFailed {
		t.Fatalf("期望 AuthFailed，实际 %s", k)
	}
}

// 取消：本机回调端口必须立刻释放（不能留个监听挂着）。
func TestCancelReleasesCallbackPort(t *testing.T) {
	a := New()
	s, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	cb := callbackURLOf(t, s.AuthURL())
	host := strings.TrimPrefix(cb, "http://")

	s.Cancel()
	s.Cancel() // 幂等

	if _, err := net.DialTimeout("tcp", host, 200*1000*1000); err == nil {
		t.Fatal("取消后回调端口应已释放")
	}
	if _, err := s.Poll(context.Background()); err == nil {
		t.Fatal("取消后 Poll 应报错")
	}
}

// 回调凭证解析的优先级：refreshToken > userJwt.RefreshToken > userJwt.Token > authCode。
func TestParseCallbackPrecedence(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want callbackInfo
	}{
		{"refreshToken", "http://127.0.0.1:1/authorize?refreshToken=rt", callbackInfo{RefreshToken: "rt"}},
		{"userJwt refresh", "http://127.0.0.1:1/authorize?userJwt=%7B%22RefreshToken%22%3A%22rt2%22%7D", callbackInfo{RefreshToken: "rt2"}},
		{"userJwt token", "http://127.0.0.1:1/authorize?userJwt=%7B%22Token%22%3A%22at2%22%7D", callbackInfo{AccessToken: "at2"}},
		{"authCode json", "http://127.0.0.1:1/authorize?authCodeInfo=%7B%22AuthCode%22%3A%22ac2%22%7D", callbackInfo{AuthCode: "ac2"}},
		{"authCode raw", "http://127.0.0.1:1/authorize?authCodeInfo=ac3", callbackInfo{AuthCode: "ac3"}},
		{"nothing", "http://127.0.0.1:1/authorize", callbackInfo{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseCallback(c.raw)
			if got.RefreshToken != c.want.RefreshToken || got.AccessToken != c.want.AccessToken || got.AuthCode != c.want.AuthCode {
				t.Fatalf("解析结果 %+v，期望 %+v", got, c.want)
			}
		})
	}
}

// 适配器必须实现 channel.Authorizer（控制台据此发现面板授权能力）。
func TestTraeImplementsAuthorizer(t *testing.T) {
	var _ channel.Authorizer = New()
}

// 回调地址要能指向**面板所在机器**，而不是写死 127.0.0.1。
//
// 这是「远端浏览器永远完不成授权」的根治点：原来回调固定 127.0.0.1，只有浏览器与
// PoolGate 同机才收得到；TraeWork 的 auth_callback_url 由我们传（实测上游接受局域网地址），
// 所以手机扫码、别的电脑的浏览器都应该能把授权码送回来。
func TestStartLoginUsesPanelCallbackHost(t *testing.T) {
	a := New()
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{CallbackBase: "http://192.0.2.10:5014"})
	if err != nil {
		t.Fatalf("发起授权失败：%v", err)
	}
	defer s.Cancel()
	raw := s.AuthURL()
	if !strings.Contains(raw, "192.0.2.10") {
		t.Fatalf("回调地址应指向面板主机，实际 %s", raw)
	}
	if strings.Contains(raw, "127.0.0.1") {
		t.Fatalf("给了面板地址就不该再用 127.0.0.1，实际 %s", raw)
	}
	if !strings.Contains(raw, "%2Fauthorize") && !strings.Contains(raw, "/authorize") {
		t.Fatalf("回调路径应为 /authorize，实际 %s", raw)
	}

	// 没给 CallbackBase（或给的是回环地址）→ 退回原行为，保持向后兼容。
	s2, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("发起授权失败：%v", err)
	}
	defer s2.Cancel()
	if !strings.Contains(s2.AuthURL(), "127.0.0.1") {
		t.Fatalf("未给面板地址时应退回 127.0.0.1，实际 %s", s2.AuthURL())
	}
}

// 手工回填：把浏览器的回调整串喂进来，Poll 应能据此拿到凭证。
func TestAcceptCallback(t *testing.T) {
	a := New()
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cancel()
	ss, ok := s.(*traeSession)
	if !ok {
		t.Fatal("session 类型不对")
	}
	// 还没回填 → pending
	if _, err := s.Poll(context.Background()); err == nil {
		t.Fatal("没回调时应返回 pending")
	}
	// 真实回调形态：authCodeInfo 是一段 JSON（内含 AuthCode），另有 host
	cb := "http://127.0.0.1:1234/authorize?authCodeInfo=" +
		url.QueryEscape(`{"AuthCode":"abc123"}`) + "&host=api.trae.cn"
	if err := ss.AcceptCallback(cb); err != nil {
		t.Fatalf("回填失败：%v", err)
	}
	if !ss.gotCB || ss.info.AuthCode != "abc123" {
		t.Fatalf("回填后应解析出授权码，实际 %+v", ss.info)
	}
	// 只贴一小段（没有 URL 形状）也应被解析
	s2 := &traeSession{a: a}
	if err := s2.AcceptCallback("refreshToken=rt-1"); err != nil {
		t.Fatalf("只贴一段 query 也应可解析：%v", err)
	}
	if !s2.gotCB || s2.info.RefreshToken != "rt-1" {
		t.Fatalf("解析结果不对：%+v", s2.info)
	}
	// 什么都没有 → 明确报错（不静默）
	s3 := &traeSession{a: a}
	if err := s3.AcceptCallback("https://example.com/nothing"); err == nil {
		t.Fatal("没有授权信息时应报错")
	}
	if err := s3.AcceptCallback("   "); err == nil {
		t.Fatal("空内容应报错")
	}
}
