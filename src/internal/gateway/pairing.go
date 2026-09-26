// pairing.go 出站请求的孤儿 tool_call↔tool 结果配对清理 + tool 结果块重排。
//
// 移植来源：wild-work internal/upstream/tool_pairing.go（该版工作在 raw JSON wire 形态上；
// PoolGate 的请求在网关层已归一成 channel.Message，故这里按内部结构重写，判据、顺序、
// 「零改动返回原 slice」等不变式逐条对齐。同一批 wild-work 文件里的 sanitize.go 由另一
// 小组负责，与本文件无关）。
//
// 背景：OpenAI 兼容协议要求带 tool_calls 的 assistant 消息，其每一个 tool_call id
// 都必须有对应的一条 role:tool 结果消息；反之 role:tool 消息也必须有对应的前置
// tool_call。缺任一侧，上游都会以 HTTP 400 拒绝整个请求。
//
// 工具执行失败时（参数非法、超时、工具不存在……）客户端会把 assistant 的 tool_calls
// 持久化进会话历史，却写不回结果消息。这条坏历史随后被每次请求原样重放——上游对之后
// 每一条用户消息都返回 400，整条会话报废。网关是最后一道防线：发出请求前剔除无法配对
// 的条目让会话自愈，宁可丢一轮工具上下文，也好过整条会话死亡。
package gateway

import "poolgate/internal/channel"

