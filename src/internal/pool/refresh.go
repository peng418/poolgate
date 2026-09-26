// refresh.go 凭证续期（pool 侧的编排）。
//
// 为什么需要：渠道的 access token 是有寿命的（千问办公的 OAuth token 实测会过期），
// 而池子里存的是**签发那一刻**的凭证。没有续期的话，表现就是——
// 「昨天授权完还能用，今天每个请求都 401」，而且账号还会被按「会话失效」禁用，
// 客户端看到的是「无可用账号」或「上游返回 HTTP 401」。
//
// 分工：真正去换 token 的是适配器（channel.Channel.Refresh），
// 落盘与回写池子在装配层（cmd/poolgate），这里只负责
// 「什么时候该换、别并发重复换、换失败别打死上游」。
package pool

import (
	"context"
	"sync"
	"time"

	"poolgate/internal/channel"
)

// RefreshFunc 由装配层提供：把凭证换成新的，并负责落盘与回写池子。
// 返回的凭证 AccessToken 必须与旧的**不同**，否则视为「没能刷新」。
type RefreshFunc func(ctx context.Context, kind channel.Kind, c channel.Credential) (*channel.Credential, error)

// refreshSkew 是「提前多久续期」。留出余量是为了避免「请求刚发出、token 正好过期」。
const refreshSkew = 15 * time.Minute

// refreshSoftGap 是「一次成功续期之后，多久内不要再主动续」的兜底间隔。
//
// 2026-09-27 事故：千问办公的一次性 refresh token 每续一次就轮换一次，而它的 expiresAt 用的是
// 上游 expires_in（≈10 分钟，其实是「多久该换一次」的提示）+ refreshSkew(15 分钟) ⇒ 「临期」
// 永远成立 ⇒ **每个请求都去续一次**（日志实测 42 秒 4 次轮换），链条就是这么被打断的。
// 现在把「什么时候该续」交给渠道自己声明（Spec.RefreshCadence），这一条只是最后一道闸。
var refreshSoftGap = 5 * time.Minute

// refreshFailCooldown 是一次续期失败后多久不再重试。
//
// 没有它的话，一个 refresh_token 已失效的账号会让**每个请求**都去敲一次
// token 端点 —— 上游会把它当攻击，我们自己也会被拖慢。
const refreshFailCooldown = 30 * time.Second

// SetRefresher 注入续期能力（装配层调用；nil = 该进程不支持续期）。
func (p *Pool) SetRefresher(f RefreshFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refresh = f
}

// Fresh 返回一个「现在可用」的凭证：该续期时先续期。
//
// 续期失败时**原样返回旧凭证**，由调用方按 401 处理 —— 这里不吞错、也不抛错，
// 因为「续期」只是优化路径，真正的判据仍是上游的响应。
func (p *Pool) Fresh(ctx context.Context, kind channel.Kind, c channel.Credential) channel.Credential {
	if !p.refreshDue(kind, c) {
		return c
	}
	nc, err := p.RefreshNow(ctx, kind, c)
	if err != nil || nc == nil {
		return c
	}
	return *nc
}

// SetRefreshCadence 注入「按渠道的续期节奏」（装配层从 Spec.RefreshCadence 取）。
// 为 nil 或返回 0 的渠道保持老行为：只在临期时续。
func (p *Pool) SetRefreshCadence(fn func(channel.Kind) time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cadenceOf = fn
}

// refreshDue 报告「这次取号要不要顺手续期」。
//
// 判据两条（任一成立且没被兜底间隔拦住就续）：
//  1. 硬到期：ExpiresAt 已进入 refreshSkew 窗口 —— 真快过期了（所有渠道的老规矩）；
//  2. 渠道节奏：该渠道声明了 RefreshCadence（一次性 refresh token 的渠道，闲置久了会失效），
//     距上次成功续期已达这个间隔。
func (p *Pool) refreshDue(kind channel.Kind, c channel.Credential) bool {
	if c.RefreshToken == "" {
		return false // 没有 refresh token：刷不了就说刷不了
	}
	if !p.needRefresh(c) && !p.cadenceElapsed(kind, c.UID) {
		return false
	}
	return p.refreshGapElapsed(kind, c.UID)
}

// cadenceElapsed 报告「距上次成功续期是否已过该渠道声明的节奏」。没声明节奏时恒为 false。
func (p *Pool) cadenceElapsed(kind channel.Kind, uid string) bool {
	p.mu.RLock()
	fn := p.cadenceOf
	lr := p.lastRefresh[keyOf(kind, uid)]
	p.mu.RUnlock()
	if fn == nil {
		return false
	}
	d := fn(kind)
	if d <= 0 {
		return false
	}
	// lr 为零值表示本进程还没给这个号续过 —— 视为「早就到点了」，接一次链。
	return time.Since(lr) >= d
}

