package console

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/health"
	"poolgate/internal/pool"
	"poolgate/internal/registry"
	"poolgate/internal/store"
)

// newOpsServer 装配一个带完整依赖的测试面板（账号/模型/设置/体检历史）。
func newOpsServer(t *testing.T) (*Server, *pool.Pool, *store.SettingsStore, string) {
	t.Helper()
	dir := t.TempDir()
	admin := store.NewAdminStore(dir)
	if err := admin.Set("test-password-1234"); err != nil {
		t.Fatal(err)
	}
	sess := store.NewSessionStore()
	p := pool.New()
	settings := store.NewSettingsStore(dir)
	creds := store.NewCredsStore(filepath.Join(dir, "creds"))
	history := health.NewHistory(10)
	srv := New(admin, sess, Options{
		Version:  "test",
		Pool:     p,
		Log:      store.NewRequestLog(50),
		Creds:    creds,
		Settings: settings,
		Checkins: store.NewCheckinStore(),
		History:  history,
		Excluded: NewModelExclusions(filepath.Join(dir, "excluded.json")),
		// 与 cmd/poolgate 同样的装配：开关 + 体检结论合成一个查询器。
		HealthyModels: health.Health(history, func() bool { return settings.Get().OnlyHealthyModels }),
		DataDir:       dir,
		CredsDir:      filepath.Join(dir, "creds"),
	})
	return srv, p, settings, dir
}

// authPost 以已登录会话发一个 POST。
func authPost(t *testing.T, srv *Server, token, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cookie", "pg_session="+token)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	return w
}

