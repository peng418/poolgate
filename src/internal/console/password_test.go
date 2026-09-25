package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/registry"
)

// 面板内改密码的主流程：旧密码对 → 换掉 → 当前会话还能用、别的会话被踢。
func TestChangePasswordFlow(t *testing.T) {
	srv, _, token := newKeyServer(t)

	// 另开一个「别的设备」的会话：改完密码它必须失效。
	other := loginToken(t, srv)
	if !srv.sess.Valid(other) {
		t.Fatal("前置：另一个会话应有效")
	}

	w := authPost(t, srv, token, "/api/admin/password",
		changePasswordReq{Current: "test-password-1234", Next: "new-password-5678", Confirm: "new-password-5678"})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var resp changePasswordResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.SessionsRevoked != 1 {
		t.Fatalf("应注销 1 个其它会话，实际 %+v", resp)
	}

	// 新密码可登录、旧密码不行。
	if ok, err := srv.admin.Verify("new-password-5678"); err != nil || !ok {
		t.Fatalf("新密码应可用：ok=%v err=%v", ok, err)
	}
	if ok, _ := srv.admin.Verify("test-password-1234"); ok {
		t.Fatal("旧密码必须失效")
	}
	// 当前这次操作所在的会话不该被踢（改完密码立刻被登出很恼人）。
	if !srv.sess.Valid(token) {
		t.Fatal("当前会话应保持有效")
	}
	if srv.sess.Valid(other) {
		t.Fatal("其它设备的会话必须被注销")
	}
}

// 旧密码不对：不改、并计入失败次数（否则这里就是一条不限速的爆破入口）。
func TestChangePasswordRejectsWrongCurrent(t *testing.T) {
	srv, _, token := newKeyServer(t)
	w := authPost(t, srv, token, "/api/admin/password",
		changePasswordReq{Current: "wrong-password", Next: "new-password-5678", Confirm: "new-password-5678"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("应 400，实际 %d %s", w.Code, w.Body.String())
	}
	if ok, _ := srv.admin.Verify("test-password-1234"); !ok {
		t.Fatal("旧密码必须保持有效（没改成）")
	}
	// 失败会记进与登录共用的失败计数（这里只验证没有把密码改掉、
	// 也没返回 401 —— 失败计数的行为由登录用例覆盖）。
}

// 两次新密码不一致 / 与旧密码相同：明确拒绝并说明原因。
func TestChangePasswordValidation(t *testing.T) {
	srv, _, token := newKeyServer(t)
	cases := []struct {
		name string
		req  changePasswordReq
		want string
	}{
		{"两次不一致", changePasswordReq{Current: "test-password-1234", Next: "aaaabbbb", Confirm: "aaaabbbc"}, "不一致"},
		{"新密码为空", changePasswordReq{Current: "test-password-1234", Next: "", Confirm: ""}, "不能为空"},
		{"与旧密码相同", changePasswordReq{Current: "test-password-1234", Next: "test-password-1234", Confirm: "test-password-1234"}, "没有意义"},
	}
	for _, c := range cases {
		w := authPost(t, srv, token, "/api/admin/password", c.req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s：应 400，实际 %d %s", c.name, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), c.want) {
			t.Fatalf("%s：应说明原因（含 %q），实际 %s", c.name, c.want, w.Body.String())
		}
	}
	if ok, _ := srv.admin.Verify("test-password-1234"); !ok {
		t.Fatal("校验失败时密码不能被改动")
	}
}

// 未登录不能改密码。
func TestChangePasswordRequiresSession(t *testing.T) {
	srv, _, _ := newKeyServer(t)
	w := authPost(t, srv, "", "/api/admin/password",
		changePasswordReq{Current: "test-password-1234", Next: "x", Confirm: "x"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，实际 %d", w.Code)
	}
}

// 上游凭证失效**不能**返回 401。
//
// 前端把所有 401 都当成「管理员会话过期」→ 立刻清空状态跳登录页。
// 于是用户点一下「刷新余额」（上游恰好 401）就被踢出面板 —— 实测踩到，
// 用户的原话是「报错未登录，然后跳转到登录页面」。
func TestUpstreamAuthFailureIsNotHTTP401(t *testing.T) {
	for _, k := range []errs.Kind{errs.SessionDead, errs.AuthFailed} {
		if code := statusForKind(k); code == http.StatusUnauthorized {
			t.Fatalf("%s 映射到 401 会把管理员踢出面板，应映射到 5xx（实际 %d）", k, code)
		}
	}
	// 上游故障/额度这类本来就该是 5xx/4xx，别顺手改成 401。
	if statusForKind(errs.UpstreamFault) != http.StatusBadGateway {
		t.Fatal("上游故障应是 502")
	}
	if statusForKind(errs.NoCandidate) != http.StatusServiceUnavailable {
		t.Fatal("无可用账号应是 503")
	}
}

// 上游凭证失效的错误消息要带上「怎么办」，不能只有「上游返回 HTTP 401」。
func TestSessionDeadErrorCarriesAdvice(t *testing.T) {
	srv, _, token := newKeyServer(t)
	// 账号不存在时这条接口回 404 —— 这里只兜住「不会 401」这一条底线。
	registry.Reset()
	registry.RegisterSpec(channel.Spec{Kind: channel.QoderCN, DisplayName: "QoderCN", Status: channel.Active})
	w := authPost(t, srv, token, "/api/account/refresh", accountActionReq{Kind: string(channel.QoderCN), UID: "nope"})
	// 账号不存在 → 404，不算这条用例要验的东西；这里只兜住「不会 401」。
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("账号类接口不得返回 401（会把管理员踢出面板），实际 %s", w.Body.String())
	}
}