// refreshGapElapsed 是「兜底间隔」：刚续过的号，短时间内不再敲上游（无论上游把到期时间说得多短）。
func (p *Pool) refreshGapElapsed(kind channel.Kind, uid string) bool {
	p.mu.RLock()
	lr := p.lastRefresh[keyOf(kind, uid)]
	p.mu.RUnlock()
	return lr.IsZero() || time.Since(lr) >= refreshSoftGap
}

// RefreshSweep 给「按渠道节奏该续期」的账号主动续一次（后台心跳用）。
//
// 为什么需要：请求路径只续**被选中**的号，闲置账号的 refresh token 会活活放坏
// （实测：闲置 ~31 小时后上游回 invalid_grant「令牌未激活」）。只对有节奏声明的渠道生效，
// 其它渠道一次都不会续 —— 不给不需要的渠道平白增加上游调用（也少一分自动化的味道）。
// 返回本次尝试续期的账号数（调用方负责记日志）。
func (p *Pool) RefreshSweep(ctx context.Context) int {
	type item struct {
		kind channel.Kind
		cred channel.Credential
	}
	p.mu.RLock()
	list := make([]item, 0, len(p.byKey))
	for _, e := range p.byKey {
		if e.state.Disabled {
			continue // 人工停用的号不碰（管理员的决定不被自动化推翻）
		}
		list = append(list, item{e.state.Kind, e.state.Cred})
	}
	p.mu.RUnlock()

	n := 0
	for _, it := range list {
		if it.cred.RefreshToken == "" || !p.cadenceElapsed(it.kind, it.cred.UID) || !p.refreshGapElapsed(it.kind, it.cred.UID) {
			continue
		}
		n++
		_, _ = p.RefreshNow(ctx, it.kind, it.cred) // 失败由装配层的续期函数留痕（红线一）
	}
	return n
}

// needRefresh 报告凭证是否临期（无到期时间、无 refresh token 时不猜）。
func (p *Pool) needRefresh(c channel.Credential) bool {
	if c.RefreshToken == "" || c.ExpiresAt.IsZero() {
		return false
	}
	return time.Until(c.ExpiresAt) <= refreshSkew
}

// RefreshNow 强制续期一次（不看到期时间）：上游回了 401 但本地以为没过期时用。
//
// 同一账号的续期串行化，成功后把新凭证返回；失败返回错误原文（红线一）。
func (p *Pool) RefreshNow(ctx context.Context, kind channel.Kind, c channel.Credential) (*channel.Credential, error) {
	p.mu.Lock()
	rf := p.refresh
	gate, ok := p.refreshLocks[keyOf(kind, c.UID)]
	if !ok {
		gate = &sync.Mutex{}
		p.refreshLocks[keyOf(kind, c.UID)] = gate
	}
	failedAt := p.refreshFailed[keyOf(kind, c.UID)]
	p.mu.Unlock()

	if rf == nil {
		return nil, nil // 没装配续期能力：不算错误，调用方按 401 处理
	}

	gate.Lock()
	defer gate.Unlock()

	// 排队等锁期间，别的请求可能已经把号刷好了：拿池里的现值比一比，
	// 不同就说明「不用再刷了，直接用新的」。没有这一步，一次 token 过期会被
	// 并发请求放大成 N 次刷新 —— 每个请求都去敲一次上游的 token 端点。
	if st, ok := p.Get(kind, c.UID); ok && st.Cred.AccessToken != "" && st.Cred.AccessToken != c.AccessToken {
		nc := st.Cred
		return &nc, nil
	}

	// 冷却期内不重复打上游（拿锁后再看一次，避免排队等锁的白跑）。
	p.mu.Lock()
	failedAt = p.refreshFailed[keyOf(kind, c.UID)]
	p.mu.Unlock()
	if !failedAt.IsZero() && time.Since(failedAt) < refreshFailCooldown {
		return nil, nil
	}

	nc, err := rf(ctx, kind, c)
	if err != nil {
		p.mu.Lock()
		p.refreshFailed[keyOf(kind, c.UID)] = time.Now()
		p.mu.Unlock()
		return nil, err
	}
	if nc == nil || nc.AccessToken == c.AccessToken {
		return nil, nil // 适配器明确表示「没得刷新」（如渠道无公开刷新端点）
	}
	p.mu.Lock()
	delete(p.refreshFailed, keyOf(kind, c.UID))
	p.lastRefresh[keyOf(kind, c.UID)] = time.Now() // 软节奏的基准：只认「成功」的时刻
	p.mu.Unlock()
	return nc, nil
}
