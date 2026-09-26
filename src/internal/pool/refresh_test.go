package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"poolgate/internal/channel"
)

func expiring(uid string, in time.Duration) channel.Credential {
	return channel.Credential{
		UID: uid, AccessToken: "old-" + uid, RefreshToken: "rt-" + uid,
		ExpiresAt: time.Now().Add(in),
	}
}

// 一次性 refresh token 的渠道靠「渠道声明的节奏」主动续期活着，但绝不能每请求都续。
// 2026-09-27 事故回归：千问办公 42 秒被续了 4 次，链断了。
func TestRefreshCadenceKeepsRotatingTokenWarm(t *testing.T) {
	oldGap := refreshSoftGap
	refreshSoftGap = time.Millisecond // 测试里把兜底间隔缩短，节奏本身用 30ms
	defer func() { refreshSoftGap = oldGap }()

	p := New()
	p.SetRefreshCadence(func(k channel.Kind) time.Duration {
		if k == channel.QwenWork {
			return 30 * time.Millisecond
		}
		return 0
	})
	// 远未到期：老规矩（只认临期）下不会被续。
	p.AddFor(channel.QwenWork, channel.Credential{UID: "a", AccessToken: "t0",
		RefreshToken: "rt-a", ExpiresAt: time.Now().Add(48 * time.Hour)})
	var calls int32
	p.SetRefresher(func(_ context.Context, kind channel.Kind, c channel.Credential) (*channel.Credential, error) {
		atomic.AddInt32(&calls, 1)
		nc := c
		nc.AccessToken = c.AccessToken + "+"
		p.AddFor(kind, nc)
		return &nc, nil
	})

	if _, ok := p.Pick(context.Background(), channel.QwenWork, nil); !ok {
		t.Fatal("应选到账号")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("首次取号该把链接上（续 1 次），实际 %d", n)
	}
	if _, ok := p.Pick(context.Background(), channel.QwenWork, nil); !ok {
		t.Fatal("应选到账号")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("刚续过不该再敲上游（每请求续期是自伤），实际 %d", n)
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := p.Pick(context.Background(), channel.QwenWork, nil); !ok {
		t.Fatal("应选到账号")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("过了渠道节奏应再续一次（否则闲置的 rt 会失效），实际 %d", n)
	}
}

// 后台心跳只碰「声明了节奏」的渠道，且到点才续 —— 别给不需要的渠道平白加上游调用。
func TestRefreshSweepOnlyTouchesCadenceChannels(t *testing.T) {
	p := New()
	p.SetRefreshCadence(func(k channel.Kind) time.Duration {
		if k == channel.QwenWork {
			return time.Minute
		}
		return 0
	})
	p.AddFor(channel.QwenWork, channel.Credential{UID: "qw", AccessToken: "t-qw",
		RefreshToken: "rt-qw", ExpiresAt: time.Now().Add(time.Hour)})
	p.AddFor(channel.QoderCN, channel.Credential{UID: "qd", AccessToken: "t-qd",
		RefreshToken: "rt-qd", ExpiresAt: time.Now().Add(time.Hour)})
	var calls int32
	p.SetRefresher(func(_ context.Context, kind channel.Kind, c channel.Credential) (*channel.Credential, error) {
		atomic.AddInt32(&calls, 1)
		nc := c
		nc.AccessToken = c.AccessToken + "2"
		p.AddFor(kind, nc)
		return &nc, nil
	})

	if n := p.RefreshSweep(context.Background()); n != 1 {
		t.Fatalf("心跳应只续声明了节奏的渠道（1 个），实际 %d", n)
	}
	if n := p.RefreshSweep(context.Background()); n != 0 {
		t.Fatalf("节奏未到不该重复续，实际 %d", n)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("续期函数应被调用 1 次，实际 %d", got)
	}
}

// 临期凭证在选号时就被换成新的：这是「昨天还能用、今天全是 401」的根治点。
func TestPickRefreshesExpiringCredential(t *testing.T) {
	p := New()
	p.AddFor(channel.QoderCN, expiring("a", -time.Minute)) // 已过期
	var calls int
	p.SetRefresher(func(_ context.Context, kind channel.Kind, c channel.Credential) (*channel.Credential, error) {
		calls++
		nc := c
		nc.AccessToken = "new-" + c.UID
		nc.ExpiresAt = time.Now().Add(48 * time.Hour)
		p.AddFor(kind, nc) // 装配层就是这么回写的
		return &nc, nil
	})
	cred, ok := p.Pick(context.Background(), channel.QoderCN, nil)
	if !ok {
		t.Fatal("应选到账号")
	}
	if cred.AccessToken != "new-a" {
		t.Fatalf("过期凭证应被续期，实际 %q", cred.AccessToken)
	}
	if calls != 1 {
		t.Fatalf("应恰好续期一次，实际 %d", calls)
	}
	// 续期后池内状态也要更新：否则下一次 Pick 又拿到旧 token。
	if st, _ := p.Get(channel.QoderCN, "a"); st.Cred.AccessToken != "new-a" {
		t.Fatalf("池内凭证未回写：%q", st.Cred.AccessToken)
	}
}

// 没过期的凭证不该白白去打上游 token 端点。
func TestPickDoesNotRefreshFreshCredential(t *testing.T) {
	p := New()
	p.AddFor(channel.QoderCN, expiring("a", 48*time.Hour))
	var calls int
	p.SetRefresher(func(_ context.Context, _ channel.Kind, c channel.Credential) (*channel.Credential, error) {
		calls++
		return &c, nil
	})
	if _, ok := p.Pick(context.Background(), channel.QoderCN, nil); !ok {
		t.Fatal("应选到账号")
	}
	if calls != 0 {
		t.Fatalf("没过期不该续期，实际调用 %d 次", calls)
	}
}

// 没有 refresh token（如从 wild-work 导入的老凭证）时不去敲上游，也不报错。
func TestPickWithoutRefreshTokenSkipsRefresh(t *testing.T) {
	p := New()
	p.AddFor(channel.QoderCN, channel.Credential{UID: "a", AccessToken: "dt-old", ExpiresAt: time.Now().Add(-time.Hour)})
	var calls int
	p.SetRefresher(func(_ context.Context, _ channel.Kind, c channel.Credential) (*channel.Credential, error) {
		calls++
		return &c, nil
	})
	cred, ok := p.Pick(context.Background(), channel.QoderCN, nil)
	if !ok || cred.AccessToken != "dt-old" {
		t.Fatal("没有 refresh token 时应原样返回旧凭证")
	}
	if calls != 0 {
		t.Fatal("没有 refresh token 不该调用续期")
	}
}

// 续期失败：原样返回旧凭证（不吞错也不抛错），且失败后短时间内不重复敲上游。
func TestRefreshFailureCooldown(t *testing.T) {
	p := New()
	p.AddFor(channel.QoderCN, expiring("a", -time.Hour))
	var calls int32
	p.SetRefresher(func(_ context.Context, _ channel.Kind, c channel.Credential) (*channel.Credential, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("invalid_grant")
	})
	for i := 0; i < 5; i++ {
		cred, ok := p.Pick(context.Background(), channel.QoderCN, nil)
		if !ok || cred.AccessToken != "old-a" {
			t.Fatalf("续期失败时应原样返回旧凭证，实际 %q", cred.AccessToken)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("失败后应进冷却期，最多敲一次上游，实际 %d 次", n)
	}
}

// RefreshNow 不看 expiry（上游回了 401 但本地以为没过期时用），并返回新凭证。
func TestRefreshNowIgnoresExpiry(t *testing.T) {
	p := New()
	p.AddFor(channel.QoderCN, expiring("a", 48*time.Hour))
	p.SetRefresher(func(_ context.Context, _ channel.Kind, c channel.Credential) (*channel.Credential, error) {
		nc := c
		nc.AccessToken = "forced-new"
		return &nc, nil
	})
	nc, err := p.RefreshNow(context.Background(), channel.QoderCN, expiring("a", 48*time.Hour))
	if err != nil || nc == nil || nc.AccessToken != "forced-new" {
		t.Fatalf("RefreshNow 应强制续期，got %v %v", nc, err)
	}
}

// 适配器表示「刷不出新东西」（返回同一 token）时，RefreshNow 返回 nil，
// 调用方按 401 处理 —— 不能假装刷新成功。
func TestRefreshNowNoChangeMeansNil(t *testing.T) {
	p := New()
	p.SetRefresher(func(_ context.Context, _ channel.Kind, c channel.Credential) (*channel.Credential, error) {
		return &c, nil // 原样返回
	})
	nc, err := p.RefreshNow(context.Background(), channel.QoderCN, expiring("a", 0))
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if nc != nil {
		t.Fatal("没换出 token 时应返回 nil，让调用方按 401 处理")
	}
}

// 未装配续期能力时一切照旧（测试与非常规启动路径）。
func TestNoRefresherIsNoop(t *testing.T) {
	p := New()
	c := expiring("a", -time.Hour)
	p.AddFor(channel.QoderCN, c)
	cred, ok := p.Pick(context.Background(), channel.QoderCN, nil)
	if !ok || cred.AccessToken != c.AccessToken {
		t.Fatal("未装配续期时应原样返回")
	}
	if nc, err := p.RefreshNow(context.Background(), channel.QoderCN, c); nc != nil || err != nil {
		t.Fatalf("未装配续期时 RefreshNow 应返回 (nil, nil)，实际 %v %v", nc, err)
	}
}

// 并发选号同一账号时只续期一次（不能每个请求都把 token 端点敲一遍）。
func TestRefreshSingleFlightPerAccount(t *testing.T) {
	p := New()
	p.AddFor(channel.QoderCN, expiring("a", -time.Hour))
	var calls int32
	p.SetRefresher(func(_ context.Context, kind channel.Kind, c channel.Credential) (*channel.Credential, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond) // 模拟网络往返
		nc := c
		nc.AccessToken = "new-a"
		nc.ExpiresAt = time.Now().Add(48 * time.Hour)
		p.AddFor(kind, nc)
		return &nc, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Pick(context.Background(), channel.QoderCN, nil)
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&calls); n > 2 {
		t.Fatalf("并发续期应基本只做一次，实际 %d 次（每请求都刷会打死上游）", n)
	}
}
