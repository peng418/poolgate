package windsurf

// stream.go Connect 多帧响应 → 标准 OpenAI chunk 流。
//
// 上游不是 SSE：响应体是一串「flags + 4 字节大端长度 + protobuf 载荷」的帧（见 frames.go）。
// 每一帧是一个完整的 GetChatMessageResponse，里面**逐帧给增量**：
//
//	#3 正文增量   #9 思考增量   #5 finish 信号   #7 用量元数据   #6 原生工具调用
//
// 正文与思考是原生分开的两个字段（见 protocol.go 顶部关于 #3/#9 的说明）——
// 把它们搞反，客户端看到的会是模型的思考草稿当正文。
//
// 另外三个「不处理就会骗客户端」的点：
//   - 上游**必须**以 end-of-stream trailer 帧收尾（成功 {}，失败 {"error":{…}}）。
//     连接直接断掉而没有 trailer = 答案被截断，此时必须报错，不能当成正常结束 ——
//     否则半截答案会被写进会话，下一轮模型看到的是残缺上下文。
//   - 结束原因不能只看 #5 那个未标定全的枚举：参考实现实测只有 2/4 被钉住，
//     其余按名字顺序猜都会把「正常完成」读成「被截断」。所以截断改用**不依赖枚举**的
//     判据：completion_tokens 正好撞上我们实际写进 wire 的输出上限。
//   - 原生工具调用（#6）是**跨帧分片**到达的（参考实现 mergeToolCallFragment 的实测，
//     devin-connect.js:1609-1632）：第一帧给 {id,name}，后续帧只给 arguments_json 的
//     碎片。必须按 id 合并、把参数碎片拼回完整 JSON，不能「每解到一条就发一条」。

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
	model     string   // 回给客户端的模型名（保留客户端请求的原名）
	maxTokens int      // 实际写进 wire 的输出上限，判截断用
	toolNames []string // 本轮下发的工具名，工具调用解不到 name 时反查用
	ch        chan chunkOrErr
}

