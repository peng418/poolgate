package router

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/pool"
)

// fakeChannel 是 channel.Channel 的测试实现：只关心 Chat 返回什么错。
// chat 回调拿得到 uid，便于按账号注入不同的失败行为。
type fakeChannel struct {
	kind channel.Kind
	// chat 每次调用都会被执行；calls 记录调用次数（含重试）。
	chat  func(c *channel.Credential) (channel.Stream, error)
	calls int
	mu    sync.Mutex
}

func (f *fakeChannel) Kind() channel.Kind                                 { return f.kind }
func (f *fakeChannel) Spec() channel.Spec                                 { return channel.Spec{Kind: f.kind, Status: channel.Active} }
func (f *fakeChannel) Login(context.Context) (*channel.Credential, error) { return nil, nil }
func (f *fakeChannel) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}
func (f *fakeChannel) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	return nil, nil
}
func (f *fakeChannel) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{}, nil
}
func (f *fakeChannel) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{}, nil
}
func (f *fakeChannel) Classify(int, []byte) errs.Kind { return errs.Parse }

func (f *fakeChannel) Chat(_ context.Context, c *channel.Credential, _ channel.ChatRequest) (channel.Stream, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.chat == nil {
		return &emptyStream{}, nil
	}
	return f.chat(c)
}

