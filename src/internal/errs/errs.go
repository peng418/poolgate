// Package errs 定义 PoolGate 的统一错误模型。
//
// 设计红线（需求 D1/D4）：
//  1. 任何上游异常必须客户端可见，并落一条可定位的流水 —— 禁止静默丢弃。
//  2. 上游千奇百怪的错误在适配器内归一成有限枚举，路由层与 UI 只认枚举。
//  3. UpstreamFault 与账号健康解耦：上游故障不得计入账号错误，否则会把好号冷却掉
//     （wild-work 的实测教训，见 docs/03-渠道能力矩阵.md §4）。
package errs

import (
	"errors"
	"time"
)

// Kind 是有限的错误分类枚举。新增分类必须同时更新 DefaultPolicy 与前端文案。
type Kind string

const (
	HardCredit       Kind = "HardCredit"       // 余额/权益不足
	SoftRate         Kind = "SoftRate"         // 限流（429）
	SessionDead      Kind = "SessionDead"      // 会话/签名失效（401、Signature invalid）
	Muted            Kind = "Muted"            // 账号被上游禁言（风控/违规处置，有解禁时间）
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
//
// Muted 反过来：禁言是**这个号**的状态（池里有别的号就该换号），所以算账号错误，
// 并按上游给的解禁时间冷却它。
func (k Kind) AccountBlamed() bool {
	switch k {
	case UpstreamFault, ContentBlocked, PromptTooLong, NoCandidate, AuthFailed:
		return false
	default:
		return true
	}
}

// CredentialKind 报告该错误是不是「这个账号的凭证不行了」。
//
// SessionDead 已经算账号错误；AuthFailed 故意不算（同一个 Kind 还用于控制台登录失败），
// 但在**用凭证去打上游**的路径上（对话路由、健康探测），它同样意味着这个号的凭证不通 ——
// 该续期、该换号，而不是把错误直接甩给客户端。
//
// Muted（上游禁言）**故意不算**凭证问题：禁言是上游对这个号的风控处置，重新登录解不开，
// 续期只会白敲一次 token 端点。官方客户端收到 MUTED 时也只置 isMuted/muteUntil 标记，
// 既不掉登录也不跳重登页（包里的 onMuted 与 onTokenInvalid/onIsBanned 是两条路）。
//
// 单点定义：router.Route 与 health.Runner 的换号纪律共用这一条判据，别各写一份。
func CredentialKind(k Kind) bool { return k == SessionDead || k == AuthFailed }

// Error 是贯穿全链路的结构化错误。upstream 字段保存上游原话摘要，
// 供面板与客户端展示 —— 「失败必带原因」是硬性验收项（F1.2a / 可见性契约）。
type Error struct {
	Kind     Kind   // 归一后的分类
	Message  string // 给人类看的结论
	Upstream string // 上游原话摘要（可为空，但账户类错误必须有）
	Channel  string // 渠道标识（可为空）
	Account  string // 账号 uid（可为空）
	Cause    error  // 原始错误（不对外暴露）
	// RetryAt 是上游给出的恢复时间点（如禁言解禁时间）。零值表示上游没说，
	// 此时按 Policy 的固定冷却处理；pool.NoteErrorAt 优先用它。
	RetryAt time.Time
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

// WithRetryAt 附上上游给出的恢复时间点（如「禁言至」）。零值表示上游没说。
func (e *Error) WithRetryAt(t time.Time) *Error { e.RetryAt = t; return e }

// RetryAtOf 取出错误里上游给的恢复时间点；没有或为零值时 ok 为 false。
//
// 冷却时长必须以上游的话为准：拿固定冷却去猜禁言时长，要么白等、要么到点又去撞枪口。
func RetryAtOf(err error) (time.Time, bool) {
	var e *Error
	if err != nil && errors.As(err, &e) && !e.RetryAt.IsZero() {
		return e.RetryAt, true
	}
	return time.Time{}, false
}

// StructuredOf 取出适配器归一过的结构化错误；普通 error 返回 false。
//
// 与 KindOf 的区别：KindOf 对普通 error 会保守地给 Parse，而这里要的是
// 「这个错误到底有没有被分类过」—— 判据/面板据此决定要不要沿用它的分类。
func StructuredOf(err error) (*Error, bool) {
	var e *Error
	if err != nil && errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

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
