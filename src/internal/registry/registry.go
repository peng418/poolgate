// Package registry 是渠道注册表 —— 红线三（渠道可插拔）的落点。
//
// 新增渠道的成本 = 实现 channel.Channel + 一行 Register。
// 摘除渠道的成本 = 把 Spec.Status 改成 Paused，或删掉注册行。核心代码零改动。
package registry

import (
	"fmt"
	"sort"
	"sync"

	"poolgate/internal/channel"
)

// Entry 是一条注册记录。
type Entry struct {
	Channel channel.Channel
	Spec    channel.Spec
}

type table struct {
	mu     sync.RWMutex
	byKind map[channel.Kind]Entry
}

var global = &table{byKind: map[channel.Kind]Entry{}}

// Register 注册一个渠道。重复注册同 Kind 会覆盖（便于测试注入）。
func Register(c channel.Channel, s channel.Spec) {
	global.mu.Lock()
	defer global.mu.Unlock()
	if c != nil {
		s.Kind = c.Kind()
	}
	global.byKind[s.Kind] = Entry{Channel: c, Spec: s}
}

// RegisterSpec 只注册能力声明而不提供实现 —— 用于「暂停但保留位置」的渠道，
// 面板能显示它并写明恢复条件，但它不参与路由（docs/03 §1 的裁定）。
func RegisterSpec(s channel.Spec) { Register(nil, s) }

// SetStatus 只改某渠道的运行状态，保留它已有的适配器实现与能力声明。
//
// 为什么不能用 RegisterSpec 来翻状态：RegisterSpec 会把 Channel 置成 nil，
// 暂停后适配器实现就丢了 —— 面板的「签到/刷新余额」经 credFor → Get 拿不到实现，
// 报「渠道未实现」，而且设置页「渠道开关」的「启用」按钮也因 implemented=false
// 变成「未实现」无法恢复。翻状态必须原位只动 Status，实现原地保留。
func SetStatus(k channel.Kind, s channel.Status) {
	global.mu.Lock()
	defer global.mu.Unlock()
	e, ok := global.byKind[k]
	if !ok {
		return
	}
	e.Spec.Status = s
	global.byKind[k] = e
}

// Get 取渠道实现；未注册或只有声明时返回 nil。
func Get(k channel.Kind) (channel.Channel, bool) {
	global.mu.RLock()
	defer global.mu.RUnlock()
	e, ok := global.byKind[k]
	if !ok || e.Channel == nil {
		return nil, false
	}
	return e.Channel, true
}

// GetSpec 取能力声明。
func GetSpec(k channel.Kind) (channel.Spec, bool) {
	global.mu.RLock()
	defer global.mu.RUnlock()
	e, ok := global.byKind[k]
	return e.Spec, ok
}

// ErrUnknownKind 在引用未注册渠道时返回 —— 不静默返回零值。
func ErrUnknownKind(k channel.Kind) error {
	return fmt.Errorf("poolgate: 未注册的渠道 %q", string(k))
}

// All 返回全部注册记录，按 Kind 字典序（保证 UI 与快照顺序稳定）。
func All() []Entry {
	global.mu.RLock()
	defer global.mu.RUnlock()
	out := make([]Entry, 0, len(global.byKind))
	for _, e := range global.byKind {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Kind < out[j].Spec.Kind })
	return out
}

// Active 返回当前应下发模型的渠道（Status == Active 且有实现）。
func Active() []Entry {
	out := []Entry{}
	for _, e := range All() {
		if e.Spec.Downstream() && e.Channel != nil {
			out = append(out, e)
		}
	}
	return out
}

// Reset 清空注册表，仅用于测试。
func Reset() {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.byKind = map[channel.Kind]Entry{}
}