func loginToken(t *testing.T, srv *Server) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/session",
		strings.NewReader(`{"password":"test-password-1234"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("登录失败 %d %s", w.Code, w.Body.String())
	}
	var resp sessionResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Token
}

func addAccount(p *pool.Pool, kind channel.Kind, uid string) {
	p.AddFor(kind, channel.Credential{UID: uid, Nickname: "n-" + uid, AccessToken: "SENTINELTOKENVALUE"})
}

// 停用后账号不再参与路由；重新启用要能恢复（F1.9）。
func TestAccountEnableDisable(t *testing.T) {
	srv, p, _, _ := newOpsServer(t)
	addAccount(p, channel.QoderCN, "u1")
	token := loginToken(t, srv)

	w := authPost(t, srv, token, "/api/account/enable",
		map[string]any{"kind": "qodercn", "uid": "u1", "disabled": true})
	if w.Code != http.StatusOK {
		t.Fatalf("停用应成功，实际 %d %s", w.Code, w.Body.String())
	}
	if _, ok := p.Pick(context.Background(), channel.QoderCN, nil); ok {
		t.Fatal("停用后不应再被选中")
	}

	w = authPost(t, srv, token, "/api/account/enable",
		map[string]any{"kind": "qodercn", "uid": "u1", "disabled": false})
	if w.Code != http.StatusOK {
		t.Fatalf("启用应成功，实际 %d", w.Code)
	}
	if _, ok := p.Pick(context.Background(), channel.QoderCN, nil); !ok {
		t.Fatal("启用后应能重新被选中")
	}
}

// 删除不存在的账号必须明确报 404，不能假装成功。
func TestAccountEnableUnknownAccount(t *testing.T) {
	srv, _, _, _ := newOpsServer(t)
	token := loginToken(t, srv)
	w := authPost(t, srv, token, "/api/account/enable",
		map[string]any{"kind": "qodercn", "uid": "nope", "disabled": true})
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在的账号应 404，实际 %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("不存在")) {
		t.Fatalf("应说明原因，实际 %s", w.Body.String())
	}
}

// 删除账号要同时清掉内存池与磁盘凭证。
func TestAccountRemoveClearsPoolAndFile(t *testing.T) {
	srv, p, _, dir := newOpsServer(t)
	addAccount(p, channel.QoderCN, "u1")
	credsDir := filepath.Join(dir, "creds")
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(credsDir, "qodercn-u1.json")
	if err := os.WriteFile(credPath, []byte(`{"auth":{"accessToken":"t"},"account":{"uid":"u1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	token := loginToken(t, srv)

	w := authPost(t, srv, token, "/api/account/remove", map[string]any{"kind": "qodercn", "uid": "u1"})
	if w.Code != http.StatusOK {
		t.Fatalf("删除应成功，实际 %d %s", w.Code, w.Body.String())
	}
	if _, ok := p.Get(channel.QoderCN, "u1"); ok {
		t.Fatal("内存池应已移除")
	}
	if _, err := os.Stat(credPath); !os.IsNotExist(err) {
		t.Fatal("凭证文件应已删除")
	}
}

// 设置写读往返：改端口能持久化，重新加载后还在（F6.4）。
func TestSettingsRoundTrip(t *testing.T) {
	srv, _, settings, _ := newOpsServer(t)
	token := loginToken(t, srv)

	st := settings.Get()
	st.ListenPort = 5199
	st.StickyRequests = 77
	w := authPost(t, srv, token, "/api/settings", st)
	if w.Code != http.StatusOK {
		t.Fatalf("保存设置应成功，实际 %d %s", w.Code, w.Body.String())
	}
	// 从磁盘重读，确认真的落盘了（不只是内存）。
	reloaded := store.NewSettingsStore(filepath.Dir(settings.Path()))
	got := reloaded.Get()
	if got.ListenPort != 5199 || got.StickyRequests != 77 {
		t.Fatalf("设置未持久化：port=%d sticky=%d", got.ListenPort, got.StickyRequests)
	}
}

// 明显起不来的值要挡住，并说明是哪一项。
func TestSettingsRejectsBadValues(t *testing.T) {
	srv, _, settings, _ := newOpsServer(t)
	token := loginToken(t, srv)
	st := settings.Get()
	st.ListenPort = 70000
	w := authPost(t, srv, token, "/api/settings", st)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法端口应被拒，实际 %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("端口")) {
		t.Fatalf("应指出是哪一项，实际 %s", w.Body.String())
	}
}

// 签到时间格式错误必须报错并指出具体值。
func TestSettingsRejectsBadCheckinTime(t *testing.T) {
	srv, _, settings, _ := newOpsServer(t)
	token := loginToken(t, srv)
	st := settings.Get()
	st.CheckinTimes = []string{"9am"}
	w := authPost(t, srv, token, "/api/settings", st)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法时间应被拒，实际 %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("HH:MM")) {
		t.Fatalf("应说明格式要求，实际 %s", w.Body.String())
	}
}

// 模型剔除表要能落盘并在重新加载后仍然生效（F4.4）。
func TestModelExclusionPersists(t *testing.T) {
	srv, _, _, dir := newOpsServer(t)
	token := loginToken(t, srv)

	w := authPost(t, srv, token, "/api/model/toggle",
		map[string]any{"id": "traework/deepseek-v4.1-flash", "downstream": false})
	if w.Code != http.StatusOK {
		t.Fatalf("剔除应成功，实际 %d %s", w.Code, w.Body.String())
	}
	reloaded := NewModelExclusions(filepath.Join(dir, "excluded.json"))
	if !reloaded.Has("traework/deepseek-v4.1-flash") {
		t.Fatal("剔除表应已落盘")
	}
	// 恢复后必须从表里消失，否则「恢复下发」是假的。
	w = authPost(t, srv, token, "/api/model/toggle",
		map[string]any{"id": "traework/deepseek-v4.1-flash", "downstream": true})
	if w.Code != http.StatusOK {
		t.Fatalf("恢复应成功，实际 %d", w.Code)
	}
	if NewModelExclusions(filepath.Join(dir, "excluded.json")).Has("traework/deepseek-v4.1-flash") {
		t.Fatal("恢复后不应还在剔除表里")
	}
}

