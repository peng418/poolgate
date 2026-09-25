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

// Fresh 返回一个「现在可用」的凭证：临期或已过期时先续期。
//
// 续期失败时**原样返回旧凭证**，由调用方按 401 处理 —— 这里不吞错、也不抛错，
// 因为「续期」只是优化路径，真正的判据仍是上游的响应。
func (p *Pool) Fresh(ctx context.Context, kind channel.Kind, c channel.Credential) channel.Credential {
	if !p.needRefresh(c) {
		return c
	}
	nc, err := p.RefreshNow(ctx, kind, c)
	if err != nil || nc == nil {
		return c
	}
	return *nc
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
	p.mu.Unlock()
	return nc, nil
}
