package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/pool"
	"poolgate/internal/registry"
	"poolgate/internal/store"
)

// fakeSession 是 channel.LoginSession 的测试实现：可控地返回 pending / 凭证 / 错误。
type fakeSession struct {
	url string

	mu      sync.Mutex
	polls   int
	pending int // 前 N 次 Poll 返回 pending
	cred    *channel.Credential
	err     error
	credit  bool // Cancel 是否被调用
}

func (f *fakeSession) AuthURL() string { return f.url }

func (f *fakeSession) Poll(context.Context) (*channel.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if f.polls <= f.pending {
		return nil, channel.ErrPending
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.cred, nil
}

func (f *fakeSession) Cancel() {
	f.mu.Lock()
	f.credit = true
	f.mu.Unlock()
}

func (f *fakeSession) cancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.credit
}

// fakeAuthChannel 同时实现 channel.Channel 与 channel.Authorizer（可插拔授权能力）。
type fakeAuthChannel struct {
	kind     channel.Kind
	sess     *fakeSession
	startErr error
	starts   int
	// sessOverride 非空时 StartLogin 返回它（用于测「实现了 CallbackAcceptor 的会话」）。
	sessOverride channel.LoginSession
	// bal / balErr 控制 Balance 的返回：授权成功后入池即刷余额这条链路要能测。
	bal    channel.Balance
	balErr error
	// balanceCalls 记录余额被问了几次（验证「加号顺手刷一次」而不是没刷）。
	balanceCalls int
}

func (f *fakeAuthChannel) Kind() channel.Kind { return f.kind }
func (f *fakeAuthChannel) Spec() channel.Spec {
	return channel.Spec{Kind: f.kind, DisplayName: string(f.kind), Status: channel.Active}
}
func (f *fakeAuthChannel) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	f.starts++
	if f.startErr != nil {
		return nil, f.startErr
	}
	if f.sessOverride != nil {
		return f.sessOverride, nil
	}
	return f.sess, nil
}
func (f *fakeAuthChannel) Login(context.Context) (*channel.Credential, error) { return nil, nil }
func (f *fakeAuthChannel) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}
func (f *fakeAuthChannel) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	return nil, nil
}
func (f *fakeAuthChannel) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	f.balanceCalls++
	return f.bal, f.balErr
}
func (f *fakeAuthChannel) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{}, nil
}
func (f *fakeAuthChannel) Chat(context.Context, *channel.Credential, channel.ChatRequest) (channel.Stream, error) {
	return nil, nil
}
func (f *fakeAuthChannel) Classify(int, []byte) errs.Kind { return errs.Parse }

// newLoginTestServer 起一个已登录的面板 + 一个可授权的假渠道 + 真实凭证存储与账号池。
func newLoginTestServer(t *testing.T, ch *fakeAuthChannel) (http.Handler, string, *store.CredsStore, *pool.Pool) {
	t.Helper()
	registry.Reset()
	if ch != nil {
		registry.Register(ch, ch.Spec())
	}
	registry.RegisterSpec(channel.Spec{Kind: "qwenwork", DisplayName: "千问办公", Status: channel.Paused})

	dir := t.TempDir()
	creds := store.NewCredsStore(dir)
	accPool := pool.New()
	srv := New(store.NewAdminStore(dir), store.NewSessionStore(), Options{Version: "test", Creds: creds, Pool: accPool})
	h := srv.Routes()

	w := post(t, h, "/api/setup", setupReq{Password: "PooLGate-2026!", Confirm: "PooLGate-2026!"})
	var sr sessionResp
	if err := json.Unmarshal(w.Body.Bytes(), &sr); err != nil || sr.Token == "" {
		t.Fatalf("设置密码失败: %s", w.Body.String())
	}
	return h, sr.Token, creds, accPool
}