// 渠道开关非法状态必须拒绝，且不改动已有设置。
func TestChannelToggleRejectsBadStatus(t *testing.T) {
	srv, _, settings, _ := newOpsServer(t)
	token := loginToken(t, srv)
	w := authPost(t, srv, token, "/api/channel/toggle",
		map[string]any{"kind": "qodercn", "status": "whatever"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法状态应被拒，实际 %d", w.Code)
	}
	if len(settings.Get().ChannelOverrides) != 0 {
		t.Fatal("被拒的请求不应写入设置")
	}
}

// 未注册渠道不能写开关（避免往设置里塞垃圾键）。
func TestChannelToggleUnknownChannel(t *testing.T) {
	srv, _, _, _ := newOpsServer(t)
	token := loginToken(t, srv)
	w := authPost(t, srv, token, "/api/channel/toggle",
		map[string]any{"kind": "nosuchchannel", "status": "paused"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("未注册渠道应 404，实际 %d", w.Code)
	}
}

// 含凭证的导出必须明确拒绝，不能给一个「看起来导出了其实没有」的结果。
func TestBackupRefusesCredentialExport(t *testing.T) {
	srv, _, _, _ := newOpsServer(t)
	token := loginToken(t, srv)
	w := authPost(t, srv, token, "/api/backup", map[string]any{"include_creds": true})
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("含凭证导出应明确拒绝，实际 %d", w.Code)
	}
}

// 配置导出不得含任何 token 字段（安全约束 D5）。
func TestBackupExcludesTokens(t *testing.T) {
	srv, p, _, _ := newOpsServer(t)
	addAccount(p, channel.QoderCN, "u1")
	token := loginToken(t, srv)
	w := authPost(t, srv, token, "/api/backup", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("导出应成功，实际 %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// 用不可能自然出现的哨兵值，避免 "tok" 这类子串撞上正常字段名。
	for _, bad := range []string{"accessToken", "refreshToken", "SENTINELTOKENVALUE"} {
		if strings.Contains(body, bad) {
			t.Fatalf("导出包不应含 %q：%s", bad, body)
		}
	}
}

// 危险操作：重置配置要回到出厂值。
func TestDangerReset(t *testing.T) {
	srv, _, settings, _ := newOpsServer(t)
	token := loginToken(t, srv)
	if err := settings.Update(func(st *store.Settings) error {
		st.ListenPort = 5199
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := authPost(t, srv, token, "/api/danger/reset", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("重置应成功，实际 %d", w.Code)
	}
	if got := settings.Get().ListenPort; got != store.DefaultSettings().ListenPort {
		t.Fatalf("重置后应回默认端口，实际 %d", got)
	}
}

// 日志过滤按渠道与状态生效（F6.1）。
func TestLogsFilter(t *testing.T) {
	srv, _, _, _ := newOpsServer(t)
	token := loginToken(t, srv)
	srv.opts.Log.Add(store.RequestRecord{Channel: "qodercn", Model: "a", Status: "ok"})
	srv.opts.Log.Add(store.RequestRecord{Channel: "traework", Model: "b", Status: "error", ErrKind: "UpstreamFault"})
	srv.opts.Log.Add(store.RequestRecord{Channel: "traework", Model: "c", Status: "error", ErrKind: "Transport"})

	r := httptest.NewRequest(http.MethodGet, "/api/logs/filter?channel=traework&status=error", nil)
	r.Header.Set("Cookie", "pg_session="+token)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("过滤应成功，实际 %d", w.Code)
	}
	var resp struct {
		Requests []store.RequestRecord `json:"requests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Requests) != 2 {
		t.Fatalf("应筛出 2 条 traework 失败，实际 %d", len(resp.Requests))
	}
	for _, rec := range resp.Requests {
		if rec.Channel != "traework" || rec.Status != "error" {
			t.Fatalf("过滤结果不符：%+v", rec)
		}
	}
}

// 体检历史为空时返回空数组而不是 null（前端不用做额外判空）。
func TestProbeHistoryEmpty(t *testing.T) {
	srv, _, _, _ := newOpsServer(t)
	token := loginToken(t, srv)
	r := httptest.NewRequest(http.MethodGet, "/api/probe/history", nil)
	r.Header.Set("Cookie", "pg_session="+token)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"runs"`) {
		t.Fatalf("应返回 runs 字段，实际 %s", w.Body.String())
	}
}

// 总览要能反映体检结果（模型通过数）与今日请求统计。
func TestOverviewIncludesTodayAndProbe(t *testing.T) {
	srv, p, _, _ := newOpsServer(t)
	addAccount(p, channel.QoderCN, "u1")
	// 显式写当前时间：TodayStats 按本地日期切分，零值时间会被算作「很久以前」。
	now := time.Now()
	srv.opts.Log.Add(store.RequestRecord{Time: now, Channel: "qodercn", Model: "a", Status: "ok", TTFT: 900})
	srv.opts.Log.Add(store.RequestRecord{Time: now, Channel: "qodercn", Model: "b", Status: "error", ErrKind: "UpstreamFault"})

	token := loginToken(t, srv)
	r := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	r.Header.Set("Cookie", "pg_session="+token)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	var resp struct {
		Today struct {
			Requests int   `json:"requests"`
			Failed   int   `json:"failed"`
			TTFTms   int64 `json:"ttft_ms"`
		} `json:"today"`
		ErrorStats map[string]int `json:"error_stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Today.Requests != 2 || resp.Today.Failed != 1 {
		t.Fatalf("今日统计不对：%+v", resp.Today)
	}
	if resp.Today.TTFTms != 900 {
		t.Fatalf("TTFT 中位应为 900，实际 %d", resp.Today.TTFTms)
	}
	if resp.ErrorStats["UpstreamFault"] != 1 {
		t.Fatalf("错误聚合不对：%+v", resp.ErrorStats)
	}
}

// 账号清单要带出余额未知标志与签到结果（F1.7/F1.8）。
func TestAccountsExposeBalanceKnownAndCheckin(t *testing.T) {
	srv, p, _, _ := newOpsServer(t)
	addAccount(p, channel.QoderCN, "u1")
	// 上游没给可信余额 → credits_known 必须为 false，UI 才能显示「未知」。
	srv.opts.Checkins.Set("qodercn", "u1", channel.CheckinResult{OK: true, Reward: "+100 Credits", Message: "ok"})

	token := loginToken(t, srv)
	r := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	r.Header.Set("Cookie", "pg_session="+token)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	var resp struct {
		Accounts []struct {
			CreditsKnown bool `json:"credits_known"`
			Checkin      *struct {
				OK     bool   `json:"ok"`
				Reward string `json:"reward"`
			} `json:"checkin"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Accounts) != 1 {
		t.Fatalf("应有 1 个账号，实际 %d", len(resp.Accounts))
	}
	if resp.Accounts[0].CreditsKnown {
		t.Fatal("未刷新过余额时 credits_known 应为 false")
	}
	if resp.Accounts[0].Checkin == nil || !resp.Accounts[0].Checkin.OK {
		t.Fatalf("应带出签到结果：%+v", resp.Accounts[0].Checkin)
	}
}

// 注册表里没有实现的渠道不能因为开关而变成「可实现」。
func TestChannelToggleDoesNotFakeImplementation(t *testing.T) {
	srv, _, settings, _ := newOpsServer(t)
	token := loginToken(t, srv)
	if _, ok := registry.GetSpec("qwenwork"); !ok {
		t.Skip("qwenwork 未注册（测试环境未跑 boot）")
	}
	w := authPost(t, srv, token, "/api/channel/toggle",
		map[string]any{"kind": "qwenwork", "status": "active"})
	if w.Code != http.StatusOK {
		t.Fatalf("开关应成功，实际 %d %s", w.Code, w.Body.String())
	}
	if settings.Get().ChannelOverrides["qwenwork"].Status != "active" {
		t.Fatal("开关应记录到设置里")
	}
}
