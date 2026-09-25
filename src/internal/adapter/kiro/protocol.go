package kiro

// protocol.go AWS Event Stream 的帧解析 + 载荷事件解析。
//
// 上游响应不是 SSE：它是一串 **AWS Event Stream** 二进制帧。每帧的布局是
//
//	┌────────────── 前导（12 字节）──────────────┐
//	│ 4 字节 总长（含前导、头、载荷、消息 CRC）    │
//	│ 4 字节 头区长度                            │
//	│ 4 字节 前导 CRC（前 8 字节的 CRC32/IEEE）   │
//	├────────────── 头区（变长）────────────────┤
//	│ 每项：1 字节名长 + 名 + 1 字节值类型 + 值   │
//	├────────────── 载荷（变长）────────────────┤
//	│ JSON，形如 {"content":"…"}                 │
//	├────────────── 4 字节 消息 CRC ────────────┤
//
// 必须**按帧切**：TCP 不按帧边界送达，长度头到了而载荷没到齐是常态（半包决不能被
// 当成内容）。这是本渠道最容易写错的地方，表现是「客户端一直转圈」或「拿到乱码」。
//
// 参考实现（AwsEventStreamParser）其实**不解析帧**：它把整个字节流当 UTF-8 文本，
// 用 {"content": 这类前缀找 JSON。那种写法能用，是因为载荷 JSON 在帧里是完整的；
// 但它没有长度校验，遇到半包只能靠「JSON 括弧没配平」猜。我们按帧切（有长度有 CRC，
// 半包判断是确定的），同时保留一条**文本扫描兜底**：万一中间层给的不是标准帧
// （或帧里混了非帧字节），退回参考实现那条路，不至于整条流哑掉。

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// ---------------------------------------------------------------------------
// 帧切分
// ---------------------------------------------------------------------------

const (
	framePreludeLen = 12 // 总长(4) + 头长(4) + 前导 CRC(4)
	frameCRCLen     = 4
	minFrameLen     = framePreludeLen + frameCRCLen
)

// eventFrame 是切出来的一个完整帧。payload 是调用方缓冲区的**子切片**，
// 必须在缓冲区被压缩/复用之前消费掉（同 kimi 的 Connect 帧，坑一模一样）。
type eventFrame struct {
	headers map[string]string
	payload []byte
}

// splitEventFrames 从 buf 里尽量切出完整帧。
//
// 返回值：
//   - frames：切出的完整帧；
//   - rest：还不完整、留着等下一批的尾巴；
//   - framed：队首看起来是不是**合法帧**。false 表示「这不是帧」（长度头不合理或
//     前导 CRC 不对），调用方应改用文本扫描兜底。缓冲不足一个前导长度时返回 true（继续等）。
func splitEventFrames(buf []byte) (frames []eventFrame, rest []byte, framed bool) {
	off := 0
	for off < len(buf) {
		if len(buf)-off < framePreludeLen {
			return frames, buf[off:], true // 半帧：等下一批，别猜
		}
		total := int(binary.BigEndian.Uint32(buf[off:]))
		hlen := int(binary.BigEndian.Uint32(buf[off+4:]))
		if total < minFrameLen || total > maxFrameLen || hlen > total-minFrameLen {
			return frames, buf[off:], false // 长度头不合法：这不是帧
		}
		// 前导 CRC 只覆盖前 8 字节，所以「够了 12 字节」就能判定，不必等整帧。
		if crc32.ChecksumIEEE(buf[off:off+8]) != binary.BigEndian.Uint32(buf[off+8:]) {
			return frames, buf[off:], false // CRC 不对：这不是帧
		}
		if off+total > len(buf) {
			return frames, buf[off:], true // 载荷还没到齐：等
		}
		hdrEnd := off + framePreludeLen + hlen
		payEnd := off + total - frameCRCLen // payEnd >= hdrEnd，上面已用 hlen 上限保证
		frames = append(frames, eventFrame{
			headers: parseFrameHeaders(buf[off+framePreludeLen : hdrEnd]),
			payload: buf[hdrEnd:payEnd],
		})
		off += total
	}
	return frames, nil, true
}