func authed(t *testing.T, h http.Handler, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		raw, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Cookie", "pg_session="+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// 授权主流程：start 拿 URL → poll 先 pending → 再 poll 成功 → 凭证落盘 + 入池。
func TestLoginFlowHappyPath(t *testing.T) {
	sess := &fakeSession{
		url:     "https://qoder.com.cn/device/selectAccounts?nonce=abc",
		pending: 1,
		cred: &channel.Credential{
			UID: "uid-9", Nickname: "新账号", AccessToken: "dt-new", RefreshToken: "drt-new",
			Extra: map[string]string{"machine_id": "m-new"},
		},
	}
	ch := &fakeAuthChannel{kind: channel.QoderCN, sess: sess}
	h, token, creds, accPool := newLoginTestServer(t, ch)

	// start
	w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)
	if w.Code != http.StatusOK {
		t.Fatalf("start 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var sr loginStartResp
	json.Unmarshal(w.Body.Bytes(), &sr)
	if sr.AuthURL != sess.url {
		t.Fatalf("应把授权页地址交给前端，实际 %q", sr.AuthURL)
	}
	if sr.Expires == "" {
		t.Fatal("应告知授权有效期（前端要显示倒计时）")
	}

	// poll #1：还没授权完 → pending（不是错误）
	w = authed(t, h, http.MethodGet, "/api/login/poll", nil, token)
	if w.Code != http.StatusOK {
		t.Fatalf("pending 应是 200，实际 %d %s", w.Code, w.Body.String())
	}
	var pr loginPollResp
	json.Unmarshal(w.Body.Bytes(), &pr)
	if pr.Status != "pending" {
		t.Fatalf("期望 pending，实际 %q", pr.Status)
	}

	// poll #2：授权完成
	w = authed(t, h, http.MethodGet, "/api/login/poll", nil, token)
	if w.Code != http.StatusOK {
		t.Fatalf("成功应 200，实际 %d %s", w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &pr)
	if pr.Status != "ok" || pr.Account["uid"] != "uid-9" {
		t.Fatalf("期望 ok + uid，实际 %+v", pr)
	}

	// 凭证落盘且能读回（重启后账号还在）
	loaded, err := creds.Load(channel.QoderCN)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("授权成功必须落盘，实际 %v %d 个", err, len(loaded))
	}
	if loaded[0].AccessToken != "dt-new" || loaded[0].Extra["machine_id"] != "m-new" {
		t.Fatalf("落盘内容不对: %+v", loaded[0])
	}
	// 立即入池，无需重启
	if n := accPool.CountHealthy(channel.QoderCN); n != 1 {
		t.Fatalf("授权成功应立即入池，实际健康账号 %d", n)
	}
	if !sess.cancelled() {
		t.Fatal("成功后应释放会话资源（本机回调 server 等）")
	}

	// 授权结束后再 poll：明确告知「没有进行中的授权」，而不是无限 pending
	w = authed(t, h, http.MethodGet, "/api/login/poll", nil, token)
	if w.Code == http.StatusOK {
		t.Fatalf("没有进行中的授权时不该回 200 pending，实际 %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "没有进行中的渠道授权") {
		t.Fatalf("应说明原因，实际 %s", w.Body.String())
	}
}

// 同一时间只允许一次授权：第二次 start 必须被明确拒绝，而不是悄悄覆盖。
func TestLoginStartRejectsConcurrent(t *testing.T) {
	ch := &fakeAuthChannel{kind: channel.QoderCN, sess: &fakeSession{url: "https://example.invalid/a", pending: 99}}
	h, token, _, _ := newLoginTestServer(t, ch)

	if w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token); w.Code != http.StatusOK {
		t.Fatalf("首次 start 应成功，实际 %d", w.Code)
	}
	w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)
	if w.Code == http.StatusOK {
		t.Fatal("已有授权进行中时第二次 start 必须被拒")
	}
	if !strings.Contains(w.Body.String(), "已有渠道授权在进行中") {
		t.Fatalf("应说明冲突原因，实际 %s", w.Body.String())
	}
	if ch.starts != 1 {
		t.Fatalf("不应重复发起上游授权，实际发起 %d 次", ch.starts)
	}
}

// 不支持面板授权的渠道（未注册实现，或实现里没有 Authorizer）：明确说清楚，别给死按钮。
func TestLoginStartUnsupportedChannel(t *testing.T) {
	ch := &fakeAuthChannel{kind: channel.QoderCN, sess: &fakeSession{url: "u"}}
	h, token, _, _ := newLoginTestServer(t, ch)

	// 已注册渠道但没实现 Authorizer：用只注册声明的暂停渠道模拟。
	w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qwenwork"}, token)
	if w.Code == http.StatusOK {
		t.Fatal("不支持面板授权的渠道必须拒绝")
	}
	if !strings.Contains(w.Body.String(), "不支持面板授权") {
		t.Fatalf("应说明该渠道不支持，实际 %s", w.Body.String())
	}

	// 完全未知的渠道
	w = authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "nope"}, token)
	if w.Code != http.StatusNotFound {
		t.Fatalf("未知渠道应 404，实际 %d", w.Code)
	}
}

