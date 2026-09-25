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
	"sort"
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

// Pool 账号池。按 Kind 分组，选号时只在同渠道内轮换。
type Pool struct {
	mu    sync.RWMutex
	byKey map[string]*entry // key = kind + "/" + uid

	// 凭证续期（见 refresh.go）：装配层注入的续期函数、每账号的续期锁、
	// 以及「上次续期失败的时刻」（用于失败冷却，避免每个请求都去敲 token 端点）。
	refresh       RefreshFunc
	refreshLocks  map[string]*sync.Mutex
	refreshFailed map[string]time.Time
}

// New 建立空池。
func New() *Pool {
	return &Pool{
		byKey:         map[string]*entry{},
		refreshLocks:  map[string]*sync.Mutex{},
		refreshFailed: map[string]time.Time{},
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
			e.state.Reason = "手动停用"
		} else {
			e.state.Reason = ""
			e.state.Until = time.Time{}
		}
	}
}

// NoteError 记录一次错误；按 kind 的 Policy 分档冷却。
// 红线：UpstreamFault 不计账号错误（AccountBlamed 为 false），由调用方决定是否跳过。
func (p *Pool) NoteError(kind channel.Kind, uid string, k errs.Kind) {
	if !k.AccountBlamed() {
		return // 上游故障/内容拦截等不计入账号错误（D4 解耦）
	}
	pol := errs.PolicyOf(k)
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[keyOf(kind, uid)]; ok {
		e.state.ErrCount++
		if pol.Disable {
			e.state.Disabled = true
			e.state.Reason = string(k)
			return
		}
		if pol.Cooldown > 0 {
			e.state.Until = time.Now().Add(pol.Cooldown)
			e.state.Reason = string(k)
		}
	}
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(kind channel.Kind, uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byKey[keyOf(kind, uid)]; ok {
		e.state.ErrCount = 0
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
	if best == nil {
		return channel.Credential{}, false
	}
	return best.state.Cred, true
}

// ErrNoCandidate 无可用账号时的错误构造。附渠道与原因，保证客户端可见。
func (p *Pool) ErrNoCandidate(kind channel.Kind) error {
	return errs.New(errs.NoCandidate, fmt.Sprintf("渠道 %s 无可用账号（可能全部冷却或禁用）", kind)).
		WithChannel(string(kind))
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
