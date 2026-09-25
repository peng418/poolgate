// sse.go 处理 QoderCN 嵌套 SSE：每行 data:{...,"body":"<json-string>"}，
// body 字段需二次解析得到标准 OpenAI chunk。移植自 wild-work qodercn/sse.go，
// 归一成 channel.Stream / channel.ChatCompletionChunk 供核心层消费。
package qodercn

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"poolgate/internal/channel"
)

// parseNestedSSE 逐行解析嵌套 SSE，每个有效 chunk 调 onChunk。
//   - body == "[DONE]" → 正常结束
//   - "event:finish" 行 → 忽略
//   - body 二次 unmarshal 失败 → 跳过该行（不致命）
func parseNestedSSE(r io.Reader, onChunk func(map[string]any) error) error {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			var env struct {
				Body string `json:"body"`
			}
			if json.Unmarshal([]byte(payload), &env) == nil && env.Body != "" {
				if env.Body == "[DONE]" {
					return nil
				}
				var chunk map[string]any
				if json.Unmarshal([]byte(env.Body), &chunk) == nil && chunk != nil {
					if err := onChunk(chunk); err != nil {
						return err
					}
				}
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// stream 是 channel.Stream 的实现：把嵌套 SSE 流边读边归一成标准 OpenAI chunk。
type stream struct {
	rc    io.ReadCloser
	model string // 客户端请求的模型名，注入每个 chunk（上游恒为 "auto"）
	ch    chan chunkOrErr
}

type chunkOrErr struct {
	chunk channel.ChatCompletionChunk
	err   error
}

// newStream 启动解析。rc 的所有权转交，Close 时关闭。
func newStream(rc io.ReadCloser, model string) *stream {
	s := &stream{rc: rc, model: model, ch: make(chan chunkOrErr, 16)}
	go s.pump()
	return s
}

func (s *stream) pump() {
	defer close(s.ch)
	send := func(c channel.ChatCompletionChunk, err error) {
		s.ch <- chunkOrErr{chunk: c, err: err}
	}
	err := parseNestedSSE(s.rc, func(raw map[string]any) error {
		c, ok := toChunk(raw, s.model)
		if !ok {
			return nil // 无法归一的行跳过，不致命（与 wild-work 一致）
		}
		send(c, nil)
		return nil
	})
	if err != nil {
		send(channel.ChatCompletionChunk{}, err)
	}
}

// toChunk 把上游 OpenAI chunk 归一成 channel.ChatCompletionChunk。
// 返回 ok=false 表示该帧不含有效增量（可跳过）。
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
		ch := channel.ChunkChoice{}
		if idx, ok := cm["index"].(float64); ok {
			ch.Index = int(idx)
		}
		if fr, ok := cm["finish_reason"].(string); ok {
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
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					var t channel.ToolCall
					if v, ok := tc["id"].(string); ok {
						t.ID = v
					}
					if v, ok := tc["type"].(string); ok {
						t.Type = v
					}
					if fn, ok := tc["function"].(map[string]any); ok {
						if v, ok := fn["name"].(string); ok {
							t.Function.Name = v
						}
						if v, ok := fn["arguments"].(string); ok {
							t.Function.Arguments = v
						}
					}
					ch.Delta.ToolCalls = append(ch.Delta.ToolCalls, t)
					used = true
				}
			}
		}
		c.Choices = append(c.Choices, ch)
	}
	if !used && c.ID == "" && c.Usage == nil {
		return c, false
	}
	return c, true
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

// aggregate 聚合完整 SSE 为单个 OpenAI chat.completion 响应（非流式用）。
// model 参数覆盖响应中的 model 字段（上游恒为 "auto"）。
func aggregate(r io.Reader, model string) (map[string]any, error) {
	var (
		id           string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		role         = "assistant"
		finishReason = "stop"
		usage        map[string]any
	)
	err := parseNestedSSE(r, func(chunk map[string]any) error {
		if v, ok := chunk["id"].(string); ok && id == "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = v
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		ch, _ := chunk["choices"].([]any)
		for _, ci := range ch {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			if delta, ok := c["delta"].(map[string]any); ok {
				if r2, ok := delta["role"].(string); ok && r2 != "" {
					role = r2
				}
				if txt, ok := delta["content"].(string); ok {
					content.WriteString(txt)
				}
				if rc, ok := delta["reasoning_content"].(string); ok {
					reasoning.WriteString(rc)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}