// 上游拒绝发起授权（如网络不通）：错误必须原样透出，不能吞成 pending。
func TestLoginStartUpstreamFailure(t *testing.T) {
	ch := &fakeAuthChannel{
		kind:     channel.QoderCN,
		sess:     &fakeSession{url: "u"},
		startErr: errs.New(errs.Transport, "连不上上游授权服务"),
	}
	h, token, _, _ := newLoginTestServer(t, ch)
	w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)
	if w.Code == http.StatusOK {
		t.Fatal("发起授权失败必须报错")
	}
	if !strings.Contains(w.Body.String(), "连不上上游授权服务") {
		t.Fatalf("应带上真实原因，实际 %s", w.Body.String())
	}
}

// 授权过程中上游报错：结束本次授权并把原因交给用户（红线一）。
func TestLoginPollPropagatesError(t *testing.T) {
	ch := &fakeAuthChannel{
		kind: channel.QoderCN,
		sess: &fakeSession{url: "u", err: errs.New(errs.AuthFailed, "用户拒绝了授权")},
	}
	h, token, creds, _ := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)

	w := authed(t, h, http.MethodGet, "/api/login/poll", nil, token)
	if w.Code == http.StatusOK {
		t.Fatalf("授权失败不能回 200，实际 %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "用户拒绝了授权") {
		t.Fatalf("应带上游原因，实际 %s", w.Body.String())
	}
	// 失败不落盘（不能留下半个账号）
	if files, _ := creds.Load(channel.QoderCN); len(files) != 0 {
		t.Fatalf("授权失败不应落盘，实际 %d 个", len(files))
	}
	// 失败后授权槽位应释放，允许重来
	if w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token); w.Code != http.StatusOK {
		t.Fatalf("失败后应能重新发起，实际 %d %s", w.Code, w.Body.String())
	}
}

// 超时：过了有效期必须明确报「超时」，并释放资源。
func TestLoginPollTimeout(t *testing.T) {
	sess := &fakeSession{url: "u", pending: 99}
	ch := &fakeAuthChannel{kind: channel.QoderCN, sess: sess}
	registry.Reset()
	registry.Register(ch, ch.Spec())
	dir := t.TempDir()
	srv := New(store.NewAdminStore(dir), store.NewSessionStore(), Options{Creds: store.NewCredsStore(dir), Pool: pool.New()})
	// 可控时钟：发起授权后把时间拨过有效期，验证超时处置。
	clock := time.Now()
	srv.login.now = func() time.Time { return clock }
	h := srv.Routes()
	w := post(t, h, "/api/setup", setupReq{Password: "PooLGate-2026!", Confirm: "PooLGate-2026!"})
	var sr sessionResp
	json.Unmarshal(w.Body.Bytes(), &sr)

	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, sr.Token)
	clock = clock.Add(LoginTimeout + time.Minute) // 用户去泡了杯咖啡
	w = authed(t, h, http.MethodGet, "/api/login/poll", nil, sr.Token)
	if w.Code == http.StatusOK {
		t.Fatalf("超时必须报错，实际 %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "授权超时") {
		t.Fatalf("应明确说明超时，实际 %s", w.Body.String())
	}
	if !sess.cancelled() {
		t.Fatal("超时应释放授权会话资源")
	}
}

// 取消：释放资源，之后 poll 明确报「没有进行中的授权」。
func TestLoginCancel(t *testing.T) {
	sess := &fakeSession{url: "u", pending: 99}
	ch := &fakeAuthChannel{kind: channel.QoderCN, sess: sess}
	h, token, _, _ := newLoginTestServer(t, ch)

	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)
	w := authed(t, h, http.MethodPost, "/api/login/cancel", nil, token)
	if w.Code != http.StatusOK {
		t.Fatalf("取消应 200，实际 %d %s", w.Code, w.Body.String())
	}
	if !sess.cancelled() {
		t.Fatal("取消必须释放授权会话资源")
	}
	w = authed(t, h, http.MethodGet, "/api/login/poll", nil, token)
	if w.Code == http.StatusOK {
		t.Fatalf("取消后 poll 不该继续 pending，实际 %s", w.Body.String())
	}

	// 取消本身没有进行中的授权时：说清楚，不报 500
	w = authed(t, h, http.MethodPost, "/api/login/cancel", nil, token)
	if w.Code == http.StatusInternalServerError {
		t.Fatalf("重复取消不该 500，实际 %s", w.Body.String())
	}
}

