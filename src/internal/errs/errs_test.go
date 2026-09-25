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

func TestPolicyMatchesDesignDoc(t *testing.T) {
	// 与 docs/02-项目设计方案.md §4 的表一一对应。
	cases := map[Kind]struct {
		cd           time.Duration
		disable      bool
		passthrough  bool
		blameChannel bool
	}{
		HardCredit:       {cd: 12 * time.Hour},
		SoftRate:         {cd: 60 * time.Second},
		SessionDead:      {disable: true},
		ContentBlocked:   {passthrough: true},
		PromptTooLong:    {passthrough: true},
		ModelUnavailable: {cd: 10 * time.Minute},
		UpstreamFault:    {cd: 5 * time.Minute, blameChannel: true},
		Transport:        {cd: 10 * time.Minute},
		Parse:            {cd: 10 * time.Minute},
	}
	for k, want := range cases {
		got := PolicyOf(k)
		if got.Cooldown != want.cd || got.Disable != want.disable ||
			got.Passthrough != want.passthrough || got.BlameChannel != want.blameChannel {
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