// parseFrameHeaders 解头区。头名带前导冒号（如 ":event-type"、":message-type"），
// 我们只用得上这几个元数据；解不动的头一律跳过（不要让一个陌生头把整帧废掉）。
func parseFrameHeaders(b []byte) map[string]string {
	out := make(map[string]string, 4)
	for i := 0; i < len(b); {
		nameLen := int(b[i])
		i++
		if nameLen == 0 || i+nameLen+1 > len(b) {
			break
		}
		name := string(b[i : i+nameLen])
		i += nameLen
		vt := b[i]
		i++
		val, n := headerValue(vt, b[i:])
		if n < 0 {
			break
		}
		i += n
		out[name] = val
	}
	return out
}

// headerValue 按 AWS Event Stream 的值类型解一个头值，返回（字符串值，消费字节数）。
// 只需要字符串（type 7）与布尔（type 0/1，占 0 字节）有用；其余类型照长度跳过。
func headerValue(vt byte, b []byte) (string, int) {
	switch vt {
	case 0: // true
		return "true", 0
	case 1: // false
		return "false", 0
	case 2: // 有符号字节
		if len(b) < 1 {
			return "", -1
		}
		return fmt.Sprintf("%d", int8(b[0])), 1
	case 3: // 16 位整数
		if len(b) < 2 {
			return "", -1
		}
		return fmt.Sprintf("%d", int16(binary.BigEndian.Uint16(b))), 2
	case 4: // 32 位整数
		if len(b) < 4 {
			return "", -1
		}
		return fmt.Sprintf("%d", int32(binary.BigEndian.Uint32(b))), 4
	case 5: // 64 位整数
		if len(b) < 8 {
			return "", -1
		}
		return fmt.Sprintf("%d", int64(binary.BigEndian.Uint64(b))), 8
	case 6: // 字节数组：2 字节长度 + 内容
		if len(b) < 2 {
			return "", -1
		}
		n := int(binary.BigEndian.Uint16(b))
		if len(b) < 2+n {
			return "", -1
		}
		return string(b[2 : 2+n]), 2 + n
	case 7: // 字符串：2 字节长度 + UTF-8
		if len(b) < 2 {
			return "", -1
		}
		n := int(binary.BigEndian.Uint16(b))
		if len(b) < 2+n {
			return "", -1
		}
		return string(b[2 : 2+n]), 2 + n
	case 8: // 时间戳
		if len(b) < 8 {
			return "", -1
		}
		return fmt.Sprintf("%d", int64(binary.BigEndian.Uint64(b))), 8
	case 9: // UUID
		if len(b) < 16 {
			return "", -1
		}
		return hex.EncodeToString(b[:16]), 16
	}
	return "", -1
}

// ---------------------------------------------------------------------------
// 载荷事件
// ---------------------------------------------------------------------------

// 事件类型（归一后的）。
const (
	evContent      = "content"
	evUsage        = "usage"
	evContextUsage = "context_usage"
	evError        = "error"
)

// parsedEvent 是 feed 交出来的一件事。工具调用不走这里 —— 它们要整条流收完、
// 去重之后才成形（见 finish）。
type parsedEvent struct {
	kind    string
	text    string  // evContent 的文本增量
	credits float64 // evUsage 的计费额度
	err     *errs.Error
}

// 事件类型靠**载荷 JSON 里最先出现的键**判定，与参考实现的 EVENT_PATTERNS 一一对应。
// 为什么不用 :event-type 头？因为上游把工具调用的增量拆成多帧时，键的组合会变
// （{"name":…,"input":…} / {"input":…} / {"stop":…}），而头里的类型名我们并没有可靠清单；
// 前缀判定是参考实现实测出来的稳定判据。头里的元数据只用来判「这一帧是不是错误」。
var eventPatterns = []struct{ prefix, kind string }{
	{`{"content":`, evContent},
	{`{"name":`, "tool_start"},
	{`{"input":`, "tool_input"},
	{`{"stop":`, "tool_stop"},
	{`{"followupPrompt":`, "followup"},
	{`{"usage":`, evUsage},
	{`{"contextUsagePercentage":`, evContextUsage},
}

