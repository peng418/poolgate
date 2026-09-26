package qwenwork

// 本文件实现千问办公的**上游会话复用**与**单帧限长自查**（0.10.0）。
//
// 背景（2026-09-27 真机定案）：
//
//	上游 chat-ws 的**单个消息上限是 32768 字节**。我们此前每一轮都把客户端给的
//	**全量历史**塞进一个 new_prompt 帧，上下文一长（实测 Hermes 会话 ~27k tokens）
//	就必然越线，上游立刻以
//	    close 1009 (message too big): read limited at 32769 bytes
//	掐断连接 —— 而适配器此前把它一律归成 Transport「上游连接中断且未收到内容」，
//	既不说明真原因（红线一），又让用户以为「上游坏了 / 连不上」。
//
// 真机实测（同一账号 9567c2c8，2026-09-27）：
//   - 正文 31,844 B → 成功；33,845 B → 立刻被 close 1009 掐断（0.12s）⇒ 上限 32768 B。
//   - 同一条 ws 连发两轮 → 第二轮答对第一轮「记住」的数字 ⇒ 上游会话**有状态**。
//   - 第一轮结束后**断开**，用新 ws + `join(lastSeqId=已收到的最大 seq)` 重挂同一会话，
//     只发最新一轮 → 第二轮仍答对 ⇒ **重挂可续上下文**。
//   - 反过来用 `join(lastSeqId=0)` 重挂：上游会全量 replay，实测**丢上下文**，
//     而且重放的历史正文会漏进本轮输出。
//
// 于是正确形态不是「重发历史」，而是「复用上游会话、每轮只发最新一轮」：
// 既绕开单帧上限，又不浪费上游配额，也和厂商客户端的行为一致（它也只发最新一轮）。

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const (
	// maxFrameBytes 是上游 chat-ws 允许的**单个消息**上限（含 JSON 外壳）。
	// 真机实测：32768 字节通过，32769 被 close 1009 掐断。
	maxFrameBytes = 32768

	// convWindow 是会话指纹的比对窗口：只比最后 N 条**用户正文**即可判定
	// 「这就是上一轮那段对话」。用用户消息而不是助手消息做指纹，是因为
	// 助手正文是上一轮我们从上游吐出去、再由客户端回填的，无法保证逐字节一致。
	convWindow = 4

	// maxConvs 是缓存的上游会话数上限（LRU 淘汰最久未用）。
	maxConvs = 8

	// convIdleTTL 是会话复用有效期：上游会话可能已被回收，超时就重新建。
	convIdleTTL = 45 * time.Minute
)

// convState 是一段「客户端对话 ↔ 上游会话」的对应关系。
type convState struct {
	uid       string
	model     string
	sessionID string

	// lastSeq 是我们已消费到的事件序号。重挂时必须 `join.lastSeqId=lastSeq`：
	// 少了它会触发上游全量 replay（实测丢上下文 + 历史正文漏进本轮）。
	lastSeq int64

	// uCount 是该上游会话里已包含的用户消息条数；uTail 是最后 convWindow 条用户正文。
	uCount int
	uTail  []string

	// sysHash 是最近一次随帧发出的系统提示指纹；变化时下一轮补发一次。
	sysHash string

	usedAt time.Time
}

// promptFrame 构造 new_prompt 的完整 ws 帧。
//
// 发送与「发前限长自查」共用同一份字节：自查量的是真发出去的那串，不是估算。
func promptFrame(sessionID, text string, id int) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type": "control",
		"payload": map[string]any{
			"jsonrpc": "2.0",
			"method":  "new_prompt",
			"id":      id,
			"params": map[string]any{
				"sessionId": sessionID,
				"text":      text,
				"_meta":     map[string]any{"channel": wsChannel},
			},
		},
	})
}

// tooLongError 是「本轮上下文过大」的统一错误。
//
// 归 PromptTooLong（策略：透传给客户端、不记账号错误、不重试）——
// 这是**调用方请求体积**的问题，不是账号问题，换号重试毫无意义。
func tooLongError(frameLen int, upstream string) error {
	e := errs.New(errs.PromptTooLong, fmt.Sprintf(
		"本轮上下文过大：组装出的上游帧 %d 字节，超过千问办公 chat-ws 单帧上限 %d 字节。"+
			"上游收到超限帧会直接掐断连接（那是「帧太大」，不是账号坏、也不是连不上上游）。"+
			"请开新会话，或缩短上下文后重试 —— 网关不会替你静默截断上下文。",
		frameLen, maxFrameBytes)).WithChannel("qwenwork")
	if upstream != "" {
		e = e.WithUpstream(upstream)
	}
	return e
}

