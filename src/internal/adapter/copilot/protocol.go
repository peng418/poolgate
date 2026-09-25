package copilot

// protocol.go OpenAI 兼容请求体的构造 + 与本渠道有关的小工具。
//
// 上游协议就是 OpenAI 官方那套，所以这里**不做任何改写**（不改 role、不拼消息、不塞标记）：
// 客户端给什么就转发什么。任何自作聪明的改写都会变成新的静默失败源（红线一）。
//
// 唯一一处「必须自己算」的东西是 X-Initiator —— 它是上游用来区分「人类发的一条消息」与
// 「agent 在多轮工具调用中继续跑」的标记，参考实现按消息角色推导，见 initiatorOf。

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"

	"poolgate/internal/channel"
)

// buildBody 把内部契约转成上游的 chat.completions 请求体。
//
// stream 恒为 true：统一走流式，客户端要的非流式由网关本地聚合（Spec.SSEOnly=true）。
func buildBody(req channel.ChatRequest) []byte {
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		// content 恒为字符串：多数上游不接受缺字段，assistant 带 tool_calls 时给空串最稳。
		msg := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			msg["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		msgs = append(msgs, msg)
	}

	obj := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   true,
	}
	if req.MaxTokens > 0 {
		obj["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	// 工具定义原样带上。客户端显式 tool_choice:"none" 时 ForwardTools 返回 nil
	// （那是明确要求，不是丢弃 —— 红线一）。
	//
	// 这里与 openaiup 的差别：Copilot 上游**原生支持**工具调用，所以不存在「声明不支持」的判断，
	// 客户端带了就转发。
	if tools := req.ForwardTools(); len(tools) > 0 {
		obj["tools"] = tools
		if req.ToolChoice != nil {
			obj["tool_choice"] = req.ToolChoice
		}
	}
	raw, _ := json.Marshal(obj)
	return raw
}

// initiatorOf 推导 X-Initiator：消息里只要出现过 assistant 或 tool 角色，就说明这是
// agent 在多轮里继续跑（agent），否则是用户新发起的一轮（user）。
//
// 为什么要跟参考实现一样只看角色、不看别的东西：X-Initiator 影响上游对「这个请求谁发起」的
// 风控判断，写错（比如一律 user）会把 agent 流量伪装成人类流量，反而更容易被识别。
func initiatorOf(msgs []channel.Message) string {
	for _, m := range msgs {
		switch m.Role {
		case "assistant", "tool":
			return "agent"
		}
	}
	return "user"
}

// newRequestID 生成一个 UUIDv4，用作 x-request-id。
//
// 用密码学随机源而不是固定串：参考实现每次请求一个新 UUID，固定值在风控眼里就是「同一个请求发了一万次」。
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源不可用时退回全零形态（宁可少一个头的区分度，也不要让请求失败）。
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// extractToken 从粘贴内容里抽 GitHub token：裸串 / "带引号" / 整段 JSON / `key = value` 都认。
//
// 与 Kimi 渠道同一套思路（用户粘的东西五花八门），但要挡住两类明显误粘：
//   - 设备码本身（形如 ABCD-1234，短且带连字符）—— 用户可能把授权页上的码粘到这里；
//   - 多词文本（真 token 不含空白）。
func extractToken(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "{") {
		var obj struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
			Value       string `json:"value"`
		}
		if json.Unmarshal([]byte(s), &obj) == nil {
			for _, v := range []string{obj.AccessToken, obj.Token, obj.Value} {
				if strings.TrimSpace(v) != "" {
					s = strings.TrimSpace(v)
					break
				}
			}
		}
	}
	// 我们引导语里的输出格式是 `key = value`，兜住它。
	if i := strings.Index(s, " = "); i >= 0 {
		s = strings.TrimSpace(s[i+3:])
	}
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	s = strings.TrimSpace(s)
	if len(s) < 20 || strings.ContainsAny(s, " \t\n") {
		return ""
	}
	return s
}

// shortHash 给账号一个稳定短标识（拿不到 GitHub 登录名时兜底，不泄露 token 本身）。
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// capFromBool 把上游给出的布尔能力位转成三态 Cap；上游没给（nil）时保持「未知」——
// 「不知道」和「不支持」在下游处置不同，不能混（见 channel.Cap 的注释）。
func capFromBool(v *bool) channel.Cap {
	if v == nil {
		return channel.CapUnknown
	}
	if *v {
		return channel.CapYes
	}
	return channel.CapNo
}

// tokenOf 取凭证里的 GitHub token（AccessToken 优先，其次 RefreshToken）。
//
// 本渠道的 AccessToken 存的就是 githubToken：它长期有效、不轮换，是换取 copilotToken 的种子。
func tokenOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	if v := strings.TrimSpace(c.AccessToken); v != "" {
		return v
	}
	return strings.TrimSpace(c.RefreshToken)
}

func uidOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	return c.UID
}

// extraOf 读凭证 Extra 里的一个值（本渠道用 account_type / copilot_base / github_login）。
func extraOf(c *channel.Credential, key string) string {
	if c == nil || c.Extra == nil {
		return ""
	}
	return strings.TrimSpace(c.Extra[key])
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
