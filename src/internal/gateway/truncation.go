// truncation.go 工具调用的残缺参数检测。
//
// 移植来源：wild-work internal/upstream/truncation.go（其语义对齐参考仓库 sse.ts:158-167
// isTruncatedArguments）。
//
// 背景：SSE 流被截断（连接中断 / finish_reason==length）时，工具调用的 arguments
// 会只剩半截 JSON。此时网关若把脏参数原样交给客户端，客户端解析会报非法 JSON 并卡死会话。
// 处置是丢弃残缺调用，而非补成 {} 伪造合法外观。
//
// 关键区分：只把「非空但无法解析」视为截断。空串是合法的无参数工具；能解析但类型不对
// （标量 / 数组）属于模型输出错误，交给客户端 schema 校验回传即可，不在此判定。
//
// 落点说明（为什么只在非流式聚合处判定）：残缺检测的输入必须是**拼装完成的完整参数串**。
// 流式出口（streamChunks / streamAnthropic）逐片透传的是 arguments 的**分片**，分片本身
// 天然就是半截 JSON（如 `{"file_path":`），对分片做本判定会把合法的流式工具调用整批误删。
// 因此残缺剔除放在「参数已由 mergeToolCalls 拼回完整串」的两处非流式出口
// （aggregateChunks 与 anthropic.go 的 aggregate）。工具模拟层（toolshim）产出的调用
// 参数在 parseCall 里已校验为合法 JSON，不会出现残缺形态。
package gateway

import (
	"encoding/json"
	"strings"

	"poolgate/internal/channel"
)

// isTruncatedArguments 判定工具参数字符串是否因分片丢失而残缺（区别于「该工具本就无参数」）。
//   - 空串 / 纯空白 → false（合法无参工具）；
//   - 非空但 JSON 解析失败 → true（截断）；
//   - 能解析（含 null/标量/数组等任何合法 JSON）→ false。
func isTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var v any
	return json.Unmarshal([]byte(trimmed), &v) != nil
}

// dropTruncatedToolCalls 过滤出 arguments 完整的 tool_call（返回新 slice）。
// 只依据 isTruncatedArguments 判定，不改动任何保留的调用（正例零改动）。
func dropTruncatedToolCalls(calls []channel.ToolCall) []channel.ToolCall {
	kept := make([]channel.ToolCall, 0, len(calls))
	for _, call := range calls {
		if isTruncatedArguments(call.Function.Arguments) {
			continue
		}
		kept = append(kept, call)
	}
	return kept
}

// truncatedFinish 报告 finish_reason 是否表明上游因长度上限中止（工具参数可能被截断）。
//
// wild-work 的 Aggregate 还额外认「EOF 收尾但没见 data: [DONE]」这一来源
// （`finishReason == "length" || !sawDone`）。PoolGate 的 channel.Stream 只以 io.EOF
// 表示结束、不暴露「是否见到 [DONE]」，故这里只保留 length 一档；连接中断那半需要
// 适配器层给出信号，超出本次改动范围（已在报告里说明）。
func truncatedFinish(finish string) bool {
	return strings.EqualFold(strings.TrimSpace(finish), "length")
}
