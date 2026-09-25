package kiro

// stream.go AWS Event Stream → 标准 OpenAI chunk 流。
//
// 上游是二进制帧（见 protocol.go），所以这里**按帧读**：按行读会把长度头当内容
//（表现是客户端拿到乱码），按整包读会一直等到连接关闭（表现是一直播一直转圈）。
//
// 两个与别家不同的发布策略：
//   - 工具调用**在流尾统一发一条**。原因见 protocol.go 的 dedupToolCalls：上游会把
//     同一条调用拆成多帧、还会先发一条空参数的占位帧，只有收完才能去重成形。
//   - 结束判据是「流干净地结束」：Kiro 没有 done 事件，读到 EOF 就是这轮结束。

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
	ch    chan chunkOrErr
}

func newStream(rc io.ReadCloser, model string) *stream {
	s := &stream{rc: rc, model: model, ch: make(chan chunkOrErr, 16)}
	go s.pump()
	return s
}

func (s *stream) pump() {
	defer close(s.ch)

	br := bufio.NewReaderSize(s.rc, 256*1024)
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)

	p := newParser()
	textMode := false // 帧解析判定失败后切到文本扫描兜底（见 protocol.go）
	sentRole := false
	gotContent := false
	credits := 0.0
	hasCredits := false

	emit := func(c channel.ChatCompletionChunk) {
		s.ch <- chunkOrErr{chunk: c}
	}
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

	// handlePayload 消费一个事件载荷；返回 true 表示这轮到此为止（已抛错）。
	handlePayload := func(raw []byte) bool {
		for _, ev := range p.feed(raw) {
			switch ev.kind {
			case evContent:
				emitRole() // 第一段内容之前先补 role 帧（OpenAI 客户端认这个）
				c := channel.ChatCompletionChunk{Model: s.model}
				ch := channel.ChunkChoice{}
				ch.Delta.Content = ev.text
				c.Choices = append(c.Choices, ch)
				emit(c)
				gotContent = true
			case evUsage:
				credits, hasCredits = ev.credits, true
			case evContextUsage:
				// 上游的上下文占用百分比：归一契约里没有对应字段，不假装成 usage 转出去
			case evError:
				s.ch <- chunkOrErr{err: ev.err}
				return true
			}
		}
		return false
	}

	// consumeFrame 看帧头（错误帧的头最可靠）+ 载荷。
	consumeFrame := func(f eventFrame) bool {
		mt := strings.ToLower(f.headers[":message-type"])
		if mt == "error" || mt == "exception" {
			body := strings.TrimSpace(string(f.payload))
			msg := body // 载荷不是 JSON 时就把原话带出去，别丢成空串（红线一）
			var obj map[string]any
			if json.Unmarshal(f.payload, &obj) == nil {
				if m := errMessageOf(obj); m != "" {
					msg = m
				}
			}
			kind := errs.UpstreamFault
			if looksLikeAuthError(msg) {
				kind = errs.SessionDead
			}
			s.ch <- chunkOrErr{err: errs.New(kind, "上游返回错误").
				WithChannel(string(channel.Kiro)).WithUpstream(truncate(msg, 200))}
			return true
		}
		return handlePayload(f.payload)
	}

	for {
		n, err := br.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)

			// 帧模式：**先消费再压缩**。帧的 payload 是 buf 的子切片，
			// 先压缩会把它们的内容覆盖掉（症状是解析出乱码，极难查）。
			if !textMode {
				frames, rest, framed := splitEventFrames(buf)
				if framed {
					for _, f := range frames {
						if consumeFrame(f) {
							return
						}
					}
					buf = buf[:copy(buf, rest)]
				} else {
					// 队首不是合法帧（中间层插了东西 / 不是标准帧）：
					// 切到参考实现那条文本扫描的路，别让整条流哑掉。
					textMode = true
				}
			}
			if textMode {
				raws, rest := scanTextEvents(buf)
				for _, r := range raws {
					if handlePayload([]byte(r)) {
						return
					}
				}
				buf = buf[:copy(buf, rest)]
			}

			if len(buf) > maxBufferBytes {
				// 攒了 8MB 还切不出东西：上游给的不是我们能认的流，
				// 明确报错比一直吃内存到最后「客户端一直转圈」强。
				s.ch <- chunkOrErr{err: errs.New(errs.Parse,
					"上游字节流里找不到可解析的事件（既不是 AWS Event Stream 帧，也不是事件 JSON）").
					WithChannel(string(channel.Kiro))}
				return
			}
		}

		if err != nil {
			if err != io.EOF {
				s.ch <- chunkOrErr{err: errs.New(errs.Transport, "读取上游流失败").
					WithChannel(string(channel.Kiro)).WithCause(err)}
				return
			}
			break
		}
	}

	// 收尾：先把缓冲里剩下的字节再扫一遍。上游最后一帧可能被截断（消息 CRC 没到，
	// 我们判不出它是完整帧），但**载荷 JSON 可能是完整的** —— 能捞回来的内容别丢。
	if len(buf) > 0 {
		raws, _ := scanTextEvents(buf)
		for _, r := range raws {
			if handlePayload([]byte(r)) {
				return
			}
		}
	}

	// 工具调用去重后整条发出去（见文件头说明）。
	calls := p.finish()
	if len(calls) > 0 {
		emitRole()
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.Delta.ToolCalls = calls
		c.Choices = append(c.Choices, ch)
		emit(c)
	}

	if !gotContent && len(calls) == 0 {
		// 一个事件都没解出来：不补结束帧（补了会让空响应看起来像「正常说完」，
		// 上层要能看到「这轮什么都没拿到」）。直接 EOF 结束。
		return
	}

	finish := "stop"
	if len(calls) > 0 {
		// 有工具调用就必须报 tool_calls：报 stop 会让客户端以为模型只是说了句话，
		// 于是等着一个永远不会来的下一段文本。
		finish = "tool_calls"
	}
	c := channel.ChatCompletionChunk{Model: s.model}
	ch := channel.ChunkChoice{}
	ch.FinishReason = finish
	c.Choices = append(c.Choices, ch)
	if hasCredits {
		// 上游不给 token 数，只给**计费额度**（credits）。如实转出，不编造 token 计数。
		c.Usage = map[string]any{"credits_used": credits}
	}
	emit(c)
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
