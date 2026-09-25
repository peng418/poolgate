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
	HardCredit:       {Cooldown: 12 * time.Hour, Retry: true},
	SoftRate:         {Cooldown: 60 * time.Second, Retry: true},
	SessionDead:      {Disable: true},
	ContentBlocked:   {Passthrough: true},
	PromptTooLong:    {Passthrough: true},
	ModelUnavailable: {Cooldown: 10 * time.Minute, Retry: true},
	// 上游故障：渠道级降级告警，不计账号错误 —— 千问办公 503 就落在这里。
	UpstreamFault: {Cooldown: 5 * time.Minute, Retry: true, BlameChannel: true},
	Transport:     {Cooldown: 10 * time.Minute, Retry: true},
	Parse:         {Cooldown: 10 * time.Minute, Retry: true},
	AuthFailed:    {},
	NoCandidate:   {},
}

// PolicyOf 取某类错误的处置策略；未知类型按最保守处理（冷却但不禁用）。
func PolicyOf(k Kind) Policy {
	if p, ok := DefaultPolicy[k]; ok {
		return p
	}
	return Policy{Cooldown: 10 * time.Minute, Retry: true}
}