// planTurn 决定本轮是「复用既有上游会话」还是「新建会话发全量」。
//
// 复用条件（全部满足才算命中）：
//   - 同一账号、同一模型档位、距上次使用未超 convIdleTTL；
//   - 请求里的用户消息条数**多于**该会话已包含的条数（确实有新的一轮）；
//   - 前 uCount 条用户正文与台账记录一致（比对最后 convWindow 条）。
//
// 未命中就走新建：发全量历史，但**先做单帧限长自查**，超限当场拒绝，
// 既不建会话也不消耗上游配额。
func (a *Adapter) planTurn(uid, model string, msgs []channel.Message) (turnPlan, error) {
	users := userTexts(msgs)
	uIdx := userIndexes(msgs)
	sys := systemPart(msgs)
	sysH := hashOf(sys)

	if st := a.matchConv(uid, model, users); st != nil {
		// 只发「第一个新用户消息」之后的内容：客户端会把我们上一轮的助手正文
		// 回填进历史，那是上游会话里已经有的，不能重发。
		from := uIdx[st.uCount]
		text := promptTextOf(msgs[from:])
		sysChanged := st.sysHash != sysH && sys != ""
		if sysChanged {
			// 系统提示变了（客户端每轮拼的 system 段可能变）：补发一次，让上游
			// 拿到最新指令。不重建会话 —— 重建就得重发全量，正是我们要避开的。
			//
			// 但补发同样受单帧上限约束：系统段本身很大时（长工具说明 / 长上下文）
			// 补发会直接把帧撑爆。这时**退回只发最新一轮**并落日志 ——
			// 宁可本轮沿用先前指令，也不能把整轮打死；不静默，日志里说得清。
			if withSys := sys + text; fitsFrame(st.sessionID, withSys) {
				text = withSys
			} else {
				log.Printf("poolgate: 渠道 qwenwork 系统提示有变化，但补发后单帧超过 %d 字节上限，本轮只发最新一轮（上游会话沿用先前指令）", maxFrameBytes)
				sysChanged = false
			}
		}
		frame, err := promptFrame(st.sessionID, text, 0)
		if err != nil {
			return turnPlan{}, errs.New(errs.Transport, "构造上游请求失败").WithCause(err).WithChannel("qwenwork")
		}
		if len(frame) > maxFrameBytes {
			return turnPlan{}, tooLongError(len(frame), "")
		}
		// 台账按「本轮结束后的对话内容」更新，成功后才由 commitConv 落库。
		have := st.uCount
		st.uCount = len(users)
		st.uTail = tailOf(users, convWindow)
		if sysChanged {
			st.sysHash = sysH
		}
		log.Printf("poolgate: 渠道 qwenwork 复用上游会话 %s（已含 %d 轮用户消息，join.lastSeqId=%d），本轮只发最新一轮 %d 字节 —— 不再重发全量历史",
			st.sessionID, have, st.lastSeq, len(text))
		return turnPlan{resumed: true, sessionID: st.sessionID, lastSeq: st.lastSeq, text: text, conv: st}, nil
	}

	text := promptTextOf(msgs)
	// 用与真实会话 id 等长的占位 id 预估外壳长度（id 是固定 36 字符 UUID）。
	probe, err := promptFrame(strings.Repeat("0", 36), text, 0)
	if err != nil {
		return turnPlan{}, errs.New(errs.Transport, "构造上游请求失败").WithCause(err).WithChannel("qwenwork")
	}
	if len(probe) > maxFrameBytes {
		return turnPlan{}, tooLongError(len(probe), "")
	}
	st := &convState{
		uid: uid, model: model,
		uCount: len(users), uTail: tailOf(users, convWindow),
		sysHash: sysH, usedAt: time.Now(),
	}
	return turnPlan{text: text, conv: st}, nil
}

// turnPlan 是本轮的执行方案。
type turnPlan struct {
	resumed   bool   // true = 复用既有上游会话（只发最新一轮）
	sessionID string // 非空即复用目标
	lastSeq   int64  // 重挂时 join 的 lastSeqId
	text      string // 本轮真正发出去的正文
	conv      *convState
}

// matchConv 在台账里找一段「请求是其延续」的上游会话。
func (a *Adapter) matchConv(uid, model string, users []string) *convState {
	if len(users) == 0 {
		return nil
	}
	a.convMu.Lock()
	defer a.convMu.Unlock()
	now := time.Now()
	var best *convState
	for _, st := range a.convs {
		if st.uid != uid || st.model != model {
			continue
		}
		if now.Sub(st.usedAt) > convIdleTTL {
			continue
		}
		if len(users) <= st.uCount {
			continue
		}
		if !tailMatches(users, st) {
			continue
		}
		if best == nil || st.uCount > best.uCount {
			best = st
		}
	}
	return best
}

