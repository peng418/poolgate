package anthropic

// stream.go Anthropic SSE 事件流 → 标准 OpenAI chunk 流。
//
// 上游事件（每帧是 `event: <名>` + `data: <JSON>` 两行，本文件只认 data 里的 type 字段
// —— 事件名与 type 历史上出现过不一致，认 type 更稳）：
//
//	message_start        消息开始，带 usage.input_tokens 与消息 id
//	content_block_start  块的 type（text / thinking / tool_use），带 index
//	content_block_delta  text_delta / thinking_delta / input_json_delta（都是增量）
//	content_block_stop   块结束
//	message_delta        stop_reason、usage.output_tokens
//	message_stop         结束
//	error                {type, message}
//	ping                 心跳，直接忽略
//
// 三个必须照实处理的点：
//
//  1. **tool_use 的参数只在块结束时一次性给出**。Anthropic 的 arguments 是逐片 JSON
//     （input_json_delta），中途把半截 arguments 发给客户端会让它解析失败；宁可晚给、不早给。
//
//  2. **usage 的换算不是简单改名**。Anthropic 的 input_tokens **不含**缓存命中的部分
//     （cache_read_input_tokens 另计），而 OpenAI 的 prompt_tokens 是含的。
//     直接改名会让客户端把上下文用量算小一大截。
//
//  3. **流提前断掉时补一个结束帧**。上游没发 message_stop 就断开（网关超时、连接被切）
//     时，若不给 finish_reason，OpenAI 客户端会把整段回复当成「被截断」——
//     内容我们已经收到了，该告诉它这轮正常结束。这是补帧，不是补内容。

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
	kind  channel.Kind
	model string
	sse   bool
	id    string // 上游消息 id（message_start 里给），沿用到每一帧
	ch    chan chunkOrErr
}

// newStream 按响应类型选择解析方式：SSE 逐帧解析，整包 JSON 走单发路径。
func newStream(rc io.ReadCloser, contentType string, kind channel.Kind, model string) *stream {
	s := &stream{
		rc:    rc,
		kind:  kind,
		model: model,
		sse:   strings.Contains(strings.ToLower(contentType), "text/event-stream"),
		ch:    make(chan chunkOrErr, 16),
	}
	go s.pump()
	return s
}

// blockState 是「当前正在累积的 content_block」的状态（跨事件）。
type blockState struct {
	index    int
	typ      string // text / thinking / tool_use
	toolID   string
	toolName string
	args     strings.Builder // input_json_delta 的累积
}

