// Package router 路由层：选号 + 粘性 + 轮转重试 + 分档冷却。
//
// 位置在 gateway 与 adapter 之间：网关把归一化的 ChatRequest 交给 router，
// router 从 pool 选号、调适配器，失败按 errs.Kind 分档冷却该号并换号重试，
// 全部失败时返回结构化错误（红线一）。router 不知道渠道协议，只认 channel.Channel
// 与 errs.Kind。
package router

import (
	"context"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/pool"
)

// maxStickyEntries 是粘性表的容量上限。会话 ID 由客户端给（X-Poolgate-Session），
// 不设上限就是一个可被撑爆的内存表；超出后按最近最少使用淘汰。
const maxStickyEntries = 4096

// stickyEntry 是一条粘性绑定：该会话当前认准的账号，以及还能复用几次。
type stickyEntry struct {
	uid  string
	left int
	seen uint64 // 最近一次使用序号，用于 LRU 淘汰
}

// Gate 限制「同一账号的并发与频率」（防封号）。可为 nil（测试或不限）。
type Gate interface {
	Acquire(ctx context.Context, kind channel.Kind, uid string) (func(), error)
}

// Options 路由策略配置。
type Options struct {
	// MaxRetry 换号重试上限（不含首次尝试）。0 = 不换号。
	MaxRetry int
	// StickyRequests 同一会话粘性复用账号的请求数。
	StickyRequests int
	// Gate 每账号串行 + 最小间隔。为 nil 时不限。
	Gate Gate
}

// DefaultOptions 默认策略。
func DefaultOptions() Options { return Options{MaxRetry: 3, StickyRequests: 50} }

// Router 路由层。
//
// Route 会被并发的 HTTP 请求调用，所以粘性表必须加锁 —— 裸 map 并发读写会直接 panic。
type Router struct {
	p    *pool.Pool
	opts Options

	mu     sync.Mutex
	sticky map[string]stickyEntry // 会话 ID → 粘性绑定
	seq    uint64                 // 单调递增使用序号（LRU 用）
}

// New 建立路由。
func New(p *pool.Pool, o Options) *Router {
	if o.StickyRequests <= 0 {
		o.StickyRequests = 50
	}
	return &Router{p: p, opts: o, sticky: map[string]stickyEntry{}}
}

// chat 是「取闸门 → 打上游 → 把释放挂到流上」的唯一入口。
//
// 为什么要包一层：闸门必须在**流读完或客户端断开时**释放，否则那个账号就被永久占住
// （表现为「这个号以后一直排队超时」，比封号还难查）。
func (r *Router) chat(ctx context.Context, ch channel.Channel, kind channel.Kind, cred channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	release := func() {}
	if r.opts.Gate != nil {
		rel, err := r.opts.Gate.Acquire(ctx, kind, cred.UID)
		if err != nil {
			return nil, err
		}
		release = rel
	}
	st, err := ch.Chat(ctx, &cred, req)
	if err != nil {
		release()
		return nil, err
	}
	return &releaseOnClose{Stream: st, release: release}, nil
}

// releaseOnClose 在流关闭或读尽时释放闸门。
type releaseOnClose struct {
	channel.Stream
	once    sync.Once
	release func()
}

func (s *releaseOnClose) Next() (channel.ChatCompletionChunk, error) {
	c, err := s.Stream.Next()
	if err != nil {
		s.once.Do(s.release)
	}
	return c, err
}

func (s *releaseOnClose) Close() error {
	s.once.Do(s.release)
	return s.Stream.Close()
}

// Result 是一次成功路由的结果：流 + 实际选中的账号。
type Result struct {
	Stream channel.Stream
	Cred   channel.Credential
}

