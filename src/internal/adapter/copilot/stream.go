package copilot

// stream.go 上游响应 → channel.Stream 归一化。
//
// 上游是标准的 OpenAI SSE（`data: {chunk}` 逐帧，末尾 `data: [DONE]`），chunk 形态与官方一致，
// 所以解析直接复用 channel.ParseOpenAIChunk —— 不自己写一份几乎一样的 delta 解析。
//
// 两种形态都要吃下：
//   - SSE（text/event-stream）：常态；
//   - 整包 JSON：个别情况下上游会在 stream:true 时回一个完整的 chat.completion（少见，
//     但真发生时如果只按 SSE 解析，客户端会拿到空回复 —— 最难查的那种故障）。
//
// 按 Content-Type 判断而不是猜：猜错的表现同样是「客户端拿到空回复」。

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
				// 上游会在流里插注释/心跳行；解不出或没有有效增量的帧跳过，
				// 当成失败会误杀正常流（channel.ParseOpenAIChunk 的 ok 就是干这个的）。
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

// pumpSingleJSON 处理「上游无视 stream:true，回了一个整包 JSON」的情况。
func (s *stream) pumpSingleJSON() {
	raw, err := io.ReadAll(io.LimitReader(s.rc, 32<<20))
	if err != nil {
		s.ch <- chunkOrErr{err: err}
		return
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		s.ch <- chunkOrErr{err: parseFailure(raw)}
		return
	}
	if e, ok := obj["error"].(map[string]any); ok {
		// 上游把错误放在 200 的信封里（红线二：这种情况不能当成功）。
		s.ch <- chunkOrErr{err: errs.New(errs.UpstreamFault, "上游在 200 响应里返回了错误").
			WithChannel(string(channel.Copilot)).
			WithUpstream(truncate(string(mustJSON(e)), 200))}
		return
	}
	chunks := channel.OpenAIChunkFromMessage(obj, s.model)
	if len(chunks) == 0 {
		s.ch <- chunkOrErr{err: parseFailure(raw)}
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

// parseFailure 把「解析不了」变成客户端可见的失败，并带上上游原话摘要（红线一）。
func parseFailure(raw []byte) error {
	return errs.New(errs.Parse, "上游响应不是合法的 OpenAI 兼容格式").
		WithChannel(string(channel.Copilot)).
		WithUpstream(truncate(string(raw), 200))
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}
