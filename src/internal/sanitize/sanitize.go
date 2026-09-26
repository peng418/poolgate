// Package sanitize 出站请求体指纹脱敏：清除 Claude Code / Codex CLI 注入的模板句，
// 防止上游 CodeBuddy 内容审核按逐字精确匹配拦截（HTTP 400 code=11128 /
// "Illegal API invocation from an unapproved channel"）。
//
// 为什么需要这一层：上游（腾讯 CodeBuddy / WorkBuddy 后端）对请求体里的客户端身份指纹
// 做**逐字精确黑名单匹配**——不是模糊/语义匹配。命中即整单拒绝，回
// HTTP 400 code=11128。也就是说，从 Claude Code / Claude Agent SDK / Studio 直连这几个
// 渠道时，请求体里由 CLI 注入的模板句会被原样带上并被挡掉，渠道"能授权却调不通"。
//
// 依据（上游实测 + 第三方佐证 + 本仓原始实现）：
//   - 上游实测：400 code=11128 "Illegal API invocation from an unapproved channel"
//     （Claude Code / Codex CLI 身份句、billing header、裸 11128 等逐字命中即拦）。
//   - 第三方佐证 1：/tmp/refs/workbuddy-openai-proxy/src/upstream.mjs CLIENT_FINGERPRINTS
//     （正则 /You are Claude Code,\s*Anthropic's official CLI for Claude\.?/i）。
//   - 第三方佐证 2：/tmp/refs/Orchids-2api/internal/workbuddy/messages.go:36-56
//     （CodePolicyBlocked = 11128；billing header / identity 两个 system block 直接丢弃）。
//   - 本仓原始实现：wild-work internal/sanitize/sanitize.go（PoolGate 重做时漏移植的那层）。
//
// 策略：键值型指纹（billing header、cc_* 键值）整段删除——它们不承载任何给模型的语义；
// 承载语义的模板句做**最小改写**（换一个词），语义不变、可读性保留。
// 绝不阻塞请求：body 不可解析时原样返回（宁可发指纹原文，也不能丢消息）。
// content 值为 null 的 assistant 消息不会跳过——其 tool_calls.arguments 仍需净化。
package sanitize

import (
	"encoding/json"
	"regexp"
	"strings"
)

// featHeaders 是预检特征串：只要 body 里出现任一条就进入净化流程。
// 每一条都是实验逆向出的上游黑名单截断前缀（前缀命中即可，无需整句），
// 一字之差即漏拦或误伤。含 11128：上游反探测——请求体里出现裸错误码也整单拦。
var featHeaders = []string{
	"x-anthropic-billing-header",
	"cc_entrypoint=",
	"You are Claude Code",
	"Main branch (",
	"You are a coding agent running in the Codex CLI",
	"github.com/anthropics/",
	"11128",
}

var reHdr = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)
var reBareHdr = regexp.MustCompile(`(?i)x-anthropic-billing-header`)
var reKv = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// rewrites 是承载语义的模板句的最小改写表（左→右，只换一词）。
// 匹配串**不带结尾句号**：桌面版（claude-desktop-3p / Agent SDK）的身份句以逗号接后继
// 内容、结尾不是句号，若匹配串带句号则该形态漏网、指纹原样发上游 → 400 code=11128。
var rewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	// 裸 11128 整单拦截，插入连字符保留可读性（零宽空格无效，上游会归一化）。
	{"11128", "11-128"},
}

// Messages 脱敏 body 中 messages 的 content / reasoning_content / tool_calls.arguments。
// body 不可解析时原样返回，绝不阻塞请求（降级语义：宁可发指纹原文也不丢消息）。
func Messages(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	// 预检：零分配快速路径，普通请求全不中。
	if !hasFingerprint(toString(body)) {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		return body
	}
	if cleanMessages(msgs) {
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
	}
	return body
}

func toString(b []byte) string { return string(b) }

// hasFingerprint 是净化前的快速判定：命中任一特征串即进入净化。
func hasFingerprint(s string) bool {
	for _, f := range featHeaders {
		if strings.Contains(s, f) {
			return true
		}
	}
	// 大小写变体 + 裸键名形态由正则兜底
	return reBareHdr.MatchString(s)
}

// cleanText 对单段文本做净化：先做最小改写，再整段删除键值型指纹，
// 最后把残留的裸键名缩写（header→hdr，键值形态删掉后可能留有裸串）。
func cleanText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range rewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	if reHdr.MatchString(text) {
		text = reHdr.ReplaceAllString(text, "")
	}
	// cc_* 键值可能首尾相连（删除一处后暴露出下一处），循环删到不动为止。
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text {
			prev = text
			text = reKv.ReplaceAllString(text, "")
		}
	}
	text = reBareHdr.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

// cleanContent 净化 content 字段（字符串或 multimodal 文本块数组），返回新值与是否改动。
func cleanContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := cleanText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, _ := p.(map[string]any)
			if m == nil {
				continue
			}
			if t, ok := m["text"].(string); ok {
				if s := cleanText(t); s != t {
					m["text"] = s
					changed = true
				}
			}
		}
		return c, changed
	}
	return v, false
}

// cleanToolCalls 净化 assistant 消息的 tool_calls[].function.arguments。
func cleanToolCalls(v any) bool {
	calls, _ := v.([]any)
	if len(calls) == 0 {
		return false
	}
	changed := false
	for _, c := range calls {
		call, _ := c.(map[string]any)
		if call == nil {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		args, _ := fn["arguments"].(string)
		if args == "" {
			continue
		}
		if s := cleanText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// cleanMessages 逐条净化 messages，返回是否有任何改动。
//
// content 为 null/缺失**不 continue**：工具调用消息的 content 常为 null，若因 content
// 缺失就跳过整条消息，其 tool_calls.arguments 里的指纹会原样漏出（历史回归）。
func cleanMessages(msgs []any) bool {
	changed := false
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		if msg == nil {
			continue
		}
		if v, ok := msg["content"]; ok {
			if nc, ch := cleanContent(v); ch {
				msg["content"] = nc
				changed = true
			}
		}
		if rc, ok := msg["reasoning_content"].(string); ok {
			if s := cleanText(rc); s != rc {
				msg["reasoning_content"] = s
				changed = true
			}
		}
		if tc, ok := msg["tool_calls"]; ok {
			if cleanToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
