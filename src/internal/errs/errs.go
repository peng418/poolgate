// Package errs 定义 PoolGate 的统一错误模型。
//
// 设计红线（需求 D1/D4）：
//  1. 任何上游异常必须客户端可见，并落一条可定位的流水 —— 禁止静默丢弃。
//  2. 上游千奇百怪的错误在适配器内归一成有限枚举，路由层与 UI 只认枚举。
//  3. UpstreamFault 与账号健康解耦：上游故障不得计入账号错误，否则会把好号冷却掉
//     （wild-work 的实测教训，见 docs/03-渠道能力矩阵.md §4）。
package errs

import "errors"

// Kind 是有限的错误分类枚举。新增分类必须同时更新 DefaultPolicy 与前端文案。
type Kind string

const (
	HardCredit       Kind = "HardCredit"       // 余额/权益不足
	SoftRate         Kind = "SoftRate"         // 限流（429）
	SessionDead      Kind = "SessionDead"      // 会话/签名失效（401、Signature invalid）
	ContentBlocked   Kind = "ContentBlocked"   // 内容拦截
	PromptTooLong    Kind = "PromptTooLong"    // 超出上下文窗口
	ModelUnavailable Kind = "ModelUnavailable" // 该账号无此模型权限
	UpstreamFault    Kind = "UpstreamFault"    // 上游 5xx / 闸门 / 目录不可用
	Transport        Kind = "Transport"        // 连不通 / TLS / 超时
	Parse            Kind = "Parse"            // 响应无法解析
	AuthFailed       Kind = "AuthFailed"       // 控制台登录失败
	NoCandidate      Kind = "NoCandidate"      // 池内无可用账号
)

// String 让 Kind 可直接用于日志与 JSON。
func (k Kind) String() string { return string(k) }

// AccountBlamed 报告该错误是否应计入账号错误。
//
// 这是 UpstreamFault 解耦的落点：上游故障时账号是好的，冷却账号等于误杀。
func (k Kind) AccountBlamed() bool {
	switch k {
	case UpstreamFault, ContentBlocked, PromptTooLong, NoCandidate, AuthFailed:
		return false
	default:
		return true
	}
}

// Error 是贯穿全链路的结构化错误。upstream 字段保存上游原话摘要，
// 供面板与客户端展示 —— 「失败必带原因」是硬性验收项（F1.2a / 可见性契约）。
type Error struct {
	Kind     Kind   // 归一后的分类
	Message  string // 给人类看的结论
	Upstream string // 上游原话摘要（可为空，但账户类错误必须有）
	Channel  string // 渠道标识（可为空）
	Account  string // 账号 uid（可为空）
	Cause    error  // 原始错误（不对外暴露）
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	s := string(e.Kind)
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Upstream != "" {
		s += " (upstream: " + e.Upstream + ")"
	}
	return s
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// New 构造一个结构化错误。upstream 为空时客户端仍能看到 kind 与 message。
func New(k Kind, msg string) *Error { return &Error{Kind: k, Message: msg} }

// WithUpstream 附上上游原话。返回自身以便链式调用。
func (e *Error) WithUpstream(u string) *Error { e.Upstream = u; return e }

// WithChannel 标注渠道。
func (e *Error) WithChannel(c string) *Error { e.Channel = c; return e }

// WithAccount 标注账号。
func (e *Error) WithAccount(a string) *Error { e.Account = a; return e }

// WithCause 保留原始错误。
func (e *Error) WithCause(c error) *Error { e.Cause = c; return e }

// WithMessage 覆盖给人类看的结论（保留 Kind 与上游原话）。
//
// 用在「同一个 Kind，但对用户要说的话不同」的场合：比如上游凭证失效，
// 适配器只说得出「上游返回 HTTP 401」，而面板该补一句「去哪个页面点什么」。
func (e *Error) WithMessage(msg string) *Error { e.Message = msg; return e }

// KindOf 从任意 error 中取出 Kind；无法识别时返回 Parse（而不是静默当成功）。
//
// 「无法识别就当成功」正是 wild-work 静默失败的成因，这里显式取反。
func KindOf(err error) (Kind, bool) {
	var e *Error
	if err == nil {
		return "", false
	}
	if errors.As(err, &e) {
		return e.Kind, true
	}
	return Parse, true
}
