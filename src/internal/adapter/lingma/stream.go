// stream.go 双层嵌套 SSE → 标准 OpenAI chunk 流。
//
// 灵码上游的响应长这样（每一行一个 data: 事件）：
//
//	data: {"body":"{\"choices\":[{\"delta\":{\"content\":\"你\"}}]}","statusCodeValue":200}
//
// 也就是说：**外层是我们自己的一层信封**（body 是个 JSON 字符串、外加一个业务状态码），
// 把 body 拿出来再解一次，才是 OpenAI 风格的分片。所以读响应不是「解一层 SSE」，
// 而是「解两层」—— 少解一层会看到一堆 choices 为 nil 的帧，表现成「客户端一个字都收不到」。
//
// 状态码语义（参考实现 parseSSEPayload）：statusCodeValue >= 400 是业务错误（此时 body 往往
// 是错误原文而非分片），必须在流里就归一成错误抛出去，不能当成「空增量」跳过了事。
//
// usage 的处理：上游流里**没有真实 token 数**，channel.ChatCompletionChunk.Usage 一律不填。
// 宁可让客户端看到「没有 usage」，也不伪造一个看起来合理的数字（F4.2）。
package lingma

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// outerEnvelope 是外层信封。字段名照参考实现（statusCodeValue 不是 statusCode）。
type outerEnvelope struct {
	Body       string `json:"body"`
	StatusCode int    `json:"statusCodeValue"`
}

