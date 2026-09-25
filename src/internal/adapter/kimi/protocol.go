package kimi

// protocol.go Connect 协议的信封编解码 + 消息拼装 + 凭证小工具。
//
// Connect（gRPC-Web 的一个变体）在 HTTP 上的形态很朴素，但**既不是 SSE 也不是整包 JSON**：
//   - 请求体：1 字节 flag（0x00 = 未压缩的数据帧）+ 4 字节**大端**长度 + JSON 字节；
//   - 响应体：同样是一串「flag + 长度 + 载荷」的帧；flag 最高位为 1 的帧是 trailer（结束元数据）；
//   - Content-Type 为 application/connect+json。
//
// 所以读响应必须**按帧切**：按行读会把二进制长度头当成内容，按整包读会一直等到连接关闭。
// 这也是本渠道最容易写错的地方（表现是「客户端一直转圈」或「拿到乱码」）。

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"poolgate/internal/channel"
)

// encodeConnect 把 payload 包成一个 Connect 请求体（单个数据帧）。
func encodeConnect(payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 5+len(body))
	out[0] = 0x00 // 未压缩的数据帧
	binary.BigEndian.PutUint32(out[1:5], uint32(len(body)))
	copy(out[5:], body)
	return out, nil
}

// frame 是切出来的一个 Connect 帧。
type frame struct {
	payload []byte
	trailer bool
}

// splitFrames 从 buffer 里尽量切出完整帧，返回帧列表与「还不完整、留着等下一批」的尾巴。
//
// 必须容忍半帧：TCP 不会按帧边界送达，长度头到了而载荷没到齐是常态。
func splitFrames(buf []byte) ([]frame, []byte) {
	var out []frame
	off := 0
	for off+5 <= len(buf) {
		flag := buf[off]
		n := int(binary.BigEndian.Uint32(buf[off+1 : off+5]))
		end := off + 5 + n
		if end > len(buf) || n < 0 {
			break
		}
		out = append(out, frame{payload: buf[off+5 : end], trailer: flag&0x80 != 0})
		off = end
	}
	return out, buf[off:]
}

// packMessages 把内部消息拼成上游要的那一段文本。
//
// 上游只吃「一条 user 文本」，所以这不是偷懒而是协议要求：历史、工具调用、工具结果
// 全都得压进这一条里。标记形态与参考实现一致（照抄比自创好：上游见惯了这种写法）。
//
// system 行统一提到最前面：参考实现就是这么做的，prompt 形态变了模型表现会跟着变。
func packMessages(msgs []channel.Message) string {
	var systemLines, bodyLines []string
	for _, m := range msgs {
		text := strings.TrimSpace(m.Content)
		role := m.Role

		// assistant 的工具调用：拍平成标记块，否则模型看到的是「自己上次什么都没调用」。
		if role == "assistant" && len(m.ToolCalls) > 0 {
			calls := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, "[call:"+tc.Function.Name+"]"+tc.Function.Arguments+"[/call]")
			}
			if len(calls) > 0 {
				text = "[function_calls]\n" + strings.Join(calls, "\n") + "\n[/function_calls]"
			}
		}

		// 工具结果没有独立的 role，改成 user 文本 + 标记（带 tool_call_id 以便模型对上号）。
		if role == "tool" {
			role = "user"
			if m.ToolCallID != "" {
				text = "[TOOL_RESULT for " + m.ToolCallID + "] " + text
			}
		}

		if strings.TrimSpace(text) == "" {
			continue
		}
		if role == "system" || role == "developer" {
			systemLines = append(systemLines, "system:"+text)
			continue
		}
		bodyLines = append(bodyLines, role+":"+text)
	}
	return strings.TrimSpace(strings.Join(append(systemLines, bodyLines...), "\n"))
}

// ---------------------------------------------------------------------------
// 凭证工具
// ---------------------------------------------------------------------------

// parseJWT 解出 JWT 的 payload。**不校验签名** —— 我们只用它读 exp 和身份标识，
// 真正的校验在上游（签名错了上游自然 401）。
func parseJWT(tok string) (map[string]any, bool) {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil, false
		}
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil, false
	}
	return out, true
}

// isAccessToken 判断这串是不是「Kimi 的 access token」。
//
// 判据与参考实现一致：是 JWT，且 payload 里 app_id == "kimi"、typ == "access"。
// 不满足的就是 refresh token（我们拿它去换 access token）—— 这一区分是必需的：
// 把 refresh token 当 Bearer 去打对话接口，上游只会回 401，看起来像「凭证失效」。
func isAccessToken(tok string) bool {
	if !strings.HasPrefix(strings.TrimSpace(tok), "eyJ") {
		return false
	}
	payload, ok := parseJWT(tok)
	if !ok {
		return false
	}
	appID, _ := payload["app_id"].(string)
	typ, _ := payload["typ"].(string)
	return appID == "kimi" && typ == "access"
}

// tokenExpiry 取 JWT 的 exp；取不到就返回零值（表示「未知」，不当成已过期）。
func tokenExpiry(tok string) time.Time {
	payload, ok := parseJWT(tok)
	if !ok {
		return time.Time{}
	}
	exp, ok := payload["exp"].(float64)
	if !ok || exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(exp), 0)
}

// deriveDeviceID / deriveSessionID 由凭证派生出**稳定**的 19 位十进制标识。
//
// 为什么要派生而不是每进程随机：上游用这两个号区分「同一个客户端」。同一账号重启后换了号，
// 在风控看来就是「一台新设备开始用这个账号」；而多账号共用一个号更糟（一眼看成同一个人）。
// 由凭证派生同时满足三件事：同账号恒定、不同账号天然不同、不需要额外落盘。
func deriveDeviceID(seed string) string {
	return nineteen(7000000000000000000, 7999999999999999999, hash64(seed, "device"))
}

func deriveSessionID(seed string) string {
	return nineteen(1700000000000000000, 1799999999999999999, hash64(seed, "session"))
}

func hash64(seed, salt string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	_, _ = h.Write([]byte("#kimi-" + salt))
	return h.Sum64()
}

// nineteen 把一个 hash 映射到 [lo, hi] 区间内的十进制串。
// 区间必须是 19 位：形状不对上游会判非法（这是它的设备号格式约定）。
func nineteen(lo, hi uint64, h uint64) string {
	span := hi - lo + 1
	return strconv.FormatUint(lo+h%span, 10)
}

// randID 生成一个随机 19 位串（仅在凭证为空、无法派生时用）。
func randID(lo, hi uint64) string {
	return strconv.FormatUint(lo+uint64(rand.Int64N(int64(hi-lo+1))), 10)
}
