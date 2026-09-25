package registry

import "poolgate/internal/channel"

// Remove 摘除一个渠道（含能力声明）。
//
// 与 RegisterSpec(Paused) 的区别：暂停是「保留位置但不下发」，删除接入源要真的
// 从所有列表里消失（面板、模型目录、路由）。两者都是配置级操作，不需要改代码。
func Remove(k channel.Kind) {
	global.mu.Lock()
	defer global.mu.Unlock()
	delete(global.byKind, k)
}
