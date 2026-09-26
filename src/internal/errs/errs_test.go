package errs

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestUpstreamFaultNotBlamedOnAccount(t *testing.T) {
	// 千问办公的 503 是渠道级故障；若计入账号错误会把好号也冷却掉。
	if UpstreamFault.AccountBlamed() {
		t.Fatal("UpstreamFault 不得计入账号错误")
	}
}

func TestPassthroughKindsNotBlamed(t *testing.T) {
	// 内容拦截与超长是用户输入导致的，冷却账号没有意义。
	for _, k := range []Kind{ContentBlocked, PromptTooLong} {
		if k.AccountBlamed() {
			t.Fatalf("%s 不应计入账号错误", k)
		}
	}
}

// 2026-09-27 事故回归 + 0.9.9 ①②：连接中断要**计数**（否则一个一直在断的号永远记不下来），
// 但单次不得冷却账号（门槛在 pool 里：连续 ErrThreshold 次才冷，成功即清零），换号仍值得试。
func TestTransportCountedButNotCooledByPolicy(t *testing.T) {
	if !Transport.AccountBlamed() {
		t.Fatal("连接中断应计入账号错误计数（0.9.9 ②：从「豁免」改回「门槛」）")
	}
	if Transport.StopRetry() {
		t.Fatal("连接中断应当允许换号重试（别的号可能通），不该直接甩给客户端")
	}
	if p := PolicyOf(Transport); p.Cooldown != 0 {
		t.Fatalf("连接中断的冷却时长由连续错误门槛给（err_cooldown），Policy 不该再带固定冷却，实际 %s", p.Cooldown)
	}
	if !Transport.Inferred() {
		t.Fatal("连接中断是「本地推断类」：单次不足以判定账号坏")
	}
	// 空流（Parse）同为推断类：也要计数、也要过门槛，不再是「一错即冷 10 分钟」。
	if !Parse.AccountBlamed() || !Parse.Inferred() {
		t.Fatal("Parse（上游 200 但空流）应计入账号错误且属推断类")
	}
	if p := PolicyOf(Parse); p.Cooldown != 0 {
		t.Fatalf("Parse 的冷却应走连续错误门槛，实际 Policy 冷却 %s", p.Cooldown)
	}
	// 上游明确裁决类不走门槛：它们立即生效，也不接受「探通了提前放开」。
	for _, k := range []Kind{HardCredit, SoftRate, Muted, SessionDead, ModelUnavailable} {
		if k.Inferred() {
			t.Fatalf("%s 是上游明确裁决，不该被当成推断类（会被提前解除）", k)
		}
	}
	for _, k := range []Kind{UpstreamFault, ContentBlocked, PromptTooLong, NoCandidate} {
		if !k.StopRetry() {
			t.Fatalf("%s 换号没意义，应当把真实错误直接交给客户端", k)
		}
	}
	if !SessionDead.AccountBlamed() || SessionDead.StopRetry() {
		t.Fatal("SessionDead 仍是账号错误且换号有意义（凭证类失败先续期再换号）")
	}
}

// 并发冲突（SessionBusy）：上游同一账号的「会话启动」还没落地 —— 2 秒级瞬态闸门。
// 必须：① 不计入账号错误（否则换一次 60 秒冷却，单号渠道=整渠道冻结）；
// ② 不阻断重试（等一两秒重试同号即可 / 有别的号就换号）。
func TestSessionBusyNotBlamedAndRetryable(t *testing.T) {
	if SessionBusy.AccountBlamed() {
		t.Fatal("并发冲突不是这个号的错，不得冷却账号（真机实测 2 秒后即 201）")
	}
	if SessionBusy.StopRetry() {
		t.Fatal("并发冲突应当允许重试（同号退避重试或换号），不该直接甩给客户端")
	}
	p := PolicyOf(SessionBusy)
	if p.Cooldown != 0 {
		t.Fatalf("并发冲突不得带冷却，实际 %s", p.Cooldown)
	}
	if !p.Retry {
		t.Fatal("并发冲突应当允许重试")
	}
	if p.Disable {
		t.Fatal("并发冲突绝不能禁用账号")
	}
}

