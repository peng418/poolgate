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

// ── 多步 / 验证码授权（StepAcceptor） ──────────────────────────────────

// fakeStepSession 是「多步 / 验证码」渠道的假会话（实现 channel.StepAcceptor）。
//
// 故意做成「脚本驱动」：测试给定一串步骤视图，Submit 时依次往前走 ——
// 这样能验证控制台真的在**转发**服务端的步骤视图，而不是自己猜第几步。
type fakeStepSession struct {
	fakeSession
	script []channel.StepView
	idx    int
	got    []map[string]string
	err    error
}

func (f *fakeStepSession) Step() channel.StepView {
	if f.idx >= len(f.script) {
		return channel.StepView{Stage: channel.StageDone}
	}
	return f.script[f.idx]
}

func (f *fakeStepSession) Submit(v map[string]string) error {
	if f.err != nil {
		// 失败时**不推进**（与真实实现一致：用户原地重试）。
		return f.err
	}
	f.got = append(f.got, v)
	f.idx++
	return nil
}

// TestLoginStartOmitsIdleStep 回归测试：实现 StepAcceptor 但当前无活跃步骤时，
// /api/login/start **不能**把 step 发给面板。
//
// 真机踩过的坑：DeepSeek 默认关短信时 Step() 返回 {stage:"idle"}，控制台原样发给面板，
// 面板把 idle 当成「有步骤」→ 账号密码表单被顶掉 → 用户只看到粘贴引导、密码框消失。
// 这条锁住「idle 等于没有 step」。
func TestLoginStartOmitsIdleStep(t *testing.T) {
	sess := &fakeStepSession{script: []channel.StepView{{Stage: channel.StageIdle}}}
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &sess.fakeSession, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)

	w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "deepseek"}, token)
	if w.Code != http.StatusOK {
		t.Fatalf("start 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应解析失败：%v", err)
	}
	// 关键：不能有非空 step —— 否则面板会用它顶掉静态表单。
	if sv, ok := raw["step"]; ok && sv != nil {
		t.Fatalf("idle 步骤不该发给面板（会把密码表单顶掉），实际 step=%v", sv)
	}
}

// TestLoginStepProviderSkipsIdle 直接测 loginManager.StepProvider 对 idle 的处理。
func TestLoginStepProviderSkipsIdle(t *testing.T) {
	for _, st := range []channel.LoginStage{channel.StageIdle, ""} {
		sess := &fakeStepSession{script: []channel.StepView{{Stage: st}}}
		al := &activeLogin{kind: channel.DeepSeek, sess: sess}
		m := &loginManager{active: al, now: time.Now}
		if v, ok := m.StepProvider(); ok {
			t.Fatalf("stage=%q 时 StepProvider 应当返回 false，实际 %+v", st, v)
		}
	}
	// 非 idle 的要正常返回。
	sess := &fakeStepSession{script: []channel.StepView{{Stage: channel.StageInput, Title: "填写手机号"}}}
	m := &loginManager{active: &activeLogin{kind: channel.DeepSeek, sess: sess}, now: time.Now}
	if v, ok := m.StepProvider(); !ok || v.Title != "填写手机号" {
		t.Fatalf("活跃步骤应当返回，实际 ok=%v v=%+v", ok, v)
	}
}

// TestLoginStartExposesStep 确认 /api/login/start 会把第一步带给面板。
func TestLoginStartExposesStep(t *testing.T) {
	sess := &fakeStepSession{script: []channel.StepView{{
		Stage:       channel.StageInput,
		Title:       "填写手机号",
		SubmitLabel: "发送验证码",
		Fields: []channel.LoginField{
			{Name: "mobile_number", Label: "手机号", Type: "text", Required: true},
		},
	}}}
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &sess.fakeSession, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)

	w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "deepseek"}, token)
	if w.Code != http.StatusOK {
		t.Fatalf("start 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Step *channel.StepView `json:"step"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败：%v（body=%s）", err, w.Body.String())
	}
	if resp.Step == nil {
		t.Fatalf("响应里应当带 step（面板全靠它渲染），实际 %s", w.Body.String())
	}
	if resp.Step.Stage != channel.StageInput || resp.Step.Title != "填写手机号" {
		t.Fatalf("第一步不对：%+v", resp.Step)
	}
	if len(resp.Step.Fields) != 1 || resp.Step.Fields[0].Name != "mobile_number" {
		t.Fatalf("字段没带上来：%+v", resp.Step.Fields)
	}
}

