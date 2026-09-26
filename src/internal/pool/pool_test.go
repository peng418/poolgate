package pool

import (
	"context"
	"strings"
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

// 0.9.8：503 的结论必须自带处置所需的事实（哪个号、等多久、要不要动手）—— 2026-09-27 事故回归。
// 0.9.9 补：推断类（软冷却）与上游裁决类（硬冷却）要说**两套话** —— 前者仍在被尝试，
// 后者必须等；否则用户会以为整条渠道都要等到冷却结束。
func TestErrNoCandidateExplainsEachAccount(t *testing.T) {
	p := New()
	kind := channel.QoderCN
	add(p, kind, "aaaa1111-2222-3333", 100)
	add(p, kind, "bbbb3333-4444-5555", 500)
	// 上游明确裁决类（限流）：硬冷却，到点自恢复。
	p.Cooldown(kind, "aaaa1111-2222-3333", 10*time.Minute, "SoftRate")
	p.Disable(kind, "bbbb3333-4444-5555", "SessionDead")

	msg := p.ErrNoCandidate(kind).Error()
	for _, want := range []string{"无可用账号", "aaaa1111", "冷却至", "到点自恢复", "bbbb3333", "SessionDead", "需重新授权"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("503 结论必须含 %q，实际：%s", want, msg)
		}
	}

	// 推断类（连接中断/空流）是软冷却：措辞要说明「仍在尝试使用」。
	p2 := New()
	add(p2, kind, "cccc5555-6666-7777", 100)
	p2.Cooldown(kind, "cccc5555-6666-7777", 10*time.Minute, "Transport")
	m2 := p2.ErrNoCandidate(kind).Error()
	for _, want := range []string{"cccc5555", "软冷却", "仍在尝试使用", "一次成功即自动解除"} {
		if !strings.Contains(m2, want) {
			t.Fatalf("软冷却结论必须含 %q，实际：%s", want, m2)
		}
	}
	if strings.Contains(m2, "到点自恢复") {
		t.Fatalf("软冷却不该说「到点自恢复」（它不等冷却结束也能用），实际：%s", m2)
	}

	// 空池也要说清「池里没账号」，而不是只丢一句「无可用账号」。
	if m := New().ErrNoCandidate(kind).Error(); !strings.Contains(m, "没有该渠道的账号") {
		t.Fatalf("空池结论应说明池里没账号，实际：%s", m)
	}
}

// 0.9.9 ①③⑤：推断类错误的完整纪律。
//
//	① 单次抖动只计数，不冷却（门槛 = 连续 N 次）；
//	③ 单账号渠道连门槛也不触发（冷唯一那个号 = 整条渠道下线）；
//	⑤ 达到门槛后是「软冷却」：仍可被选用（排在健康号之后），一次成功即自动解除。
func TestInferredErrorThresholdAndSoftCooldown(t *testing.T) {
	kind := channel.QoderCN
	ctx := context.Background()

	// ① 多账号渠道：连续错误要攒到门槛才冷。
	p := New()
	add(p, kind, "a", 100)
	add(p, kind, "b", 200)
	p.SetErrorPolicy(3, time.Minute)
	for i := 1; i <= 2; i++ {
		p.NoteErrorAt(kind, "a", errs.Transport, time.Time{})
		st, _ := p.Get(kind, "a")
		if !st.Until.IsZero() {
			t.Fatalf("第 %d 次推断类错误不该冷却（门槛 3 次），实际冷却至 %s", i, st.Until)
		}
		if st.ErrCount != i {
			t.Fatalf("连续错误计数应为 %d，实际 %d", i, st.ErrCount)
		}
	}
	p.NoteErrorAt(kind, "a", errs.Transport, time.Time{})
	st, _ := p.Get(kind, "a")
	if st.Until.IsZero() {
		t.Fatal("达到门槛（3 次）后应当冷却")
	}
	if st.Reason != string(errs.Transport) {
		t.Fatalf("冷却原因应记录触发的错误类型，实际 %q", st.Reason)
	}
	if st.ErrCount != 0 {
		t.Fatalf("冷却后连续计数应清零（下一轮重新攒），实际 %d", st.ErrCount)
	}

	// ⑤ 软冷却：健康号优先，但它仍然可用（否则一次抖动就等于整条渠道停摆到冷却结束）。
	cred, ok := p.Pick(ctx, kind, nil)
	if !ok || cred.UID != "b" {
		t.Fatalf("软冷却期间应优先选健康号 b，实际 %v %v", cred.UID, ok)
	}
	if cred, ok := p.Pick(ctx, kind, map[string]bool{"b": true}); !ok || cred.UID != "a" {
		t.Fatalf("健康号都试过后，软冷却的号必须还能兜底，实际 %v %v", cred.UID, ok)
	}
	// 一次成功即解除（⑤）。
	p.NoteSuccess(kind, "a")
	if st, _ := p.Get(kind, "a"); !st.Until.IsZero() {
		t.Fatalf("成功后应立刻解除推断类冷却，实际仍冷却至 %s", st.Until)
	}

	// 上游裁决类不吃这一套：限流冷却必须等（软冷却/提前解除都不适用）。
	p2 := New()
	add(p2, kind, "a", 100)
	add(p2, kind, "b", 200)
	p2.NoteErrorAt(kind, "a", errs.SoftRate, time.Time{})
	if p2.States(kind)[0].SoftCooling {
		// a 排在前面（UID 排序），且限流是硬冷却 → SoftCooling 必须为 false。
		t.Fatal("限流（上游裁决类）不是软冷却：必须等上游放行")
	}
	if cred, _ := p2.Pick(ctx, kind, map[string]bool{"b": true}); cred.UID != "" {
		t.Fatalf("硬冷却的号不该被兜底选中，实际 %s", cred.UID)
	}
	p2.NoteSuccess(kind, "a")
	if st, _ := p2.Get(kind, "a"); st.Until.IsZero() {
		t.Fatal("上游裁决类冷却不许被一次成功提前解除")
	}
}

// ③ 单账号渠道纪律：唯一的号不会被推断类错误冷却（冷它 = 整条渠道下线）。
func TestSingleAccountChannelNeverCooledByInferredError(t *testing.T) {
	kind := channel.QoderCN
	p := New()
	add(p, kind, "only", 100)
	p.SetErrorPolicy(1, time.Minute) // 门槛压到 1，最容易触发
	for i := 0; i < 5; i++ {
		p.NoteErrorAt(kind, "only", errs.Transport, time.Time{})
	}
	st, _ := p.Get(kind, "only")
	if !st.Until.IsZero() {
		t.Fatalf("单账号渠道不该因推断类错误被冷却，实际冷却至 %s", st.Until)
	}
	if _, ok := p.Pick(context.Background(), kind, nil); !ok {
		t.Fatal("单账号渠道的号必须始终可用")
	}
	// 但上游明确裁决类照旧生效（429/402 是上游说的真话）。
	p.NoteErrorAt(kind, "only", errs.SoftRate, time.Time{})
	if st, _ := p.Get(kind, "only"); st.Until.IsZero() {
		t.Fatal("单账号渠道也要尊重上游的限流处置")
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