func (f *fakeChannel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// emptyStream 是空流：立即 EOF，够用（路由层不读内容，只交接流）。
type emptyStream struct{}

func (s *emptyStream) Next() (channel.ChatCompletionChunk, error) {
	return channel.ChatCompletionChunk{}, io.EOF
}
func (s *emptyStream) Close() error { return nil }

// newPool 造一个含 A/B 两个账号的池，A 余额更高（默认首选 A）。
//
// 余额必须真的不等：pool.Pick 遍历的是 map，余额相同时顺序是随机的 ——
// 那样「先试 A 再换到 B」的用例会偶发失败（实测踩过）。
func newPool(kind channel.Kind) *pool.Pool {
	p := pool.New()
	p.AddFor(kind, channel.Credential{UID: "A", Nickname: "账号A"})
	p.AddFor(kind, channel.Credential{UID: "B", Nickname: "账号B"})
	p.SetCredits(kind, "A", 200)
	p.SetCredits(kind, "B", 100)
	return p
}

func kindOf(t *testing.T, err error) errs.Kind {
	t.Helper()
	k, ok := errs.KindOf(err)
	if !ok {
		t.Fatalf("期望得到结构化错误，实际 err=%v", err)
	}
	return k
}

// 无可用账号：必须返回带渠道名与原因的 NoCandidate，而不是 nil（红线一）。
func TestRouteNoAccounts(t *testing.T) {
	kind := channel.Kind("test")
	p := pool.New()
	r := New(p, Options{MaxRetry: 2, StickyRequests: 1})
	ch := &fakeChannel{kind: kind}

	_, err := r.Route(context.Background(), ch, kind, "s1", channel.ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("无账号时必须报错，不能静默返回")
	}
	if k := kindOf(t, err); k != errs.NoCandidate {
		t.Fatalf("期望 NoCandidate，实际 %s", k)
	}
	if got := ch.count(); got != 0 {
		t.Fatalf("无账号时不应调用上游，实际调用 %d 次", got)
	}
}

// MaxRetry=0：失败不换号，只试一个账号 —— 免得把「重试」变成对上游的连打。
func TestRouteMaxRetryZero(t *testing.T) {
	kind := channel.Kind("test")
	p := newPool(kind)
	r := New(p, Options{MaxRetry: 0, StickyRequests: 1})
	ch := &fakeChannel{kind: kind, chat: func(c *channel.Credential) (channel.Stream, error) {
		return nil, errs.New(errs.SoftRate, "限流")
	}}

	_, err := r.Route(context.Background(), ch, kind, "", channel.ChatRequest{Model: "m"})
	if k := kindOf(t, err); k != errs.SoftRate {
		t.Fatalf("期望原样返回 SoftRate，实际 %s", k)
	}
	if got := ch.count(); got != 1 {
		t.Fatalf("MaxRetry=0 时只应调用 1 次，实际 %d 次", got)
	}
}

// 账号类错误（SessionDead）→ 禁用该号并换号重试，最终由另一个号成功。
func TestRouteRotatesOnAccountError(t *testing.T) {
	kind := channel.Kind("test")
	p := newPool(kind)
	r := New(p, Options{MaxRetry: 3, StickyRequests: 1})
	ch := &fakeChannel{kind: kind, chat: func(c *channel.Credential) (channel.Stream, error) {
		if c.UID == "A" {
			return nil, errs.New(errs.SessionDead, "401 会话失效").WithAccount("A")
		}
		return &emptyStream{}, nil
	}}

	res, err := r.Route(context.Background(), ch, kind, "", channel.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("换号后应成功，实际 %v", err)
	}
	if res.Cred.UID != "B" {
		t.Fatalf("期望换到 B，实际 %s", res.Cred.UID)
	}
	// SessionDead 的 Policy 是 Disable：坏号必须被摘掉，不能继续被选中。
	for _, st := range p.List(kind) {
		if st.Cred.UID == "A" && !st.Disabled {
			t.Fatal("SessionDead 应禁用账号 A，实际仍可用")
		}
	}
}

// 全部账号失败：返回最后一次的真实错误，不能吞成 NoCandidate。
func TestRouteReturnsLastRealError(t *testing.T) {
	kind := channel.Kind("test")
	p := newPool(kind)
	r := New(p, Options{MaxRetry: 3, StickyRequests: 1})
	ch := &fakeChannel{kind: kind, chat: func(c *channel.Credential) (channel.Stream, error) {
		return nil, errs.New(errs.ModelUnavailable, "账号 "+c.UID+" 无此模型")
	}}

	_, err := r.Route(context.Background(), ch, kind, "", channel.ChatRequest{Model: "m"})
	if k := kindOf(t, err); k != errs.ModelUnavailable {
		t.Fatalf("期望 ModelUnavailable（真实原因），实际 %s：%v", k, err)
	}
	if got := ch.count(); got != 2 {
		t.Fatalf("两个账号各试一次，实际调用 %d 次", got)
	}
}

// 红线（D4）：上游故障不换号、不计账号错误 —— 否则一次上游 503 会把好号全冷却掉。
func TestRouteUpstreamFaultNotBlamedNoRetry(t *testing.T) {
	kind := channel.Kind("test")
	p := newPool(kind)
	r := New(p, Options{MaxRetry: 3, StickyRequests: 1})
	ch := &fakeChannel{kind: kind, chat: func(c *channel.Credential) (channel.Stream, error) {
		return nil, errs.New(errs.UpstreamFault, "503 Model catalog unavailable").WithUpstream("503")
	}}

	_, err := r.Route(context.Background(), ch, kind, "", channel.ChatRequest{Model: "m"})
	if k := kindOf(t, err); k != errs.UpstreamFault {
		t.Fatalf("期望 UpstreamFault 原样返回，实际 %s", k)
	}
	if got := ch.count(); got != 1 {
		t.Fatalf("上游故障不应换号重试，实际调用 %d 次", got)
	}
	if n := p.CountHealthy(kind); n != 2 {
		t.Fatalf("上游故障不得冷却账号，期望 2 个健康账号，实际 %d", n)
	}
}

// 内容拦截：上游明确拒绝，直接透传给客户端，不换号白折腾。
func TestRouteContentBlockedNoRetry(t *testing.T) {
	kind := channel.Kind("test")
	p := newPool(kind)
	r := New(p, Options{MaxRetry: 3, StickyRequests: 1})
	ch := &fakeChannel{kind: kind, chat: func(c *channel.Credential) (channel.Stream, error) {
		return nil, errs.New(errs.ContentBlocked, "内容被拦截")
	}}

	_, err := r.Route(context.Background(), ch, kind, "", channel.ChatRequest{Model: "m"})
	if k := kindOf(t, err); k != errs.ContentBlocked {
		t.Fatalf("期望 ContentBlocked，实际 %s", k)
	}
	if got := ch.count(); got != 1 {
		t.Fatalf("内容拦截不应换号，实际调用 %d 次", got)
	}
}

// 2026-09-27 事故回归：上游连接中断不是这个号的错 —— 不冷却账号，但换号接着试，
// 都失败就把真实错误交给客户端（而不是一句 503「无可用账号」）。
// 事故现场：一条「上游连接中断且未收到内容」把两个号各冷却 10 分钟，整渠道躺了 10 分钟。
func TestRouteTransportFailureRetriesWithoutCooling(t *testing.T) {
	kind := channel.Kind("test")
	p := newPool(kind)
	r := New(p, Options{MaxRetry: 3, StickyRequests: 1})
	ch := &fakeChannel{kind: kind, chat: func(*channel.Credential) (channel.Stream, error) {
		return nil, errs.New(errs.Transport, "上游连接中断且未收到内容")
	}}

	_, err := r.Route(context.Background(), ch, kind, "", channel.ChatRequest{Model: "m"})
	if k := kindOf(t, err); k != errs.Transport {
		t.Fatalf("应把真实错误（Transport）交给客户端，实际 %s", k)
	}
	if got := ch.count(); got != 2 {
		t.Fatalf("两个号都该被试过（换号重试），实际调用 %d 次", got)
	}
	if n := p.CountHealthy(kind); n != 2 {
		t.Fatalf("连接中断不得冷却账号，期望 2 个健康账号，实际 %d", n)
	}
}

// 粘性：同一会话认准一个号，即使别的号余额更高也不换 —— 上游会话有上下文，
// 频繁换号会让对话「失忆」，这正是粘性路由存在的理由。
func TestRouteStickyReusesAccount(t *testing.T) {
	kind := channel.Kind("test")
	p := newPool(kind)
	p.SetCredits(kind, "B", 1000) // 第一次选 B（余额最高）
	r := New(p, Options{MaxRetry: 0, StickyRequests: 10})
	ch := &fakeChannel{kind: kind}

	res1, err := r.Route(context.Background(), ch, kind, "s1", channel.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("首次路由失败: %v", err)
	}
	if res1.Cred.UID != "B" {
		t.Fatalf("首次应选余额最高的 B，实际 %s", res1.Cred.UID)
	}

	// 让 A 的余额反超：粘性命中时仍应留在 B。
	p.SetCredits(kind, "A", 5000)
	res2, err := r.Route(context.Background(), ch, kind, "s1", channel.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("第二次路由失败: %v", err)
	}
	if res2.Cred.UID != "B" {
		t.Fatalf("同会话应粘住 B，实际换成了 %s", res2.Cred.UID)
	}

	// 另一个会话不受影响：按余额选 A。
	res3, err := r.Route(context.Background(), ch, kind, "s2", channel.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("第三个请求失败: %v", err)
	}
	if res3.Cred.UID != "A" {
		t.Fatalf("新会话应按余额选 A，实际 %s", res3.Cred.UID)
	}
}

// 粘性不是永久绑定：StickyRequests 次用满后重新参与轮换（否则一个会话永远钉死一个号）。
func TestRouteStickyExpiresAfterNRequests(t *testing.T) {
	kind := channel.Kind("test")
	p := newPool(kind)
	p.SetCredits(kind, "B", 1000)
	r := New(p, Options{MaxRetry: 0, StickyRequests: 2})
	ch := &fakeChannel{kind: kind}

	for i := 1; i <= 2; i++ {
		res, err := r.Route(context.Background(), ch, kind, "s1", channel.ChatRequest{Model: "m"})
		if err != nil {
			t.Fatalf("第 %d 次路由失败: %v", i, err)
		}
		if res.Cred.UID != "B" {
			t.Fatalf("第 %d 次应仍在粘性额度内选 B，实际 %s", i, res.Cred.UID)
		}
	}

	// 额度用尽：A 的余额已反超，第 3 次应重新选 A。
	p.SetCredits(kind, "A", 5000)
	res, err := r.Route(context.Background(), ch, kind, "s1", channel.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("第三次路由失败: %v", err)
	}
	if res.Cred.UID != "A" {
		t.Fatalf("粘性额度用尽后应重新选余额最高的 A，实际 %s", res.Cred.UID)
	}
}

// 客户端没带会话 ID：不粘，照常轮换（无会话凭证时不能凭空绑一个号）。
func TestRouteNoSessionNoSticky(t *testing.T) {
	kind := channel.Kind("test")
	p := newPool(kind)
	p.SetCredits(kind, "B", 1000)
	r := New(p, Options{MaxRetry: 0, StickyRequests: 10})
	ch := &fakeChannel{kind: kind}

	if _, err := r.Route(context.Background(), ch, kind, "", channel.ChatRequest{Model: "m"}); err != nil {
		t.Fatalf("首次路由失败: %v", err)
	}
	p.SetCredits(kind, "A", 5000)
	res, err := r.Route(context.Background(), ch, kind, "", channel.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("第二次路由失败: %v", err)
	}
	if res.Cred.UID != "A" {
		t.Fatalf("无会话 ID 时应按余额选 A，实际 %s", res.Cred.UID)
	}
}

// 粘性表上限：会话 ID 由客户端提供，不设上限就是一处能被撑爆的内存。
func TestRouteStickyTableBounded(t *testing.T) {
	kind := channel.Kind("test")
	p := newPool(kind)
	r := New(p, Options{MaxRetry: 0, StickyRequests: 10})
	ch := &fakeChannel{kind: kind}

	for i := 0; i < maxStickyEntries+50; i++ {
		sid := fmt.Sprintf("session-%d", i)
		if _, err := r.Route(context.Background(), ch, kind, sid, channel.ChatRequest{Model: "m"}); err != nil {
			t.Fatalf("会话 %s 路由失败: %v", sid, err)
		}
	}

	r.mu.Lock()
	n := len(r.sticky)
	r.mu.Unlock()
	if n > maxStickyEntries {
		t.Fatalf("粘性表应有上限 %d，实际 %d 条", maxStickyEntries, n)
	}
}

// 并发：Route 由并发的 HTTP 请求调用，粘性表必须线程安全（-race 下验证）。
func TestRouteConcurrent(t *testing.T) {
	kind := channel.Kind("test")
	p := newPool(kind)
	r := New(p, Options{MaxRetry: 1, StickyRequests: 3})
	ch := &fakeChannel{kind: kind}

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := fmt.Sprintf("s-%d", i%4) // 只读/写少数几个会话，制造竞争
			for j := 0; j < 8; j++ {
				if _, err := r.Route(context.Background(), ch, kind, sid, channel.ChatRequest{Model: "m"}); err != nil {
					t.Errorf("并发路由失败: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// 凭证过期的正确处置：先续期再重试，账号不该被禁用。
//
// 这是「每过一阵子这个渠道就全 401、只能重新授权」的根治点：
// token 过期是账号**好着**的号遇到了过期凭证，禁用等于把好号扔了。
func TestSessionDeadRefreshesAndRetries(t *testing.T) {
	kind := channel.QoderCN
	ch := &fakeChannel{kind: kind}
	ch.chat = func(c *channel.Credential) (channel.Stream, error) {
		if c.AccessToken == "stale" {
			return nil, errs.New(errs.SessionDead, "上游返回 HTTP 401").WithChannel(string(kind))
		}
		return &emptyStream{}, nil // 换上新 token 就成功
	}
	p := pool.New()
	p.AddFor(kind, channel.Credential{UID: "u1", AccessToken: "stale", RefreshToken: "rt"})
	var refreshed int
	p.SetRefresher(func(_ context.Context, k channel.Kind, c channel.Credential) (*channel.Credential, error) {
		refreshed++
		nc := c
		nc.AccessToken = "fresh"
		p.AddFor(k, nc)
		return &nc, nil
	})

	r := New(p, DefaultOptions())
	res, err := r.Route(context.Background(), ch, kind, "", channel.ChatRequest{})
	if err != nil {
		t.Fatalf("续期后应成功，实际 %v", err)
	}
	if res.Cred.AccessToken != "fresh" {
		t.Fatalf("应使用续期后的凭证，实际 %q", res.Cred.AccessToken)
	}
	if refreshed != 1 {
		t.Fatalf("应续期一次，实际 %d", refreshed)
	}
	if st, _ := p.Get(kind, "u1"); st.Disabled {
		t.Fatal("续期成功就不该把号禁用（用户不该被迫重新授权）")
	}
	if ch.count() != 2 {
		t.Fatalf("应是「失败 → 续期 → 重试」两次调用，实际 %d", ch.count())
	}
}

// 续期也救不回来（refresh_token 已失效）→ 禁用该号并把原因说清楚。
func TestSessionDeadWithoutRefreshDisables(t *testing.T) {
	kind := channel.QoderCN
	ch := &fakeChannel{kind: kind}
	ch.chat = func(*channel.Credential) (channel.Stream, error) {
		return nil, errs.New(errs.SessionDead, "上游返回 HTTP 401").WithChannel(string(kind))
	}
	p := pool.New()
	p.AddFor(kind, channel.Credential{UID: "u1", AccessToken: "stale", RefreshToken: "rt"})
	p.SetRefresher(func(context.Context, channel.Kind, channel.Credential) (*channel.Credential, error) {
		return nil, errs.New(errs.SessionDead, "refresh_token 已失效，需要重新授权")
	})

	r := New(p, DefaultOptions())
	_, err := r.Route(context.Background(), ch, kind, "", channel.ChatRequest{})
	if err == nil {
		t.Fatal("该报错")
	}
	st, _ := p.Get(kind, "u1")
	if !st.Disabled {
		t.Fatal("凭证彻底失效时应禁用该号（面板可见，提示重新登录）")
	}
	if st.Reason != string(errs.SessionDead) {
		t.Fatalf("禁用原因应记录错误分类，实际 %q", st.Reason)
	}
}

// 一个号凭证坏了、另一个号好的：必须换到好号上，而不是把错误直接甩给客户端。
func TestAuthFailureFailsOverToAnotherAccount(t *testing.T) {
	kind := channel.QoderCN
	ch := &fakeChannel{kind: kind}
	ch.chat = func(c *channel.Credential) (channel.Stream, error) {
		if c.UID == "bad" {
			return nil, errs.New(errs.AuthFailed, "上游返回 HTTP 401").WithChannel(string(kind))
		}
		return &emptyStream{}, nil
	}
	p := pool.New()
	p.AddFor(kind, channel.Credential{UID: "bad", AccessToken: "x"})
	p.SetBalance(kind, "bad", channel.Balance{Credits: 100, Known: true})
	p.AddFor(kind, channel.Credential{UID: "good", AccessToken: "y"})
	p.SetBalance(kind, "good", channel.Balance{Credits: 50, Known: true})

	r := New(p, DefaultOptions())
	res, err := r.Route(context.Background(), ch, kind, "", channel.ChatRequest{})
	if err != nil {
		t.Fatalf("应换到另一个账号成功，实际 %v", err)
	}
	if res.Cred.UID != "good" {
		t.Fatalf("应选到好号，实际 %s", res.Cred.UID)
	}
}

// fakeGate 记录闸门的取用与释放次数。
type fakeGate struct {
	mu       sync.Mutex
	acquired int
	released int
	err      error
}

func (g *fakeGate) Acquire(context.Context, channel.Kind, string) (func(), error) {
	if g.err != nil {
		return nil, g.err
	}
	g.mu.Lock()
	g.acquired++
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		g.released++
		g.mu.Unlock()
	}, nil
}

// 闸门必须在**流读尽或关闭时**释放：漏了会让账号被永久占住（表现为「这个号以后一直排队超时」，
// 比封号还难查）。这里把两条出口路径都钉住。
func TestRouterReleasesGate(t *testing.T) {
	for _, closeEarly := range []bool{true, false} {
		p := newPool(channel.QoderCN)
		ch := &fakeChannel{kind: channel.QoderCN, chat: func(*channel.Credential) (channel.Stream, error) {
			return &emptyStream{}, nil // 一个 chunk 都没有：读一次就 EOF
		}}
		gate := &fakeGate{}
		r := New(p, Options{MaxRetry: 0, StickyRequests: 1, Gate: gate})

		res, err := r.Route(context.Background(), ch, channel.QoderCN, "", channel.ChatRequest{Model: "m"})
		if err != nil {
			t.Fatalf("Route 失败: %v", err)
		}
		if closeEarly {
			_ = res.Stream.Close()
		} else {
			for {
				if _, err := res.Stream.Next(); err != nil {
					break
				}
			}
		}
		gate.mu.Lock()
		got := gate.released
		gate.mu.Unlock()
		if gate.acquired != 1 || got != 1 {
			t.Fatalf("closeEarly=%v 时闸门没释放：acquired=%d released=%d", closeEarly, gate.acquired, got)
		}
		// 再取一次：说明锁确实被放掉了（没放掉这里会等到 ctx 超时）
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		rel, err := gate.Acquire(ctx, channel.QoderCN, "A")
		if err != nil {
			t.Fatalf("闸门没真正释放: %v", err)
		}
		rel()
	}
}

// 取闸门失败（例如该账号排队超时）时不该调用上游，也不该把号冷却掉。
func TestRouterGateAcquireFailureSkipsUpstream(t *testing.T) {
	p := newPool(channel.QoderCN)
	called := false
	ch := &fakeChannel{kind: channel.QoderCN, chat: func(*channel.Credential) (channel.Stream, error) {
		called = true
		return &emptyStream{}, nil
	}}
	gate := &fakeGate{err: errs.New(errs.SoftRate, "排队超时")}
	r := New(p, Options{MaxRetry: 0, StickyRequests: 1, Gate: gate})
	if _, err := r.Route(context.Background(), ch, channel.QoderCN, "", channel.ChatRequest{Model: "m"}); err == nil {
		t.Fatal("取闸门失败时应返回错误")
	}
	if called {
		t.Fatal("取闸门失败时不该打到上游")
	}
}
