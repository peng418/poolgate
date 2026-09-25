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
)

type soloEvent struct {
	Event        string
	Response     string
	Reasoning    string
	Usage        map[string]any
	FinishReason string
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

func (s *stream) pump() {
	defer close(s.ch)
	br := bufio.NewReaderSize(s.rc, 64*1024)
	id := "chatcmpl-" + time.Now().Format("20060102150405.000000000")
	var pendingUsage map[string]any
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
					if ev.Response != "" || ev.Reasoning != "" {
						emit(contentDelta{content: ev.Response, reasoning: ev.Reasoning}, "")
					}
				case "token_usage":
					pendingUsage = ev.Usage
				case "done":
					emit(contentDelta{}, ev.FinishReason)
				case "error":
					// 上游明确报错：立刻把错误交给客户端，不等连接关闭。
					// 之前是攒到流结束才发 —— 上游报错后若继续压着连接不关，
					// 客户端就会「既不回复也不报错」（wild-work 的老毛病，红线一）。
					s.ch <- chunkOrErr{err: &soloErr{code: ev.ErrorCode, msg: ev.ErrorMessage}}
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
}

type soloErr struct {
	code int64
	msg  string
}

func (e *soloErr) Error() string { return "solo error code=" + itoa64(e.code) + " msg=" + e.msg }

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
