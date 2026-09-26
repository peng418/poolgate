package iflow

// protocol.go 协议层：签名、请求体组装、凭证小工具。
//
// 与 Kimi 那种「自定义二进制帧」不同，iFlow 上游就是标准 OpenAI 兼容 JSON，
// 所以这里的重点不是解包，而是**把签名与请求形态精确对齐官方 CLI** —— 签名错一个字节
// 上游就拒，而且它不会告诉你错在哪（只回 401/403），调试成本极高，所以原文与头的拼法
// 一字不改地照抄参考实现。

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"poolgate/internal/channel"
)

// ---------------------------------------------------------------------------
// 签名
// ---------------------------------------------------------------------------

// signature 计算 iFlow 的请求签名。
//
// 算法（照抄 proxy.py::generate_signature）：
//
//	HMAC-SHA256(key = apiKey, msg = "{cliUserAgent}:{sessionID}:{timestampMs}") → 小写十六进制
//
// 三个坑：
//   - 原文里的 UA 是**常量** "iFlow-Cli"，不是请求头里被网关改写过的值；
//   - session-id 必须与头里发的 `session-id` **完全一致**（同一个值参与签名又发出去）；
//   - timestamp 是**毫秒**，且与头里 x-iflow-timestamp 是同一个数（不是各取一次时间）。
//
// 我们不去「校验」这些，只是严格保证三处用同一个值：只要它们一致，签名就成立。
//
// 交叉印证：第二份**独立**实现 AIClient2API（src/providers/openai/iflow-core.js:286-303）
// 的 `createIFlowSignature(UA, sessionID, timestamp, apiKey)` 逐字节相同 —— payload 同为
// `${userAgent}:${sessionID}:${timestamp}`、key 同为 apiKey、`createHmac('sha256').digest('hex')`、
// 时间戳同取 `Date.now()`（毫秒）。两份实现在此完全一致，说明这里抄对了。
func signature(apiKey, sessionID string, tsMillis int64) string {
	msg := cliUserAgent + ":" + sessionID + ":" + strconv.FormatInt(tsMillis, 10)
	mac := hmac.New(sha256.New, []byte(apiKey))
	_, _ = mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// traceparent 生成一个 W3C Trace Context 头值：`00-<32hex trace_id>-<16hex parent_id>-01`。
//
// 参考实现每个请求都带这个头（proxy.py:68-74 生成、:129 发送），形态是硬性的四段式；
// 它**不参与签名**，上游按「有则传」的可选头处理（proxy.py:127 注释）。我们没有埋点链路，
// 每请求现生成一个随机值即可 —— 只要形态合法，不追求跨请求复用同一个 trace_id。
func traceparent() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源不可用时退化成派生值：头值只需形态合法（它不进签名）。
		h := sha256.Sum256([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
		copy(b[:], h[:24])
	}
	return "00-" + hex.EncodeToString(b[0:16]) + "-" + hex.EncodeToString(b[16:24]) + "-01"
}

// ---------------------------------------------------------------------------
// 请求体
// ---------------------------------------------------------------------------

// buildBody 组装上游请求体。
//
// 形态与默认值照抄参考实现 _align_official_body_defaults：
//   - 流式请求带 `stream: true`，非流式**不带** stream 字段（上游按「无 stream = 整包 JSON」处理）；
//   - 补齐 temperature / top_p / max_new_tokens；
//   - **tools 即使为空也带上 `[]`** —— 这是官方 CLI 的实测行为，不是我们多此一举；
//     带上空数组与不带 tools 在上游走的是不同分支。
//
// req.Tools 通过 ForwardTools() 取（它会吃掉 tool_choice="none" 这种明确禁用），
// **原样放进请求体**：上游原生支持 tools，网关不做任何翻译，这正是 Tools=true 的含义。
func buildBody(req channel.ChatRequest, model string) map[string]any {
	body := map[string]any{
		"model":          model,
		"messages":       messagePayloads(req.Messages),
		"temperature":    defaultTemperature,
		"top_p":          defaultTopP,
		"max_new_tokens": defaultMaxNewTokens,
		"tools":          []any{},
	}
	if req.Stream {
		body["stream"] = true
	}
	if req.MaxTokens > 0 {
		// 客户端显式要了上限就以它为准（上游字段名是 max_new_tokens，不是 max_tokens）。
		body["max_new_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if tools := req.ForwardTools(); len(tools) > 0 {
		body["tools"] = tools
		if req.ToolChoice != nil {
			body["tool_choice"] = req.ToolChoice
		}
	}
	// 模型专属的思考参数：不加这些，GLM/DeepSeek 这类模型在上游可能不返回思考过程
	// （甚至请求被拒）。来源：参考实现 _configure_model_request。
	for k, v := range thinkingParams(model) {
		if _, exists := body[k]; !exists {
			body[k] = v
		}
	}
	return body
}

// messagePayloads 把内部消息翻成 OpenAI 形态。
//
// 这里是**原样透传**，不做 Kimi 那种「把所有话压成一条文本」的改写：
// 上游认 role/tool_calls/tool_call_id 这些字段，改写反而会让模型看不到历史工具调用。
func messagePayloads(msgs []channel.Message) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		if role == "developer" {
			role = "system" // OpenAI 的 developer 只是 system 的新名字，上游只认 system
		}
		msg := map[string]any{"role": role}
		if m.Content != "" {
			msg["content"] = m.Content
		}
		if role == "assistant" && len(m.ToolCalls) > 0 {
			calls := make([]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				t := tc.Type
				if t == "" {
					t = "function"
				}
				calls = append(calls, map[string]any{
					"id":   tc.ID,
					"type": t,
					"function": map[string]any{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				})
			}
			msg["tool_calls"] = calls
		}
		if role == "tool" {
			// tool 消息必须带 tool_call_id（模型靠它把结果与调用对上号）；
			// name 是部分上游额外要求的字段，有就带上。
			msg["tool_call_id"] = m.ToolCallID
			if m.Name != "" {
				msg["name"] = m.Name
			}
		}
		out = append(out, msg)
	}
	return out
}

// thinkingParams 返回某模型需要的思考参数（来源：参考实现 _configure_model_request，
// 规则逐条注释自 its 源码，不是我们编的）。
//
// 分支顺序有意义：先命中的分支生效（例如 glm-5 走专属分支，不进 glm-* 通用分支）。
//
// 交叉印证（部分）：第二份独立实现 AIClient2API（iflow-core.js:316-364 applyIFlowThinkingConfig）
// 也认「glm-4.x 用 chat_template_kwargs.enable_thinking」，与我们 glm-4.6/4.7 的处理一致；
// 但它是**由客户端 reasoning_effort 驱动**的（没传就不加），且对 glm-5 / deepseek 不加任何静态
// 参数、并多一个 clear_thinking 键 —— 与 iflow2api 的静态规则冲突。这里仍以 iflow2api
// （自称来自 iflow-cli configureRequest 源码）为准，不引入第二份的分歧规则。
func thinkingParams(model string) map[string]any {
	m := strings.ToLower(strings.TrimSpace(model))
	out := map[string]any{}
	switch {
	case strings.HasPrefix(m, "deepseek"):
		// configureRequest: reasoning=true + thinking_mode=true
		out["thinking_mode"] = true
		out["reasoning"] = true
	case m == "glm-5":
		out["chat_template_kwargs"] = map[string]any{"enable_thinking": true}
		out["enable_thinking"] = true
		out["thinking"] = map[string]any{"type": "enabled"}
	case m == "glm-4.7":
		out["chat_template_kwargs"] = map[string]any{"enable_thinking": true}
	case strings.HasPrefix(m, "glm-"):
		out["chat_template_kwargs"] = map[string]any{"enable_thinking": true}
	case strings.HasPrefix(m, "kimi-k2.5"):
		out["thinking"] = map[string]any{"type": "enabled"}
	case strings.Contains(m, "thinking"):
		out["thinking_mode"] = true
	case strings.HasPrefix(m, "mimo-"):
		out["thinking"] = map[string]any{"type": "enabled"}
	case strings.Contains(m, "claude"):
		out["chat_template_kwargs"] = map[string]any{"enable_thinking": true}
	case strings.Contains(m, "sonnet-"):
		out["chat_template_kwargs"] = map[string]any{"enable_thinking": true}
	case strings.Contains(m, "reasoning"):
		out["reasoning"] = true
	}
	// qwen*4b 明确不支持思考，需要把可能被上游默认塞入的思考参数删掉。
	if strings.Contains(m, "qwen") && strings.Contains(m, "4b") {
		for _, k := range []string{"thinking_mode", "reasoning", "chat_template_kwargs"} {
			delete(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// 凭证小工具
// ---------------------------------------------------------------------------

// extractAPIKey 从粘贴内容里抽 apiKey。认这几种形态（用户会以各种方式复制）：
//   - 裸串；
//   - `"带引号"`；
//   - 整段 settings.json（或它的片段）→ 取其中的 `apiKey` / `api_key` / `key` / `value`；
//   - 面板引导语里的 `"apiKey" = "xxx"` 这种键值对写法。
//
// apiKey 的取法对齐参考实现：优先 `apiKey`，其次是兼容字段 `searchApiKey`
// （config.py:109 `data.get("apiKey") or data.get("searchApiKey")`）——
// 有的账号/版本只写了后者，不认就会「照引导语粘了却说没识别出」。
//
// 挡掉明显误粘的内容（空、太短、含空白）——把 URL 或整段配置当 key 存进去，
// 表现是「装上了但一调用就 401」，比当场拒绝难查得多。
func extractAPIKey(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "{") {
		var obj struct {
			APIKey       string `json:"apiKey"`
			SearchAPIKey string `json:"searchApiKey"`
			APIKey2      string `json:"api_key"`
			Key          string `json:"key"`
			Value        string `json:"value"`
		}
		if json.Unmarshal([]byte(s), &obj) == nil {
			for _, v := range []string{obj.APIKey, obj.SearchAPIKey, obj.APIKey2, obj.Key, obj.Value} {
				if strings.TrimSpace(v) != "" {
					s = strings.TrimSpace(v)
					break
				}
			}
		}
	}
	// 形如 `"apiKey" = "xxx"` 或 `apiKey=xxx`：取等号右边。引导语给的就是前一种。
	// 只在等号左边确实写着 apiKey 时才切 —— 否则会把「本身含 = 的 apiKey」拦腰截断。
	if i := strings.Index(s, "="); i >= 0 && strings.Contains(strings.ToLower(s[:i]), "apikey") {
		s = strings.TrimSpace(s[i+1:])
	}
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	s = strings.TrimSpace(s)
	// apiKey 是较长的不透明串；含空白的多词文本一定是误粘（比如整段日志）。
	if len(s) < 8 || strings.ContainsAny(s, " \t\n") {
		return ""
	}
	return s
}

// shortHash 给账号一个稳定短标识（面板显示用，不泄露 apiKey 本身）。
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// seedOf 取派生种子：优先 UID（同一账号重登后稳定），没有才退回 apiKey。
func seedOf(c *channel.Credential, apiKey string) string {
	if c != nil && strings.TrimSpace(c.UID) != "" {
		return strings.TrimSpace(c.UID)
	}
	return apiKey
}

// sessionIDFor / conversationIDFor 由种子派生**稳定**的会话号与对话号。
//
// 为什么要派生而不是每进程随机：参考实现是每进程一个 uuid4，但那意味着同一账号
// 重启后会话号就变了。上游把 session-id 既当签名成分又当埋点维度，同账号恒定更合理
// （不同账号天然不同，也不会互串）。形态做成 UUID v4 只是为了与官方一致。
func sessionIDFor(seed string) string { return "session-" + deriveUUID(seed+"#iflow-session") }

func conversationIDFor(seed string) string { return deriveUUID(seed + "#iflow-conversation") }

// deriveUUID 由种子派生一个稳定的 RFC4122 v4 形态 UUID（FNV-1a 双哈希）。
func deriveUUID(seed string) string {
	h1 := fnv.New64a()
	_, _ = h1.Write([]byte(seed))
	h2 := fnv.New64a()
	_, _ = h2.Write([]byte(seed + "#iflow-uuid"))
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], h1.Sum64())
	binary.BigEndian.PutUint64(b[8:16], h2.Sum64())
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
