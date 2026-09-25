// Package chatgpt 是「ChatGPT 网页版」渠道适配器 —— **登录式**（不要 API key，不要密码）。
//
// 协议来自公开参考实现（都在 /tmp/refs/，2026-09 仍在更新）：
//   - ChatGPT2API-GO/internal/app/upstream.go（Go，主参考：鉴权 + 对话 + 指纹）
//   - ChatGPT2API-GO/internal/app/turnstile.go（sentinel turnstile 的 VM 解释器）
//   - gpt4free/g4f/Provider/needs_auth/OpenaiChat.py 与 openai/{proofofwork,new,turnstile_vm}.py
//   - chatgpt2api-yukkcat/utils/pow.py（PoW 的字节比较写法）
//
// 与其它网页渠道最大的不同：**对话前要过三道门**，任何一道不过都是 403，而且报错
// 长得像「网络问题」。三道门依次是：
//
//  1. 引导：GET https://chatgpt.com/ —— 顺带从首页 HTML 里抓 PoW 脚本地址与 data-build；
//  2. 挑战：POST /backend-api/sentinel/chat-requirements，body `{"p": <requirements token>}`，
//     p = "gAAAAAC" + base64(配置数组)，**p 本身就是一次 PoW 的解**（难度固定 0fffff）；
//  3. 校验和：上游回的 proofofwork{seed,difficulty} 要自己算 —— 拿同一形状的配置数组，
//     循环改第 4 个元素，base64(json(数组)) 后算 sha3_512(seed+base64)，
//     与 difficulty 的字节前缀比较，通过就得到 "gAAAAAB"+base64。
//
// 另有 turnstile（人机校验）：上游下发一段 base64 后与 p 异或的 **opcode 程序**，
// 由我们本地的 VM 解释器跑出 token（见 turnstile.go）；arkose 上游要求时**明确报错**
// （参考实现也没实现，我们不假装能过）。
//
// 凭证只有 accessToken（JWT）一条路：用户在自己浏览器里登录 chatgpt.com，把
// /api/auth/session 里的 accessToken 粘回面板。**没有 refresh token 就刷不了**
// —— JWT 到期必须重新粘一次，这一点写在引导语与 Refresh 里，不假装能续期。
//
// 设备指纹（oai-did / OAI-Session-Id / OAI-Client-Version …）必须**持久化到账号上**：
// 每次请求随机等于告诉上游「同一账号换了一台新设备」，是风控眼里最直的白给。
//
// 已知边界（**上线前必须在真上游复核**）：两份参考实现都用了 TLS 指纹伪装
// （ChatGPT2API-GO 走 curl-impersonate 二进制，gpt4free 走 curl_cffi），
// 说明 chatgpt.com 前面的 Cloudflare 很可能看 JA3/JA4。本适配器沿用了网关统一的
// HTTP 客户端（net/http，标准 TLS 指纹），**没有**做伪装 —— 如果真上游对指纹敏感，
// 症状是「引导首页 403/挑战循环」，而不是本文里任何一条明确报错。
// 真遇到时要做的是给这个渠道换一个能伪装 TLS 的出口，而不是改这里的协议代码。
package chatgpt

import (
	"time"

	"poolgate/internal/channel"
)

// ---------------------------------------------------------------------------
// 端点（chatgpt.com）
// ---------------------------------------------------------------------------

const (
	apiBase = "https://chatgpt.com"

	// epSentinel 是「对话前挑战」端点。上游还有两段式的 prepare/finalize 变体
	// （见 /tmp/refs/notes/chatgpt.md），但主参考（Go）与 gpt4free 都走这个单端点，
	// 我们先跟主流；若哪天它返回 404/409，就是这条路径漂移了。
	epSentinel = "/backend-api/sentinel/chat-requirements"
	// epConversation 是对话端点（SSE）。
	epConversation = "/backend-api/conversation"
	// epModels 是模型目录（slug 会漂移，所以优先问上游而不是写死）。
	epModels = "/backend-api/models?history_and_training_disabled=false"
	// epMe 是账号信息（登录时用它当场验一次 accessToken）。
	epMe = "/backend-api/me"
)

const (
	// httpTimeout 给足：网页端长思考 + 首字延迟都可能很久（与 Kimi 渠道同一个量级）。
	httpTimeout = 10 * time.Minute

	// defaultMinIntervalSec 是同账号最小请求间隔出厂默认（秒）。
	//
	// 网页渠道反爬按账号算，且 ChatGPT 的挑战流程本身就是「别打太快」的信号；
	// 取 3 秒：比 Kimi（2 秒）保守一档 —— 这里多一次 sentinel + PoW 计算，
	// 同号猛打不只是限流，是直接进风控名单。
	defaultMinIntervalSec = 3
)