// TestLoginStepAdvancesAndReturnsNextView 交一步 → 拿回推进后的新屏。
//
// 这是这套设计的关键契约：面板**不需要自己知道第几步**，只要拿服务端回的 step 重绘。
func TestLoginStepAdvancesAndReturnsNextView(t *testing.T) {
	sess := &fakeStepSession{script: []channel.StepView{
		{Stage: channel.StageInput, Title: "填写手机号", SubmitLabel: "发送验证码"},
		{Stage: channel.StageInput, Title: "输入短信验证码", SubmitLabel: "登录", ResendAfterSecs: 60},
	}}
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &sess.fakeSession, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "deepseek"}, token)

	w := authed(t, h, http.MethodPost, "/api/login/step",
		map[string]any{"values": map[string]any{"mobile_number": "13800138000"}}, token)
	if w.Code != http.StatusOK {
		t.Fatalf("step 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK   bool              `json:"ok"`
		Step *channel.StepView `json:"step"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败：%v", err)
	}
	if !resp.OK || resp.Step == nil {
		t.Fatalf("应当返回 ok + 新一步，实际 %s", w.Body.String())
	}
	if resp.Step.Title != "输入短信验证码" {
		t.Fatalf("应当返回**推进后**的屏，实际 %q", resp.Step.Title)
	}
	if resp.Step.ResendAfterSecs != 60 {
		t.Fatalf("重发窗口没带上来：%d", resp.Step.ResendAfterSecs)
	}
	if len(sess.got) != 1 || sess.got[0]["mobile_number"] != "13800138000" {
		t.Fatalf("提交值没到适配器：%+v", sess.got)
	}
}

// TestLoginStepPropagatesRejection 服务端拒了这一步时：报错 + 状态机不前进。
//
// 对应红线一：失败必须有原因，且用户能原地重试 —— 控制台不能把拒绝吞掉。
func TestLoginStepPropagatesRejection(t *testing.T) {
	sess := &fakeStepSession{
		script: []channel.StepView{
			{Stage: channel.StageInput, Title: "输入短信验证码", SubmitLabel: "登录",
				Message: "验证码不对，请照短信重新输入"},
		},
		err: errs.New(errs.AuthFailed, "验证码登录失败：验证码不对，请照短信重新输入"),
	}
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &sess.fakeSession, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "deepseek"}, token)

	w := authed(t, h, http.MethodPost, "/api/login/step",
		map[string]any{"values": map[string]any{"sms_verification_code": "000000"}}, token)
	if w.Code == http.StatusOK {
		t.Fatalf("被拒的步骤不该返回 200，实际 %s", w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "验证码不对") {
		t.Fatalf("错误原因必须带给面板（红线一），实际 %s", body)
	}
	if sess.idx != 0 {
		t.Fatalf("被拒时状态机不该前进，实际 idx=%d", sess.idx)
	}
}

// TestLoginStepImageChallengeCarriesDataURL 图验那一屏要把图（data URL）带给面板。
func TestLoginStepImageChallengeCarriesDataURL(t *testing.T) {
	const png = "data:image/png;base64,iVBORw0KGgo="
	sess := &fakeStepSession{script: []channel.StepView{{
		Stage:       channel.StageImageChallenge,
		Title:       "图片验证码",
		ImageURL:    png,
		SubmitLabel: "提交",
		Fields: []channel.LoginField{
			{Name: channel.FieldCaptcha, Label: "图片中的字符", Type: "text", Required: true},
		},
	}}}
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &sess.fakeSession, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)

	w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "deepseek"}, token)
	var resp struct {
		Step *channel.StepView `json:"step"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Step == nil || resp.Step.Stage != channel.StageImageChallenge {
		t.Fatalf("应当是图验屏，实际 %+v", resp.Step)
	}
	if resp.Step.ImageURL != png {
		t.Fatalf("图片没带给面板：%q", resp.Step.ImageURL)
	}
	if len(resp.Step.Fields) != 1 || resp.Step.Fields[0].Name != channel.FieldCaptcha {
		t.Fatalf("图验输入字段名不对：%+v", resp.Step.Fields)
	}
}

// TestLoginStepWidgetCarriesConfig 交互控件那一屏要把控件标识与初始化参数带给面板。
//
// 这是「挂数美人机校验」的关键一环：面板只认 Widget 这个名字，参数（organization /
// mode / 脚本地址 / region）全在 WidgetConfig 里，控制台必须原样透传、**不得解释**它。
// 少带一样，前端就挂不起来，而现场只会是一句「initSMCaptcha is not a function」。
func TestLoginStepWidgetCarriesConfig(t *testing.T) {
	cfg := json.RawMessage(`{"organization":"ORG-X","appId":"default","mode":"spatial_select","lang":"zh-cn","region":"CN","script":"https://castatic.example/smcp.min.js"}`)
	sess := &fakeStepSession{script: []channel.StepView{{
		Stage:        channel.StageWidget,
		Title:        "人机校验",
		Widget:       channel.WidgetShumei,
		WidgetConfig: cfg,
	}}}
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &sess.fakeSession, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)

	w := authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "deepseek"}, token)
	var resp struct {
		Step *channel.StepView `json:"step"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Step == nil || resp.Step.Stage != channel.StageWidget {
		t.Fatalf("应当是控件屏，实际 %+v", resp.Step)
	}
	if resp.Step.Widget != channel.WidgetShumei {
		t.Fatalf("控件标识没带给面板：%q", resp.Step.Widget)
	}
	// 参数必须**一字不改**地到面板（控制台不认识这些键，也不该动它们）。
	var got map[string]string
	if err := json.Unmarshal(resp.Step.WidgetConfig, &got); err != nil {
		t.Fatalf("WidgetConfig 应当是原样的 JSON 对象，实际 %s（%v）", resp.Step.WidgetConfig, err)
	}
	want := map[string]string{"organization": "ORG-X", "appId": "default",
		"mode": "spatial_select", "lang": "zh-cn", "region": "CN",
		"script": "https://castatic.example/smcp.min.js"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("WidgetConfig[%q] = %q, 想要 %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("WidgetConfig 键数 = %d, 想要 %d（控制台不该增删字段）：%s", len(got), len(want), resp.Step.WidgetConfig)
	}
}

// TestLoginStepAcceptsWidgetResult 面板回传的控件凭据必须**原样**到渠道，
// 且不能因为它是 JSON 字符串就被拦下或改写。
//
// 为什么值得单测：这几步的失败最难查 —— 凭据被截断/转义错一点，上游只会回
// 「没通过校验」，看起来像用户点错了题，实际是搬运环节的问题。
func TestLoginStepAcceptsWidgetResult(t *testing.T) {
	sess := &fakeStepSession{script: []channel.StepView{
		{Stage: channel.StageWidget, Title: "人机校验", Widget: channel.WidgetShumei},
		{Stage: channel.StageInput, Title: "输入短信验证码"},
	}}
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &sess.fakeSession, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "deepseek"}, token)

	payload := `{"region":"CN","rid":"2026092620051945f0f652c9a10a8884"}`
	w := authed(t, h, http.MethodPost, "/api/login/step",
		map[string]any{"values": map[string]any{channel.FieldWidgetResult: payload}}, token)
	if w.Code != http.StatusOK {
		t.Fatalf("提交控件凭据应当成功，实际 %d %s", w.Code, w.Body.String())
	}
	if len(sess.got) != 1 {
		t.Fatalf("渠道应当收到 1 次提交，实际 %d", len(sess.got))
	}
	if got := sess.got[0][channel.FieldWidgetResult]; got != payload {
		t.Fatalf("凭据被改写了：%q，想要 %q", got, payload)
	}
	// 返回的应当是推进后的新屏（输入验证码）。
	var resp struct {
		Step *channel.StepView `json:"step"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Step == nil || resp.Step.Stage != channel.StageInput {
		t.Fatalf("返回的应当是推进后的新屏，实际 %+v", resp.Step)
	}
}

