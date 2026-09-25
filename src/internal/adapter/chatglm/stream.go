package chatglm

// stream.go SSE → 标准 OpenAI chunk 流。
//
// 上游每帧给一份 `parts[]` **快照**（按 logic_id 增量更新），正文/思考要重新渲染再求差 ——
// 直接拿某帧的 text 当增量会重复吐字（见 protocol.go 的 streamState）。
//
// 结束判据是帧里的 `status`：`finish` = 正常说完；`intervene` = 内容被拦
// （上游把拦截说明放在 last_error.intervene_text 里，我们照实转达，不假装正常结束）。

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

type chunkOrErr struct {
	chunk channel.ChatCompletionChunk
	err   error
}

type stream struct {
	rc    io.ReadCloser
	model string
	sse   bool
	ch    chan chunkOrErr
}

func newStream(rc io.ReadCloser, contentType, model string) *stream {
	s := &stream{
		rc:    rc,
		model: model,
		sse:   strings.Contains(strings.ToLower(contentType), "text/event-stream"),
		ch:    make(chan chunkOrErr, 16),
	}
	go s.pump()
	return s
}

func (s *stream) pump() {
	defer close(s.ch)
	if !s.sse {
		s.pumpWholeBody()
		return
	}

	br := bufio.NewReaderSize(s.rc, 256*1024)
	state := newStreamState()
	sentRole := false
	gotContent := false
	finishSent := false

	emit := func(c channel.ChatCompletionChunk) { s.ch <- chunkOrErr{chunk: c} }

	emitRole := func() {
		if sentRole {
			return
		}
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.Delta.Role = "assistant"
		c.Choices = append(c.Choices, ch)
		emit(c)
		sentRole = true
	}

	emitDelta := func(text, reasoning string) {
		if text == "" && reasoning == "" {
			return
		}
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.Delta.Content = text
		ch.Delta.ReasoningContent = reasoning
		c.Choices = append(c.Choices, ch)
		emit(c)
		gotContent = true
	}

	emitFinish := func(tail string) {
		if finishSent {
			return
		}
		if tail != "" {
			emitDelta(tail, "")
		}
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.FinishReason = "stop"
		c.Choices = append(c.Choices, ch)
		emit(c)
		finishSent = true
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			s.ch <- chunkOrErr{err: err}
			return
		}
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		if !strings.HasPrefix(trimmed, "data:") {
			if err == io.EOF {
				if gotContent {
					emitFinish("") // 没等到 finish 就断了：有内容就补结束帧
				}
				return
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload != "" && payload != "[DONE]" {
			var ev map[string]any
			if json.Unmarshal([]byte(payload), &ev) == nil {
				// 业务码错误（HTTP 200 + code != 0）绝不能当成功（红线二）。
				if kind, msg, isErr := businessError(ev); isErr {
					s.ch <- chunkOrErr{err: errs.New(kind, "上游返回错误："+msg).
						WithChannel(string(channel.ChatGLM)).WithUpstream(truncate(payload, 200))}
					return
				}
				emitRole()
				status, _ := ev["status"].(string)
				switch status {
				case "finish", "intervene":
					tail := ""
					if status == "intervene" {
						// 被内容审核拦下：把上游的说明原样带给客户端（不假装正常结束）。
						if le, ok := ev["last_error"].(map[string]any); ok {
							tail = str(le["intervene_text"])
						}
						if tail == "" {
							tail = "内容被上游审核拦截（上游没有给出说明）"
						}
					}
					// 收尾前把最后一帧的 parts 增量也发出去（finish 帧可能带新内容）。
					if txt, rsn := state.feed(ev); txt != "" || rsn != "" {
						emitDelta(txt, rsn)
					}
					emitFinish(tail)
					return
				default:
					txt, rsn := state.feed(ev)
					emitDelta(txt, rsn)
				}
			}
		}
		if err == io.EOF {
			if gotContent {
				emitFinish("")
			}
			return
		}
	}
}

// pumpWholeBody 处理「上游没按 SSE 回，而是一整包 JSON」的情况。
//
// 少见但真实（风控/网关会换掉响应形态）。不处理的话表现是「客户端拿到空回复」——
// 最难查的那种故障（红线二：不能把「没内容」当成功）。
func (s *stream) pumpWholeBody() {
	raw, err := io.ReadAll(io.LimitReader(s.rc, 32<<20))
	if err != nil {
		s.ch <- chunkOrErr{err: err}
		return
	}
	var ev map[string]any
	if json.Unmarshal(raw, &ev) != nil {
		s.ch <- chunkOrErr{err: errs.New(errs.Parse, "上游响应既不是 SSE 也不是合法 JSON").
			WithChannel(string(channel.ChatGLM)).WithUpstream(truncate(string(raw), 200))}
		return
	}
	if kind, msg, isErr := businessError(ev); isErr {
		s.ch <- chunkOrErr{err: errs.New(kind, "上游返回错误："+msg).
			WithChannel(string(channel.ChatGLM)).WithUpstream(truncate(string(raw), 200))}
		return
	}
	// 整包形态下 parts 是完整快照：渲染一次即可。
	state := newStreamState()
	txt, rsn := state.feed(ev)
	c := channel.ChatCompletionChunk{Model: s.model}
	ch := channel.ChunkChoice{}
	ch.Delta.Role = "assistant"
	ch.Delta.Content = txt
	ch.Delta.ReasoningContent = rsn
	c.Choices = append(c.Choices, ch)
	s.ch <- chunkOrErr{chunk: c}
	if txt == "" && rsn == "" {
		s.ch <- chunkOrErr{err: errs.New(errs.Parse, "上游响应里没有任何可用内容").
			WithChannel(string(channel.ChatGLM)).WithUpstream(truncate(string(raw), 200))}
		return
	}
	tail := channel.ChatCompletionChunk{Model: s.model}
	tc := channel.ChunkChoice{}
	tc.FinishReason = "stop"
	tail.Choices = append(tail.Choices, tc)
	s.ch <- chunkOrErr{chunk: tail}
}

// businessError 判断一帧/整包里是不是业务码错误，并给出归一化后的种类与说明。
//
// 智谱习惯把错误放在 code/status 里而 HTTP 仍是 200，所以这条路径是必需的 ——
// 漏了它，用户看到的是「模型不回话」，而不是「凭证过期了」。
func businessError(ev map[string]any) (errs.Kind, string, bool) {
	code, hasCode := numeric(ev["code"])
	if !hasCode {
		if c, ok := numeric(ev["status"]); ok {
			code, hasCode = c, true
		}
	}
	if !hasCode || code == 0 {
		return errs.Parse, "", false
	}
	msg := str(ev["message"])
	if msg == "" {
		msg = str(ev["msg"])
	}
	// 40102 = refresh_token 过期（参考实现里明确认定的语义）；1xxxx 里与 token 相关的同样处理。
	s := strings.ToLower(msg)
	switch {
	case code == 40102, code == 401, code == 403:
		return errs.SessionDead, "凭证已失效（业务码 " + itoa(code) + "）：" + msg, true
	case strings.Contains(s, "token"), strings.Contains(s, "登录"), strings.Contains(s, "expire"):
		return errs.SessionDead, "凭证已失效（业务码 " + itoa(code) + "）：" + msg, true
	case code >= 500:
		return errs.UpstreamFault, "上游故障（业务码 " + itoa(code) + "）：" + msg, true
	}
	return errs.Parse, "业务码 " + itoa(code) + "：" + msg, true
}

func numeric(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