// eventKind 返回载荷所属的事件类型（认不出返回空串）。
func eventKind(text string) string {
	best, kind := -1, ""
	for _, p := range eventPatterns {
		i := strings.Index(text, p.prefix)
		if i >= 0 && (best < 0 || i < best) {
			best, kind = i, p.kind
		}
	}
	return kind
}

// awsEventParser 累积一次响应里的内容与工具调用。
type awsEventParser struct {
	lastContent string // 上一段正文，用于去重（上游会重复发同一段）
	cur         *channel.ToolCall
	tools       []channel.ToolCall
}

func newParser() *awsEventParser { return &awsEventParser{} }

// feed 吃一个帧载荷，吐出即时事件（正文增量 / 计费 / 错误）。
// 工具调用的帧在这里只更新内部状态，不产出事件。
func (p *awsEventParser) feed(payload []byte) []parsedEvent {
	text := strings.TrimSpace(string(payload))
	if text == "" {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(text), &obj) != nil {
		return nil // 解不出的载荷跳过：上游会插自己的心跳/元数据帧
	}

	// 正文/计费/心跳按参考实现的前缀判据（扫描兜底那条路也靠同一套前缀找边界）。
	switch eventKind(text) {
	case evContent:
		return p.contentEvent(obj)
	case evUsage:
		if n, ok := numOf(obj["usage"]); ok {
			return []parsedEvent{{kind: evUsage, credits: n}}
		}
		return nil
	case evContextUsage:
		// 上游的「上下文占用百分比」在我们的归一契约里没有对应字段，只当心跳
		return []parsedEvent{{kind: evContextUsage}}
	case "followup":
		return nil // 上游的「下一步建议」，不是模型输出
	}

	// 工具相关帧按**键**分派，而不是按前缀：参考实现靠「前缀在文本里出现得最早」
	// 判类型，那隐含依赖上游的键序（{"name":…,"input":…}）。上游的键序一旦变
	//（比如 input 排到 name 前面），前缀判据就会把「开始一次工具调用」误判成
	//「追加参数」，那条调用就丢了。按键分派与顺序无关，也不改变原判据下的行为。
	switch {
	case hasKey(obj, "name"):
		p.toolStart(obj) // 开始一次调用（同一帧里带 stop 就直接收口）
	case hasKey(obj, "input"):
		p.toolInput(obj) // 追加参数增量
	case hasKey(obj, "stop"):
		p.toolStop(obj) // 收口
	case hasKey(obj, "content"):
		return p.contentEvent(obj) // 键序异常但确实是正文的帧
	default:
		// 认不出的载荷：如果它长得像错误（有 message / __type），就当成错误抛出去。
		// 参考实现在这里是**静默丢弃**的 —— 那正是我们明令禁止的（红线一）。
		if msg := errMessageOf(obj); msg != "" {
			kind := errs.UpstreamFault
			if looksLikeAuthError(msg) {
				kind = errs.SessionDead // 令牌/身份问题 → 该重新登录，不是上游故障
			}
			return []parsedEvent{{kind: evError, err: errs.New(kind, "上游返回错误："+msg).
				WithChannel(string(channel.Kiro)).WithUpstream(truncate(text, 200))}}
		}
	}
	return nil
}

// hasKey 判断 JSON 对象里有没有这个键（不看值的真假）。
func hasKey(obj map[string]any, key string) bool {
	_, ok := obj[key]
	return ok
}

// contentEvent 取正文增量。
//
// 两条过滤：带 followupPrompt 的帧是上游的「下一步建议」（不是模型输出）；
// 与上一段完全相同的增量丢掉（上游会重复发 —— 参考实现同样处理）。
func (p *awsEventParser) contentEvent(obj map[string]any) []parsedEvent {
	if nonEmpty(obj["followupPrompt"]) {
		return nil
	}
	s, _ := obj["content"].(string)
	if s == "" || s == p.lastContent {
		return nil
	}
	p.lastContent = s
	return []parsedEvent{{kind: evContent, text: s}}
}

