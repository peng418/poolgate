package doubao

// stream.go 豆包的命名事件 SSE → 标准 OpenAI chunk 流。
//
// 与其它网页渠道不同，豆包的帧**带事件名**（`event: CHUNK_DELTA`），所以解析要同时看
// 事件名与 JSON 体：
//   - SSE_ACK         会话建立（带 conversation_id）
//   - CHUNK_DELTA     增量文本，体极简：`{"text":"…"}`
//   - STREAM_ERROR    流内错误（含风控码）
//   - gateway-error   网关级错误（登录态失效多半走这条）
//   - done            结束
//
// **思考与正文的分流**靠一个开关块：`block_type=10040` 第一次出现进入思考、第二次出现退出，
// 两次之间的内容算思考。这是本渠道最容易写错的地方（写错的表现是思考混进正文）。

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const (
	blockThinking = 10040 // 思考开关块（1 次进、2 次出）
	blockText     = 10000 // 文本块
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

	br := bufio.NewReaderSize(s.rc, 1<<20) // 豆包的帧可能很长（读缓冲给足，否则会截断）
	event := ""
	inThinking := false
	thinkBlocks := 0
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

	emitText := func(text, reasoning string) {
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
			if payload != "" && payload != "[DONE]" {
				var obj map[string]any
				if json.Unmarshal([]byte(payload), &obj) == nil {
					done, ferr := s.handleEvent(event, obj, &inThinking, &thinkBlocks, emitRole, emitText)
					if ferr != nil {
						emitRole()
						s.ch <- chunkOrErr{err: ferr}
						return
					}
					if done {
						emitFinish()
						return
					}
				}
			}
		}
		if err == io.EOF {
			// 上游用「连接关闭」表示结束（不一定发 done）：有内容就补结束帧。
			if gotContent {
				emitFinish()
			}
			return
		}
	}
}

// handleEvent 处理一个事件，返回（是否结束，错误）。
func (s *stream) handleEvent(event string, obj map[string]any, inThinking *bool, thinkBlocks *int,
	emitRole func(), emitText func(string, string)) (bool, error) {

	switch event {
	case "done":
		return true, nil
	case "SSE_ACK":
		// 会话已建立：我们每次都把完整历史拼进 prompt（无状态），所以 conversation_id
		// 对我们没用 —— 但它意味着这一轮真的开始了，发一个角色帧。
		emitRole()
		return false, nil
	case "gateway-error":
		code, _ := numeric(obj["code"])
		msg := str(obj["message"])
		if msg == "" {
			msg = truncate(string(mustJSON(obj)), 200)
		}
		if code == codeLoginExpired {
			return false, errs.New(errs.SessionDead, "登录态已失效（上游："+msg+"），请重新粘贴 Cookie").
				WithChannel(string(channel.Doubao)).WithUpstream(truncate(string(mustJSON(obj)), 200))
		}
		return false, errs.New(errs.UpstreamFault, "上游网关错误："+msg).
			WithChannel(string(channel.Doubao)).WithUpstream(truncate(string(mustJSON(obj)), 200))
	case "STREAM_ERROR":
		// 往下走：可能是风控码，也可能只是普通错误码
	}

	// 风控/错误码：无论是事件名还是字段，都要抓出来（宁可多判一次，也不要当内容发出去）。
	if code, ok := numeric(obj["error_code"]); ok && code != 0 {
		msg := str(obj["error_msg"])
		return false, riskError(code, msg, obj)
	}
	if code, ok := numeric(obj["code"]); ok && (code == riskCodeMsToken || code == riskCodeCaptcha) {
		return false, riskError(code, str(obj["message"]), obj)
	}

	if event == "STREAM_ERROR" {
		return false, errs.New(errs.UpstreamFault, "上游返回流内错误："+truncate(string(mustJSON(obj)), 200)).
			WithChannel(string(channel.Doubao)).WithUpstream(truncate(string(mustJSON(obj)), 200))
	}

	// 增量形态一：极简的 {"text": "…"}（CHUNK_DELTA）
	if txt, ok := obj["text"].(string); ok && txt != "" {
		if _, isErr := obj["error_code"]; !isErr {
			if *inThinking {
				emitText("", txt)
			} else {
				emitText(txt, "")
			}
			return false, nil
		}
	}

	// 增量形态二：content_block 数组（块内先给 block_type，再给文本）
	for _, cb := range iterBlocks(obj) {
		bt, _ := numeric(cb["block_type"])
		content, _ := cb["content"].(map[string]any)
		switch bt {
		case blockThinking:
			*thinkBlocks++
			*inThinking = *thinkBlocks == 1 // 第一次进思考、第二次退出
		case blockText:
			tb, _ := content["text_block"].(map[string]any)
			if t, ok := tb["text"].(string); ok && t != "" {
				if *inThinking {
					emitText("", t)
				} else {
					emitText(t, "")
				}
			}
		}
	}
	return false, nil
}

