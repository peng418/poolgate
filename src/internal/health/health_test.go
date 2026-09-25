package health

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 这些用例是红线二（健康判据必须是「收到有效内容」）的回归测试。
// wild-work 的假绿就是「HTTP 200 即通过」造成的，这里逐条钉死。
func TestJudgeEmptyStreamIsFailure(t *testing.T) {
	// HTTP 200 + 0 字节曾被判为通过 —— 这是千问办公假绿的直接成因。
	v := Judge("", nil, 0)
	if v.OK {
		t.Fatal("空流被判为通过，健康判据失效")
	}
	if v.Kind != "UpstreamFault" {
		t.Fatalf("空流应归为 UpstreamFault，实际 %q", v.Kind)
	}
	if v.Reason == "" {
		t.Fatal("失败结论必须带原因（F5.1）")
	}
}
func TestJudgeWhitespaceOnlyIsFailure(t *testing.T) {
	if v := Judge("   \n\t ", nil, 0); v.OK {
		t.Fatal("只有空白字符也应判失败")
	}
}
func TestJudgeErrorIsFailureWithUpstream(t *testing.T) {
	// 失败必须带上上游原话，否则用户无从判断是本地还是上游问题。
	v := Judge("", errors.New("dial tcp: i/o timeout"), 0)
	if v.OK {
		t.Fatal("出错不应判通过")
	}
	if !strings.Contains(v.Upstream, "timeout") {
		t.Fatalf("应保留上游原话，实际 %q", v.Upstream)
	}
}
func TestJudgeContentIsSuccess(t *testing.T) {
	v := Judge("你好", nil, 1200*time.Millisecond)
	if !v.OK {
		t.Fatalf("收到内容应判通过，实际 %v", v.Reason)
	}
	if v.TTFT != 1200*time.Millisecond {
		t.Fatalf("TTFT 未记录: %v", v.TTFT)
	}
}
func TestJudgeEnvelope(t *testing.T) {
	// 上游把错误包在 200 信封里是最常见的欺骗形态。
	if v := JudgeEnvelope("Model catalog unavailable", 0); v.OK {
		t.Fatal("错误信封必须判失败")
	}
	if v := JudgeEnvelope("", 800*time.Millisecond); !v.OK {
		t.Fatal("无错误信封且有 TTFT 应判通过")
	}
}