// nonEmpty 判断一个 JSON 字段是不是「有内容」。
//
// 参考实现直接拿它做真值判断（非空字符串、true、非 0 数字都算有），
// 所以这里不能用 `.(bool)` 断言 —— 上游有时给字符串 "true"，有时给一段提示文本。
func nonEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return strings.TrimSpace(x) != ""
	case float64:
		return x != 0
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	}
	return true
}

// toolStart 开始一次工具调用。若上一通还没等到 stop 就又来了一个 start，
// 先把上一通收口（宁可多发一条，也不能把已经收到的参数丢掉）。
func (p *awsEventParser) toolStart(obj map[string]any) {
	if p.cur != nil {
		p.finalize()
	}
	name, _ := obj["name"].(string)
	id, _ := obj["toolUseId"].(string)
	if id == "" {
		id = newCallID() // 上游没给 id：自己造一个，客户端要靠它回传工具结果
	}
	p.cur = &channel.ToolCall{
		ID:       id,
		Type:     "function",
		Function: channel.FunctionCall{Name: name, Arguments: inputOf(obj["input"])},
	}
	if nonEmpty(obj["stop"]) {
		p.finalize()
	}
}

// toolInput 追加参数增量。上游把一条工具调用的 input 拆成多帧发。
func (p *awsEventParser) toolInput(obj map[string]any) {
	if p.cur == nil {
		return
	}
	p.cur.Function.Arguments += inputOf(obj["input"])
}

// toolStop 收口。只有显式 stop=true 才收 —— stop=false 的帧是「还没完」。
func (p *awsEventParser) toolStop(obj map[string]any) {
	if p.cur != nil && nonEmpty(obj["stop"]) {
		p.finalize()
	}
}

// finalize 把当前的半成品工具调用落到列表里，并归一参数。
func (p *awsEventParser) finalize() {
	if p.cur == nil {
		return
	}
	c := *p.cur
	p.cur = nil
	args := strings.TrimSpace(c.Function.Arguments)
	switch {
	case args == "":
		args = "{}" // 没有参数的调用：给合法空对象，客户端才能解析
	case !json.Valid([]byte(args)):
		// 参数被上游截断/拼不成合法 JSON：给 "{}" 而不是把坏 JSON 透给客户端
		//（参考实现同样兜 "{}"；它还会打一条截断诊断日志）。
		args = "{}"
	}
	c.Function.Arguments = args
	p.tools = append(p.tools, c)
}

// finish 收尾：finalize 残留的半成品，去重，补上 index。
func (p *awsEventParser) finish() []channel.ToolCall {
	if p.cur != nil {
		p.finalize()
	}
	out := dedupToolCalls(p.tools)
	for i := range out {
		out[i].Index = i // OpenAI 流式规范要求显式 index，客户端靠它拼分片
	}
	return out
}

// dedupToolCalls 去掉上游重复下发的工具调用。
//
// 两条判据（照参考实现）：
//  1. 同一个 id 出现多次 → 留参数更全的那条（上游会先发一条空参数的占位）；
//  2. name+arguments 完全相同的 → 只留一条。
//
// 不过滤会怎样：客户端拿到两条同 id 的 tool_call，回传工具结果时上游会认不出
// （它只等一条），整轮 agent 卡死。
func dedupToolCalls(in []channel.ToolCall) []channel.ToolCall {
	byID := make(map[string]channel.ToolCall, len(in))
	order := make([]string, 0, len(in))
	var noID []channel.ToolCall
	for _, tc := range in {
		if tc.ID == "" {
			noID = append(noID, tc)
			continue
		}
		old, ok := byID[tc.ID]
		if !ok {
			byID[tc.ID] = tc
			order = append(order, tc.ID)
			continue
		}
		if betterArgs(tc.Function.Arguments, old.Function.Arguments) {
			byID[tc.ID] = tc
		}
	}
	seen := make(map[string]bool, len(in))
	out := make([]channel.ToolCall, 0, len(in))
	add := func(tc channel.ToolCall) {
		key := tc.Function.Name + "-" + tc.Function.Arguments
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, tc)
	}
	for _, id := range order {
		add(byID[id])
	}
	for _, tc := range noID {
		add(tc)
	}
	return out
}

