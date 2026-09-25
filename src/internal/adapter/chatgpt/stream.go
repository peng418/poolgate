package chatgpt

// stream.go SSE 帧 → 标准 OpenAI chunk 流。
//
// 上游是标准 SSE（`data: <json>`，结束 `data: [DONE]`），但**同一条增量有两种形态**，
// 所以不能简单地「见到 v 就当正文」：
//
//	形态一（patch）：{"p":"/message/content/parts/0","o":"append","v":"tok"}
//	                 路径决定它算正文还是思考，o 是 append/replace；
//	形态二（整条消息）：{"v":{"conversation_id":…,"message":{author,content,status,…}}}
//	                 一次给全文快照，还带 status/end_turn 这些结束信号。
//
// 两种形态**可能同时出现**（上游既发增量也发快照）。照搬参考实现的做法：
// 每条事件都算出「到目前为止的完整文本」，与本地的做**前缀差分**，只把多出来的部分
// 发出去。这样重复的快照自然变成空增量，不需要额外去重逻辑 —— 手写去重几乎都会
// 在「快照比本地长一点」这类边界上把正文吞掉。
//
// 思考与正文分流（对上我们的 ReasoningContent）：
//
//	/message/content/thoughts/content   → 思考正文
//	/message/content/thoughts/summary   → 思考摘要（独立累积，不与上面混流）
//	/message/content/parts/0            → 正式回答
//
// 结束判据：助手消息 status == "finished_successfully"、end_turn == true，
// 或流上的 [DONE]。三条都认，因为不同账号/不同版本上游给的不是同一条。

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// ChatGPT 的引用/实体标记用的是 Unicode 私有区码点，肉眼看不见但会原样出现在流里。
// 不剥掉的话，客户端会看到一串莫名其妙的方框或者干脆丢字。
const (
	markStart = '' // 标记开始
	markEnd   = '' // 标记结束
	markSep   = '' // 类型与载荷的分隔
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

// streamState 是流式解析的累计状态。
type streamState struct {
	raw          string // 正文原始累计（含未剥掉的标记）
	clean        string // 正文剥离标记后的累计（做前缀差分用这个）
	rawReason    string // thoughts/content 原始累计
	cleanReason  string
	rawSummary   string // thoughts/summary 原始累计
	cleanSummary string
	// finished 只表示「流结束」（收到 [DONE]），是读取循环的退出条件。
	finished bool
	// turnDone 表示「上游说这一轮答完了」（助手消息 status=finished_successfully /
	// end_turn=true / message_stream_complete）。
	//
	// 它**不**用来提前停止读流 —— 原因见 applySnapshot 的注释，这是本文件里
	// 唯一一处故意偏离「见到结束判据就收工」的地方。
	turnDone bool
}

// emitted 是一次要发出去的增量。
type emitted struct {
	content   string
	reasoning string
}

func (s *stream) pump() {
	defer close(s.ch)

	br := bufio.NewReaderSize(s.rc, 256*1024)
	st := &streamState{}
	sentRole := false
	gotContent := false
	sentFinish := false

	emitRole := func() {
		if sentRole {
			return
		}
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.Delta.Role = "assistant"
		c.Choices = append(c.Choices, ch)
		s.ch <- chunkOrErr{chunk: c}
		sentRole = true
	}
	// emitFinish 发结束帧。
	//
	// finish_reason 恒为 "stop"：工具调用是**网关模拟层**（toolshim）解析正文标记得到的，
	// 它会在网关侧按事实改写 finish_reason；适配器在这里说 "tool_calls" 反而会
	// 让客户端在没有 tool_calls 字段时拿到一个自相矛盾的结束原因。
	emitFinish := func() {
		if sentFinish {
			return
		}
		c := channel.ChatCompletionChunk{Model: s.model}
		ch := channel.ChunkChoice{}
		ch.FinishReason = "stop"
		c.Choices = append(c.Choices, ch)
		s.ch <- chunkOrErr{chunk: c}
		sentFinish = true
	}
	emitDeltas := func(list []emitted) {
		for _, d := range list {
			if d.content == "" && d.reasoning == "" {
				continue
			}
			emitRole()
			c := channel.ChatCompletionChunk{Model: s.model}
			ch := channel.ChunkChoice{}
			ch.Delta.Content = d.content
			ch.Delta.ReasoningContent = d.reasoning
			c.Choices = append(c.Choices, ch)
			s.ch <- chunkOrErr{chunk: c}
			gotContent = true
		}
	}

	for {
		line, err := br.ReadString('\n')
		if line != "" {
			payload, isData := ssePayload(line)
			if isData {
				if payload == "[DONE]" {
					st.finished = true
				} else {
					deltas, ferr := st.apply(payload)
					if ferr != nil {
						s.ch <- chunkOrErr{err: ferr}
						return
					}
					emitDeltas(deltas)
				}
			}
			if st.finished {
				break
			}
		}
		if err != nil {
			if err != io.EOF {
				s.ch <- chunkOrErr{err: errs.New(errs.Transport, "读取上游流失败").
					WithChannel(string(channel.ChatGPT)).WithCause(err)}
			}
			break
		}
	}

	// 有内容就补一个结束帧：流断了但没等到 [DONE] 时，客户端会以为回复被截断
	// （内容其实收全了）。这是补帧，不是补内容。
	//
	// turnDone 但一个字都没有（上游答了一段空内容）也要补：让它拿到一个正常的
	// 「空回复」，而不是一个没有结束原因的半截流。
	if gotContent || st.turnDone {
		emitFinish()
	}
}

// ssePayload 从一行里取出 data 载荷。
func ssePayload(line string) (string, bool) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
}

