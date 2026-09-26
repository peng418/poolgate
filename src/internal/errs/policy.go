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
	// 连接中断：**推断类**（见 Kind.Inferred）—— 计数、连续到阈值才冷却，且冷却可被一次成功解除。
	// 所以这里没有 Cooldown（时长由 pool 的 err_cooldown 决定）。
	// 2026-09-27 两段历史：先是 10 分钟账号冷却（一次抖动把两个号一起冷掉，整渠道 503 十分钟）；
	// 0.9.8 又整个豁免（连「一直在断的号」也记不下来）。现在恢复计数 + 门槛，这才是与参考实现
	// wild-work 逐字对齐的形态（它的 err_threshold 门槛我们当初漏移植了）。
	Transport: {Retry: true},
	// Parse（上游 200 却一个字都没有）同为推断类：同样计数到阈值才软冷却。
	Parse: {Retry: true},
	// 并发冲突：上游同一账号的「会话启动」还在处理中（409 CONCURRENT_OPERATION）。
	// **不冷却账号**（Cooldown 0 是刻意的）：真机实测这个闸门 2 秒级就放行
	// （+0.2s → 409；+2.4s → 201），适配器内部退避重试即可，账号本身没有任何问题。
	// 2026-09-27 用户投诉「一直冻结」的根因就在这一类以前落到 SoftRate（60 秒冷却 = 单号渠道冻结一分钟）。
	SessionBusy: {Retry: true},
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
