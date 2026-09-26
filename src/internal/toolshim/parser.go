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

	// 原生标记模式（见 native.go）：模型用自己那套语法输出工具调用时，
	// 从这里开始把整段标记收进缓冲，收尾标记到了再一次性抽取。
	inNative   bool
	family     string
	nativeOpen string // 原文里的开标记，抽取失败时原样还原

	// tagIdx 记「当前进的是哪一对规范标记」（单数/复数，见 canonicalTags）。
	tagIdx int
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

		// ① 正在我们的规范标记里：等收尾。
		if p.inCall {
			open, closeTag := canonicalTags[p.tagIdx].open, canonicalTags[p.tagIdx].close
			if j := strings.Index(text, closeTag); j >= 0 {
				payload := text[:j]
				p.reset(text[j+len(closeTag):])
				p.inCall = false
				got, leftover := extractCalls(payload, p.callSeq, open, closeTag)
				if len(got) > 0 {
					p.callSeq += len(got)
					calls = append(calls, got...)
				}
				if leftover != "" {
					// 标记体里有认不出的部分：原样交出去（红线一，不吞内容）。
					out.WriteString(leftover)
				}
				continue
			}
			// 模型可能在规范标记里写成原生收尾（两套语法混用）—— 一并当结束处理。
			if m := nativeEnd(text, familyDSML); m != nil {
				payload := text[:m[0]]
				p.reset(text[m[1]:])
				p.inCall = false
				if cs := parseNativeRegion(payload, p.callSeq); len(cs) > 0 {
					p.callSeq += len(cs)
					calls = append(calls, cs...)
				} else {
					out.WriteString(OpenTag + payload)
				}
				continue
			}
			return out.String(), calls // 等下一片
		}

		// ② 正在模型自己的原生标记里：等它那套收尾。
		if p.inNative {
			m := nativeEnd(text, p.family)
			if m == nil {
				return out.String(), calls // 等下一片（Flush 会兜底）
			}
			region, closing := text[:m[0]], text[m[0]:m[1]]
			p.reset(text[m[1]:])
			open := p.nativeOpen
			p.inNative, p.family, p.nativeOpen = false, "", ""
			if cs := parseNativeRegion(region, p.callSeq); len(cs) > 0 {
				p.callSeq += len(cs)
				calls = append(calls, cs...)
			} else {
				// 认不出来：连同标记原样交出去（红线一）。
				out.WriteString(open + region + closing)
			}
			continue
		}

		// ③ 正文里找下一个标记（我们约定的与模型原生的，谁先出现用谁）。
		i, tagIdx := canonicalOpen(text)
		ni, fam, nlen := nativeStart(text)
		switch {
		case i >= 0 && (ni < 0 || i <= ni):
			out.WriteString(text[:i])
			p.reset(text[i+len(canonicalTags[tagIdx].open):])
			p.inCall, p.tagIdx = true, tagIdx
		case ni >= 0:
			out.WriteString(text[:ni])
			p.reset(text[ni+nlen:])
			p.inNative, p.family, p.nativeOpen = true, fam, text[ni:ni+nlen]
		default:
			// 标记可能被下一片补齐：把「像标记开头」的尾巴留下。
			keep := partialSuffixLen(text, OpenTag)
			for _, t := range canonicalTags {
				if k := partialSuffixLen(text, t.open); k > keep {
					keep = k
				}
			}
			if k := nativePartialLen(text); k > keep {
				keep = k
			}
			if len(text) > keep {
				out.WriteString(text[:len(text)-keep])
				p.reset(text[len(text)-keep:])
			}
			return out.String(), calls
		}
	}
}

// canonicalTags 是我们约定的两种等价标记。
//
// 为什么两套都要认：单数 <tool_call> 是提示词里写的那个，但模型经常写成复数
// <tool_calls>…</tool_calls>（参考实现里两种都在用），偶尔还把多个单数调用
// 包在复数外壳里 —— 不认复数就等于「标记泄漏成正文」，正是用户看到的乱码形态。
var canonicalTags = []struct{ open, close string }{
	{OpenTag, CloseTag},
	{"<tool_calls>", "</tool_calls>"},
}

// canonicalOpen 找最早出现的规范开标记，返回位置与属于哪一对（都不在返回 -1,0）。
func canonicalOpen(text string) (int, int) {
	best, idx := -1, 0
	for i, t := range canonicalTags {
		if j := strings.Index(text, t.open); j >= 0 && (best < 0 || j < best) {
			best, idx = j, i
		}
	}
	return best, idx
}

// extractCalls 从一对标记的**标记体**里抽工具调用。
//
// 吃三种写法：① 标记体直接是一条调用；② 复数外壳里嵌多个单数标记；
// ③ 都不是 → 把原文（连标记）原样还回去当正文（红线一，绝不吞内容）。
// 返回的第二项是「要当正文发出去的残留」，没有则为空串。
func extractCalls(payload string, seq int, open, closeTag string) ([]channel.ToolCall, string) {
	if !strings.Contains(payload, OpenTag) {
		if tc, ok := parseCall(payload, seq); ok {
			return []channel.ToolCall{tc}, ""
		}
		return nil, open + payload + closeTag
	}
	var (
		calls    []channel.ToolCall
		leftover strings.Builder
		rest     = payload
	)
	for {
		i := strings.Index(rest, OpenTag)
		if i < 0 {
			leftover.WriteString(rest)
			break
		}
		leftover.WriteString(rest[:i])
		rest = rest[i+len(OpenTag):]
		j := strings.Index(rest, CloseTag)
		var body string
		if j < 0 {
			body, rest = rest, ""
		} else {
			body, rest = rest[:j], rest[j+len(CloseTag):]
		}
		if tc, ok := parseCall(body, seq+len(calls)); ok {
			calls = append(calls, tc)
		} else {
			leftover.WriteString(OpenTag + body + CloseTag)
		}
	}
	// 只剩空白就别还了：那是标记之间的换行，不是内容。
	if strings.TrimSpace(leftover.String()) == "" {
		return calls, ""
	}
	return calls, leftover.String()
}

// Flush 在流结束时调用：吐出残留，并尽力解析一次未闭合的标记。
func (p *Parser) Flush() (string, []channel.ToolCall) {
	text := p.buf.String()
	inNative, open := p.inNative, p.nativeOpen
	p.reset("")
	p.inNative, p.family, p.nativeOpen = false, "", ""
	if text == "" {
		return "", nil
	}
	// 原生标记没等到收尾（模型忘了收尾，或那套语法压根没有 calls 收尾那行）：
	// 剥掉尾部正文后抽取；抽不出来就把原文（含标记）还回去。
	if inNative {
		region, tailText := splitNativeTrailing(text)
		if cs := parseNativeRegion(region, p.callSeq); len(cs) > 0 {
			p.callSeq += len(cs)
			return tailText, cs
		}
		return open + region + tailText, nil
	}
	if !p.inCall {
		return text, nil
	}
	p.inCall = false
	// 未闭合：试着解析（模型常忘了收尾标签），解析不出来就按正文还回去（不补收尾标签，
	// 原文里没有的东西不要凭空加上）。
	open, closeTag := canonicalTags[p.tagIdx].open, canonicalTags[p.tagIdx].close
	got, leftover := extractCalls(text, p.callSeq, open, closeTag)
	if len(got) > 0 {
		p.callSeq += len(got)
		return leftover, got
	}
	return open + text, nil
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