// apply 处理一条 SSE 事件，返回本次要发出的增量。
func (st *streamState) apply(payload string) ([]emitted, error) {
	var ev map[string]any
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return nil, nil // 解不出的帧跳过：上游会插自己的元数据/心跳
	}

	// 1) 帧内错误：身分类 → 凭证要重登；其余 → 上游故障。
	//    分错的代价很实际：判成凭证问题会**禁用账号**（SessionDead 的策略是 Disable），
	//    而真·凭证过期判成上游故障只会白重试几轮。
	if e, ok := ev["error"].(map[string]any); ok {
		msg := strAny(e["message"], "")
		if strings.TrimSpace(msg) == "" {
			msg = truncate(payload, 200)
		}
		kind := errs.UpstreamFault
		if looksLikeAuthError(msg) {
			kind = errs.SessionDead
		}
		return nil, errs.New(kind, "上游返回错误："+msg).
			WithChannel(string(channel.ChatGPT)).WithUpstream(truncate(payload, 200))
	}

	// 2) 内容审核：这是明确的「本轮被拦」，透传给客户端（ContentBlocked 会原样透传，
	//    用户需要知道是内容问题而不是渠道坏了）。
	if strAny(ev["type"], "") == "moderation" {
		if mod, ok := ev["moderation_response"].(map[string]any); ok && boolAny(mod["blocked"], false) {
			return nil, errs.New(errs.ContentBlocked,
				"上游内容审核拦下了这轮回复（moderation.blocked=true）").
				WithChannel(string(channel.ChatGPT)).WithUpstream(truncate(payload, 200))
		}
	}

	// 3) 结束信号之一：流完成事件。
	if strAny(ev["type"], "") == "message_stream_complete" {
		st.turnDone = true
	}

	var out []emitted
	st.walk(ev, &out)
	return out, nil
}

// walk 递归走一条事件，把增量追加到 out。
func (st *streamState) walk(ev map[string]any, out *[]emitted) {
	if strAny(ev["type"], "") == "title_generation" {
		return // 标题生成事件，不是回复内容
	}
	v, hasV := ev["v"]
	if !hasV {
		return
	}
	switch val := v.(type) {
	case string:
		st.applyPatch(strAny(ev["p"], ""), strAny(ev["o"], ""), val, out)
	case map[string]any:
		st.applySnapshot(val, out)
	case []any:
		// 批量形态：数组里每一项要么是 patch（带 p），要么是整条消息（带 v）。
		for _, item := range val {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := m["v"].(string); ok {
				st.applyPatch(strAny(m["p"], ""), strAny(m["o"], ""), s, out)
				continue
			}
			if inner, ok := m["v"].(map[string]any); ok {
				st.applySnapshot(inner, out)
				continue
			}
			// 嵌套的批量补丁（项自己带 o:"patch" 与一段数组）→ 递归再走一遍。
			if _, ok := m["v"].([]any); ok {
				st.walk(m, out)
				continue
			}
			// 有些项直接就是消息对象（没有外层 v）。
			if _, ok := m["message"]; ok {
				st.applySnapshot(m, out)
			}
		}
	}
}

