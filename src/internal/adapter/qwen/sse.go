package qwen

// sse.go 上游响应 → channel.Stream。
//
// 通义 CLI 端点回的就是**标准 OpenAI SSE**（`data: {...}` … `data: [DONE]`），
// 所以解析直接用 channel 里的共享实现 —— 各家上游形态一致的部分不该各写一遍。
// 仍然保留「整包 JSON」分支：非流式请求（或上游无视 stream:true）时会走它。

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
		rc: rc, model: model,
		sse: strings.Contains(strings.ToLower(contentType), "text/event-stream"),
		ch:  make(chan chunkOrErr, 16),
	}
	go s.pump()
	return s
}

func (s *stream) pump() {
	defer close(s.ch)
	if !s.sse {
		s.pumpSingleJSON()
		return
	}
	br := bufio.NewReaderSize(s.rc, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			s.ch <- chunkOrErr{err: err}
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				return
			}
			var raw map[string]any
			if json.Unmarshal([]byte(payload), &raw) == nil {
				if c, ok := channel.ParseOpenAIChunk(raw, s.model); ok {
					s.ch <- chunkOrErr{chunk: c}
				}
			}
		}
		if err == io.EOF {
			return
		}
	}
}

func (s *stream) pumpSingleJSON() {
	raw, err := io.ReadAll(io.LimitReader(s.rc, 32<<20))
	if err != nil {
		s.ch <- chunkOrErr{err: err}
		return
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		s.ch <- chunkOrErr{err: errs.New(errs.Parse, "上游响应不是合法的 OpenAI 格式").
			WithChannel(string(channel.Qwen)).WithUpstream(truncate(string(raw), 200))}
		return
	}
	if _, hasErr := obj["error"]; hasErr {
		s.ch <- chunkOrErr{err: errs.New(errs.UpstreamFault, "上游在 200 响应里返回了错误").
			WithChannel(string(channel.Qwen)).WithUpstream(truncate(string(raw), 200))}
		return
	}
	chunks := channel.OpenAIChunkFromMessage(obj, s.model)
	if len(chunks) == 0 {
		s.ch <- chunkOrErr{err: errs.New(errs.Parse, "上游响应里没有可用的内容").
			WithChannel(string(channel.Qwen)).WithUpstream(truncate(string(raw), 200))}
		return
	}
	for _, c := range chunks {
		s.ch <- chunkOrErr{chunk: c}
	}
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
