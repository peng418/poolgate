// sse.go WorkBuddyCN 的 SSE 处理：上游已是 OpenAI 兼容 SSE，逐行解析 data: 帧
// 归一成 channel.ChatCompletionChunk。
package workbuddyai

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"poolgate/internal/channel"
)

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
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			s.ch <- chunkOrErr{err: err}
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimSpace(payload)
			if payload == "[DONE]" {
				return
			}
			var raw map[string]any
			if json.Unmarshal([]byte(payload), &raw) == nil {
				if c, ok := toChunk(raw, s.model); ok {
					s.ch <- chunkOrErr{chunk: c}
				}
			}
		}
		if err == io.EOF {
			return
		}
	}
}

func toChunk(raw map[string]any, model string) (channel.ChatCompletionChunk, bool) {
	var c channel.ChatCompletionChunk
	c.Model = model
	if v, ok := raw["id"].(string); ok {
		c.ID = v
	}
	if v, ok := raw["usage"].(map[string]any); ok {
		c.Usage = v
	}
	choices, _ := raw["choices"].([]any)
	used := false
	for _, ci := range choices {
		cm, _ := ci.(map[string]any)
		if cm == nil {
			continue
		}
		ch := choiceChunk{}
		if v, ok := cm["index"].(float64); ok {
			ch.Index = int(v)
		}
		if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
			ch.FinishReason = fr
			used = true
		}
		if delta, ok := cm["delta"].(map[string]any); ok {
			if v, ok := delta["role"].(string); ok {
				ch.Delta.Role = v
			}
			if v, ok := delta["content"].(string); ok && v != "" {
				ch.Delta.Content = v
				used = true
			}
			if v, ok := delta["reasoning_content"].(string); ok && v != "" {
				ch.Delta.ReasoningContent = v
				used = true
			}
		}
		c.Choices = append(c.Choices, ch)
	}
	if !used && c.ID == "" && c.Usage == nil {
		return c, false
	}
	return c, true
}

// choiceChunk 是 channel.ChunkChoice 的类型别名，避免匿名结构体啰嗦。
type choiceChunk = channel.ChunkChoice

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