func (s *stream) pump() {
	defer close(s.ch)
	if !s.sse {
		s.pumpSingleJSON()
		return
	}

	br := bufio.NewReaderSize(s.rc, 256*1024)
	block := &blockState{}
	var usage map[string]any
	finish := ""
	sentUsage := false
	sentFinish := false
	gotContent := false
	sawToolCall := false

	emit := func(c channel.ChatCompletionChunk) { s.ch <- chunkOrErr{chunk: c} }

	// emitFinish 发结束帧（usage 帧 + finish_reason 帧）。
	// usage 只发一次：重复发会让客户端把 token 数累加两遍。
	emitFinish := func() {
		if usage != nil && !sentUsage {
			c := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
			c.Usage = normalizeUsage(usage)
			emit(c)
			sentUsage = true
		}
		if sentFinish {
			return
		}
		// 拿到了工具调用但上游没报 stop_reason 时，按事实纠正成 tool_calls。
		if finish == "" {
			if sawToolCall {
				finish = "tool_calls"
			} else {
				finish = "stop"
			}
		}
		c := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
		ch := channel.ChunkChoice{}
		ch.FinishReason = finish
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

		// 只处理 data: 行；event: / 空行 / 注释行都不是数据。
		if !strings.HasPrefix(trimmed, "data:") {
			if err == io.EOF {
				// 流在 message_stop 之前就断了：若有内容，补结束帧（见文件头第 3 点）。
				if gotContent {
					emitFinish()
				}
				return
			}
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" || data == "[DONE]" {
			if err == io.EOF {
				if gotContent {
					emitFinish()
				}
				return
			}
			continue
		}

		var ev map[string]any
		if json.Unmarshal([]byte(data), &ev) == nil {
			typ, _ := ev["type"].(string)
			switch typ {
			case "error":
				msg, _ := ev["error"].(map[string]any)
				text := strOf(msg["message"])
				if text == "" {
					text = truncate(data, 200)
				}
				s.ch <- chunkOrErr{err: errs.New(errs.UpstreamFault, "上游返回错误："+text).
					WithChannel(string(s.kind)).WithUpstream(truncate(data, 200))}
				return

			case "message_start":
				if m, ok := ev["message"].(map[string]any); ok {
					if id, _ := m["id"].(string); id != "" {
						s.id = id
					}
					if u, ok := m["usage"].(map[string]any); ok {
						usage = u
					}
				}

			case "content_block_start":
				*block = blockState{}
				if i, ok := ev["index"].(float64); ok {
					block.index = int(i)
				}
				if cb, ok := ev["content_block"].(map[string]any); ok {
					block.typ, _ = cb["type"].(string)
					if block.typ == "tool_use" {
						block.toolID, _ = cb["id"].(string)
						block.toolName, _ = cb["name"].(string)
					}
				}
				if block.typ == "text" || block.typ == "thinking" || block.typ == "redacted_thinking" {
					// 块开始：给一个角色帧（OpenAI 首帧惯例）。
					c := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
					ch := channel.ChunkChoice{Index: block.index}
					ch.Delta.Role = "assistant"
					c.Choices = append(c.Choices, ch)
					emit(c)
					if block.typ == "redacted_thinking" {
						block.typ = "thinking" // 加密思考：内容拿不到，按思考块处理（不会产出文本）
					}
				}

			case "content_block_delta":
				d, _ := ev["delta"].(map[string]any)
				if d == nil {
					break
				}
				switch block.typ {
				case "text":
					if text, _ := d["text"].(string); text != "" {
						gotContent = true
						c := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
						ch := channel.ChunkChoice{Index: block.index}
						ch.Delta.Content = text
						c.Choices = append(c.Choices, ch)
						emit(c)
					}
				case "thinking":
					if text, _ := d["thinking"].(string); text != "" {
						gotContent = true
						c := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
						ch := channel.ChunkChoice{Index: block.index}
						ch.Delta.ReasoningContent = text
						c.Choices = append(c.Choices, ch)
						emit(c)
					}
				case "tool_use":
					// partial_json 先攒着，块结束才给（见文件头第 1 点）。
					if pj, _ := d["partial_json"].(string); pj != "" {
						block.args.WriteString(pj)
					}
				}

			case "content_block_stop":
				if block.typ == "tool_use" && strings.TrimSpace(block.toolName) != "" {
					args := block.args.String()
					if strings.TrimSpace(args) == "" {
						args = "{}" // 无参工具：给一个合法 JSON，别让客户端解析空串
					}
					c := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
					ch := channel.ChunkChoice{Index: block.index}
					ch.Delta.ToolCalls = []channel.ToolCall{{
						Index:    block.index,
						ID:       block.toolID,
						Type:     "function",
						Function: channel.FunctionCall{Name: block.toolName, Arguments: args},
					}}
					c.Choices = append(c.Choices, ch)
					emit(c)
					gotContent = true
					sawToolCall = true
				}
				*block = blockState{}

			case "message_delta":
				if d, ok := ev["delta"].(map[string]any); ok {
					if sr, _ := d["stop_reason"].(string); sr != "" {
						finish = anthropicToOpenAIFinish(sr)
					}
				}
				if u, ok := ev["usage"].(map[string]any); ok {
					if usage == nil {
						usage = map[string]any{}
					}
					for k, v := range u {
						usage[k] = v
					}
				}

			case "message_stop":
				emitFinish()
				return
			}
		}
		if err == io.EOF {
			if gotContent {
				emitFinish()
			}
			return
		}
	}
}