// applyPatch 处理 patch 形态的增量。
func (st *streamState) applyPatch(path, op, value string, out *[]emitted) {
	switch {
	case path == "/message/content/parts/0":
		st.pushText(&st.raw, &st.clean, value, op, out, true)
	case path == "/message/content/thoughts/content":
		st.pushText(&st.rawReason, &st.cleanReason, value, op, out, false)
	case path == "/message/content/thoughts/summary":
		st.pushText(&st.rawSummary, &st.cleanSummary, value, op, out, false)
	}
	// 其余路径（metadata/*、asset_pointer、status…）不是给用户看的文本，忽略。
}

// applySnapshot 处理「整条消息」形态。
func (st *streamState) applySnapshot(m map[string]any, out *[]emitted) {
	msg, ok := m["message"].(map[string]any)
	if !ok {
		// 少数事件把消息直接摊在 v 上（有 author 就是消息体）。
		if _, hasAuthor := m["author"]; !hasAuthor {
			return
		}
		msg = m
	}
	author, _ := msg["author"].(map[string]any)
	if strAny(author["role"], "") != "assistant" {
		// 用户消息/系统上下文节点的快照**绝不能**当正文发出去 ——
		// 那会把用户自己刚发的提示词当成模型的回答（客户端看着像「模型复读」）。
		//
		// 也正因为如此，**不能**在这里认结束判据：一条对话流的第一帧往往就是
		// 用户的快照，而它同样带 status=finished_successfully（用户那句话本来就已经"发完"了）。
		// 早先版本在这里判结束，症状是「正文一个字都没发出来，流就结束了」。
		return
	}
	// 结束判据只认**助手**消息；而且只记账、不提前收工。
	//
	// 为什么不收工：一轮对话里助手可能产生**多条**消息（先一条 thoughts、再一条正文；
	// 或工具场景下 code + 正文）。在第一条 finished_successfully 上就停，会把后面的正文整段丢掉
	// —— 那正是红线一（禁止静默丢弃）最不能接受的一种。参考实现（Go / gpt4free）
	// 也都是读到流尾（[DONE]）为止。
	if strAny(msg["status"], "") == "finished_successfully" || boolAny(msg["end_turn"], false) {
		st.turnDone = true
	}
	content, _ := msg["content"].(map[string]any)
	if content == nil {
		return
	}
	text, isReason := snapshotText(content, msg)
	switch {
	case isReason:
		st.pushText(&st.rawReason, &st.cleanReason, text, "replace", out, false)
	case text != "":
		st.pushText(&st.raw, &st.clean, text, "replace", out, true)
	}
}

// snapshotText 从一条消息的 content 里取出文本，并判断它是思考还是正文。
//
// 只认这几种 content_type，其余（code / execution_output / *_editable_context …）
// 一律当「不是给用户看的文本」跳过：把它们当正文发出去，用户会看到
// 「模型在回答里念了一段系统上下文」，比丢字更糟。
func snapshotText(content map[string]any, msg map[string]any) (string, bool) {
	ct := strAny(content["content_type"], "")
	if ct == "thoughts" {
		return joinParts(content["parts"]), true
	}
	if ct != "text" && ct != "multimodal_text" {
		return "", false
	}
	text := joinParts(content["parts"])
	meta, _ := msg["metadata"].(map[string]any)
	if boolAny(meta["is_thinking"], false) {
		return text, true
	}
	return text, false
}

