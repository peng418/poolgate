// Package pool 账号池：按渠道分组的账号状态机（冷却/禁用/轮换）。
//
// 红线一（零静默失败）：选号失败返回明确错误（errs.NoCandidate + 原因），
// 而不是 nil —— 上层据此返回结构化错误给客户端。
// 红线三（可插拔）：Pool 不知道任何渠道协议，只操作 channel.Credential 与
// errs.Kind 驱动的冷却策略。
package pool

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// State 是单个账号的运行态。
type State struct {
	Kind    channel.Kind
	Cred    channel.Credential
	Credits int64
	// CreditsKnown 为 false 表示上游没给可信余额，UI 应显示「未知」而不是 0。
	CreditsKnown bool
	ExpiresAt    time.Time
	Disabled     bool
	Reason       string
	Until        time.Time // 冷却截止；零值 = 未冷却
	ErrCount     int
}

type entry struct {
	state State
}

// 说明：凭证续期相关的字段与逻辑在 refresh.go，这里只放状态与选号。

func (e *entry) healthy(now time.Time) bool {
	if e.state.Disabled {
		return false
	}
	if !e.state.Until.IsZero() && now.Before(e.state.Until) {
		return false
	}
	return true
}

// softCooling 报告该号处于「推断类软冷却」：仍可继续使用（排在健康号之后）。
//
// 语义（与 errs.Kind.Inferred 配对）：连接中断 / 空流这类是**本地推断**，
// 一次成功就足以证伪，所以冷却只降优先级、不下线；一旦成功，NoteSuccess 立刻清除。
// 上游明确裁决类（额度/限流/禁言）不在其列：那是上游说「现在别用」，必须等。
func (e *entry) softCooling(now time.Time) bool {
	if e.state.Disabled || e.state.Until.IsZero() || !now.Before(e.state.Until) {
		return false
	}
	return errs.Kind(e.state.Reason).Inferred()
}

// Pool 账号池。按 Kind 分组，选号时只在同渠道内轮换。
type Pool struct {
	mu    sync.RWMutex
	byKey map[string]*entry // key = kind + "/" + uid

	// 凭证续期（见 refresh.go）：装配层注入的续期函数、每账号的续期锁、
	// 以及「上次续期失败的时刻」（用于失败冷却，避免每个请求都去敲 token 端点）。
	refresh       RefreshFunc
	refreshLocks  map[string]*sync.Mutex
	refreshFailed map[string]time.Time
	// lastRefresh 是每账号「上次续期成功的时刻」—— 软节奏判定用（见 refresh.go）。
	lastRefresh map[string]time.Time
	// cadenceOf 按渠道给出「主动续期的节奏」（装配层从 Spec.RefreshCadence 注入）。
	cadenceOf func(channel.Kind) time.Duration

	// 连续错误门槛（①，2026-09-27 起）：推断类错误（连接中断 / 空流）连续发生
	// errThreshold 次才冷却 errCooldown。参考实现 wild-work 的 cooldown 块
	// （err_threshold / err_cooldown）当初被漏移植，导致「一错即冷」。
	errThreshold int
	errCooldown  time.Duration
}

// 出厂门槛，与 wild-work 的 config.json cooldown 块一致（err_threshold 5 / err_cooldown 10m）。
const (
	DefaultErrThreshold = 5
	DefaultErrCooldown  = 10 * time.Minute
)

// SetErrorPolicy 装配「连续错误门槛」（面板设置 err_threshold / err_cooldown_seconds 的落点）。
func (p *Pool) SetErrorPolicy(threshold int, cooldown time.Duration) {
	if threshold <= 0 {
		threshold = DefaultErrThreshold
	}
	if cooldown <= 0 {
		cooldown = DefaultErrCooldown
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errThreshold, p.errCooldown = threshold, cooldown
}

// New 建立空池。
func New() *Pool {
	return &Pool{
		byKey:         map[string]*entry{},
		refreshLocks:  map[string]*sync.Mutex{},
		refreshFailed: map[string]time.Time{},
		lastRefresh:   map[string]time.Time{},
		errThreshold:  DefaultErrThreshold,
		errCooldown:   DefaultErrCooldown,
	}
}

func keyOf(kind channel.Kind, uid string) string { return string(kind) + "/" + uid }

// AddFor 加入某渠道的账号；已存在则更新凭证、保留状态。
func (p *Pool) AddFor(kind channel.Kind, c channel.Credential) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := keyOf(kind, c.UID)
	if e, ok := p.byKey[k]; ok {
		e.state.Cred = c
		return
	}
	p.byKey[k] = &entry{state: State{Kind: kind, Cred: c}}
}