func TestPolicyMatchesDesignDoc(t *testing.T) {
	// 与 docs/02-项目设计方案.md §4 的表一一对应。
	cases := map[Kind]struct {
		cd           time.Duration
		disable      bool
		passthrough  bool
		blameChannel bool
		retry        bool
	}{
		HardCredit:       {cd: 12 * time.Hour, retry: true},
		SoftRate:         {cd: 60 * time.Second, retry: true},
		SessionDead:      {disable: true},
		ContentBlocked:   {passthrough: true},
		PromptTooLong:    {passthrough: true},
		ModelUnavailable: {cd: 10 * time.Minute, retry: true},
		UpstreamFault:    {cd: 5 * time.Minute, blameChannel: true, retry: true},
		// 0.9.9 ①②：连接中断/空流是**推断类** → 冷却时长由 pool 的连续错误门槛给
		// （err_cooldown），Policy 里不带固定冷却；换号仍然有意义。
		Transport: {retry: true},
		Parse:     {retry: true},
		// 并发冲突：瞬态闸门，不冷却（适配器内部退避重试）。
		SessionBusy: {retry: true},
	}
	for k, want := range cases {
		got := PolicyOf(k)
		if got.Cooldown != want.cd || got.Disable != want.disable ||
			got.Passthrough != want.passthrough || got.BlameChannel != want.blameChannel ||
			got.Retry != want.retry {
			t.Errorf("%s: got %+v want %+v", k, got, want)
		}
	}
}

func TestErrorKeepsUpstream(t *testing.T) {
	e := New(UpstreamFault, "上游不可用").
		WithUpstream("503 Model catalog unavailable").
		WithChannel("qwenwork").
		WithAccount("u-demo-1")

	if e.Upstream == "" {
		t.Fatal("错误必须能携带上游原话（可见性契约）")
	}
	msg := e.Error()
	if !contains(msg, "503") || !contains(msg, "UpstreamFault") {
		t.Fatalf("错误信息不完整: %q", msg)
	}
}

func TestUnwrapPreservesCause(t *testing.T) {
	base := errors.New("boom")
	e := New(Transport, "连不上").WithCause(base)
	if !errors.Is(e, base) {
		t.Fatal("应保留原始错误以便排查")
	}
}

func TestKindOfUnknownErrorIsParse(t *testing.T) {
	// 关键：未知错误不得被当成成功 —— 这曾是 wild-work 静默失败的成因。
	k, ok := KindOf(fmt.Errorf("something weird"))
	if !ok {
		t.Fatal("未知错误必须被识别，不能返回 false 让上层当成功处理")
	}
	if k != Parse {
		t.Fatalf("未知错误应归为 Parse，实际 %q", k)
	}
}

func TestKindOfNil(t *testing.T) {
	if _, ok := KindOf(nil); ok {
		t.Fatal("nil 不应被识别为错误")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

// 禁言（Muted）是上游对**这个号**的风控处置，且带解禁时间：
// 算账号错误（池里有别的号就该换号），但**不是**凭证问题（重登解不开、续期白敲），
// 也**不**禁用（临时状态，到点自恢复）。这条锁的就是 0.9.5 修的误判。
func TestMutedIsAccountBlamedNotCredentialNotDisabled(t *testing.T) {
	if !Muted.AccountBlamed() {
		t.Fatal("禁言是这个号的状态 → 应算账号错误（否则池子会反复选它去撞枪口）")
	}
	if CredentialKind(Muted) {
		t.Fatal("禁言不是凭证问题：重新登录解不开，走续期只会白敲一次 token 端点")
	}
	pol := PolicyOf(Muted)
	if pol.Disable {
		t.Fatal("禁言是临时的（上游给了 mute_until）→ 不许永久禁用账号")
	}
	if !pol.Retry {
		t.Fatal("池里还有别的号 → Policy.Retry 应为 true")
	}
	if !pol.Passthrough {
		t.Fatal("上游的处置结果要原话透传给用户 → Passthrough 应为 true")
	}
}

// 禁言必须带得出「解禁时间点」：面板与客户端都要看到它，不能只写一个固定冷却。
func TestMutedCarriesRetryAt(t *testing.T) {
	until := time.Date(2026, 9, 29, 21, 50, 38, 0, time.Local)
	err := New(Muted, "账号被上游禁言").WithRetryAt(until)
	got, ok := RetryAtOf(err)
	if !ok || !got.Equal(until) {
		t.Fatalf("RetryAt 应带出解禁时间：%v ok=%v", got, ok)
	}
	if _, ok := RetryAtOf(New(Muted, "上游没给时间")); ok {
		t.Fatal("上游没给时间时 RetryAtOf 应返回 false（按固定冷却兜底），不许编一个")
	}
}
