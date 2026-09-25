package kimi

// stream.go Connect 帧流 → 标准 OpenAI chunk 流。
//
// 上游不是 SSE：响应体是一串「flag + 4 字节长度 + JSON」的帧（见 protocol.go）。
// 所以这里按**帧**读，而不是按行读 —— 按行读会把长度头当内容，表现是「客户端拿到乱码」。
//
// 思考与正文的分流规则照参考实现（它是实测出来的）：
//   - 事件里 `block.multiStage.stages[0].name == STAGE_NAME_THINKING` 时，
//     status 是 completed 才算「思考结束、开始作答」，否则当前处于思考阶段；
//   - `block.text.flags` 直接写 "thinking"/"answer" 时以它为准；
//   - `block.think.content` 是思考增量；只有**显式**处于 thinking 阶段时，
//     `block.text.content` 才算思考，否则算正文。
//
// 最后一条容易写反：阶段是跨事件持续的，但参考实现只认「当前事件里显式标了 thinking」，
// 我们也照做 —— 擅自把「上一个事件说是思考阶段」的事件也归成思考，会让正文被吞进 reasoning。

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const thinkingStage = "STAGE_NAME_THINKING"

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

	phase := ""
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

	// emitFinish 发结束帧。挑不出工具调用的判据就按 stop —— 网页协议的结束是
	// 「模型说完了」，工具调用是网关模拟层的产物，到这里已经由网关自己按事实纠正。
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
		n, err := br.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			frames, rest := splitFrames(buf)

			// 先处理帧，再压缩缓冲区：帧的 payload 是 buf 的**子切片**，
			// 先压缩会把它们的内容覆盖掉（症状是解析出乱码或解析失败，极难查）。
			for _, f := range frames {
				if f.trailer {
					continue // 结束元数据帧，不是内容
				}
				text := strings.TrimSpace(string(f.payload))
				if text == "" {
					continue
				}
				var ev map[string]any
				if json.Unmarshal([]byte(text), &ev) != nil {
					continue // 解不出的帧跳过：上游会插自己的心跳/元数据帧
				}
				if e, ok := ev["error"].(map[string]any); ok {
					msg, _ := e["message"].(string)
					if strings.TrimSpace(msg) == "" {
						msg = truncate(text, 200)
					}
					kind := errs.UpstreamFault
					// 帧内错误里若是「令牌/身份」问题，那就是凭证失效（要重新粘贴），
					// 不是上游故障 —— 分错了用户会去查网络，而真正该做的是重新登录。
					if looksLikeAuthError(msg) {
						kind = errs.SessionDead
					}
					s.ch <- chunkOrErr{err: errs.New(kind, "上游返回错误："+msg).
						WithChannel(string(channel.Kimi)).WithUpstream(truncate(text, 200))}
					return
				}
				if _, hb := ev["heartbeat"]; hb {
					continue // 心跳帧
				}

				emitRole()

				content, reasoning := extractDelta(&phase, ev)
				if content != "" || reasoning != "" {
					c := channel.ChatCompletionChunk{Model: s.model}
					ch := channel.ChunkChoice{}
					ch.Delta.Content = content
					ch.Delta.ReasoningContent = reasoning
					c.Choices = append(c.Choices, ch)
					emit(c)
					gotContent = true
				}

				if _, done := ev["done"]; done {
					emitFinish()
					return
				}
			}

			// 压掉已消费的前缀，只留半帧（overlapping copy 用 copy 语义即可）。
			buf = buf[:copy(buf, rest)]
		}

		if err != nil {
			if err != io.EOF {
				s.ch <- chunkOrErr{err: err}
				return
			}
			break
		}
	}

	// 流断了但没等到 done：有内容就补一个结束帧（否则 OpenAI 客户端会把整段回复
	// 当成「被截断」—— 内容我们已经收到了，该告诉它这轮正常结束）。这是补帧，不是补内容。
	if gotContent {
		emitFinish()
	}
}

// extractDelta 从事件里取出增量文本，并按需更新当前阶段。
func extractDelta(phase *string, ev map[string]any) (content, reasoning string) {
	explicit := explicitPhase(ev)
	if explicit != "" {
		*phase = explicit
	}
	block, _ := ev["block"].(map[string]any)
	if block == nil {
		return "", ""
	}
	mask, _ := ev["mask"].(string)

	if strings.Contains(mask, "block.think") {
		if think, ok := block["think"].(map[string]any); ok {
			if *phase == "" {
				*phase = "thinking"
			}
			s, _ := think["content"].(string)
			return "", s
		}
	}

	var s string
	if txt, ok := block["text"].(map[string]any); ok {
		s, _ = txt["content"].(string)
	}
	if s == "" {
		return "", ""
	}
	if explicit == "thinking" {
		return "", s
	}
	return s, ""
}

// explicitPhase 读事件里**显式**标出的阶段（没标就返回空串，表示「沿用当前阶段」）。
func explicitPhase(ev map[string]any) string {
	block, _ := ev["block"].(map[string]any)
	if block == nil {
		return ""
	}
	if ms, ok := block["multiStage"].(map[string]any); ok {
		if stages, ok := ms["stages"].([]any); ok && len(stages) > 0 {
			if st, ok := stages[0].(map[string]any); ok {
				if name, _ := st["name"].(string); name == thinkingStage {
					if status, _ := st["status"].(string); status == "completed" {
						return "answer"
					}
					return "thinking"
				}
			}
		}
	}
	if txt, ok := block["text"].(map[string]any); ok {
		if flags, _ := txt["flags"].(string); flags == "thinking" || flags == "answer" {
			return flags
		}
	}
	return ""
}

// looksLikeAuthError 判断帧内错误是不是「凭证问题」。
func looksLikeAuthError(msg string) bool {
	s := strings.ToLower(msg)
	for _, kw := range []string{"token", "auth", "unauthorized", "forbidden", "login", "登录", "凭证"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