func joinParts(raw any) string {
	parts, ok := raw.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if s, ok := p.(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

// pushText 用前缀差分把「到目前为止的全文」变成增量。
//
// isContent=true 表示这是正文（而不是思考），决定增量落到哪个字段。
func (st *streamState) pushText(raw, clean *string, value, op string, out *[]emitted, isContent bool) {
	candidate := value
	if op != "replace" {
		// append（以及没给 op 的形态）都按「接在后面」处理；replace 是整段替换。
		candidate = *raw + value
	}
	next := normalizeMarkup(candidate)
	if next == *clean {
		return
	}
	delta := next
	if strings.HasPrefix(next, *clean) {
		delta = next[len(*clean):]
	}
	// 前缀对不上（replace 覆盖、上游回退重发）时，delta 就是整段 ——
	// 宁可重复一点，也不能丢内容（红线一：禁止静默丢弃）。
	*raw = candidate
	*clean = next
	if delta == "" {
		return
	}
	if isContent {
		*out = append(*out, emitted{content: delta})
	} else {
		*out = append(*out, emitted{reasoning: delta})
	}
}

// ---------------------------------------------------------------------------
// 标记剥离
// ---------------------------------------------------------------------------

// normalizeMarkup 剥掉上游的引用/实体标记。
//
// 标记是 Unicode 私有区里的三个不可见码点：`<E200>kind<E202>payload<E201>`。
// 处理规则（照参考实现）：
//   - entity：载荷是一段 JSON 数组，把里面的可读名用 " - " 连起来（如「歌曲 - 歌手」）；
//   - cite / 其它：整段丢掉（引用编号对客户端没有意义，留着是噪声）；
//   - 没等到结束标记的（半截标记）**整段丢掉**，等下一片到了再说 ——
//     这正是流式场景下必须这么做的原因：标记可能被切成两片。
func normalizeMarkup(text string) string {
	if !strings.ContainsRune(text, markStart) {
		return text
	}
	var b strings.Builder
	i := 0
	for i < len(text) {
		rel := strings.IndexRune(text[i:], markStart)
		if rel < 0 {
			b.WriteString(text[i:])
			break
		}
		start := i + rel
		b.WriteString(text[i:start])
		relEnd := strings.IndexRune(text[start:], markEnd)
		if relEnd < 0 {
			break // 半截标记：丢弃尾段，下一片再拼
		}
		end := start + relEnd
		b.WriteString(readableMark(text[start+len(string(markStart)) : end]))
		i = end + len(string(markEnd))
	}
	return b.String()
}

// readableMark 把一个标记体翻译成人话。
func readableMark(inner string) string {
	kind, payload, ok := strings.Cut(inner, string(markSep))
	if !ok {
		return ""
	}
	if strings.TrimSpace(kind) != "entity" {
		return "" // cite 等一律丢掉
	}
	payload = strings.TrimSpace(payload)
	payload = leadingJSONArray(payload)
	if payload == "" {
		return ""
	}
	var parts []string
	if json.Unmarshal([]byte(payload), &parts) != nil || len(parts) == 0 {
		return ""
	}
	// 第一项若是纯小写的「类型名」（song / book / …），它不是内容，去掉。
	if len(parts) > 1 && isEntityType(parts[0]) {
		parts = parts[1:]
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " - ")
}

// leadingJSONArray 截出开头的那个完整 JSON 数组。
// 标记体后面可能还跟别的东西，直接 Unmarshal 整段会失败。
func leadingJSONArray(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "[") {
		return ""
	}
	inString, escaped, depth := false, false, 0
	for i, r := range text {
		if inString {
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == '"':
				inString = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return text[:i+1]
			}
		}
	}
	return ""
}

// isEntityType 判断一项是不是「类型名」（只由小写字母、下划线、连字符组成）。
func isEntityType(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return false
	}
	for _, r := range v {
		if (r < 'a' || r > 'z') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// looksLikeAuthError 判断帧内错误是不是「凭证问题」。
func looksLikeAuthError(msg string) bool {
	s := strings.ToLower(msg)
	for _, kw := range []string{"token", "unauthorized", "forbidden", "invalid auth", "login", "session", "登录", "凭证"} {
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
