package chatgpt

// protocol.go 三道门（引导 / 挑战 / PoW）+ 对话请求体的组装。
//
// 顺序不能变，也不能跳：上游把「有没有走完这三步」编进 sentinel token 里，
// 跳过任何一步拿到的都是 403 —— 而且返回体长得像网络错误，极难定位。
// 所以这里的每一步失败都必须带上「是哪一步」，见 sentinelFlow 的注释。

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// sentinel 是一次挑战流程的全部产物，逐项对应请求头。
type sentinel struct {
	Token          string // openai-sentinel-chat-requirements-token
	ProofToken     string // openai-sentinel-proof-token
	TurnstileToken string // openai-sentinel-turnstile-token
	SOToken        string // openai-sentinel-so-token
}

// bootstrapInfo 是首页引导里抓到的东西：PoW 脚本地址与 data-build。
//
// 它们要原样进 PoW 配置数组（第 6/7 个元素）：上游用它确认「你拿到的挑战是它刚发的」。
// 抓不到时回落到默认脚本地址，data-build 允许为空串。
type bootstrapInfo struct {
	scripts   []string
	dataBuild string
}

// bootstrap 走第一道门：GET https://chatgpt.com/。
//
// 为什么要真去 GET 首页：上游在这一步下发 PoW 脚本路径与 build 号，且会把
// 「这个客户端来过首页」记进会话。直接跳到 sentinel 的请求通过率明显更低。
//
// 只有「上游自己抖」（网络错、3xx、429、5xx）才重试；401/403 不重试 ——
// 那不是抖动，是凭证或风控，重试只会让情况更糟。
func (a *Adapter) bootstrap(ctx context.Context, c *channel.Credential, fp fingerprint) (bootstrapInfo, error) {
	var lastErr error
	for attempt := 1; attempt <= bootstrapMaxAttempts; attempt++ {
		if attempt > 1 {
			if !sleepCtx(ctx, backoffOf(attempt)) {
				return bootstrapInfo{}, errs.New(errs.Transport, "引导首页时被取消").
					WithChannel(string(channel.ChatGPT)).WithCause(ctx.Err())
			}
		}
		info, err := a.bootstrapOnce(ctx, c, fp)
		if err == nil {
			return info, nil
		}
		lastErr = err
		if !retryableBootstrap(err) {
			return bootstrapInfo{}, err
		}
	}
	return bootstrapInfo{}, lastErr
}