// ---------------------------------------------------------------------------
// PoW / sentinel 常量
// ---------------------------------------------------------------------------

const (
	// powPrefix 是 proof token 的前缀（"gAAAAAB"），其后的 base64 才是哈希过的答案。
	powPrefix = "gAAAAAB"
	// requirementsPrefix 是 requirements token（`p`）的前缀（注意比上面少一个 A）。
	requirementsPrefix = "gAAAAAC"
	// requirementsDifficulty 是 `p` 的固定难度（十六进制串）。
	requirementsDifficulty = "0fffff"
	// powMaxAttempts 是循环次数上限，与参考实现一致（超过就认输，不无限算）。
	powMaxAttempts = 500000

	// defaultPowScript 是首页里找不到 script 时的兜底 PoW 脚本地址。
	defaultPowScript = "https://chatgpt.com/backend-api/sentinel/sdk.js"
)

// 上游默认版本头。这两个会随官网改版失效 —— 失效的表现是 sentinel 直接 403，
// 所以换版本号时只改这里一处（也可以由账号指纹 Extra 覆盖，见 fingerprint.go 的键）。
const (
	defaultClientVersion = "prod-be885abbfcfe7b1f511e88b3003d9ee44757fbad"
	defaultClientBuild   = "5955942"
)

// ---------------------------------------------------------------------------
// 设备指纹的存档键（Credential.Extra）
// ---------------------------------------------------------------------------

// 这些键是**账号级**的：写进 Credential.Extra 落盘，之后每个请求都从那里读。
// 绝不能每请求随机 —— 换了设备号在风控看来就是「新设备开始用这个账号」。
const (
	fpDeviceID      = "oai-did"                 // 也就是 cookie oai-did
	fpSessionID     = "oai-session-id"          //
	fpLanguage      = "oai-language"            //
	fpClientVersion = "oai-client-version"      //
	fpClientBuild   = "oai-client-build-number" //
	fpUserAgent     = "user-agent"              //
	fpSecCHUA       = "sec-ch-ua"               //
	fpSecCHUAMobile = "sec-ch-ua-mobile"        //
	fpSecCHUAPlat   = "sec-ch-ua-platform"      //
)

// 指纹缺省值（只有在 Extra 里没有、且派生不出来时才用）。
//
// UA 与 sec-ch-ua 必须是**同一台机器**的自洽组合：UA 说 Edge/143，sec-ch-ua 也得是
// Edge/143，混搭（比如 UA 写 Chrome 而 sec-ch-ua 写 Edge）比不带还容易被挑出来。
const (
	defaultUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"
	defaultSecCHUA       = `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`
	defaultSecCHUAMobile = "?0"
	defaultSecCHUAPlat   = `"Windows"`
	defaultLanguage      = "zh-CN"
)

// ---------------------------------------------------------------------------
// 模型
// ---------------------------------------------------------------------------

// defaultModel 是模型名认不出来时的兜底档。
//
// 用 `auto` 而不是猜一个 slug：上游的 slug 一直在漂（gpt-5-2 / gpt-5.6 / …），
// 猜错的结果是「模型不存在」；auto 由上游自己选，最稳。
const defaultModel = "auto"

// webModels 是**上游目录拿不到时**的兜底清单。
//
// 这份清单不是「所有模型」，而是参考实现里 normalizeTextModelOptions 明确认得的档位
// （即 ChatGPT 网页端的思考强度别名）—— 只列有依据的，不编 slug。
var webModels = []struct {
	ID     string
	Name   string
	Reason channel.Cap
}{
	// auto 会落到哪个 slug 由上游决定，思考与否**不知道** —— 标未知而不是标「不支持」：
	// 「不知道」和「不支持」在网关清洗时处置不同（后者可直接拒绝，前者不猜）。
	{ID: "auto", Name: "ChatGPT（自动选择）", Reason: channel.CapUnknown},
	{ID: "gpt-5-1", Name: "GPT-5（低思考）", Reason: channel.CapYes},
	{ID: "gpt-5-2", Name: "GPT-5（中思考）", Reason: channel.CapYes},
	{ID: "gpt-5-3", Name: "GPT-5（高思考）", Reason: channel.CapYes},
	{ID: "gpt-5-mini", Name: "GPT-5 mini（低思考）", Reason: channel.CapYes},
	{ID: "gpt-5-3-mini", Name: "GPT-5 mini（高思考）", Reason: channel.CapYes},
}