// tailMatches 比对指纹窗口：users 的第 uCount 条**之前**的 convWindow 条要与台账一致。
func tailMatches(users []string, st *convState) bool {
	w := len(st.uTail)
	if w == 0 || st.uCount < w {
		return false
	}
	start := st.uCount - w
	if start+w > len(users) {
		return false
	}
	for i := 0; i < w; i++ {
		if users[start+i] != st.uTail[i] {
			return false
		}
	}
	return true
}

// commitConv 一轮成功后才把会话记入台账 —— 失败的会话不敢再复用。
func (a *Adapter) commitConv(st *convState, lastSeq int64) {
	if st == nil || st.sessionID == "" {
		return
	}
	a.convMu.Lock()
	defer a.convMu.Unlock()
	st.lastSeq = lastSeq
	st.usedAt = time.Now()
	for i, e := range a.convs {
		if e.sessionID == st.sessionID {
			a.convs[i] = st
			return
		}
	}
	a.convs = append(a.convs, st)
	if len(a.convs) > maxConvs {
		a.convs = append([]*convState(nil), a.convs[len(a.convs)-maxConvs:]...)
	}
}

// dropConv 摘掉一个上游会话（本轮失败 ⇒ 不再复用）。
func (a *Adapter) dropConv(sessionID string) {
	if sessionID == "" {
		return
	}
	a.convMu.Lock()
	defer a.convMu.Unlock()
	out := a.convs[:0]
	for _, e := range a.convs {
		if e.sessionID != sessionID {
			out = append(out, e)
		}
	}
	a.convs = out
}

// ---------------------------------------------------------------------------
// 消息工具
// ---------------------------------------------------------------------------

// userTexts 取所有用户消息的正文（会话指纹只用用户消息）。
func userTexts(msgs []channel.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == "user" {
			out = append(out, m.Content)
		}
	}
	return out
}

// userIndexes 取所有用户消息在 msgs 里的下标。
func userIndexes(msgs []channel.Message) []int {
	var out []int
	for i, m := range msgs {
		if m.Role == "user" {
			out = append(out, i)
		}
	}
	return out
}

// systemPart 把系统消息拼成与 promptTextOf 同口径的文本（无系统消息返回空串）。
func systemPart(msgs []channel.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			b.WriteString("[系统] " + m.Content + "\n\n")
		}
	}
	return b.String()
}

// hashOf 取短指纹（16 位十六进制足够区分）。
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)[:16]
}

// tailOf 取切片末尾 n 个元素的副本。
func tailOf(xs []string, n int) []string {
	if n <= 0 || len(xs) == 0 {
		return nil
	}
	if len(xs) <= n {
		return append([]string(nil), xs...)
	}
	return append([]string(nil), xs[len(xs)-n:]...)
}

// promptTextOf 把 messages 拼成单条 prompt（与 Adapter.promptText 同口径）。
//
// 上游服务端虽然有状态，但**新建**会话时仍要把整段历史一次性带过去 ——
// 此时正文必须控制在单帧上限以内（planTurn 已自查）。
func promptTextOf(msgs []channel.Message) string {
	if len(msgs) == 1 {
		return msgs[0].Content
	}
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "system":
			b.WriteString("[系统] " + m.Content + "\n\n")
		case "assistant":
			b.WriteString("[助手] " + m.Content + "\n\n")
		default:
			b.WriteString(m.Content + "\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// fitsFrame 判断一段正文装进 new_prompt 帧后是否还在单帧上限内。
func fitsFrame(sessionID, text string) bool {
	frame, err := promptFrame(sessionID, text, 0)
	return err == nil && len(frame) <= maxFrameBytes
}

// closeCodeOf 取出上游 close 帧的错误码（不是 close 帧则返回 0）。
func closeCodeOf(err error) int {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return 0
}

// tooLongFromUpstream 把上游的 close 1009 翻成 PromptTooLong（附上游原话）。
//
// 真机原话形如：close 1009 (message too big): read limited at 32769 bytes
func tooLongFromUpstream(err error) *errs.Error {
	var ce *websocket.CloseError
	text := ""
	if errors.As(err, &ce) {
		text = ce.Text
	}
	return errs.New(errs.PromptTooLong, fmt.Sprintf(
		"上游以 close 1009（消息过大）掐断了本轮连接：%q。该渠道 chat-ws 单帧上限 %d 字节，"+
			"通常是本轮上下文超出上限。请开新会话，或缩短上下文后重试 —— 网关不会替你静默截断上下文。",
		text, maxFrameBytes)).
		WithCause(err).
		WithUpstream("close 1009 " + text).
		WithChannel("qwenwork")
}