// SetCredits 更新余额。
func (p *Pool) SetCredits(kind channel.Kind, uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[keyOf(kind, uid)]; ok {
		e.state.Credits = credits
	}
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(kind channel.Kind, uid string, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[keyOf(kind, uid)]; ok {
		e.state.Until = time.Now().Add(d)
		e.state.Reason = reason
		e.state.ErrCount = 0
	}
}

// ReasonManualDisabled 是「管理员手动停用」的原因文案（ReasonNoCandidate 据此换措辞：
// 手动停用的号不需要重新授权，在面板启用即可）。
const ReasonManualDisabled = "手动停用"

// Disable 永久禁用（session 死亡），需人工重登恢复。
func (p *Pool) Disable(kind channel.Kind, uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[keyOf(kind, uid)]; ok {
		e.state.Disabled = true
		e.state.Reason = reason
	}
}

// SetDisabled 手动启停。
func (p *Pool) SetDisabled(kind channel.Kind, uid string, d bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[keyOf(kind, uid)]; ok {
		e.state.Disabled = d
		if d {
			e.state.Reason = ReasonManualDisabled
		} else {
			e.state.Reason = ""
			e.state.Until = time.Time{}
		}
	}
}

// minRetryAtCooldown 是「沿用上游给的恢复时间点」时的下限。
//
// 上游要是回一个荒谬的（比如 1 秒后）解禁时间，照做等于把冷却变成空转、
// 下一轮请求立刻又去撞枪口；1 分钟既不会误放行，也不会把号白关。
const minRetryAtCooldown = time.Minute

// NoteError 记录一次错误；按 kind 的 Policy 分档冷却。
// 红线：UpstreamFault 不计账号错误（AccountBlamed 为 false），由调用方决定是否跳过。
func (p *Pool) NoteError(kind channel.Kind, uid string, k errs.Kind) {
	p.NoteErrorAt(kind, uid, k, time.Time{})
}

// NoteErrorAt 同 NoteError，但带上**上游给出的恢复时间点**（如禁言解禁时间）。
//
// 分两条路（2026-09-27 起，逐字对齐参考实现 wild-work 的 err_threshold 机制）：
//
//	A. 上游明确裁决类（429/402/401/403/禁言/模型无权限）：立即按 Policy 冷却或禁用 ——
//	   这是上游给的判决，没有「再试几次」的余地。
//	B. 推断类（连接中断 / 空流，见 errs.Kind.Inferred）：**先计数**，连续 ErrThreshold 次
//	   才冷却 errCooldown；成功即清零（见 NoteSuccess）。单次抖动不该罚号 ——
//	   但「一直在断的号」必须被记下来，否则会一直拿它去撞。
//	   单账号渠道干脆不冷却（③）：池里就那一个号，冷它 = 整条渠道下线，
//	   而且它「忙/抖」的时候大多不是它自己的问题 → 只计数 + 落日志，继续用。
//
// 优先级（裁决类）：上游的时间点 > Policy 的固定冷却。上游才是「这个号什么时候能用」的
// 权威，面板上「还要等多久」必须与上游一致 —— 拿固定冷却去猜，禁言 3 天却只冷 5 分钟，
// 结果就是每个请求都去撞一次枪口（实测：被禁言的号反复被选中探测）。
func (p *Pool) NoteErrorAt(kind channel.Kind, uid string, k errs.Kind, retryAt time.Time) {
	if !k.AccountBlamed() {
		return // 上游故障/内容拦截等不计入账号错误（D4 解耦）
	}
	pol := errs.PolicyOf(k)
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byKey[keyOf(kind, uid)]
	if !ok {
		return
	}
	e.state.ErrCount++
	now := time.Now()

	if pol.Disable {
		e.state.Disabled = true
		e.state.Reason = string(k)
		return
	}

	if k.Inferred() {
		p.noteInferredLocked(kind, e, k, now)
		return
	}

	deadline := time.Time{}
	if pol.Cooldown > 0 {
		deadline = now.Add(pol.Cooldown)
	}
	if retryAt.After(now) {
		if d := retryAt.Sub(now); d < minRetryAtCooldown {
			deadline = now.Add(minRetryAtCooldown)
		} else {
			deadline = retryAt
		}
	}
	if !deadline.IsZero() {
		e.state.Until = deadline
		e.state.Reason = string(k)
	}
}

// noteInferredLocked 处理推断类错误的记账（调用方必须已持写锁）。
func (p *Pool) noteInferredLocked(kind channel.Kind, e *entry, k errs.Kind, now time.Time) {
	threshold := p.errThreshold
	if threshold <= 0 {
		threshold = DefaultErrThreshold
	}
	// ③ 单账号渠道纪律：唯一的号不能被冷（那等于把整条渠道下线）。
	if p.enabledCountLocked(kind) <= 1 {
		log.Printf("poolgate: 渠道 %s 账号 %s 记到推断类错误 %s（连续 %d/%d）—— 该渠道只有 1 个可用账号，按单账号纪律不冷却，继续使用",
			kind, shortUID(e.state.Cred.UID), k, e.state.ErrCount, threshold)
		return
	}
	if e.state.ErrCount < threshold {
		log.Printf("poolgate: 渠道 %s 账号 %s 记到推断类错误 %s（连续 %d/%d，未达门槛不冷却）",
			kind, shortUID(e.state.Cred.UID), k, e.state.ErrCount, threshold)
		return
	}
	cool := p.errCooldown
	if cool <= 0 {
		cool = DefaultErrCooldown
	}
	e.state.ErrCount = 0
	e.state.Until = now.Add(cool)
	e.state.Reason = string(k)
	log.Printf("poolgate: 渠道 %s 账号 %s 连续 %d 次推断类错误（%s），已冷却至 %s（软冷却：仍可被选用，一次成功即解除）",
		kind, shortUID(e.state.Cred.UID), threshold, k, e.state.Until.Local().Format("15:04:05"))
}

// enabledCountLocked 数该渠道「启用中」的账号个数（调用方必须已持锁）。
func (p *Pool) enabledCountLocked(kind channel.Kind) int {
	n := 0
	for _, e := range p.byKey {
		if e.state.Kind == kind && !e.state.Disabled {
			n++
		}
	}
	return n
}

// NoteSuccess 成功请求：清零连续错误计数，并**立即解除推断类软冷却**（⑤）。
//
// 「体检正常就该放开，而不是一直死等到时间结束」—— 一次成功就是最直接的证据，
// 足以证伪「连接中断 / 空流」这类本地推断。上游裁决类（额度/限流/禁言）不在此列：
// 那些是上游说「现在别用」，给不出证据就不许提前放开。
func (p *Pool) NoteSuccess(kind channel.Kind, uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byKey[keyOf(kind, uid)]
	if !ok {
		return
	}
	e.state.ErrCount = 0
	if !e.state.Until.IsZero() && errs.Kind(e.state.Reason).Inferred() {
		prev := e.state.Reason
		e.state.Until = time.Time{}
		e.state.Reason = ""
		log.Printf("poolgate: 渠道 %s 账号 %s 请求成功，已提前解除推断类冷却（原因为 %s）",
			kind, shortUID(uid), prev)
	}
}

// Pick 返回该渠道健康账号中余额最高者；tried 为请求级已试过的 uid 集合。
// 无可用返回 (Credential{}, false)。
//
// ctx 只用于凭证续期（见 refresh.go）：拿到的凭证临期时会先续期再返回。
// 这里刻意要 ctx 而不是偷偷用 Background —— 调用方必须显式带上请求的生命周期。
func (p *Pool) Pick(ctx context.Context, kind channel.Kind, tried map[string]bool) (channel.Credential, bool) {
	return p.pickFresh(ctx, kind, tried, "")
}

// PickPreferred 优先选 preferred 账号（粘性路由）；若其已试过/不健康，回退到余额最高者。
func (p *Pool) PickPreferred(ctx context.Context, kind channel.Kind, preferred string, tried map[string]bool) (channel.Credential, bool) {
	return p.pickFresh(ctx, kind, tried, preferred)
}

// pickFresh = 选号 + 临期续期。
//
// 续期放在选号**之后、返回之前**：选号只看池内状态，不需要凭证；
// 续期要走网络（可能几百毫秒），不能拿着读锁做（会挡住并发的选号与状态更新）。
func (p *Pool) pickFresh(ctx context.Context, kind channel.Kind, tried map[string]bool, preferred string) (channel.Credential, bool) {
	cred, ok := p.pickInternal(kind, tried, preferred)
	if !ok {
		return channel.Credential{}, false
	}
	return p.Fresh(ctx, kind, cred), true
}

func (p *Pool) pickInternal(kind channel.Kind, tried map[string]bool, preferred string) (channel.Credential, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	// 粘性优先：preferred 健康且未试过则直接返回。
	if preferred != "" {
		if e, ok := p.byKey[keyOf(kind, preferred)]; ok && e.healthy(now) &&
			(tried == nil || !tried[e.state.Cred.UID]) {
			return e.state.Cred, true
		}
	}
	var best *entry
	for _, e := range p.byKey {
		if e.state.Kind != kind {
			continue
		}
		if tried != nil && tried[e.state.Cred.UID] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if best == nil || e.state.Credits > best.state.Credits {
			best = e
		}
	}
	if best != nil {
		return best.state.Cred, true
	}
	// 第二遍：健康号一个都没有时，退而用「推断类软冷却」的号（⑤）。
	//
	// 软冷却的语义是「降优先级」而不是「下线」：连接中断/空流是本地推断，
	// 一次成功即可证伪。宁可拿它再试一次（成功就自动解冻），也不要直接 503 ——
	// 单账号渠道尤其如此：不这么做，一次抖动就等于整条渠道停摆到冷却结束。
	// 「上游明确裁决」的冷却（额度/限流/禁言）不会走到这里 —— 它们仍然是硬排除。
	for _, e := range p.byKey {
		if e.state.Kind != kind {
			continue
		}
		if tried != nil && tried[e.state.Cred.UID] {
			continue
		}
		if !e.softCooling(now) {
			continue
		}
		if best == nil || e.state.Credits > best.state.Credits {
			best = e
		}
	}
	if best == nil {
		return channel.Credential{}, false
	}
	return best.state.Cred, true
}

// ErrNoCandidate 无可用账号时的错误构造。附渠道与**逐号运行态**，保证客户端看得懂也看得见。
//
// 让结论自带处置所需的事实（2026-09-27 事故：客户端只收到一句「可能全部冷却或禁用」，
// 分不清「等 10 分钟自恢复」和「要重新授权」，只能来问「是不是被你改坏了」）。
// 网关 503 与面板体检共用 pool.ReasonNoCandidate —— 两边必须同一句话。
func (p *Pool) ErrNoCandidate(kind channel.Kind) error {
	msg := fmt.Sprintf("渠道 %s 无可用账号（可能全部冷却或禁用）", kind)
	if s := ReasonNoCandidate(p.States(kind)); s != "" {
		msg += "：" + s
	}
	return errs.New(errs.NoCandidate, msg).WithChannel(string(kind))
}

// ReasonNoCandidate 把「池里没号可用」写成一句能处置的话。
//
// 只报「无可用账号」等于把问题原样丢回给用户：既看不出是**等一会儿**（冷却到点自恢复）
// 还是**要动手**（已禁用，需重新授权），也看不出是哪个号的锅。所以这里逐个列出运行态，
// uid 只留前 8 位（够对上面板的账号行）。空池返回「池里没有该渠道的账号」。
func ReasonNoCandidate(states []channel.AccountState) string {
	if len(states) == 0 {
		return "池里没有该渠道的账号"
	}
	now := time.Now()
	parts := make([]string, 0, len(states))
	for _, st := range states {
		switch {
		case st.Disabled:
			switch st.Reason {
			case ReasonManualDisabled:
				// 管理员自己停的：不需要重新授权，启用即可 —— 别把话说反。
				parts = append(parts, shortUID(st.UID)+" 已禁用（手动停用），在面板启用即可")
			case "":
				parts = append(parts, shortUID(st.UID)+" 已禁用（原因未记录），可在面板重新授权或启用")
			default:
				parts = append(parts, shortUID(st.UID)+" 已禁用（"+st.Reason+"），需重新授权或手动启用")
			}
		case !st.Until.IsZero() && now.Before(st.Until):
			if st.SoftCooling {
				// 软冷却（推断类）：号仍会被选用，只是降了优先级 —— 说清这点，用户不必干等。
				parts = append(parts, shortUID(st.UID)+" 软冷却至 "+
					st.Until.Local().Format("01-02 15:04")+"（推断类：仍在尝试使用，一次成功即自动解除）")
				break
			}
			parts = append(parts, shortUID(st.UID)+" 冷却至 "+st.Until.Local().Format("01-02 15:04")+"（到点自恢复）")
		default:
			parts = append(parts, shortUID(st.UID)+" 状态未知")
		}
	}
	return strings.Join(parts, "；")
}

// shortUID 把 uid 截到前 8 位：面板账号行就是这么显示的，够对上号又不啰嗦。
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// List 返回某渠道全部账号状态（按 UID 排序）。
func (p *Pool) List(kind channel.Kind) []State {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := []State{}
	for _, e := range p.byKey {
		if e.state.Kind == kind {
			out = append(out, e.state)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cred.UID < out[j].Cred.UID })
	return out
}

// States 返回某渠道各账号的运行态（按 UID 排序）。
//
// 给体检/面板用：「这个渠道为什么没有可用账号」必须答得出来 ——
// 冷却到什么时候、被禁用的原因是什么，都是用户处置时要看的事实。
func (p *Pool) States(kind channel.Kind) []channel.AccountState {
	listed := p.List(kind)
	threshold := p.ErrThreshold()
	out := make([]channel.AccountState, 0, len(listed))
	for _, st := range listed {
		out = append(out, channel.AccountState{
			UID:          st.Cred.UID,
			Disabled:     st.Disabled,
			Reason:       st.Reason,
			Until:        st.Until,
			SoftCooling:  SoftCoolingOf(st),
			ErrCount:     st.ErrCount,
			ErrThreshold: threshold,
		})
	}
	return out
}

// SoftCoolingOf 报告某个运行态是否处于「推断类软冷却」（States 与面板共用同一判据：
// 判据不许写两份，否则面板说「软冷却」、路由却当硬冷却 —— 那正是出事的方式）。
func SoftCoolingOf(st State) bool {
	if st.Disabled || st.Until.IsZero() || !time.Now().Before(st.Until) {
		return false
	}
	return errs.Kind(st.Reason).Inferred()
}

// ErrThreshold 返回当前的连续错误门槛（面板展示「连续错误 n/m」用）。
func (p *Pool) ErrThreshold() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.errThreshold <= 0 {
		return DefaultErrThreshold
	}
	return p.errThreshold
}