// pumpSingleJSON 处理「上游无视 stream:true，回了一个整包 message 对象」的情况。
//
// 少见但真实（部分 Anthropic 兼容网关会这样），若不处理，客户端拿到的是空回复 ——
// 最难查的那种故障（红线二：不能把「没内容」当成功）。
func (s *stream) pumpSingleJSON() {
	raw, err := io.ReadAll(io.LimitReader(s.rc, 32<<20))
	if err != nil {
		s.ch <- chunkOrErr{err: err}
		return
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		s.ch <- chunkOrErr{err: errs.New(errs.Parse, "上游响应既不是 SSE 也不是合法 JSON").
			WithChannel(string(s.kind)).WithUpstream(truncate(string(raw), 200))}
		return
	}
	if e, ok := obj["error"].(map[string]any); ok {
		// 上游把错误塞进 200 的信封里：这不能当成功（红线二）。
		s.ch <- chunkOrErr{err: errs.New(errs.UpstreamFault, "上游在 200 响应里返回了错误："+strOf(e["message"])).
			WithChannel(string(s.kind)).WithUpstream(truncate(string(mustJSON(e)), 200))}
		return
	}
	if id, _ := obj["id"].(string); id != "" {
		s.id = id
	}

	head := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
	hc := channel.ChunkChoice{}
	hc.Delta.Role = "assistant"
	head.Choices = append(head.Choices, hc)

	blocks, _ := obj["content"].([]any)
	index := 0
	used := false
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		if bm == nil {
			continue
		}
		typ, _ := bm["type"].(string)
		switch typ {
		case "text":
			if text, _ := bm["text"].(string); text != "" {
				c := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
				ch := channel.ChunkChoice{Index: index}
				ch.Delta.Content = text
				c.Choices = append(c.Choices, ch)
				s.ch <- chunkOrErr{chunk: c}
				used = true
				index++
			}
		case "thinking":
			if text, _ := bm["thinking"].(string); text != "" {
				c := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
				ch := channel.ChunkChoice{Index: index}
				ch.Delta.ReasoningContent = text
				c.Choices = append(c.Choices, ch)
				s.ch <- chunkOrErr{chunk: c}
				used = true
				index++
			}
		case "tool_use":
			name, _ := bm["name"].(string)
			if strings.TrimSpace(name) == "" {
				continue
			}
			args := "{}"
			if in, ok := bm["input"]; ok {
				if b, err := json.Marshal(in); err == nil {
					args = string(b)
				}
			}
			id, _ := bm["id"].(string)
			c := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
			ch := channel.ChunkChoice{Index: index}
			ch.Delta.ToolCalls = []channel.ToolCall{{
				Index: index, ID: id, Type: "function",
				Function: channel.FunctionCall{Name: name, Arguments: args},
			}}
			c.Choices = append(c.Choices, ch)
			s.ch <- chunkOrErr{chunk: c}
			used = true
			index++
		}
	}
	if !used {
		s.ch <- chunkOrErr{err: errs.New(errs.Parse, "上游响应里没有任何可用内容").
			WithChannel(string(s.kind)).WithUpstream(truncate(string(raw), 200))}
		return
	}
	s.ch <- chunkOrErr{chunk: head}

	if u, ok := obj["usage"].(map[string]any); ok {
		c := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
		c.Usage = normalizeUsage(u)
		s.ch <- chunkOrErr{chunk: c}
	}
	tail := channel.ChatCompletionChunk{ID: s.id, Model: s.model}
	tc := channel.ChunkChoice{}
	sr, _ := obj["stop_reason"].(string)
	tc.FinishReason = anthropicToOpenAIFinish(sr)
	tail.Choices = append(tail.Choices, tc)
	s.ch <- chunkOrErr{chunk: tail}
}

// anthropicToOpenAIFinish 把 Anthropic stop_reason 转成 OpenAI finish_reason。
func anthropicToOpenAIFinish(sr string) string {
	switch strings.ToLower(strings.TrimSpace(sr)) {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		// 模型按安全策略拒绝：对客户端来说是「正常结束但没内容」，
		// 编成 stop 而不是 error —— 报文里的原话由网关如实转达。
		return "stop"
	case "pause_turn":
		// 服务端工具执行中暂停：这一轮到此为止，客户端可继续（语义上同 stop）。
		return "stop"
	case "end_turn", "stop_sequence":
		return "stop"
	}
	return "stop"
}

// normalizeUsage Anthropic usage → OpenAI 字段名。
//
// 关键：**prompt_tokens 要把缓存计入**。Anthropic 的 input_tokens 只算「没命中缓存」的
// 那部分，缓存命中/写入分别记在 cache_read_input_tokens / cache_creation_input_tokens；
// 而 OpenAI 的 prompt_tokens 含缓存命中（cached_tokens 是它的子集）。
// 只做改名的话，客户端算出来的上下文占用会少一大截。
func normalizeUsage(u map[string]any) map[string]any {
	read := intOf(u["cache_read_input_tokens"])
	write := intOf(u["cache_creation_input_tokens"])
	in := intOf(u["input_tokens"]) + read + write
	out := intOf(u["output_tokens"])

	m := map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      in + out,
	}
	switch {
	case read > 0:
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": read}
	case write > 0:
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": write}
	}
	return m
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
