package channel

// openai.go 「OpenAI 兼容线上格式」的共享解析 —— 只处理**协议形态**，不含任何渠道专有逻辑。
//
// 为什么放这里：WorkBuddy 两家、以及 0.4.3 起的「通用 OpenAI 兼容上游」适配器
// 上游都是同一套 SSE（choices[].delta），以前每家写一份几乎一样的解析，
// 修一处要改四处。归一化的产物就是 channel.ChatCompletionChunk，核心层只认它。

// ParseOpenAIChunk 把一个 OpenAI 兼容的 chunk 对象归一成 ChatCompletionChunk。
// 返回 ok=false 表示这一帧没有有效增量（心跳帧、纯空帧），调用方应跳过而不是当成内容——
// 红线二：没收到内容就不算成功，空帧不能顶数。
func ParseOpenAIChunk(raw map[string]any, model string) (ChatCompletionChunk, bool) {
	var c ChatCompletionChunk
	c.Model = model
	if v, ok := raw["id"].(string); ok {
		c.ID = v
	}
	if v, ok := raw["usage"].(map[string]any); ok {
		c.Usage = v
	}
	choices, _ := raw["choices"].([]any)
	used := false
	for _, ci := range choices {
		cm, _ := ci.(map[string]any)
		if cm == nil {
			continue
		}
		ch := ChunkChoice{}
		if v, ok := cm["index"].(float64); ok {
			ch.Index = int(v)
		}
		if v, ok := cm["finish_reason"].(string); ok && v != "" {
			ch.FinishReason = v
			used = true
		}
		if delta, ok := cm["delta"].(map[string]any); ok {
			if v, ok := delta["role"].(string); ok {
				ch.Delta.Role = v
			}
			if v, ok := delta["content"].(string); ok && v != "" {
				ch.Delta.Content = v
				used = true
			}
			if v, ok := delta["reasoning_content"].(string); ok && v != "" {
				ch.Delta.ReasoningContent = v
				used = true
			}
			if tcs := ParseOpenAIToolCalls(delta["tool_calls"]); len(tcs) > 0 {
				ch.Delta.ToolCalls = tcs
				used = true
			}
		}
		c.Choices = append(c.Choices, ch)
	}
	if !used && c.ID == "" && c.Usage == nil {
		return c, false
	}
	return c, true
}

// OpenAIChunkFromMessage 把**非流式**的 chat.completion 响应（choices[].message）
// 归一成「一个带内容的 chunk + 一个带 finish_reason 的 chunk」。
//
// 上游在 stream:false 时回的是整包 JSON，但 PoolGate 内部统一按 chunk 流消费
// （SSEOnly 渠道与非流式客户端都走同一条路），所以这里做一次等价转换。
func OpenAIChunkFromMessage(raw map[string]any, model string) []ChatCompletionChunk {
	var head ChatCompletionChunk
	head.Model = model
	if v, ok := raw["id"].(string); ok {
		head.ID = v
	}
	if v, ok := raw["usage"].(map[string]any); ok {
		head.Usage = v
	}
	var tail ChatCompletionChunk
	tail.Model = head.Model
	tail.ID = head.ID
	choices, _ := raw["choices"].([]any)
	for _, ci := range choices {
		cm, _ := ci.(map[string]any)
		if cm == nil {
			continue
		}
		msg, _ := cm["message"].(map[string]any)
		if msg == nil {
			continue
		}
		ch := ChunkChoice{}
		if v, ok := msg["role"].(string); ok {
			ch.Delta.Role = v
		}
		if v, ok := msg["content"].(string); ok {
			ch.Delta.Content = v
		}
		if v, ok := msg["reasoning_content"].(string); ok {
			ch.Delta.ReasoningContent = v
		}
		// 非流式的 tool_calls 没有 index，补 0..n-1，便于上层按 index 归并。
		for i, tc := range ParseOpenAIToolCalls(msg["tool_calls"]) {
			tc.Index = i
			ch.Delta.ToolCalls = append(ch.Delta.ToolCalls, tc)
		}
		if idx, ok := cm["index"].(float64); ok {
			ch.Index = int(idx)
		}
		head.Choices = append(head.Choices, ch)

		tc := ChunkChoice{}
		if idx, ok := cm["index"].(float64); ok {
			tc.Index = int(idx)
		}
		if fr, ok := cm["finish_reason"].(string); ok {
			tc.FinishReason = fr
		}
		tail.Choices = append(tail.Choices, tc)
	}
	if len(head.Choices) == 0 {
		return nil
	}
	return []ChatCompletionChunk{head, tail}
}
