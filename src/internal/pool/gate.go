package pool

// gate.go 每账号「串行 + 最小请求间隔」的闸门 —— 防封号的一等公民。
//
// 网页渠道被判定为「异常行为」的两种典型模式：**同一账号并发**打上游、以及**请求过于密集**。
// 这两件事都不该让每个适配器各写一遍（写歪一个就漏一个渠道），所以放在取号之后、
// 打上游之前统一卡住：
//
//	release, err := gate.Acquire(ctx, kind, uid)   // 同账号串行；距上次不足间隔则等待
//	defer release()                                // 流关闭时释放
//
// 与 errs 冷却的分工：冷却管「这个号现在不能用」（失败之后），闸门管「这个号现在不能
// 同时/太频繁地用」（正常请求之间）。

import (
	"context"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Gate 按账号限流。intervalOf 按渠道返回最小间隔（返回 0 即只串行、不设间隔）——
// 做成函数是因为间隔要能跟着面板设置变，而闸门是启动时建好的。
type Gate struct {
	mu         sync.Mutex
	accounts   map[string]*accountGate
	intervalOf func(channel.Kind) time.Duration
}

type accountGate struct {
	mu   sync.Mutex
	last time.Time
}

// NewGate 建闸门。intervalOf 为 nil 时只串行、不设间隔。
func NewGate(intervalOf func(channel.Kind) time.Duration) *Gate {
	if intervalOf == nil {
		intervalOf = func(channel.Kind) time.Duration { return 0 }
	}
	return &Gate{accounts: map[string]*accountGate{}, intervalOf: intervalOf}
}

func (g *Gate) account(kind channel.Kind, uid string) *accountGate {
	key := string(kind) + "/" + uid
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.accounts[key]
	if !ok {
		a = &accountGate{}
		g.accounts[key] = a
	}
	return a
}

// Acquire 取到该账号的执行权。返回的 release 必须被调用（通常 defer 在流关闭处）。
//
// 等待发生在**锁内**：这段时间同一账号的其它请求排队 —— 这正是「串行 + 间隔」要的效果，
// 而不是让它们并发冲上去（那才是封号的典型触发方式）。
func (g *Gate) Acquire(ctx context.Context, kind channel.Kind, uid string) (func(), error) {
	a := g.account(kind, uid)
	if err := lockCtx(ctx, &a.mu); err != nil {
		return nil, errs.New(errs.SoftRate, "等待该账号空闲超时").
			WithChannel(string(kind)).WithAccount(uid).WithCause(err)
	}
	if d := g.intervalOf(kind) - time.Since(a.last); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			a.mu.Unlock()
			return nil, errs.New(errs.SoftRate, "等待请求间隔超时").
				WithChannel(string(kind)).WithAccount(uid).WithCause(ctx.Err())
		}
	}
	a.last = time.Now()

	var once sync.Once
	return func() { once.Do(a.mu.Unlock) }, nil
}

// lockCtx 是「可取消的 Lock」：sync.Mutex 没有带 ctx 的版本，这里用 TryLock 轮询实现。
// 轮询间隔 5ms 起步、上限 50ms —— 这些渠道本身是低频的，这点开销可以忽略。
func lockCtx(ctx context.Context, mu *sync.Mutex) error {
	wait := 5 * time.Millisecond
	for {
		if mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if wait < 50*time.Millisecond {
			wait *= 2
		}
	}
}