// TestLoginStepRejectsUnsupportedChannel 不支持多步的渠道要明确报错，不静默成功。
func TestLoginStepRejectsUnsupportedChannel(t *testing.T) {
	ch := &fakeAuthChannel{kind: channel.QoderCN, sess: &fakeSession{url: "u", pending: 99}}
	h, token, _, _ := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "qodercn"}, token)

	w := authed(t, h, http.MethodPost, "/api/login/step",
		map[string]any{"values": map[string]any{"x": "y"}}, token)
	if w.Code == http.StatusOK {
		t.Fatal("不支持多步授权的渠道必须报错")
	}
	if !strings.Contains(w.Body.String(), "不支持多步验证码授权") {
		t.Fatalf("应说明不支持，实际 %s", w.Body.String())
	}
}

// TestLoginStepAcceptsBoolAndNumber 多步接口同样要宽容类型（与直登表单一个毛病）。
func TestLoginStepAcceptsBoolAndNumber(t *testing.T) {
	sess := &fakeStepSession{script: []channel.StepView{
		{Stage: channel.StageInput, Title: "填写手机号"},
		{Stage: channel.StageInput, Title: "下一步"},
	}}
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &sess.fakeSession, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "deepseek"}, token)

	w := authed(t, h, http.MethodPost, "/api/login/step",
		map[string]any{"values": map[string]any{"remember": true, "n": 3, "s": "x"}}, token)
	if w.Code != http.StatusOK {
		t.Fatalf("布尔/数字字段应被接受，实际 %d %s", w.Code, w.Body.String())
	}
	got := sess.got[0]
	if got["remember"] != "on" {
		t.Fatalf("布尔 true 应转成 \"on\"，实际 %q", got["remember"])
	}
	if got["n"] != "3" {
		t.Fatalf("数字应转成 \"3\"，实际 %q", got["n"])
	}
	if got["s"] != "x" {
		t.Fatalf("字符串应原样保留，实际 %q", got["s"])
	}
}

