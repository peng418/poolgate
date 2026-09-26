package pool

import (
	"context"
	"testing"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

func cred(uid string, credits int64) channel.Credential {
	return channel.Credential{UID: uid, Nickname: uid}
}

// add 加入账号并设置余额。
func add(p *Pool, kind channel.Kind, uid string, credits int64) {
	p.AddFor(kind, cred(uid, credits))
	p.SetCredits(kind, uid, credits)
}

func TestPickHighestCredit(t *testing.T) {
	p := New()
	add(p, channel.QoderCN, "a", 100)
	add(p, channel.QoderCN, "b", 500)
	add(p, channel.QoderCN, "c", 300)

	c, ok := p.Pick(context.Background(), channel.QoderCN, nil)
	if !ok || c.UID != "b" {
		t.Fatalf("应选余额最高账号 b，got %v ok=%v", c.UID, ok)
	}
}

func TestPickSkipsTried(t *testing.T) {
	p := New()
	add(p, channel.QoderCN, "a", 100)
	add(p, channel.QoderCN, "b", 500)

	c, ok := p.Pick(context.Background(), channel.QoderCN, map[string]bool{"b": true})
	if !ok || c.UID != "a" {
		t.Fatalf("跳过已试账号后应选 a，got %v ok=%v", c.UID, ok)
	}
}

func TestCooldownExcludesAccount(t *testing.T) {
	p := New()
	add(p, channel.QoderCN, "a", 100)
	add(p, channel.QoderCN, "b", 500)

	p.Cooldown(channel.QoderCN, "b", time.Minute, "限流")

	c, ok := p.Pick(context.Background(), channel.QoderCN, nil)
	if !ok || c.UID != "a" {
		t.Fatalf("冷却中的 b 不应参与选号，应选 a，got %v ok=%v", c.UID, ok)
	}
}

func TestDisableExcludesAccount(t *testing.T) {
	p := New()
	add(p, channel.QoderCN, "a", 100)
	p.Disable(channel.QoderCN, "a", "session dead")

	if _, ok := p.Pick(context.Background(), channel.QoderCN, nil); ok {
		t.Fatal("禁用账号不应被选中")
	}
}

// 红线一：选号失败必须可定位（NoCandidate + 渠道），不是静默 nil。
func TestErrNoCandidateCarriesKind(t *testing.T) {
	p := New()
	err := p.ErrNoCandidate(channel.QoderCN)
	k, ok := errs.KindOf(err)
	if !ok || k != errs.NoCandidate {
		t.Fatalf("ErrNoCandidate 应归 NoCandidate，got %v ok=%v", k, ok)
	}
}

// 红线 D4：UpstreamFault 不计入账号错误，好号不被冷却。
func TestUpstreamFaultDoesNotCoolAccount(t *testing.T) {
	p := New()
	add(p, channel.QoderCN, "a", 100)
	p.NoteError(channel.QoderCN, "a", errs.UpstreamFault)

	// 账号仍可被选中 —— 上游故障不应把好号冷却掉。
	if _, ok := p.Pick(context.Background(), channel.QoderCN, nil); !ok {
		t.Fatal("UpstreamFault 不应冷却账号")
	}
}

func TestSessionDeadDisablesAccount(t *testing.T) {
	p := New()
	add(p, channel.QoderCN, "a", 100)
	p.NoteError(channel.QoderCN, "a", errs.SessionDead)

	if _, ok := p.Pick(context.Background(), channel.QoderCN, nil); ok {
		t.Fatal("SessionDead 应禁用账号")
	}
	if s := p.List(channel.QoderCN)[0]; !s.Disabled {
		t.Fatal("SessionDead 后账号应被标记 Disabled")
	}
}

func TestKindIsolation(t *testing.T) {
	p := New()
	add(p, channel.QoderCN, "a", 100)
	add(p, channel.WorkBuddyCN, "b", 500)

	if _, ok := p.Pick(context.Background(), channel.WorkBuddyCN, nil); !ok {
		t.Fatal("WorkBuddyCN 应能选到 b")
	}
	if c, ok := p.Pick(context.Background(), channel.QoderCN, nil); !ok || c.UID != "a" {
		t.Fatal("QoderCN 应能选到 a，渠道互不干扰")
	}
}

// 上游给了恢复时间点（禁言解禁）时，冷却必须**冷到那个点**，而不是拍一个固定时长；
// 并且禁言**不**禁用账号（Disable 只留给 SessionDead 那种真死号）。
func TestNoteErrorAtUsesUpstreamDeadline(t *testing.T) {
	p := New()
	add(p, channel.DeepSeek, "a", 100)
	add(p, channel.DeepSeek, "b", 500)

	p.NoteErrorAt(channel.DeepSeek, "b", errs.Muted, time.Now().Add(3*time.Hour))

	st, ok := p.Get(channel.DeepSeek, "b")
	if !ok {
		t.Fatal("账号 b 应还在池子里")
	}
	if st.Disabled {
		t.Fatal("禁言是临时的，不该被永久禁用")
	}
	if st.Reason != string(errs.Muted) {
		t.Fatalf("原因应记为 Muted，got %q", st.Reason)
	}
	// b 被冷到 3 小时后 → 选号只能选 a
	c, ok := p.Pick(context.Background(), channel.DeepSeek, nil)
	if !ok || c.UID != "a" {
		t.Fatalf("冷却中的 b 不该被选中，应选 a，got %v ok=%v", c.UID, ok)
	}
}

// 上游给的解禁时间比 Policy 的固定冷却长得多 → 以**上游**为准
// （禁言 3 天却只冷 30 分钟，等于每个请求都去撞一次枪口）。
func TestNoteErrorAtPrefersUpstreamDeadline(t *testing.T) {
	p := New()
	add(p, channel.DeepSeek, "a", 100)
	p.NoteErrorAt(channel.DeepSeek, "a", errs.Muted, time.Now().Add(72*time.Hour))
	st, ok := p.Get(channel.DeepSeek, "a")
	if !ok {
		t.Fatal("账号 a 应还在池子里")
	}
	if !st.Until.After(time.Now().Add(71 * time.Hour)) {
		t.Fatalf("冷却截止应在上游给的解禁时间（≈72h 后），got %s", st.Until)
	}
}
