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
			// 读流失败也要归一成 errs.Error：普通 error 会被网关换成通用文案（红线一）。
			s.ch <- chunkOrErr{err: errs.New(errs.Transport, "读取上游流失败").
				WithChannel(string(channel.Qwen)).WithUpstream(truncate(err.Error(), 200)).WithCause(err)}
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
				if msg, isErr := upstreamError(raw); isErr {
					// 上游把错误塞进流内帧（HTTP 已是 200）。不归一的话这帧会被当空帧丢掉，
					// 客户端只看到一条空流 —— 正是 TraeWork 那次「原话被换成通用文案」的坑。
					if msg == "" {
						msg = truncate(payload, 200)
					}
					s.ch <- chunkOrErr{err: errs.New(errs.UpstreamFault, "上游在流中返回错误："+msg).
						WithChannel(string(channel.Qwen)).WithUpstream(truncate(payload, 200))}
					return
				}
				if c, ok := channel.ParseOpenAIChunk(raw, s.model); ok {
					s.ch <- chunkOrErr{chunk: c}
				}
			}
			// 解不出来的帧跳过：上游常在流里插注释/心跳行，当成失败会误杀正常流。
		}
		if err == io.EOF {
			return
		}
	}
}

func (s *stream) pumpSingleJSON() {
	raw, err := io.ReadAll(io.LimitReader(s.rc, 32<<20))
	if err != nil {
		s.ch <- chunkOrErr{err: errs.New(errs.Transport, "读取上游响应失败").
			WithChannel(string(channel.Qwen)).WithUpstream(truncate(err.Error(), 200)).WithCause(err)}
		return
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		s.ch <- chunkOrErr{err: errs.New(errs.Parse, "上游响应不是合法的 OpenAI 格式").
			WithChannel(string(channel.Qwen)).WithUpstream(truncate(string(raw), 200))}
		return
	}
	if msg, isErr := upstreamError(obj); isErr {
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		s.ch <- chunkOrErr{err: errs.New(errs.UpstreamFault, "上游在 200 响应里返回了错误："+msg).
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

// upstreamError 从一个上游帧/回包对象里取错误文案，返回 (文案, 是否错误)。
//
// 认 OpenAI 形态的顶层 `error`（对象带 message/details，或直接是字符串）——portal.qwen.ai 是
// OpenAI 兼容端，错误帧就是这个形状。取不到文案时返回空串，由调用方回退成原始报文摘要。
func upstreamError(raw map[string]any) (string, bool) {
	v, ok := raw["error"]
	if !ok || v == nil {
		return "", false
	}
	switch e := v.(type) {
	case string:
		return strings.TrimSpace(e), true
	case map[string]any:
		for _, k := range []string{"message", "details", "code"} {
			if s, ok := e[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s), true
			}
		}
	}
	return "", true
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