// iterBlocks 从上下一帧里挖出 content_block 数组（可能在 patch_op 或 content 里）。
func iterBlocks(obj map[string]any) []map[string]any {
	var out []map[string]any
	if patch, ok := obj["patch_op"].([]any); ok {
		for _, p := range patch {
			pm, _ := p.(map[string]any)
			if pv, ok := pm["patch_value"].(map[string]any); ok {
				out = append(out, blocksOf(pv)...)
			}
		}
	}
	if c, ok := obj["content"].(map[string]any); ok {
		out = append(out, blocksOf(c)...)
	}
	if cbs, ok := obj["content_block"].([]any); ok {
		out = append(out, toMaps(cbs)...)
	}
	return out
}

func blocksOf(m map[string]any) []map[string]any {
	list, _ := m["content_block"].([]any)
	return toMaps(list)
}

func toMaps(list []any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// riskError 把风控码翻成「用户能照着做」的错误。
//
// 关键判断：这两个码**不是**「等一会儿就好」——710022004 要人去过验证码，
// 710022002 是我们不该带伪造的 msToken（我们现在不带）。所以归成 SessionDead：
// 面板会把账号标成需要重新登录，用户看到的是「去浏览器过一下验证再粘一次」，
// 而不是一个含糊的「上游故障」（那会让他反复重试，越试越糟）。
func riskError(code int, msg string, obj map[string]any) error {
	up := truncate(string(mustJSON(obj)), 200)
	switch code {
	case riskCodeCaptcha:
		return errs.New(errs.SessionDead,
			"上游要求人工验证码（710022004）：请在浏览器里打开豆包完成一次验证，然后重新粘贴 Cookie").
			WithChannel(string(channel.Doubao)).WithUpstream(up)
	case riskCodeMsToken:
		return errs.New(errs.SessionDead,
			"上游风控拦下了这次请求（710022002）：这条路不带 a_bogus 签名，可能被判定为非官方客户端。"+
				"请在浏览器里正常用一次豆包后再重试；持续失败请重新粘贴 Cookie").
			WithChannel(string(channel.Doubao)).WithUpstream(up)
	}
	m := msg
	if m == "" {
		m = "上游错误码 " + strconv.Itoa(code)
	}
	return errs.New(errs.UpstreamFault, m).WithChannel(string(channel.Doubao)).WithUpstream(up)
}

// pumpWholeBody 处理「上游没按 SSE 回」的情况：整包 JSON 要么是错误信封，要么是解析失败。
// 绝不能当成「成功但没内容」（红线二）。
func (s *stream) pumpWholeBody() {
	raw, err := io.ReadAll(io.LimitReader(s.rc, 32<<20))
	if err != nil {
		s.ch <- chunkOrErr{err: err}
		return
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		if code, ok := numeric(obj["code"]); ok && code != 0 {
			s.ch <- chunkOrErr{err: riskError(code, str(obj["message"]), obj)}
			return
		}
	}
	s.ch <- chunkOrErr{err: errs.New(errs.Parse, "上游响应既不是 SSE 也不是可识别的错误信封").
		WithChannel(string(channel.Doubao)).WithUpstream(truncate(string(raw), 200))}
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }

func numeric(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}
