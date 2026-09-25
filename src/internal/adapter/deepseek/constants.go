// Package deepseek 是「DeepSeek 网页版」渠道适配器 —— **登录式**（不要 API key）。
//
// 协议取自公开参考实现（NIyueeE/ds-free-api，752★，2026-09 仍在更新）的实测结论：
//   - 对话端点是 agent 协议，不是 OpenAI 格式：SSE 帧里是 **p/o/v patch**（跨帧持久的路径+操作），
//     文本在 `response/fragments[-1].content` 上按 APPEND 累积，fragments[].type 区分 THINK/RESPONSE；
//   - 每次 completion 前要**现场解一次 PoW**（官方那份 WASM，业界没有纯 Go 重实现 → 用 wazero 跑）；
//   - **上游没有原生工具调用**（参考实现也是把工具定义降级成文本）→ 本渠道走 toolshim 模拟层；
//   - 登录用「用户在自己浏览器登录后粘回 userToken」：它的设备指纹必须由真实浏览器产出（数美 SDK），
//     伪造会被判 `RISK_DEVICE_DETECTED`，所以二维码那条路给不了 —— 但我们**不存用户密码**。
package deepseek

import "time"

// 端点与客户端常量（2026-09 抓包对齐的公开客户端标识）。
const (
	apiBase = "https://chat.deepseek.com/api/v0"

	epSessionCreate = apiBase + "/chat_session/create"
	epSessionDelete = apiBase + "/chat_session/delete"
	epPowChallenge  = apiBase + "/chat/create_pow_challenge"
	epCompletion    = apiBase + "/chat/completion"
	epAuthCheck     = apiBase + "/users/auth_token/check_device"

	// powTargetPath 是 PoW 挑战要绑定的目标路径（换别的接口要改成对应的）。
	powTargetPath = "/api/v0/chat/completion"

	// wasmURL 是官方 PoW 求解器：公开、无鉴权、26KB，**不注册任何 import**（所以能直接用 wazero 跑）。
	// 文件名里的 hash 段会随上游发版变化，做成可配（设置里能改）。
	wasmURL = "https://fe-static.deepseek.com/chat/static/sha3_wasm_bg.7b9ca65ddd.wasm"

	// 客户端标识（公开常量，参考实现 2026-09 抓包对齐）：
	// UA 必须是 App 身份 —— 桌面 Chrome UA 会被 WAF 拦（README 原文）。
	ua                  = "DeepSeek/2.5.0 Android/35"
	clientVersion       = "2.5.0"
	clientPlatform      = "android"
	clientLocale        = "zh_CN"
	clientBundleID      = "com.deepseek.chat"
	clientTimezoneOfset = "28800"
	deviceModel         = "Pixel 7"
)

// defaultMinIntervalSec 是每账号最小请求间隔出厂默认（秒）。
//
// 上游对免费账号有小时配额（参考实现默认 60 次/小时），并发也要压到「账号数/2」；
// 同号猛打既容易撞限流，也更容易被风控盯上。
const defaultMinIntervalSec = 3

const httpTimeout = 10 * time.Minute

// powTimeout 是解一次 PoW 的上限（官方挑战本身 5 分钟过期，给足但不无限等）。
const powTimeout = 90 * time.Second

// webModels 是网页端可选档位。
//
// 上游其实只有 `default`（快速模式）稳定可用，`expert`（专家模式）在上游被标 disabled、
// 仍可调用但不稳 —— 列表里如实写清楚，别让用户以为它和 default 一样可靠。
// `deepseek-chat` / `deepseek-reasoner` 是给习惯官方叫法的人用的别名（映射到同一档，
// 用 thinking_enabled 区分要不要思考）。
var webModels = []struct {
	ID    string
	Name  string
	Type  string // 上游 model_type
	Think bool   // 是否开思考
	Note  string
}{
	{ID: "default", Name: "DeepSeek 默认（快速模式）", Type: "default", Think: true},
	{ID: "deepseek-chat", Name: "DeepSeek Chat（官方叫法，同默认档，不开思考）", Type: "default"},
	{ID: "deepseek-reasoner", Name: "DeepSeek Reasoner（官方叫法，同默认档，开思考）", Type: "default", Think: true},
	{ID: "expert", Name: "DeepSeek 专家模式", Type: "expert", Think: true, Note: "上游标为 disabled，实测可用但不稳"},
}

// modelOf 按客户端模型名找档位；找不到就用 default（宁可给默认档，也不失败 —— 但要能看出来）。
func modelOf(id string) (typ string, think bool, ok bool) {
	for _, m := range webModels {
		if m.ID == id {
			return m.Type, m.Think, true
		}
	}
	return "default", true, false
}

// fragment 类型：THINK = 思考（→ reasoning_content），RESPONSE = 正文。
const (
	fragThink    = "THINK"
	fragResponse = "RESPONSE"
)