// betterArgs 判断新参数是否比旧的更全（非空、且不比旧的短）。
func betterArgs(newArgs, oldArgs string) bool {
	if newArgs == "{}" || newArgs == "" {
		return false
	}
	return oldArgs == "{}" || oldArgs == "" || len(newArgs) > len(oldArgs)
}

// ---------------------------------------------------------------------------
// 文本扫描兜底
// ---------------------------------------------------------------------------

// scanTextEvents 是「不按帧切」的那条退路：在原始字节里找事件前缀，配平括弧取出 JSON。
// 与参考实现的做法一致。只在帧解析判定失败时才用（见 stream.go）。
//
// 返回取出的 JSON 串与还不完整的尾巴。
func scanTextEvents(buf []byte) ([]string, []byte) {
	s := string(buf)
	var out []string
	for {
		start, kind := earliestPattern(s)
		if start < 0 {
			return out, []byte(s)
		}
		_ = kind
		end := findMatchingBrace(s, start)
		if end < 0 {
			return out, []byte(s[start:]) // JSON 还没配平：留着等下一批
		}
		out = append(out, s[start:end+1])
		s = s[end+1:]
	}
}

// earliestPattern 找最先出现的事件前缀，返回（位置，类型）。
func earliestPattern(s string) (int, string) {
	best, kind := -1, ""
	for _, p := range eventPatterns {
		i := strings.Index(s, p.prefix)
		if i >= 0 && (best < 0 || i < best) {
			best, kind = i, p.kind
		}
	}
	return best, kind
}

// findMatchingBrace 从 s[start]（必须是 '{'）开始配平括弧，返回闭括号位置，找不到返回 -1。
// 必须跳过字符串里的括弧与转义（内容里带 `{`/`}` 是常态，比如工具参数或代码片段）。
func findMatchingBrace(s string, start int) int {
	if start < 0 || start >= len(s) || s[start] != '{' {
		return -1
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if inStr && c == '\\' {
			esc = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// inputOf 把工具参数片段转成字符串。
//
// 空对象/空数组要返回**空串**而不是 "{}"：上游先发一帧 {"input":{}} 占位、
// 再发若干帧补增量，若把 "{} "当成已有内容拼进去，最终会得到 "{}{…}" 这种非法 JSON。
func inputOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case map[string]any:
		if len(x) == 0 {
			return ""
		}
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	case []any:
		if len(x) == 0 {
			return ""
		}
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		b, err := json.Marshal(x)
		if err != nil || string(b) == "null" {
			return ""
		}
		return string(b)
	}
}

// numOf 取一个数字（JSON 解出来都是 float64）。
func numOf(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

// errMessageOf 从错误载荷里取人话。AWS 用 message / Message，Amz-Json 用 __type。
func errMessageOf(obj map[string]any) string {
	for _, k := range []string{"message", "Message", "errorMessage"} {
		if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if s, ok := obj["__type"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return ""
}

// looksLikeAuthError 判断一句上游原话是不是「凭证问题」。
// 分错的代价：判成上游故障 → 用户去查网络；判成凭证问题 → 用户去重新登录。
func looksLikeAuthError(msg string) bool {
	s := strings.ToLower(msg)
	for _, kw := range []string{"token", "unauthorized", "forbidden", "expired", "credential",
		"not authorized", "accessdenied", "登录", "凭证"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// truncate 截断上游原话，避免把整段响应塞进错误流水。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// randHex 生成 n 字节的随机十六进制串。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 极端情况（熵源不可用）退回时间戳：绝不因为随机数取不到就让请求失败。
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// newCallID 生成一个工具调用 id（上游没给 toolUseId 时的兜底）。
func newCallID() string { return "call_" + randHex(4) }

// uuid4 生成 v4 UUID（amz-sdk-invocation-id 要的是这个形态）。
func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}
