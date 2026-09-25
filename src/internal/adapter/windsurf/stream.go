package windsurf

// stream.go Connect 多帧响应 → 标准 OpenAI chunk 流。
//
// 上游不是 SSE：响应体是一串「flags + 4 字节大端长度 + protobuf 载荷」的帧（见 frames.go）。
// 每一帧是一个完整的 GetChatMessageResponse，里面**逐帧给增量**：
//
//	#3 正文增量      #9 思考增量      #5 finish 信号      #7 用量元数据
//
// 正文与思考是原生分开的两个字段（见 protocol.go 顶部关于 #3/#9 的说明）——
// 把它们搞反，客户端看到的会是模型的思考草稿当正文。
//
// 另外两个「不处理就会骗客户端」的点：
//   - 上游**必须**以 end-of-stream trailer 帧收尾（成功 {}，失败 {"error":{…}}）。
//     连接直接断掉而没有 trailer = 答案被截断，此时必须报错，不能当成正常结束 ——
//     否则半截答案会被写进会话，下一轮模型看到的是残缺上下文。
//   - 结束原因不能只看 #5 那个未标定全的枚举：参考实现实测只有 2/4 被钉住，
//     其余按名字顺序猜都会把「正常完成」读成「被截断」。所以截断改用**不依赖枚举**的
//     判据：completion_tokens 正好撞上我们实际写进 wire 的输出上限。

import (
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
	rc        io.ReadCloser
	model     string // 回给客户端的模型名（保留客户端请求的原名）
	maxTokens int    // 实际写进 wire 的输出上限，判截断用
	ch        chan chunkOrErr
}

func newStream(rc io.ReadCloser, model string, maxTokens int) *stream {
	s := &stream{rc: rc, model: model, maxTokens: maxTokens, ch: make(chan chunkOrErr, 16)}
	go s.pump()
	return s
}