// 授权接口必须要求登录（渠道授权 ≠ 管理员登录，但都要先登录）。
func TestLoginRequiresSession(t *testing.T) {
	ch := &fakeAuthChannel{kind: channel.QoderCN, sess: &fakeSession{url: "u"}}
	h, _, _, _ := newLoginTestServer(t, ch)

	r := httptest.NewRequest(http.MethodPost, "/api/login/start", strings.NewReader(`{"channel":"qodercn"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w.Code)
	}
	if ch.starts != 0 {
		t.Fatal("未登录不得发起上游授权")
	}
}

// 上游返回的 uid 为空：拒绝落盘并报错（否则会写出一份不可用的凭证）。
func TestLoginRejectsCredentialWithoutUID(t *testing.T) {
	ch := &fakeAuthChannel{
		kind: channel.QoderCN,
		sess: &fakeSession{url: "u", cred: &channel.Credential{AccessToken: "dt"}},
	}
	h, token, creds, accPool := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)

	w := authed(t, h, http.MethodGet, "/api/login/poll", nil, token)
	if w.Code == http.StatusOK {
		t.Fatalf("缺 uid 必须报错，实际 %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "uid") {
		t.Fatalf("应说明缺 uid，实际 %s", w.Body.String())
	}
	if files, _ := creds.Load(channel.QoderCN); len(files) != 0 {
		t.Fatalf("不应落盘不完整凭证，实际 %d 个", len(files))
	}
	if n := accPool.CountHealthy(channel.QoderCN); n != 0 {
		t.Fatalf("不应把不完整凭证入池，实际 %d", n)
	}
}

// 授权成功后必须顺手把余额问出来：否则用户看到的是「刚加完号，余额一片未知」，
// 以为没接上，其实只是没人去问过上游（F1.7）。
func TestLoginPollRefreshesBalance(t *testing.T) {
	exp := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	ch := &fakeAuthChannel{
		kind: channel.QoderCN,
		sess: &fakeSession{url: "u", cred: &channel.Credential{UID: "uid-7", Nickname: "n", AccessToken: "dt"}},
		bal:  channel.Balance{Credits: 120, Known: true, ExpiresAt: exp},
	}
	h, token, _, accPool := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)

	w := authed(t, h, http.MethodGet, "/api/login/poll", nil, token)
	if w.Code != http.StatusOK {
		t.Fatalf("poll 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var pr loginPollResp
	if err := json.Unmarshal(w.Body.Bytes(), &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Status != "ok" {
		t.Fatalf("期望 ok，实际 %q", pr.Status)
	}
	if pr.Balance == nil {
		t.Fatal("授权成功应带回余额（前端授权完成页要显示它）")
	}
	// 走的是真实 JSON 编解码，数字回来是 float64。
	if got, _ := pr.Balance["credits"].(float64); got != 120 {
		t.Fatalf("余额应为 120，实际 %v", pr.Balance["credits"])
	}
	if pr.Balance["known"] != true {
		t.Fatal("known 应为 true")
	}
	if pr.BalanceError != "" {
		t.Fatalf("成功时不该带失败原因，实际 %q", pr.BalanceError)
	}
	// 池子里也要真的写进去 —— 前端刷新账号页读的是池子，不是这个响应。
	st, ok := accPool.Get(channel.QoderCN, "uid-7")
	if !ok {
		t.Fatal("账号应在池中")
	}
	if !st.CreditsKnown || st.Credits != 120 {
		t.Fatalf("池中余额未刷新: known=%v credits=%d", st.CreditsKnown, st.Credits)
	}
	if !st.ExpiresAt.Equal(exp) {
		t.Fatalf("到期时间未刷新: %v", st.ExpiresAt)
	}
	if ch.balanceCalls != 1 {
		t.Fatalf("应恰好问一次余额，实际 %d 次", ch.balanceCalls)
	}
}

// 余额取不到不能把「授权成功」变成失败 —— 但必须说出原因（红线一）。
func TestLoginBalanceFailureKeepsAuthSuccess(t *testing.T) {
	ch := &fakeAuthChannel{
		kind:   channel.QoderCN,
		sess:   &fakeSession{url: "u", cred: &channel.Credential{UID: "uid-8", Nickname: "n", AccessToken: "dt"}},
		balErr: errs.New(errs.HardCredit, "上游 429：额度已用尽"),
	}
	h, token, creds, accPool := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)

	w := authed(t, h, http.MethodGet, "/api/login/poll", nil, token)
	if w.Code != http.StatusOK {
		t.Fatalf("授权已成功，余额失败不该 4xx/5xx，实际 %d %s", w.Code, w.Body.String())
	}
	var pr loginPollResp
	json.Unmarshal(w.Body.Bytes(), &pr)
	if pr.Status != "ok" {
		t.Fatalf("状态仍应是 ok，实际 %q", pr.Status)
	}
	if pr.Balance != nil {
		t.Fatal("没取到时不该编一个余额出来")
	}
	if !strings.Contains(pr.BalanceError, "429") {
		t.Fatalf("余额失败要带上上游原话，实际 %q", pr.BalanceError)
	}
	// 凭证照旧落盘入池，账号可用性不受余额影响。
	if files, _ := creds.Load(channel.QoderCN); len(files) != 1 {
		t.Fatalf("凭证仍应落盘，实际 %d 个", len(files))
	}
	if st, ok := accPool.Get(channel.QoderCN, "uid-8"); !ok || st.CreditsKnown {
		t.Fatalf("未取到余额时应保持「未知」，实际 %+v", st)
	}
}

// acceptSession 是实现了 channel.CallbackAcceptor 的会话（千问办公/TraeWork 那种
// 手工回填回调地址的能力）。
type acceptSession struct {
	url      string
	raw      string
	accepted int
	hint     string
}

func (a *acceptSession) AuthURL() string { return a.url }
func (a *acceptSession) Poll(context.Context) (*channel.Credential, error) {
	return nil, channel.ErrPending
}
func (a *acceptSession) Cancel() {}
func (a *acceptSession) AcceptCallback(raw string) error {
	a.accepted++
	a.raw = raw
	return nil
}
func (a *acceptSession) Hint() string { return a.hint }

// 手工回填回调地址：面板要把它转给当前这次授权，并且只对实现了该能力的渠道开放。
func TestLoginCallbackHandoff(t *testing.T) {
	sess := &acceptSession{url: "https://qwenwork.cn/oauth2/auth?x=1"}
	ch := &fakeAuthChannel{kind: channel.QoderCN, sess: &fakeSession{url: "u"}, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)

	// 还没发起授权就回填 → 明确报错（不是静默）
	w := authed(t, h, http.MethodPost, "/api/login/callback", map[string]string{"raw": "code=x"}, token)
	if w.Code == http.StatusOK {
		t.Fatal("没有进行中的授权时应报错")
	}
	if !strings.Contains(w.Body.String(), "没有进行中") {
		t.Fatalf("应说明原因，实际 %s", w.Body.String())
	}

	// 发起授权 → 回填 → 落到会话里
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)
	w = authed(t, h, http.MethodPost, "/api/login/callback",
		map[string]string{"raw": "http://127.0.0.1:1/callback?code=pasted"}, token)
	if w.Code != http.StatusOK {
		t.Fatalf("回填应成功，实际 %d %s", w.Code, w.Body.String())
	}
	if sess.accepted != 1 || sess.raw != "http://127.0.0.1:1/callback?code=pasted" {
		t.Fatalf("回填内容没落到会话：accepted=%d raw=%q", sess.accepted, sess.raw)
	}
}

// 不支持手工回填的渠道（没实现 CallbackAcceptor）→ 说清楚「去浏览器完成」，别让人白填。
func TestLoginCallbackRejectsUnsupportedChannel(t *testing.T) {
	ch := &fakeAuthChannel{kind: channel.QoderCN, sess: &fakeSession{url: "u"}}
	h, token, _, _ := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)

	w := authed(t, h, http.MethodPost, "/api/login/callback", map[string]string{"raw": "code=x"}, token)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("该渠道不该支持回填，应 400，实际 %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "不需要手工回填") {
		t.Fatalf("应说明该渠道不需要回填，实际 %s", w.Body.String())
	}
}

// 粘贴式渠道（实现 Hint() 的会话，如 DeepSeek 粘 userToken）：
// /api/login/start 的响应要带上 paste_hint，面板据此切换成粘贴形态。
func TestLoginStartExposesPasteHint(t *testing.T) {
	sess := &acceptSession{url: "https://chat.deepseek.com", hint: "把 userToken 整段粘过来"}
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &fakeSession{url: "u"}, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)

	w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "deepseek"}, token)
	if w.Code != http.StatusOK {
		t.Fatalf("start 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var sr loginStartResp
	json.Unmarshal(w.Body.Bytes(), &sr)
	if sr.PasteHint != sess.hint {
		t.Fatalf("应带出粘贴引导语，实际 %q", sr.PasteHint)
	}

	// 普通渠道（无 Hint 能力）不带这个字段，也不报错。
	sess2 := &fakeSession{url: "u", pending: 1}
	ch2 := &fakeAuthChannel{kind: channel.QoderCN, sess: sess2}
	h2, token2, _, _ := newLoginTestServer(t, ch2)
	w2 := authed(t, h2, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token2)
	var sr2 loginStartResp
	json.Unmarshal(w2.Body.Bytes(), &sr2)
	if sr2.PasteHint != "" {
		t.Fatalf("普通渠道不应带 paste_hint，实际 %q", sr2.PasteHint)
	}
}

// fakePwSession 是「账号密码直登」渠道的假会话（实现 channel.PasswordAcceptor）。
type fakePwSession struct {
	fakeSession
	fields []channel.LoginField
	got    map[string]string
	accErr error
}

func (f *fakePwSession) LoginFields() []channel.LoginField { return f.fields }

func (f *fakePwSession) AcceptPassword(v map[string]string) error {
	if f.accErr != nil {
		return f.accErr
	}
	f.got = v
	return nil
}

// TestLoginPasswordAcceptsCheckboxBool 是回归测试：
// 面板的「记住密码」是 checkbox，v-model 发的是布尔 true —— 服务端若把 values
// 收成 map[string]string，整个请求体会解析失败（实测症状：表单提交报「请求体无法解析」）。
// 这里锁住「布尔/数字/字符串都认」这条契约。
func TestLoginPasswordAcceptsCheckboxBool(t *testing.T) {
	sess := &fakePwSession{
		fakeSession: fakeSession{url: "u", pending: 99},
		fields: []channel.LoginField{
			{Name: "account", Label: "账号", Type: "text", Required: true},
			{Name: "password", Label: "密码", Type: "password", Required: true},
			{Name: "remember", Label: "记住密码", Type: "text"},
		},
	}
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &sess.fakeSession, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)

	// 先发起授权，才有「进行中的会话」可提交。
	if w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "deepseek"}, token); w.Code != http.StatusOK {
		t.Fatalf("start 应 200，实际 %d %s", w.Code, w.Body.String())
	}

	// 关键：remember 是布尔 true（前端 checkbox 的真实形态）。
	body := `{"values":{"account":"a@b.com","password":"pw","remember":true}}`
	r := httptest.NewRequest(http.MethodPost, "/api/login/password", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("布尔字段应被接受，实际 %d %s", w.Code, w.Body.String())
	}
	if sess.got["account"] != "a@b.com" || sess.got["password"] != "pw" {
		t.Fatalf("字段值传丢了：%+v", sess.got)
	}
	if sess.got["remember"] != "on" {
		t.Fatalf("布尔 true 应转成 \"on\"，实际 %q", sess.got["remember"])
	}

	// 数字与 false 也要能过（同一份宽容度）。
	body2 := `{"values":{"account":"a@b.com","password":"pw","remember":false,"extra":3}}`
	r2 := httptest.NewRequest(http.MethodPost, "/api/login/password", strings.NewReader(body2))
	r2.Header.Set("Authorization", "Bearer "+token)
	r2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("false/数字应被接受，实际 %d %s", w2.Code, w2.Body.String())
	}
	if sess.got["remember"] != "" {
		t.Fatalf("布尔 false 应为空串，实际 %q", sess.got["remember"])
	}
	if sess.got["extra"] != "3" {
		t.Fatalf("数字应转成 \"3\"，实际 %q", sess.got["extra"])
	}
}

// TestLoginPasswordUnsupportedChannel 校验非直登渠道被明确拒绝（不是静默成功）。
func TestLoginPasswordUnsupportedChannel(t *testing.T) {
	ch := &fakeAuthChannel{kind: channel.QoderCN, sess: &fakeSession{url: "u", pending: 99}}
	h, token, _, _ := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)

	body := `{"values":{"account":"a","password":"b"}}`
	r := httptest.NewRequest(http.MethodPost, "/api/login/password", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatal("不支持直登的渠道必须报错")
	}
	if !strings.Contains(w.Body.String(), "不支持账号密码直登") {
		t.Fatalf("应说明不支持，实际 %s", w.Body.String())
	}
}