func newStream(rc io.ReadCloser, model string, maxTokens int, toolNames []string) *stream {
	s := &stream{rc: rc, model: model, maxTokens: maxTokens, toolNames: toolNames, ch: make(chan chunkOrErr, 16)}
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
	var toolAcc []toolCallAcc
	// 跨帧多字节字符拼接（wire-01）：正文与思考各一个，互不干扰。
	contentDec := &utf8Stream{}
	reasoningDec := &utf8Stream{}

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
	// emitToolCalls 把合并好的工具调用作为一条 delta 发出（整段参数一次性给，
	// 与 OpenAI 的「按 index 分片」合同兼容，客户端会自己拼）。参考实现是在流尾
	// 组装完成后一次性发出（devin-connect-openai.js:657-676），这里同款。
	emitToolCalls := func() {
		if len(toolAcc) == 0 {
			return
		}
		emitRole()
		calls := make([]channel.ToolCall, 0, len(toolAcc))
		for i, tc := range toolAcc {
			name := tc.name
			if name == "" {
				// 响应侧 ChatToolCall 的 name 字段参考实现自己都存疑（见 protocol.go
				// respFieldToolCalls 处说明）：解不到就用「本轮只下发了一个工具」反查。
				if len(s.toolNames) == 1 {
					name = s.toolNames[0]
				} else {
					name = "unknown" // 与参考实现 devin-connect-openai.js:672 一致
				}
			}
			args := tc.args
			if args == "" {
				args = "{}"
			}
			calls = append(calls, channel.ToolCall{
				Index:    i,
				ID:       tc.id,
				Type:     "function",
				Function: channel.FunctionCall{Name: name, Arguments: args},
			})
		}
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.Delta.ToolCalls = calls
		c.Choices = append(c.Choices, ch)
		emit(c)
	}
	emitFinish := func() {
		if sentFinish {
			return
		}
		emitToolCalls()
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.FinishReason = finishReason(lastFinish, hasFinish, lastUsage, s.maxTokens, len(toolAcc) > 0)
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
				// 工具调用碎片先合并（无论这帧有没有正文）。
				for _, tc := range d.toolCalls {
					toolAcc = mergeToolCallFragment(toolAcc, tc)
				}
				// 跨帧拼好多字节字符后再判空 —— 被「扣下」的半字符这一帧会解出空串。
				content := contentDec.write(d.contentBytes)
				reasoning := reasoningDec.write(d.reasoningBytes)
				if content == "" && reasoning == "" {
					// 元数据帧 / 空帧 / 半字符：仍要收下 finish 与 usage。
					if d.hasFinish {
						lastFinish, hasFinish = d.finish, true
					}
					if d.usage != nil {
						lastUsage = d.usage
					}
					continue
				}
				emitRole()
				if reasoning != "" {
					c := channel.ChatCompletionChunk{Model: s.model}
					ch := channel.ChunkChoice{}
					ch.Delta.ReasoningContent = reasoning
					c.Choices = append(c.Choices, ch)
					emit(c)
				}
				if content != "" {
					c := channel.ChatCompletionChunk{Model: s.model}
					ch := channel.ChunkChoice{}
					ch.Delta.Content = content
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

	// 冲刷跨帧缓冲里最后残留的字节（正常情况下为空）。参考实现在 done 分支先 end()
	// 两个 decoder、再判「没有 trailer 就是截断」（devin-connect.js:2655-2672）——
	// 顺序很重要：已经收到的字节要交付，但截断仍必须报错，不能被当成正常结束。
	if tail := reasoningDec.flush(); tail != "" {
		emitRole()
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.Delta.ReasoningContent = tail
		c.Choices = append(c.Choices, ch)
		emit(c)
	}
	if tail := contentDec.flush(); tail != "" {
		emitRole()
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.Delta.Content = tail
		c.Choices = append(c.Choices, ch)
		emit(c)
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
//
// 正文/思考保留**原始字节**而不是就地转 string：一个多字节字符可能被上游切在两帧
// 边界上，逐帧转 string 会把它解成 U+FFFD（参考实现的 wire-01 修复）。由 pump 里的
// utf8Stream 跨帧拼好再交出去。
type frameDelta struct {
	contentBytes   []byte
	reasoningBytes []byte
	finish         uint64
	hasFinish      bool
	usage          map[string]any
	toolCalls      []rawToolCall
}

// rawToolCall 是一帧里解出的一个（可能是分片的）ChatToolCall。
type rawToolCall struct {
	id   string
	name string
	args string // arguments_json，已按参考实现做过「坏参数 → {}」的容错处置
}

// toolCallAcc 是跨帧累积的、合并后的工具调用。
type toolCallAcc struct {
	id   string
	name string
	args string
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
		contentBytes:   getBytes(fields, respFieldContent),
		reasoningBytes: getBytes(fields, respFieldReasoning),
		toolCalls:      decodeToolCalls(fields),
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

// decodeToolCalls 从一个顶层帧里解出全部 repeated ChatToolCall（#6）。
//
// 子 tag 与字段语义照参考实现 decodeOneToolCall（devin-connect.js:1544-1600）：
//   - 有效 JSON 的 arguments_json → 原样用；
//   - arguments_json 存在但坏、且有 invalid_json_* 信号 → 换成 {} 占位（防止坏参数
//     污染整条工具链），丢弃原文；
//   - arguments_json 存在但坏、且无 invalid 信号 → 保留原串（绝不静默丢数据）；
//   - 只有 invalid 信号、没有 arguments_json → {} 占位。
//
// 解不出任何内容的空条目丢弃；单个子消息解析失败只跳过它，绝不冒泡成流错误。
func decodeToolCalls(fields []pfield) []rawToolCall {
	var out []rawToolCall
	for _, f := range fields {
		if f.field != respFieldToolCalls || f.wire != wireBytes {
			continue
		}
		sub, err := parseFields(f.bytes)
		if err != nil {
			continue
		}
		tc := rawToolCall{
			id:   getString(sub, callFieldID),
			name: getString(sub, callFieldName),
		}
		rawArgs, hasRaw := getBytesOK(sub, callFieldArguments)
		_, hasInvalidStr := getBytesOK(sub, callFieldInvalidJSONStr)
		_, hasInvalidErr := getBytesOK(sub, callFieldInvalidJSONErr)
		hasInvalid := hasInvalidStr || hasInvalidErr
		switch {
		case hasRaw && looksLikeValidJSON(rawArgs):
			tc.args = string(rawArgs)
		case hasRaw && hasInvalid:
			tc.args = "{}"
		case hasRaw:
			tc.args = string(rawArgs)
		case hasInvalid:
			tc.args = "{}"
		}
		if tc.id != "" || tc.name != "" || hasRaw || hasInvalid {
			out = append(out, tc)
		}
	}
	return out
}

// mergeToolCallFragment 把一个分片并入累积器（参考实现 mergeToolCallFragment，
// devin-connect.js:1620-1632）：
//   - 带 id 且与当前打开的那条不同 → 开一条新的；
//   - 不带 id（或 id 相同）→ 参数碎片追加到当前那条；
//   - name 只在第一条上给，后续帧不会重复。
func mergeToolCallFragment(acc []toolCallAcc, tc rawToolCall) []toolCallAcc {
	if len(acc) == 0 {
		return append(acc, toolCallAcc{id: tc.id, name: tc.name, args: tc.args})
	}
	open := &acc[len(acc)-1]
	if tc.id != "" && tc.id != open.id {
		return append(acc, toolCallAcc{id: tc.id, name: tc.name, args: tc.args})
	}
	if tc.id != "" && open.id == "" {
		open.id = tc.id
	}
	if tc.name != "" && open.name == "" {
		open.name = tc.name
	}
	if tc.args != "" {
		open.args += tc.args
	}
	return acc
}

// looksLikeValidJSON 判断一段字节是不是合法 JSON 文本（非空才算）。
func looksLikeValidJSON(b []byte) bool {
	s := strings.TrimSpace(string(b))
	return s != "" && json.Valid([]byte(s))
}

// utf8Stream 跨帧拼接文本增量，避免把一个多字节字符切在两帧边界上解成 U+FFFD。
//
// 参考实现用 Node 的 StringDecoder 做同一件事（devin-connect.js:2466-2467 建两个
// decoder、:2557-2558 逐帧 write、:2655-2658 在流尾 end() 冲刷），并把它记为 wire-01
// 修复：上游按任意边界切帧，中文/emoji 会被拦腰截断，逐帧单独 toString 就成乱码。
// Go 标准库没有现成的增量解码器，这里按 UTF-8 编码规则手做一个小状态机：
// 只交出「当前能完整解码的前缀」，不完整的尾序列留到下一批。
type utf8Stream struct {
	pending []byte
}

// write 追加一批字节，返回当前可安全解码的完整前缀。
func (u *utf8Stream) write(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	u.pending = append(u.pending, b...)
	// 从尾部最多回看 3 字节，找最后一个「非续字节」（0b10xxxxxx 之外）作为候选起始。
	// 找不到（尾部全是续字节，属于非法输入）就按 0 处理，走「原样交付」分支，不无限挂起。
	start := 0
	for i := 1; i <= 3 && i <= len(u.pending); i++ {
		if c := u.pending[len(u.pending)-i]; c&0xC0 != 0x80 {
			start = len(u.pending) - i
			break
		}
	}
	// 候选起始是合法多字节前导、且剩余字节不够拼完这个字符 → 扣下，等下一批。
	if lead := u.pending[start]; validUTF8Lead(lead) {
		if need := utf8LeadLen(lead); need > len(u.pending)-start {
			out := string(u.pending[:start])
			u.pending = append([]byte(nil), u.pending[start:]...)
			return out
		}
	}
	out := string(u.pending)
	u.pending = nil
	return out
}

// flush 交出所有残留字节（流结束时的兜底；若上游在字符中途断流，这里原样解，最多出一个
// U+FFFD，不会有字节被吞掉）。
func (u *utf8Stream) flush() string {
	if len(u.pending) == 0 {
		return ""
	}
	out := string(u.pending)
	u.pending = nil
	return out
}

// validUTF8Lead 判断一个字节是不是合法的多字节前导（不含续字节，也不含 0xC0/0xC1 这种
// 会造成 overlong 编码的非法前导）。
func validUTF8Lead(c byte) bool { return c >= 0xC2 && c <= 0xF4 }

// utf8LeadLen 返回某个前导字节对应的 UTF-8 序列总长度。
func utf8LeadLen(c byte) int {
	switch {
	case c < 0x80:
		return 1
	case c < 0xE0:
		return 2
	case c < 0xF0:
		return 3
	default:
		return 4
	}
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
//   - 有原生工具调用 → tool_calls（参考实现 devin-connect-openai.js:536-545 在组装出
//     tool_calls 后一律把 finish_reason 翻成 tool_calls）；
//   - 只有 completion_tokens **正好等于**我们写进 wire 的输出上限时才判 length ——
//     把「正常完成」误报成 length 是有害的（会自动续写的客户端会无限循环）；
//   - 其余一律 stop。
//
// 不看 #5 枚举的具体值，因为它没有被完整标定，而错读的代价（把完整回复标成被截断）
// 远大于「少报一次截断」。
func finishReason(finish uint64, hasFinish bool, usage map[string]any, maxTokens int, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
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