// TestLoginStepRejectsArrayValue 表单不会产生数组/对象，明确报错而不是静默丢弃。
func TestLoginStepRejectsArrayValue(t *testing.T) {
	sess := &fakeStepSession{script: []channel.StepView{{Stage: channel.StageInput}}}
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &sess.fakeSession, sessOverride: sess}
	h, token, _, _ := newLoginTestServer(t, ch)
	authed(t, h, http.MethodPost, "/api/login/start", channelLoginReq{Channel: "deepseek"}, token)

	w := authed(t, h, http.MethodPost, "/api/login/step",
		map[string]any{"values": map[string]any{"bad": []string{"a"}}}, token)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("数组字段应当报 400，实际 %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "类型不被支持") {
		t.Fatalf("应说明类型不支持，实际 %s", w.Body.String())
	}
}

// TestLoginStepNoActiveSession 没有进行中的授权时要报清楚，不是 500。
func TestLoginStepNoActiveSession(t *testing.T) {
	ch := &fakeAuthChannel{kind: channel.DeepSeek, sess: &fakeSession{url: "u", pending: 99}}
	h, token, _, _ := newLoginTestServer(t, ch)
	// 故意不调 /api/login/start。

	w := authed(t, h, http.MethodPost, "/api/login/step",
		map[string]any{"values": map[string]any{"a": "b"}}, token)
	if w.Code == http.StatusOK {
		t.Fatal("没有进行中的授权时应报错")
	}
	if !strings.Contains(w.Body.String(), "没有进行中的渠道授权") {
		t.Fatalf("应说清原因，实际 %s", w.Body.String())
	}
}
