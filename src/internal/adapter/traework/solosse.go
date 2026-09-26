// solosse.go TraeWork 的 SOLO 自定义 SSE 事件序列解析 → channel.ChatCompletionChunk。
// 事件序列（移植自 wild-work traework/solosse.go，实测）：
//
//	event:metadata / timing_cost → 忽略
//	event:output → {"response":增量,"reasoning_content":增量,"tool_calls":...}
//	event:token_usage → usage
//	event:done → {"finish_reason":"stop"}
//	event:error → {"code":..., "message":...}
package traework

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

type soloEvent struct {
	Event        string
	Response     string
	Reasoning    string
	Usage        map[string]any
	FinishReason string
	ToolCalls    []channel.ToolCall
	ErrorCode    int64
	ErrorMessage string
}

func parseSOLOLine(eventName, dataLine string) *soloEvent {
	ev := &soloEvent{Event: strings.TrimSpace(eventName)}
	if dataLine == "" {
		return ev
	}
	var raw map[string]any
	if json.Unmarshal([]byte(dataLine), &raw) != nil {
		return ev
	}
	switch ev.Event {
	case "output":
		if v, ok := raw["response"].(string); ok {
			ev.Response = v
		}
		if v, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = v
		}
		ev.ToolCalls = parseToolCalls(raw["tool_calls"])
	case "token_usage":
		ev.Usage = raw
	case "done":
		if v, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = v
		}
	case "error":
		if v, ok := raw["code"].(float64); ok {
			ev.ErrorCode = int64(v)
		}
		if v, ok := raw["message"].(string); ok {
			ev.ErrorMessage = v
		}
	}
	return ev
}

type stream struct {
	rc    io.ReadCloser
	model string
	ch    chan chunkOrErr
}

type chunkOrErr struct {
	chunk channel.ChatCompletionChunk
	err   error
}

func newStream(rc io.ReadCloser, model string) *stream {
	s := &stream{rc: rc, model: model, ch: make(chan chunkOrErr, 16)}
	go s.pump()
	return s
}

// parseToolCalls 把 output 事件里的 tool_calls 归一化。
// 两种形态都吃下：OpenAI 的 {"function":{"name","arguments"}} 与 SOLO 的
// {"function_call":{...}}（wild-work 实测 SOLO 用后者，前者是兼容旧形态）。
func parseToolCalls(raw any) []channel.ToolCall {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	out := make([]channel.ToolCall, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]any)
		if !ok {
			fn, ok = m["function_call"].(map[string]any)
		}
		if !ok {
			continue
		}
		tc := channel.ToolCall{Type: "function"}
		if v, ok := m["id"].(string); ok {
			tc.ID = v
		}
		if v, ok := m["type"].(string); ok && v != "" {
			tc.Type = v
		}
		if v, ok := m["index"].(float64); ok {
			tc.Index = int(v)
		}
		if v, ok := fn["name"].(string); ok {
			tc.Function.Name = v
		}
		if v, ok := fn["arguments"].(string); ok {
			tc.Function.Arguments = v
		}
		out = append(out, tc)
	}
	return out
}

func (s *stream) pump() {
	defer close(s.ch)
	br := bufio.NewReaderSize(s.rc, 64*1024)
	id := "chatcmpl-" + time.Now().Format("20060102150405.000000000")
	var pendingUsage map[string]any
	sawToolCalls := false
	event := ""
	var data strings.Builder

	emit := func(delta contentDelta, finish string) {
		var c channel.ChatCompletionChunk
		c.ID = id
		c.Model = s.model
		if pendingUsage != nil {
			c.Usage = pendingUsage
			pendingUsage = nil
		}
		ch := choiceChunk{}
		ch.Delta.Role = "assistant"
		ch.Delta.Content = delta.content
		ch.Delta.ReasoningContent = delta.reasoning
		ch.Delta.ToolCalls = delta.toolCalls
		ch.FinishReason = finish
		c.Choices = append(c.Choices, ch)
		s.ch <- chunkOrErr{chunk: c}
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			s.ch <- chunkOrErr{err: err}
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if event != "" {
				ev := parseSOLOLine(event, data.String())
				switch ev.Event {
				case "output":
					if ev.Response != "" || ev.Reasoning != "" || len(ev.ToolCalls) > 0 {
						if len(ev.ToolCalls) > 0 {
							sawToolCalls = true
						}
						emit(contentDelta{content: ev.Response, reasoning: ev.Reasoning, toolCalls: ev.ToolCalls}, "")
					}
				case "token_usage":
					pendingUsage = ev.Usage
				case "done":
					finish := ev.FinishReason
					// 有工具调用时 finish_reason 必须是 tool_calls，客户端据此先执行工具。
					// SOLO 常报 stop/空，这里按事实纠正。
					if sawToolCalls && (finish == "" || finish == "stop") {
						finish = "tool_calls"
					}
					emit(contentDelta{}, finish)
				case "error":
					// 上游明确报错：立刻把错误交给客户端，不等连接关闭。
					// 之前是攒到流结束才发 —— 上游报错后若继续压着连接不关，
					// 客户端就会「既不回复也不报错」（wild-work 的老毛病，红线一）。
					s.ch <- chunkOrErr{err: soloError(ev.ErrorCode, ev.ErrorMessage)}
					return
				}
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(line, "data:"))
		}
		if err == io.EOF {
			break
		}
	}
}

type contentDelta struct {
	content   string
	reasoning string
	toolCalls []channel.ToolCall
}

// soloError 把 SOLO 的 `event:error` 归一成结构化错误（errs.Error）。
//
// 为什么必须归一：这些错**只出现在 SSE 流里**（HTTP 状态码一律 200），所以走不到
// Chat 里那条 `resp.StatusCode >= 400` 的分支。以前这里返回的是普通 error，
// 网关取 Kind 时落到兜底的 Parse、消息被换成通用的「上游请求失败」——
// 上游原话整句丢掉（实测：额度耗尽时上游回的是
// `{"code":4008,"message":"Your requests have exceeded the quota."}`，
// 客户端却只看到「解析失败」，既定位不了原因，也让账号健康判定失去依据）。
func soloError(code int64, msg string) error {
	up := "code=" + itoa64(code) + " " + msg
	return errs.New(soloKind(code, msg), "上游流内错误："+msg).
		WithChannel(string(channel.TraeWork)).
		WithUpstream(up)
}

// soloKind 把 SOLO 的流内错误码/文案归一成错误分类。
//
// 码表来自上游实际回包：4008 = 请求超出配额（"exceeded the quota"，实测额度用完后
// 每次对话都回它），1005 = 套餐失效（与 HTTP 路径的 Classify 同判据）。
// 判不出来的一律归 UpstreamFault —— 它是上游自己报的错，不是我们解析不出来，
// 归 Parse 会误导用户，也会让「上游故障不冷却账号」这条解耦失效。
func soloKind(code int64, msg string) errs.Kind {
	lower := strings.ToLower(msg)
	switch {
	case code == 4008 || code == 1005,
		strings.Contains(lower, "quota"),
		strings.Contains(lower, "exceeded"):
		return errs.HardCredit
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "too many"):
		return errs.SoftRate
	}
	return errs.UpstreamFault
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// choiceChunk 与 channel.ChunkChoice 元素类型一致。
type choiceChunk = channel.ChunkChoice

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
