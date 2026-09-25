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
