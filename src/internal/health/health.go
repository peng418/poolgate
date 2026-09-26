// Package health 实现健康探测与判据（需求 F5.1–F5.5，红线二）。
//
// 红线二：健康判据必须是「收到了有效内容」，不是 HTTP 200。
// wild-work 的实测教训：测速只看状态码，于是千问办公 3 个模型即使上游回
// 503 信封也显示「通过 ≤667ms」——面板全绿但实际不可用。
// 本包把判据收敛到唯一一处，测速与体检共用，避免两套逻辑打架（设计方案 §5.2）。
package health

import (
	"strings"
	"time"

	"poolgate/internal/errs"
)

// truncate 截断上游原话，避免把整段响应塞进面板与日志。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Verdict 是一次探测的结论。Reason 与 Upstream 必须能让用户看懂「为什么判它坏」。
type Verdict struct {
	OK        bool          // 是否通过
	Kind      string        // 失败分类（errs.Kind 的字符串形式）
	Reason    string        // 人话结论
	Upstream  string        // 上游原话摘要
	TTFT      time.Duration // 首字延迟；未通过时为 0
	CheckedAt time.Time
}

// Judge 是唯一的健康判据实现。
//
// 通过条件：收到了非空的有效内容，或正常收到流结束标记。
// 失败条件：空流 / 错误信封 / 超时 / 任何错误 —— 一律判失败并记录上游原话。
func Judge(text string, streamErr error, ttft time.Duration) Verdict {
	v := Verdict{CheckedAt: time.Now().UTC()}
	switch {
	case streamErr != nil:
		v.OK = false
		v.Kind = "Transport"
		v.Reason = "请求未能完成"
		v.Upstream = truncate(streamErr.Error(), 400)
		// 适配器已经归一过的错误不能被压平成 Transport：那会把「上游禁言」这类
		// 上游对账号的明确处置显示成「传输失败」，用户以为网络坏了，去查错方向。
		// 判据本身不变（仍然是不通过），只是分类与结论沿用适配器给的（红线二：看内容，不看状态码）。
		if ee, ok := errs.StructuredOf(streamErr); ok {
			v.Kind = string(ee.Kind)
			if ee.Message != "" {
				v.Reason = ee.Message
			}
		}
		return v

	case strings.TrimSpace(text) == "":
		// 空流是 wild-work 最典型的假绿来源：HTTP 200 + 0 字节曾被判为通过。
		v.OK = false
		v.Kind = "UpstreamFault"
		v.Reason = "收到响应但没有任何内容（空流），不判通过"
		return v

	default:
		v.OK = true
		v.TTFT = ttft
		return v
	}
}

// JudgeEnvelope 用于上游把错误包在 200 信封里的情况（千问办公就是这么干的）。
//
// errText 非空即视为失败 —— 不看 HTTP 状态码。
func JudgeEnvelope(errText string, ttft time.Duration) Verdict {
	v := Verdict{CheckedAt: time.Now().UTC()}
	if strings.TrimSpace(errText) == "" {
		v.OK = true
		v.TTFT = ttft
		return v
	}
	v.OK = false
	v.Kind = "UpstreamFault"
	v.Reason = "上游在成功状态码里返回了错误信封"
	v.Upstream = truncate(errText, 400)
	return v
}
