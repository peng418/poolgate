package toolshim

// parser.go 把「聊天模型输出的文本流」里的工具调用标记解析回结构化 tool_calls。
//
// 三个必须处理好的现实情况：
//  1. **标记被分片切开** —— SSE 一次可能只吐几个字符，`<tool_` 和 `call>` 分两片到；
//  2. **模型不老实** —— 写代码围栏、arguments 给成字符串、标记前后多余空白；
//  3. **解析不出来时绝不丢内容** —— 一律当正文交出去（红线一：宁可客户端看到原文，
//     也不能因为「我们没解析懂」就把模型说的话吞掉）。

import (
	"encoding/json"
	"io"
	"strings"

	"poolgate/internal/channel"
)

// Parser 是增量解析器：喂文本、拿正文与工具调用。
type Parser struct {
	buf     strings.Builder
	inCall  bool
	callSeq int
}

// Feed 传入一段增量文本，返回「可安全输出的正文」与「刚解析完整的工具调用」。
//
// 未决内容会留在内部：没有起始标记时只留「可能是标记前缀」的尾巴，其余立刻输出，
// 这样正常对话不会被缓冲拖成一块一块的。
func (p *Parser) Feed(s string) (string, []channel.ToolCall) {
	if s != "" {
		p.buf.WriteString(s)
	}
	var out strings.Builder
	var calls []channel.ToolCall
	for {
		text := p.buf.String()
		if !p.inCall {
			i := strings.Index(text, OpenTag)
			if i < 0 {
				keep := partialSuffixLen(text, OpenTag)
				if len(text) > keep {
					out.WriteString(text[:len(text)-keep])
					p.reset(text[len(text)-keep:])
				}
				break
			}
			out.WriteString(text[:i])
			p.reset(text[i+len(OpenTag):])
			p.inCall = true
			continue
		}
		j := strings.Index(text, CloseTag)
		if j < 0 {
			break // 等下一片
		}
		payload := text[:j]
		p.reset(text[j+len(CloseTag):])
		p.inCall = false
		if tc, ok := parseCall(payload, p.callSeq); ok {
			p.callSeq++
			calls = append(calls, tc)
		} else {
			// 标记在、但内容不是合法调用：把它当正文还原出去（不吞内容）。
			out.WriteString(OpenTag + payload + CloseTag)
		}
	}
	return out.String(), calls
}

// Flush 在流结束时调用：吐出残留，并尽力解析一次未闭合的标记。
func (p *Parser) Flush() (string, []channel.ToolCall) {
	text := p.buf.String()
	p.reset("")
	if text == "" {
		return "", nil
	}
	if !p.inCall {
		return text, nil
	}
	p.inCall = false
	// 未闭合：试着解析（模型常忘了收尾标签），解析不出来就按正文还回去。
	if tc, ok := parseCall(text, p.callSeq); ok {
		p.callSeq++
		return "", []channel.ToolCall{tc}
	}
	return OpenTag + text, nil
}

func (p *Parser) reset(s string) {
	p.buf.Reset()
	p.buf.WriteString(s)
}

// partialSuffixLen 返回 text 末尾有多长是 tag 的前缀（用于「可能是标记开头」时先别输出）。
func partialSuffixLen(text, tag string) int {
	max := len(tag) - 1
	if len(text) < max {
		max = len(text)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(text, tag[:n]) {
			return n
		}
	}
	return 0
}

// parseCall 解析一个标记体：{"name":"x","arguments":{…}}。
//
// 宽容处理：去掉代码围栏、取第一个 { 到最后一个 }、arguments 允许是对象或 JSON 字符串。
func parseCall(payload string, index int) (channel.ToolCall, bool) {
	s := strings.TrimSpace(payload)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		s = s[i : j+1]
	} else {
		return channel.ToolCall{}, false
	}
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal([]byte(s), &raw) != nil || strings.TrimSpace(raw.Name) == "" {
		return channel.ToolCall{}, false
	}
	args := strings.TrimSpace(string(raw.Arguments))
	if args == "" || args == "null" {
		args = "{}"
	}
	// arguments 可能是对象，也可能是被再包了一层的 JSON 字符串（模型常见手滑）。
	if strings.HasPrefix(args, `"`) {
		var inner string
		if json.Unmarshal([]byte(args), &inner) == nil && strings.TrimSpace(inner) != "" {
			args = strings.TrimSpace(inner)
		}
	}
	return channel.ToolCall{
		Index: index,
		ID:    "call_shim_" + itoa(index+1),
		Type:  "function",
		Function: channel.FunctionCall{
			Name:      strings.TrimSpace(raw.Name),
			Arguments: args,
		},
	}, true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Wrap 包装一个 chunk 流：正文照旧透传，工具调用标记变成结构化 tool_calls 增量。
//
// finish_reason 会**推迟到末尾**再发：解析器可能要到流结束才吐出最后一个调用
// （标记被切开、或模型忘了收尾标签），先发 finish 会让客户端提前收工。
// 有工具调用时 finish_reason 归一成 "tool_calls"（客户端据此决定「执行工具后继续」）。
func Wrap(st channel.Stream) channel.Stream {
	return &wrappedStream{src: st, p: &Parser{}}
}

type wrappedStream struct {
	src     channel.Stream
	p       *Parser
	queue   []channel.ChatCompletionChunk
	meta    channel.ChatCompletionChunk
	finish  string
	sawCall bool
	done    bool
}

func (w *wrappedStream) Next() (channel.ChatCompletionChunk, error) {
	for {
		if len(w.queue) > 0 {
			c := w.queue[0]
			w.queue = w.queue[1:]
			return c, nil
		}
		if w.done {
			return channel.ChatCompletionChunk{}, io.EOF
		}
		c, err := w.src.Next()
		if err == io.EOF {
			w.flush()
			w.done = true
			continue
		}
		if err != nil {
			return c, err
		}
		if c.ID != "" || c.Model != "" || c.Usage != nil {
			if c.ID != "" {
				w.meta.ID = c.ID
			}
			if c.Model != "" {
				w.meta.Model = c.Model
			}
			if c.Usage != nil {
				w.meta.Usage = c.Usage
			}
		}
		for _, ch := range c.Choices {
			text, calls := w.p.Feed(ch.Delta.Content)
			if text != "" {
				w.queue = append(w.queue, w.mk(text, nil, ""))
			}
			if len(calls) > 0 {
				w.sawCall = true
				w.queue = append(w.queue, w.mk("", calls, ""))
			}
			if ch.FinishReason != "" {
				w.finish = ch.FinishReason
			}
		}
	}
}

func (w *wrappedStream) flush() {
	text, calls := w.p.Flush()
	if text != "" {
		w.queue = append(w.queue, w.mk(text, nil, ""))
	}
	if len(calls) > 0 {
		w.sawCall = true
		w.queue = append(w.queue, w.mk("", calls, ""))
	}
	finish := w.finish
	if w.sawCall && (finish == "" || finish == "stop") {
		finish = "tool_calls"
	}
	w.queue = append(w.queue, w.mk("", nil, finish))
}

func (w *wrappedStream) mk(content string, calls []channel.ToolCall, finish string) channel.ChatCompletionChunk {
	c := channel.ChatCompletionChunk{ID: w.meta.ID, Model: w.meta.Model, Usage: w.meta.Usage}
	ch := channel.ChunkChoice{}
	ch.Delta.Role = "assistant"
	ch.Delta.Content = content
	ch.Delta.ToolCalls = calls
	ch.FinishReason = finish
	c.Choices = append(c.Choices, ch)
	return c
}

func (w *wrappedStream) Close() error { return w.src.Close() }