// Route 处理一次对话：选号 → 调适配器 → 失败换号重试。返回归一化流。
//
// session 为会话 ID（可为空）；粘性命中时优先复用同账号。失败时按 errs.Kind
// 分档冷却（UpstreamFault 不计账号错误），并换号重试。全部失败返回 errs.Error。
func (r *Router) Route(ctx context.Context, ch channel.Channel, kind channel.Kind, session string, req channel.ChatRequest) (*Result, error) {
	tried := map[string]bool{}
	preferred := r.stickyUID(session)
	var lastErr error

	for attempt := 0; attempt <= r.opts.MaxRetry; attempt++ {
		cred, ok := r.pick(ctx, kind, tried, preferred)
		if !ok {
			// 无更多可用账号：若有具体错误，返回它（保证客户端看到真实原因）；否则 NoCandidate。
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, r.p.ErrNoCandidate(kind)
		}
		preferred = "" // 粘性只作用于首次
		tried[cred.UID] = true

		stream, err := r.chat(ctx, ch, kind, cred, req)
		if err == nil {
			r.setSticky(session, cred.UID)
			return &Result{Stream: stream, Cred: cred}, nil
		}
		lastErr = err
		k, ok := errs.KindOf(err)

		// 凭证类失败（401/签名失效）先试续期再谈换号。
		//
		// 这是「token 到期」这类问题唯一体面的处理方式：账号本身是好的，
		// 直接把号禁用掉等于让用户每小时重新授权一次（实测踩过：千问办公）。
		if ok && errs.CredentialKind(k) {
			if nc, rerr := r.p.RefreshNow(ctx, kind, cred); rerr == nil && nc != nil {
				cred = *nc
				if stream, err = r.chat(ctx, ch, kind, cred, req); err == nil {
					r.setSticky(session, cred.UID)
					return &Result{Stream: stream, Cred: cred}, nil
				}
				lastErr = err
				k, ok = errs.KindOf(err)
			}
		}

		// 失败：按 kind 冷却该号，换号重试。
		if ok {
			r.p.NoteError(kind, cred.UID, k)
			if !k.AccountBlamed() && !errs.CredentialKind(k) {
				// 上游故障/内容拦截/超长：非账号问题，直接返回错误，不换号白折腾。
				return nil, err
			}
		}
	}
	// 全部账号失败：返回最后一个真实错误（保证客户端可见，而非吞成 NoCandidate）。
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errs.New(errs.NoCandidate, "渠道 "+string(kind)+" 所有可用账号均失败").WithChannel(string(kind))
}

// 说明：换号纪律里的「凭证类失败」判据统一用 errs.CredentialKind（单点定义）。

func (r *Router) pick(ctx context.Context, kind channel.Kind, tried map[string]bool, preferred string) (channel.Credential, bool) {
	if preferred != "" {
		if c, ok := r.p.PickPreferred(ctx, kind, preferred, tried); ok {
			return c, true
		}
	}
	return r.p.Pick(ctx, kind, tried)
}

// stickyUID 取该会话当前粘定的账号；无绑定或额度用尽返回 ""。
func (r *Router) stickyUID(session string) string {
	if session == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.sticky[session]
	if !ok {
		return ""
	}
	if e.left <= 0 {
		// 粘性额度耗尽：解除绑定，下次重新选号（否则一个会话会永久钉在同一个号上）。
		delete(r.sticky, session)
		return ""
	}
	return e.uid
}

// setSticky 把会话粘到 uid 上。同一账号连续命中时递减剩余复用次数；
// StickyRequests 次用满即解绑，让后续请求重新参与轮换。
func (r *Router) setSticky(session, uid string) {
	if session == "" {
		// 无会话 ID（客户端没带 X-Poolgate-Session）：不粘，每次正常轮换。
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	if e, ok := r.sticky[session]; ok && e.uid == uid {
		e.left--
		e.seen = r.seq
		if e.left <= 0 {
			delete(r.sticky, session)
			return
		}
		r.sticky[session] = e
		return
	}
	if len(r.sticky) >= maxStickyEntries {
		r.evictLRU()
	}
	r.sticky[session] = stickyEntry{uid: uid, left: r.opts.StickyRequests - 1, seen: r.seq}
}

// evictLRU 淘汰最久未使用的绑定位。调用方持锁。
func (r *Router) evictLRU() {
	var oldestKey string
	var oldest uint64
	first := true
	for k, e := range r.sticky {
		if first || e.seen < oldest {
			oldestKey, oldest, first = k, e.seen, false
		}
	}
	if oldestKey != "" {
		delete(r.sticky, oldestKey)
	}
}