func (s *stream) pump() {
	defer close(s.ch)

	splitter := &frameSplitter{}
	readBuf := make([]byte, 32<<10)

	sentRole := false
	sentFinish := false
	sawTrailer := false
	var lastUsage map[string]any
	var lastFinish uint64
	hasFinish := false

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
	emitFinish := func() {
		if sentFinish {
			return
		}
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.FinishReason = finishReason(lastFinish, hasFinish, lastUsage, s.maxTokens)
		c.Choices = append(c.Choices, ch)
		if lastUsage != nil {
			c.Usage = lastUsage
		}
		emit(c)
		sentFinish = true
	}

	for {
		n, err := s.rc.Read(readBuf)
		if n > 0 {
			splitter.push(readBuf[:n])
			frames, ferr := splitter.drain()
			if ferr != nil {
				s.ch <- chunkOrErr{err: errs.New(errs.UpstreamFault, "上游响应帧无法解析："+ferr.Error()).
					WithChannel(string(channel.Windsurf)).WithCause(ferr)}
				return
			}
			for _, f := range frames {
				if f.trailer {
					sawTrailer = true
					if e := trailerError(f.payload); e != nil {
						s.ch <- chunkOrErr{err: e}
						return
					}
					continue
				}
				d := decodeFrame(f.payload)
				if d.content == "" && d.reasoning == "" {
					// 元数据帧 / 空帧：仍要收下 finish 与 usage。
					if d.hasFinish {
						lastFinish, hasFinish = d.finish, true
					}
					if d.usage != nil {
						lastUsage = d.usage
					}
					continue
				}
				emitRole()
				if d.reasoning != "" {
					c := channel.ChatCompletionChunk{Model: s.model}
					ch := channel.ChunkChoice{}
					ch.Delta.ReasoningContent = d.reasoning
					c.Choices = append(c.Choices, ch)
					emit(c)
				}
				if d.content != "" {
					c := channel.ChatCompletionChunk{Model: s.model}
					ch := channel.ChunkChoice{}
					ch.Delta.Content = d.content
					c.Choices = append(c.Choices, ch)
					emit(c)
				}
				if d.hasFinish {
					lastFinish, hasFinish = d.finish, true
				}
				if d.usage != nil {
					lastUsage = d.usage
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				s.ch <- chunkOrErr{err: errs.New(errs.Transport, "读取上游响应失败").
					WithChannel(string(channel.Windsurf)).WithCause(err)}
				return
			}
			break
		}
	}

	// 连接断了却没有 trailer：答案被截断。绝不能补一个 finish 当正常结束 ——
	// 那会把半截回复当成完整轮次写进会话。
	if !sawTrailer {
		s.ch <- chunkOrErr{err: errs.New(errs.UpstreamFault,
			"上游流在没有 end-of-stream 帧的情况下结束（响应被截断）").
			WithChannel(string(channel.Windsurf))}
		return
	}
	emitFinish()
}

// frameDelta 是一帧解出来的增量。
type frameDelta struct {
	content   string
	reasoning string
	finish    uint64
	hasFinish bool
	usage     map[string]any
}

// decodeFrame 解一帧 GetChatMessageResponse。
//
// 解析失败时返回**空增量**而不是错误：上游会在流里插自己的心跳/元数据帧，
// 一帧脏数据不该让整条流报错（参考实现对 malformed frame 也是跳过 + 记日志）。
// 但「连接断了没 trailer」是另一回事，那在 pump 里报错。
func decodeFrame(payload []byte) frameDelta {
	fields, err := parseFields(payload)
	if err != nil {
		return frameDelta{}
	}
	d := frameDelta{
		content:   getString(fields, respFieldContent),
		reasoning: getString(fields, respFieldReasoning),
	}
	if v, ok := getVarint(fields, respFieldFinish); ok {
		d.finish, d.hasFinish = v, true
	}
	if meta := getBytes(fields, respFieldMeta); meta != nil {
		if mf, err := parseFields(meta); err == nil {
			d.usage = decodeUsage(mf)
		}
	}
	return d
}

// decodeUsage 把 #7 metadata 子消息里的用量计数器归一成 OpenAI 口径。
//
// 口径与参考实现 normalizeConnectUsage 一致（也是 Cascade 路已经采纳的那套）：
//
//	prompt_tokens = 新鲜输入 + cache_read（cached_tokens 是它的**子集明细**）
//	total_tokens  = prompt + completion + cache_write（账单口径，含缓存写入）
//
// 免费账号上缓存计数器恒为 0，而 protobuf 不编码 0 值，字段自然缺席 ——
// 解不到就是「没有缓存」，不会误判成 0。
func decodeUsage(mf []pfield) map[string]any {
	prompt, _ := getVarint(mf, metaUsagePrompt)
	completion, _ := getVarint(mf, metaUsageCompletion)
	cacheWrite, hasWrite := getVarint(mf, metaUsageCacheWrite)
	cacheRead, hasRead := getVarint(mf, metaUsageCacheRead)

	promptTokens := prompt + cacheRead
	u := map[string]any{
		"prompt_tokens":     int64(promptTokens),
		"completion_tokens": int64(completion),
		"total_tokens":      int64(promptTokens + completion + cacheWrite),
	}
	if hasRead {
		u["prompt_tokens_details"] = map[string]any{"cached_tokens": int64(cacheRead)}
	}
	if hasWrite {
		u["cache_creation_input_tokens"] = int64(cacheWrite)
	}
	return u
}

// finishReason 决定 finish_reason。
//
// 判据刻意保守（照参考实现 resolveFinishReason）：
//   - 只有 completion_tokens **正好等于**我们写进 wire 的输出上限时才判 length ——
//     把「正常完成」误报成 length 是有害的（会自动续写的客户端会无限循环）；
//   - 其余一律 stop。
//
// 不看 #5 枚举的具体值，因为它没有被完整标定，而错读的代价（把完整回复标成被截断）
// 远大于「少报一次截断」。
func finishReason(finish uint64, hasFinish bool, usage map[string]any, maxTokens int) string {
	if !hasFinish {
		return "stop"
	}
	if maxTokens > 0 && usage != nil {
		if c, ok := usage["completion_tokens"].(int64); ok && c == int64(maxTokens) {
			return "length"
		}
	}
	return "stop"
}

// trailerError 解析 end-of-stream 帧里的错误。
//
// trailer 是 JSON：成功为 `{}`，失败为 `{"error":{"code":…,"message":…}}`。
// 非 JSON / 无 error 字段一律当成功（上游 trailer 里也可能带别的东西）。
func trailerError(payload []byte) error {
	text := strings.TrimSpace(string(payload))
	if text == "" || text == "{}" {
		return nil
	}
	var parsed struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil || parsed.Error == nil {
		return nil
	}
	msg := strings.TrimSpace(parsed.Error.Message)
	if msg == "" {
		msg = truncate(text, 200)
	}
	kind := classify(200, []byte(msg))
	return errs.New(kind, "上游返回错误："+msg).
		WithChannel(string(channel.Windsurf)).WithUpstream(truncate(text, 200))
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
