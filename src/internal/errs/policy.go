package errs

import "time"

// Policy 描述某类错误的处置方式（需求 F1.6：按错误类型分档冷却）。
type Policy struct {
	// Cooldown 是该错误触发后账号需冷却的时长；0 表示不冷却。
	Cooldown time.Duration
	// Disable 为真时禁用该账号（需人工重新登录），而非仅冷却。
	Disable bool
	// Passthrough 为真时把上游原文透传给客户端，不改写（用户需要知道真实原因）。
	Passthrough bool
	// Retry 为真时允许换号重试。
	Retry bool
	// BlameChannel 为真时该错误记在渠道头上，不计入账号错误。
	BlameChannel bool
}

// DefaultPolicy 是 §4 错误模型表的代码化表达。
//
// 与 docs/02-项目设计方案.md §4 的表一一对应，改这里必须同步改文档。
var DefaultPolicy = map[Kind]Policy{
	HardCredit: {Cooldown: 12 * time.Hour, Retry: true},
	SoftRate:   {Cooldown: 60 * time.Second, Retry: true},
	// 禁言：上游对账号的风控处置（「由于违反用户使用规范」），**有解禁时间**。
	// 30m 只是上游没给时间时的兜底；真实冷却取上游给的 RetryAt（见 pool.NoteErrorAt）。
	// Retry=true：池里还有别的号就该换号接着服务，而不是把整个渠道判死。
	// 不禁用（Disable=false）：禁言是临时的，到点自己恢复；禁用等于逼用户重新登录，还解不开。
	Muted:            {Cooldown: 30 * time.Minute, Retry: true, Passthrough: true},
	SessionDead:      {Disable: true},
	ContentBlocked:   {Passthrough: true},
	PromptTooLong:    {Passthrough: true},
	ModelUnavailable: {Cooldown: 10 * time.Minute, Retry: true},
	// 上游故障：渠道级降级告警，不计账号错误 —— 千问办公 503 就落在这里。
	UpstreamFault: {Cooldown: 5 * time.Minute, Retry: true, BlameChannel: true},
	// 连接中断：渠道/网络级抖动，**不冷却账号**（Cooldown 0 是刻意的），但保留换号重试 ——
	// 别的号可能通，都通不了就把真实错误交给客户端。
	// 2026-09-27 真机事故：这里原本是 10 分钟账号冷却，一次抖动把两个号一起冷却，整渠道 503
	// 十分钟，而面板体检刚刚还是绿的（见 docs/02 §4 的补充说明）。
	Transport: {Retry: true, BlameChannel: true},
	// Parse 仍是账号错误：它代表「上游 200 却一个字都没有」（空流）——那种号该被冷掉。
	Parse:       {Cooldown: 10 * time.Minute, Retry: true},
	AuthFailed:  {},
	NoCandidate: {},
}

// PolicyOf 取某类错误的处置策略；未知类型按最保守处理（冷却但不禁用）。
func PolicyOf(k Kind) Policy {
	if p, ok := DefaultPolicy[k]; ok {
		return p
	}
	return Policy{Cooldown: 10 * time.Minute, Retry: true}
}