// Remove 移除一个账号（F1.9 的删除侧）。返回是否确实删掉了。
// 删除只影响内存池；磁盘凭证由 store.CredsStore 负责，两层分开是为了
// 「删掉凭证但保留运行态观察」这类操作不至于互相牵连。
func (p *Pool) Remove(kind channel.Kind, uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := keyOf(kind, uid)
	if _, ok := p.byKey[k]; !ok {
		return false
	}
	delete(p.byKey, k)
	return true
}

// Get 返回某账号的运行态快照。
func (p *Pool) Get(kind channel.Kind, uid string) (State, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byKey[keyOf(kind, uid)]
	if !ok {
		return State{}, false
	}
	return e.state, true
}

// SetBalance 更新余额与到期时间（F1.7：余额与到期感知）。
func (p *Pool) SetBalance(kind channel.Kind, uid string, b channel.Balance) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[keyOf(kind, uid)]; ok {
		e.state.Credits = b.Credits
		e.state.CreditsKnown = b.Known
		e.state.ExpiresAt = b.ExpiresAt
	}
}

// ClearCooldown 解除冷却（面板「恢复」按钮用）。
func (p *Pool) ClearCooldown(kind channel.Kind, uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[keyOf(kind, uid)]; ok {
		e.state.Until = time.Time{}
		e.state.Reason = ""
		e.state.ErrCount = 0
	}
}

// UIDs 返回某渠道的全部账号 UID（健康探测遍历用）。
func (p *Pool) UIDs(kind channel.Kind) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := []string{}
	for _, e := range p.byKey {
		if e.state.Kind == kind {
			out = append(out, e.state.Cred.UID)
		}
	}
	sort.Strings(out)
	return out
}

// CountHealthy 返回某渠道健康账号数（总览用）。
func (p *Pool) CountHealthy(kind channel.Kind) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range p.byKey {
		if e.state.Kind == kind && e.healthy(now) {
			n++
		}
	}
	return n
}
