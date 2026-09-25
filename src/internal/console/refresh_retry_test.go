package console

import (
	"strings"

	"context"
	"encoding/json"
	"net/http"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/pool"
	"poolgate/internal/registry"
)

// balChan 只关心 Balance 的行为：第一次可失败，之后返回固定余额。
type balChan struct {
	kind      channel.Kind
	calls     int
	failFirst bool
}

func (f *balChan) Kind() channel.Kind                                 { return f.kind }
func (f *balChan) Spec() channel.Spec                                 { return channel.Spec{Kind: f.kind, Status: channel.Active} }
func (f *balChan) Login(context.Context) (*channel.Credential, error) { return nil, nil }
func (f *balChan) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}
func (f *balChan) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	return []channel.ModelInfo{{ID: "pro"}}, nil
}
func (f *balChan) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: true}, nil
}
func (f *balChan) Chat(context.Context, *channel.Credential, channel.ChatRequest) (channel.Stream, error) {
	return nil, nil
}
func (f *balChan) Classify(int, []byte) errs.Kind { return errs.Parse }

func (f *balChan) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	f.calls++
	if f.failFirst && f.calls == 1 {
		return channel.Balance{}, errs.New(errs.SessionDead, "上游返回 HTTP 401").
			WithChannel(string(f.kind)).WithUpstream(`{"code":"invalid-credential","msg":"Invalid JWT token"}`)
	}
	return channel.Balance{Credits: 1234, Known: true}, nil
}

func newRetryServer(t *testing.T, ch *balChan, refresher pool.RefreshFunc) (*Server, string) {
	t.Helper()
	registry.Reset()
	registry.Register(ch, ch.Spec())
	srv, p, _, _ := newOpsServer(t)
	p.AddFor(ch.kind, channel.Credential{UID: "u1", AccessToken: "stale", RefreshToken: "rt"})
	if refresher != nil {
		p.SetRefresher(refresher)
	}
	return srv, loginToken(t, srv)
}

// 「刷新余额」撞上「本地没过期、上游已经拒绝」时必须自救：强制续期一次再重试。
//
// 不做这一步的话，用户装完修复版第一次点按钮看到的还是同样的 401，
// 会合理地认为「修复没生效」——千问那个 device_token 问题的现场就是这样。
func TestAccountRefreshRetriesAfterCredentialFailure(t *testing.T) {
	ch := &balChan{kind: channel.QoderCN, failFirst: true}
	srv, token := newRetryServer(t, ch, func(_ context.Context, kind channel.Kind, c channel.Credential) (*channel.Credential, error) {
		nc := c
		nc.AccessToken = "fresh"
		return &nc, nil
	})

	w := authPost(t, srv, token, "/api/account/refresh", accountActionReq{Kind: "qodercn", UID: "u1"})
	if w.Code != http.StatusOK {
		t.Fatalf("续期后应成功，实际 %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Credits int64 `json:"credits"`
		Known   bool  `json:"known"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Credits != 1234 || !resp.Known {
		t.Fatalf("应回续期后重试拿到的余额，实际 %+v", resp)
	}
	if ch.calls != 2 {
		t.Fatalf("应是「失败 → 续期 → 重试」两次调用，实际 %d", ch.calls)
	}
}

// 续期也救不回来时，交出去的必须是**原始错误**（带上游原话与怎么办），
// 而不是「续期失败」这种更难查的说法；且绝不能是 401（会被前端当成被登出）。
func TestAccountRefreshSurfacesOriginalErrorWhenRefreshFails(t *testing.T) {
	ch := &balChan{kind: channel.QoderCN, failFirst: true}
	srv, token := newRetryServer(t, ch, func(context.Context, channel.Kind, channel.Credential) (*channel.Credential, error) {
		return nil, errs.New(errs.SessionDead, "refresh_token 已失效，需要重新授权")
	})

	w := authPost(t, srv, token, "/api/account/refresh", accountActionReq{Kind: "qodercn", UID: "u1"})
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("上游凭证问题不得回 401（前端会当成被登出），实际 %s", w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "上游返回 HTTP 401") || !strings.Contains(body, "重新登录") {
		t.Fatalf("应带上游原话 + 下一步指引，实际 %s", body)
	}
}