// repackToolResultBlocks 把插在 assistant.tool_calls 与其 tool 结果之间的非 tool 消息
// 挪到整组之后，保证同一批 tool_call 的结果在 wire 上连续。
//
// 背景：Codex 的 image_resize_notice 特性会把 <image_resize_notice> 作为一条
// developer/system 消息插在 tool 输出后面。并行调用时它插在两份 tool 结果中间：
//
//	assistant tool_calls=[c00 c01]
//	tool c00
//	developer <image_resize_notice>   <- 插在中间
//	tool c01
//
// OpenAI 兼容协议要求 tool 结果紧跟 assistant，中间插任何消息都算配对断裂，上游判
// 11148（tool_call_sequence_broken）并顶死整条会话。这里只调顺序、不改内容：
//
//	assistant tool_calls=[c00 c01] | tool c00 | X | tool c01
//	→ assistant tool_calls=[c00 c01] | tool c00 | tool c01 | X
//
// 结果顺序保持不变（同批 tool_call 的原相对顺序 = 结果顺序），不引入新的顺序敏感
// 问题。无插入消息时零改动零分配（返回原 slice）。
func repackToolResultBlocks(messages []channel.Message) ([]channel.Message, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]channel.Message, 0, len(messages))
	changed := false
	i := 0
	for i < len(messages) {
		m := messages[i]
		if m.Role != "assistant" {
			out = append(out, m)
			i++
			continue
		}
		if len(m.ToolCalls) == 0 {
			out = append(out, m)
			i++
			continue
		}
		want := map[string]bool{}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				want[tc.ID] = true
			}
		}
		// 收集紧随其后（允许被其他消息打断）的同批 tool 结果，按原相对顺序。
		out = append(out, m)
		i++
		var results []channel.Message
		var between []channel.Message
		sawNonTool := false
		for i < len(messages) {
			mm := messages[i]
			if mm.Role == "tool" {
				if !want[mm.ToolCallID] {
					break
				}
				results = append(results, mm)
				if sawNonTool {
					changed = true
				}
				i++
				continue
			}
			if len(results) == 0 {
				break // assistant 后没有结果：交由 cleanupOrphanToolCalls 处理
			}
			// 下一组 assistant.tool_calls 是新的组头，绝不能当插入物吞掉：一旦被收进
			// between，它永远不再被外层循环当作组头处理，它自己那批结果也就永远得不
			// 到重排（wild-work 真实会话 msg[181] 正是这样漏掉的）。必须 break 交还外层循环。
			if mm.Role == "assistant" && len(mm.ToolCalls) > 0 {
				break
			}
			// 同批结果尚未收齐时，中间消息视为插入物，暂存待后移。
			between = append(between, mm)
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// cleanupOrphanToolCalls 剔除无法配对的 tool_call 与 tool 结果（所有模型，独立于任何
// 脱敏开关）。语义对齐 wild-work 的 resolveToolPairing：
//
//   - 收集全线 role:tool 消息的 tool_call_id（结果集）与 assistant.tool_calls[].id（调用集）；
//   - 一批 assistant.tool_calls 按 keepCalls 对称裁剪：只留有结果配对的调用（部分保留
//     不会留下无结果的 tool_call），过滤后为空才清掉整个 ToolCalls（置 nil，等价 wire 上删键）；
//   - role:tool 只在对应 tool_call 被保留时才保留，否则删除整条消息；
//   - 无任何工具流量 → 原 slice 原样返回，changed=false（零分配零改动）。
//
// 这是「让请求通过」的安全网：只要存在合法配对就整段保留这些字段，绝不吞掉正确配对。
// 返回清理后的 slice（无改动时等于原 slice，勿依赖其是否新分配）及是否发生删除。
//
// 注意：裁剪 tool_calls 是**原地**改 slice 元素（与 wild-work 一致）——只有确实发生删除
// 时才会写回，调用方持有的是本次请求自己的 messages，无共享风险。
func cleanupOrphanToolCalls(messages []channel.Message) ([]channel.Message, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	hasTraffic := false
	for _, m := range messages {
		switch m.Role {
		case "tool":
			if m.ToolCallID != "" {
				resultIDs[m.ToolCallID] = true
				hasTraffic = true
			}
		case "assistant":
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					callIDs[tc.ID] = true
					hasTraffic = true
				}
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	// keepCalls：调用 id 是否双侧齐全（调用存在且结果存在）。重复 id 与乱序均按集合处理。
	keepCalls := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keepCalls[id] = true
		}
	}
	changed := false
	// 1) assistant.tool_calls：按 keepCalls 对称裁剪——只留有结果的调用，过滤后为空则清键。
	//
	// wild-work 历史实现是「批内每个 id 都齐才整批保留，否则删掉整个 tool_calls 键」。那会
	// 留下无主结果：批 [c1,c2] 只回了 c1 时，调用侧整批被删，而 tool{c1} 仍按 id 命中
	// keepCalls 得以保留 —— 出站载荷于是变成「无 tool_calls 的 assistant + 孤儿 tool」，
	// 上游判 11148（tool calls and tool results do not match）并顶死整条会话。
	// 现在两侧共用同一份 keepCalls 按 id 对称裁剪（与 2) 的删除侧同口径），
	// 任何输入都不会再产生半截配对。
	for i := range messages {
		m := &messages[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		keptCalls := make([]channel.ToolCall, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			if keepCalls[tc.ID] {
				keptCalls = append(keptCalls, tc)
			}
		}
		if len(keptCalls) == len(m.ToolCalls) {
			continue // 整批齐全：零改动
		}
		changed = true
		if len(keptCalls) == 0 {
			m.ToolCalls = nil // 等价 wire 上删除 tool_calls 键
			continue
		}
		m.ToolCalls = keptCalls
	}
	// 2) role:tool 结果：只有对应 tool_call 被保留才保留；孤儿结果整条删除。
	kept := make([]channel.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == "tool" && !keepCalls[m.ToolCallID] {
			changed = true
			continue
		}
		kept = append(kept, m)
	}
	if !changed {
		return messages, false
	}
	return kept, true
}

// sanitizeToolPairing 是网关侧最后一道防线：发出上游前把出站消息里的孤儿 tool_call /
// tool 结果清理掉，并把插在配对组中间的消息挪到整组之后。先 repack 再 cleanup
// （与 wild-work PrepareBody 的两步顺序一致）：重排后才能保证「结果连续」这一不变量；
// 两步都零改动时返回原 slice。
func sanitizeToolPairing(messages []channel.Message) []channel.Message {
	messages, _ = repackToolResultBlocks(messages)
	messages, _ = cleanupOrphanToolCalls(messages)
	return messages
}