// innerChunk 是信封里那层 OpenAI 风格分片。
type innerChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls any    `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Error any `json:"error"`
}

type chunkOrErr struct {
	chunk channel.ChatCompletionChunk
	err   error
}

// stream 是 channel.Stream 的实现：把双层 SSE 边读边归一成标准 chunk。
type stream struct {
	rc    io.ReadCloser
	model string // 客户端请求的模型名：注入每个 chunk（上游回的模型名对它没有意义）
	ch    chan chunkOrErr
}

func newStream(rc io.ReadCloser, model string) *stream {
	s := &stream{rc: rc, model: model, ch: make(chan chunkOrErr, 16)}
	go s.pump()
	return s
}

// sseParser 保存一次流的解析状态（拆出来是为了让「补 role」「补结束帧」这类状态
// 不必靠一串出入参传递 —— 那种写法最容易在分支里漏掉一次更新）。
type sseParser struct {
	s            *stream
	sentRole     bool
	sentFinish   bool
	gotContent   bool
	gotToolCalls bool
}

func (p *sseParser) send(c channel.ChatCompletionChunk) { p.s.ch <- chunkOrErr{chunk: c} }

func (p *sseParser) sendErr(err error) { p.s.ch <- chunkOrErr{err: err} }

// emitRole 在首个有效增量之前补一个 role 分片。
// 上游的信封里通常不带 role，而标准 OpenAI 流的第一个分片应当有它 ——
// 只在本流还没发过 role、且上游这条分片自己也没给 role 时补，不会重复。
func (p *sseParser) emitRole(explicit string) {
	if p.sentRole || explicit != "" {
		p.sentRole = true
		return
	}
	c := channel.ChatCompletionChunk{Model: p.s.model}
	ch := channel.ChunkChoice{}
	ch.Delta.Role = "assistant"
	c.Choices = append(c.Choices, ch)
	p.send(c)
	p.sentRole = true
}

// emitFinish 发结束帧。reason 为空时按「有没有工具调用」自动选 stop / tool_calls。
// 已发过则不重复（上游的 finish_reason 有时会连发多条）。
func (p *sseParser) emitFinish(reason string) {
	if p.sentFinish {
		return
	}
	if reason == "" {
		reason = "stop"
		if p.gotToolCalls {
			// 有工具调用时按 OpenAI 规范给 "tool_calls"：客户端据此知道
			// 「这轮结束是为了让你去执行工具」，而不是「模型说完了」。
			reason = "tool_calls"
		}
	}
	c := channel.ChatCompletionChunk{Model: p.s.model}
	ch := channel.ChunkChoice{}
	ch.FinishReason = reason
	c.Choices = append(c.Choices, ch)
	p.send(c)
	p.sentFinish = true
}

// pump 是读循环：逐行读 data:，剥两层壳，把增量发进 channel。
func (s *stream) pump() {
	defer close(s.ch)
	p := &sseParser{s: s}
	br := bufio.NewReaderSize(s.rc, 256*1024)

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			if payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
				stop, perr := p.handlePayload(strings.TrimSpace(payload))
				if perr != nil {
					p.sendErr(perr)
					return
				}
				if stop {
					p.emitFinish("")
					return
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				p.sendErr(errs.New(errs.Transport, "读取上游流失败").
					WithChannel(string(channel.Lingma)).WithCause(err))
				return
			}
			break
		}
	}

	// 流断了但没等到 [DONE]：只要收到过内容，就补一个结束帧 ——
	// 客户端拿到的是一段完整回复，不该因为上游没发结束标记就被判成「被截断」。
	if p.gotContent || p.gotToolCalls {
		p.emitFinish("")
	}
}

// handlePayload 解析一行 data 载荷；stop=true 表示这一行本身宣告了流结束。
func (p *sseParser) handlePayload(payload string) (stop bool, err error) {
	switch payload {
	case "":
		return false, nil // 空 data 行是心跳
	case "[DONE]":
		return true, nil
	}
	var outer outerEnvelope
	if json.Unmarshal([]byte(payload), &outer) != nil {
		// 解不出的行直接跳过：上游偶尔会插自己的心跳/元数据行，
		// 为一行杂音掐断整条流是得不偿失的（真正的错误会以 statusCode 或 error 的形式出现）。
		return false, nil
	}
	if outer.StatusCode >= 400 {
		body := strings.TrimSpace(outer.Body)
		return false, errs.New(Classify(outer.StatusCode, []byte(body)), "上游在流里返回错误").
			WithChannel(string(channel.Lingma)).
			WithUpstream(truncate(body, 200))
	}
	body := strings.TrimSpace(outer.Body)
	if body == "" {
		return false, nil
	}
	if body == "[DONE]" {
		return true, nil
	}

	var inner innerChunk
	if json.Unmarshal([]byte(body), &inner) != nil {
		return false, nil // 内层解不出的行同样跳过（格式未变时不会出现）
	}
	if inner.Error != nil {
		// 分片里带 error：多数是上游把鉴权/风控问题塞在这一层。
		text := strings.TrimSpace(stringifyError(inner.Error))
		kind := errs.UpstreamFault
		if looksLikeAuthError(text) {
			kind = errs.SessionDead
		}
		return false, errs.New(kind, "上游返回错误："+text).
			WithChannel(string(channel.Lingma)).WithUpstream(truncate(body, 200))
	}

	for _, choice := range inner.Choices {
		role := strings.TrimSpace(choice.Delta.Role)
		p.emitRole(role)

		content := choice.Delta.Content
		toolCalls := channel.ParseOpenAIToolCalls(choice.Delta.ToolCalls)
		if content == "" && len(toolCalls) == 0 && choice.FinishReason == "" {
			continue
		}
		c := channel.ChatCompletionChunk{ID: inner.ID, Model: p.s.model}
		ch := channel.ChunkChoice{Index: choice.Index}
		if role != "" {
			ch.Delta.Role = role
		}
		if content != "" {
			ch.Delta.Content = content
			p.gotContent = true
		}
		if len(toolCalls) > 0 {
			// 工具调用片段**逐片透传**（OpenAI 规范里 name 与 arguments 本就分多片到达，
			// 客户端靠 index 拼回去）。不要在这里聚合：聚合会破坏流式语义，
			// 也让「先到 name、后到 arguments」的时序信息丢失。
			ch.Delta.ToolCalls = append(ch.Delta.ToolCalls, toolCalls...)
			p.gotToolCalls = true
		}
		if choice.FinishReason != "" {
			// 上游自己给了结束原因：就以它为准（不改成我们猜的值），并且不再补结束帧。
			ch.FinishReason = choice.FinishReason
			c.Choices = append(c.Choices, ch)
			p.send(c)
			p.sentFinish = true
			return false, nil
		}
		c.Choices = append(c.Choices, ch)
		p.send(c)
	}
	return false, nil
}

// stringifyError 把 error 字段（可能是字符串，也可能是对象）压成一行文本。
func stringifyError(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case map[string]any:
		if msg, ok := t["message"].(string); ok && strings.TrimSpace(msg) != "" {
			return msg
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// looksLikeAuthError 判断一段错误文本是不是「凭证/签名问题」。
func looksLikeAuthError(msg string) bool {
	s := strings.ToLower(msg)
	for _, kw := range []string{
		"unauthorized", "forbidden", "invalid signature", "signature invalid",
		"token", "auth", "login", "凭证", "登录", "签名",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// Next 返回下一个归一化 chunk；流结束返回 io.EOF。
func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