func (a *Adapter) bootstrapOnce(ctx context.Context, c *channel.Credential, fp fingerprint) (bootstrapInfo, error) {
	resp, err := a.sendDocument(ctx, c, a.base+"/", fp)
	if err != nil {
		return bootstrapInfo{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		kind := a.Classify(resp.StatusCode, raw)
		return bootstrapInfo{}, stepErr(kind, "引导首页（第 1 道门）失败", c, raw)
	}
	scripts, build := parsePowResources(string(raw))
	if len(scripts) == 0 {
		scripts = []string{defaultPowScript}
	}
	return bootstrapInfo{scripts: scripts, dataBuild: build}, nil
}

// retryableBootstrap 判断引导失败是不是「上游自己抖」。
func retryableBootstrap(err error) bool {
	if err == nil {
		return false
	}
	kind, _ := errs.KindOf(err)
	switch kind {
	case errs.Transport, errs.UpstreamFault, errs.SoftRate:
		return true
	}
	return false
}

// sentinelFlow 走第二、三道门，返回对话请求要带的全部挑战头。
//
// 三道门在这一步里各自有明确的失败点：
//
//	① 引导失败 → 「引导首页（第 1 道门）失败」
//	② 挑战请求失败 / 没有 token → 「挑战接口（第 2 道门）失败」
//	③ PoW 或 turnstile 解不出 → 「挑战求解（第 3 道门）失败」
//	arkose 要求时 → 直接失败，明确说「上游要求 arkose，本渠道没实现」
//
// 把这三步分开报，是因为它们的处置完全不同：①多半是网络/出口问题，
// ②多半是凭证过期，③才是本地实现跟不上下游改版 —— 混成一句「403」等于什么都没说。
func (a *Adapter) sentinelFlow(ctx context.Context, c *channel.Credential, fp fingerprint) (sentinel, error) {
	var out sentinel
	bs, err := a.bootstrap(ctx, c, fp)
	if err != nil {
		return out, err
	}
	// `p` 本身是一次 PoW 的解（难度固定 0fffff）。造不出来说明本地实现坏了，
	// 明确报错，而不是发一段随便的 base64 上去换一个 403。
	p, ok := requirementsToken(fp.UserAgent, bs.scripts, bs.dataBuild)
	if !ok {
		return out, errs.New(errs.UpstreamFault,
			"requirements token 计算失败（第 2 道门）：这是本地 PoW 实现问题，不是账号问题").
			WithChannel(string(channel.ChatGPT)).WithAccount(uidOf(c))
	}

	body, _ := json.Marshal(map[string]any{"p": p})
	resp, err := a.send(ctx, c, http.MethodPost, a.base+epSentinel, fp, body, map[string]string{
		"Content-Type": "application/json",
		"Accept":       "*/*",
	})
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, stepErr(a.Classify(resp.StatusCode, raw),
			"挑战接口（第 2 道门）失败：多半是 accessToken 已过期或出口被风控", c, raw)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return out, errs.New(errs.Parse, "挑战接口返回的不是 JSON（第 2 道门）").
			WithChannel(string(channel.ChatGPT)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	out.Token = strings.TrimSpace(strAny(data["token"], ""))
	out.SOToken = strings.TrimSpace(strAny(data["so_token"], ""))

	// arkose 是另一套人机校验（Arkose Labs）。参考实现与我们**都没有实现**，
	// 上游要求时只有一条正确路径：明确失败并说清楚为什么 —— 静默降级（不带 arkose 头
	// 硬发）换来的 403 会让用户以为是账号被封，白折腾很久。
	if ark, ok := data["arkose"].(map[string]any); ok && boolAny(ark["required"], false) {
		return out, errs.New(errs.UpstreamFault,
			"上游要求 arkose 人机校验（本渠道未实现 Arkose 求解）：这是明确的能力边界，"+
				"不是账号问题，请更换账号或等待上游放宽").
			WithChannel(string(channel.ChatGPT)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}

	// PoW：上游给出 seed 与难度，我们自己算。
	if pow, ok := data["proofofwork"].(map[string]any); ok && boolAny(pow["required"], false) {
		seed := strAny(pow["seed"], "")
		diff := strAny(pow["difficulty"], "")
		tok, solved := proofToken(seed, diff, fp.UserAgent, bs.scripts, bs.dataBuild)
		if !solved {
			return out, errs.New(errs.UpstreamFault,
				fmt.Sprintf("PoW 求解失败（第 3 道门）：难度 %s 在 %d 次尝试内无解", diff, powMaxAttempts)).
				WithChannel(string(channel.ChatGPT)).WithAccount(uidOf(c)).
				WithUpstream(truncate(string(raw), 200))
		}
		out.ProofToken = tok
	}

	// turnstile：dx 是 opcode 程序，本地 VM 解释执行。
	if ts, ok := data["turnstile"].(map[string]any); ok && boolAny(ts["required"], false) {
		dx := strAny(ts["dx"], "")
		if strings.TrimSpace(dx) == "" {
			// 上游说要 turnstile 却没给挑战体：参考实现（及其测试）在这种情况下
			// 照样继续，只是不带这个头。我们跟它 —— 此时确实「没有东西可解」，
			// 不额外报错，避免把能过的请求挡下来。
			out.TurnstileToken = ""
		} else {
			// 带凭证的请求里，异或密钥用**空串**（参考实现如此：p 由服务端重新下发，
			// 本地这份不作密钥）。空串在 xorCipher 里等价于不异或。
			tok, solved := solveTurnstileToken(dx, "", bs.scripts, fp.UserAgent)
			if !solved {
				return out, errs.New(errs.UpstreamFault,
					"turnstile 挑战求解失败（第 3 道门）：上游下发的 opcode 程序本地解释不出来"+
						"（已知边界：解释器覆盖 1–35 号指令，上游换程序形态时会在这里明确失败）").
					WithChannel(string(channel.ChatGPT)).WithAccount(uidOf(c)).
					WithUpstream("dx 长度 " + fmt.Sprint(len(dx)))
			}
			out.TurnstileToken = tok
		}
	}

	if out.Token == "" {
		return out, errs.New(errs.UpstreamFault,
			"挑战接口没有返回 sentinel token（第 2 道门）：上游可能改版或该账号已被风控").
			WithChannel(string(channel.ChatGPT)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return out, nil
}

// conversationHeaders 组装对话请求的挑战头。
func conversationHeaders(cr sentinel) map[string]string {
	h := map[string]string{
		"Accept":       "text/event-stream",
		"Content-Type": "application/json",
		"openai-sentinel-chat-requirements-token": cr.Token,
	}
	if cr.ProofToken != "" {
		h["openai-sentinel-proof-token"] = cr.ProofToken
	}
	if cr.TurnstileToken != "" {
		h["openai-sentinel-turnstile-token"] = cr.TurnstileToken
	}
	if cr.SOToken != "" {
		h["openai-sentinel-so-token"] = cr.SOToken
	}
	return h
}

// ---------------------------------------------------------------------------
// 请求体
// ---------------------------------------------------------------------------

// buildConversationPayload 组装一次对话请求体。
//
// 字段是「Go 参考实现 + gpt4free」的并集：上游对多余字段是容忍的，对**缺字段**不是
// （缺 force_use_sse 会拿到非流式响应，缺 conversation_mode 会被当成别的产品形态）。
// 所以宁可多带。
func buildConversationPayload(req channel.ChatRequest, spec modelSpec) map[string]any {
	payload := map[string]any{
		"action":                        "next",
		"messages":                      toConversationMessages(req.Messages),
		"model":                         spec.slug,
		"parent_message_id":             newUUID(),
		"conversation_mode":             map[string]any{"kind": "primary_assistant"},
		"conversation_origin":           nil,
		"force_paragen":                 false,
		"force_paragen_model_slug":      "",
		"force_rate_limit":              false,
		"force_use_sse":                 true, // 不给它选，必须流式（我们要的就是增量与思考分流）
		"supports_buffering":            true,
		"history_and_training_disabled": true, // 不把内容送进上游的训练集
		"reset_rate_limits":             false,
		"suggestions":                   []any{},
		"supported_encodings":           []any{},
		"system_hints":                  []any{},
		"timezone":                      defaultTimezone,
		"timezone_offset_min":           defaultTimezoneOffsetMin,
		"variant_purpose":               "comparison_implicit",
		"websocket_request_id":          newUUID(),
		"client_contextual_info":        clientContextualInfo(),
	}
	// 网页端的「思考强度」靠这个字段表达（gpt-5-1 → low、gpt-5-3 → high）。
	if spec.effort != "" {
		payload["thinking_effort"] = spec.effort
	}
	return payload
}

// toConversationMessages 把内部消息转成上游要的消息数组。
//
// 上游只认 user / assistant 两个角色：
//   - system / developer 拍成 user，前面加 "System instructions:" 标记（照参考实现）；
//   - tool 结果拍成 user，前面加 "Tool result from <name>:"，否则模型看到的是
//     「上一轮我说了话、然后突然出现一段没人认领的文本」；
//   - assistant 的历史工具调用拍成与 toolshim 同一套 <tool_call> 标记，
//     让模型在下一轮认得出自己上次调了什么（不拍平它会重复调同一个工具）。
func toConversationMessages(msgs []channel.Message) []map[string]any {
	if len(msgs) == 0 {
		msgs = []channel.Message{{Role: "user", Content: "Hello"}}
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		text := m.Content
		switch role {
		case "assistant":
		case "user":
		case "system", "developer":
			role = "user"
			text = "System instructions:\n" + text
		case "tool", "function":
			name := firstNonEmpty(m.Name, m.ToolCallID, "tool")
			role = "user"
			text = "Tool result from " + name + ":\n" + text
		default:
			role = "user"
		}
		if role == "assistant" && len(m.ToolCalls) > 0 {
			var b strings.Builder
			b.WriteString(text)
			for _, tc := range m.ToolCalls {
				if strings.TrimSpace(text) != "" || b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString("<tool_call>")
				b.WriteString(`{"name":` + jsonString(tc.Function.Name) +
					`,"arguments":` + jsonString(tc.Function.Arguments) + `}`)
				b.WriteString("</tool_call>")
			}
			text = b.String()
		}
		out = append(out, map[string]any{
			"id":      newUUID(),
			"author":  map[string]any{"role": role},
			"content": map[string]any{"content_type": "text", "parts": []any{text}},
		})
	}
	return out
}

// clientContextualInfo 是网页端会带的一小段「窗口环境」，纯装饰但缺了会显得不像浏览器。
func clientContextualInfo() map[string]any {
	return map[string]any{
		"is_dark_mode":      false,
		"time_since_loaded": 120,
		"page_height":       900,
		"page_width":        1400,
		"pixel_ratio":       2,
		"screen_height":     1440,
		"screen_width":      2560,
		"app_name":          "chatgpt.com",
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// parsePowResources 从首页 HTML 里抓 script 地址与 data-build。
func parsePowResources(html string) ([]string, string) {
	reSrc := regexp.MustCompile(`<script[^>]+src=["']([^"']+)["']`)
	matches := reSrc.FindAllStringSubmatch(html, -1)
	scripts := make([]string, 0, len(matches))
	build := ""
	reBuild := regexp.MustCompile(`c/[^/]*/_`)
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		scripts = append(scripts, m[1])
		if build == "" {
			if bm := reBuild.FindString(m[1]); bm != "" {
				build = bm
			}
		}
	}
	if build == "" {
		reAttr := regexp.MustCompile(`<html[^>]*data-build=["']([^"']*)`)
		if am := reAttr.FindStringSubmatch(html); len(am) > 1 {
			build = am[1]
		}
	}
	return scripts, build
}

// newUUID 生成 RFC4122 v4 的 UUID（parent_message_id / websocket_request_id / 消息 id 用）。
//
// 这里的随机**不是**设备指纹：这几个字段每请求必须新，重复反而会让上游把两次请求
// 当成同一次。设备指纹是账号级的，两者别混（见 fingerprint.go）。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// stepErr 造一个带上游原话的结构化错误。
// 返回具体类型 *errs.Error（而不是 error）：调用方有时要再补一句给用户看的话
// （WithMessage），那需要拿到结构体。
func stepErr(kind errs.Kind, msg string, c *channel.Credential, raw []byte) *errs.Error {
	return errs.New(kind, msg).
		WithChannel(string(channel.ChatGPT)).
		WithAccount(uidOf(c)).
		WithUpstream(truncate(string(raw), 200))
}

func strAny(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func boolAny(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

// jsonString 把字符串编成 JSON 字符串字面量（拼 tool_call 标记用）。
func jsonString(s string) string {
	b, err := marshalCompact(s)
	if err != nil {
		return `""`
	}
	return b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func uidOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	return c.UID
}
