package health

// Snapshot 是「每个模型最近一次体检结论」的索引，键为 `<渠道>/<模型>`。
//
// 为什么要索引：`/v1/models` 与面板模型页都要逐个模型判断一次，
// 每次都扫一遍历史就是 O(模型数 × 批次 × 每条结果)。这里建一次、查 O(1)。
//
// 「最近一次」是**每个模型各自最近**，不是「最近一个批次」：抽样体检只覆盖
// 少数模型，拿批次当基准会把上一轮全量体检的结论一股脑丢掉。
type Snapshot struct {
	Passed map[string]bool // 明确通过
	Failed map[string]bool // 明确失败（不可用）
}

// ID 拼出索引键（渠道 + 客户端模型名），与 /v1/models 的模型 ID 同形。
func ID(kind, model string) string { return kind + "/" + model }

// Hide 报告该模型是否应因健康结论被隐藏。
//
// **未体检过的返回 false**：「不知道」不等于「不可用」。把未体检的一起藏掉，
// 刚装完（还没跑过体检）的机器会向客户端下发一个空模型列表 —— 用户看到的是
// 「服务坏了」，而不是「模型不健康」。这条是刻意的，别图省事改成白名单。
func (s Snapshot) Hide(id string) bool { return s.Failed[id] }

// Known 报告该模型是否有体检结论。
func (s Snapshot) Known(id string) bool { return s.Passed[id] || s.Failed[id] }

// Empty 报告索引里是否一条结论都没有。
func (s Snapshot) Empty() bool { return len(s.Passed) == 0 && len(s.Failed) == 0 }

// Snapshot 返回最近一次体检结论索引（带缓存，Add 之后自动失效）。
func (h *History) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.snapValid {
		return h.snap
	}
	h.snap = buildSnapshot(h.runs)
	h.snapValid = true
	return h.snap
}

// buildSnapshot 按「新批次覆盖旧批次、同一模型只留最新结论」建索引。
//
// 从最旧的批次往后覆盖，天然实现「后写的更新」；同一批次内同一模型
// 只可能有一条结论，无需再比较时间戳（也不该比：并行批次的时间戳可能颠倒）。
func buildSnapshot(runs []Run) Snapshot {
	s := Snapshot{Passed: map[string]bool{}, Failed: map[string]bool{}}
	for _, run := range runs {
		for _, res := range run.Results {
			if res.Channel == "" || res.Model == "" {
				continue // 残缺记录不进索引（宁可不隐藏，也不要误伤）
			}
			id := ID(res.Channel, res.Model)
			delete(s.Passed, id)
			delete(s.Failed, id)
			if res.OK {
				s.Passed[id] = true
			} else {
				s.Failed[id] = true
			}
		}
	}
	return s
}

// Health 让「体检历史 + 设置开关」组合成一个查询器：开关关着就返回空索引，
// 调用方不必自己判断开关（少一处能忘的地方）。
//
// 供装配层使用（cmd/poolgate）：gateway 与面板拿到的过滤结论因此永远同源。
func Health(h *History, enabled func() bool) func() Snapshot {
	return func() Snapshot {
		if h == nil || (enabled != nil && !enabled()) {
			return Snapshot{}
		}
		return h.Snapshot()
	}
}
