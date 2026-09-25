package yuanbao

// stream.go 元宝的 SSE → 标准 OpenAI chunk 流。
//
// 帧结构（两份参考实现一致，Rust 那份真的解析了它，所以这里不是猜的）：
//
//	event: message
//	data: {"type":"think","content":"思考片段"}      ← 字段是 content
//	data: {"type":"text","msg":"正文片段"}           ← 字段是 msg（两者不一样！）
//	data: {"stopReason":"..."}                      ← 结束
//
// 取错字段的表现是「思考变成正文」或「一个字都没有」，而两者都不报错 —— 所以按 type 分流。

import (
	"bufio"
	"encoding/json"
	"io"
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
	event := ""
	sentRole := false
	gotContent := false
	sentFinish := false

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

	emitFinish := func() {
		if sentFinish {
			return
		}
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.FinishReason = "stop"
		c.Choices = append(c.Choices, ch)
		emit(c)
		sentFinish = true
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			s.ch <- chunkOrErr{err: err}
			return
		}
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		switch {
		case strings.HasPrefix(trimmed, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			// 只认 event: message 的帧：上游会在同一路流里混入其它事件名（心跳、计费…），
			// 把它们当内容处理会吐出莫名其妙的文本。
			if payload != "" && payload != "[DONE]" && event == "message" {
				var obj map[string]any
				if json.Unmarshal([]byte(payload), &obj) == nil {
					emitRole()
					typ, _ := obj["type"].(string)
					switch strings.ToLower(typ) {
					case "think":
						if v, _ := obj["content"].(string); v != "" {
							emitDelta("", v)
						}
					case "text":
						if v, _ := obj["msg"].(string); v != "" {
							emitDelta(v, "")
						}
					default:
						// 其余帧看有没有结束原因（stopReason）
						if sr, _ := obj["stopReason"].(string); strings.TrimSpace(sr) != "" {
							emitFinish()
							return
						}
					}
				}
			}
		}
		if err == io.EOF {
			// 上游用「连接关闭」表示结束：有内容就补结束帧（否则客户端会当成被截断）。
			if gotContent {
				emitFinish()
			}
			return
		}
	}
}

// pumpWholeBody 处理「上游没按 SSE 回」的情况：整包 JSON 要么是错误，要么无法解析。
// 绝不能当成「成功但没内容」（红线二）。
func (s *stream) pumpWholeBody() {
	raw, err := io.ReadAll(io.LimitReader(s.rc, 32<<20))
	if err != nil {
		s.ch <- chunkOrErr{err: err}
		return
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		// 上游的错误信封常见形态：{code, message}
		if msg, _ := obj["message"].(string); msg != "" {
			s.ch <- chunkOrErr{err: errs.New(errs.UpstreamFault, "上游返回错误："+msg).
				WithChannel(string(channel.Yuanbao)).WithUpstream(truncate(string(raw), 200))}
			return
		}
	}
	s.ch <- chunkOrErr{err: errs.New(errs.Parse, "上游响应既不是 SSE 也不是可识别的错误信封").
		WithChannel(string(channel.Yuanbao)).WithUpstream(truncate(string(raw), 200))}
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
