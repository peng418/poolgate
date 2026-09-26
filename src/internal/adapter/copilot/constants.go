// Package copilot 是「GitHub Copilot」渠道适配器 —— **登录式**（GitHub 设备码授权，不要 API key）。
//
// 协议按公开参考实现移植（2026-09 核对，不凭记忆发明字段）：
//   - copilot-api（TypeScript）：端点、请求头、请求体字段全部按它的实测结论照搬；
//   - gpt4free 的 Provider/github/GithubCopilot.py 与 copilotTokenProvider.py（Python）：
//     交叉验证令牌换取、模型目录与配额接口。
//
// 与其它登录式渠道最大的不同：**上游本身就是标准 OpenAI 协议**（api.githubcopilot.com 的
// /chat/completions、/models 与 OpenAI 官方一字不差）。所以本适配器是一层「薄翻译」，
// 只做两件事：
//  1. 把 GitHub 设备码登录换成长期可用凭证（githubToken → copilotToken → 对话）；
//  2. 把每个请求伪装成 VS Code 的 Copilot 插件。
//
// 没有协议编解码、没有消息拼装、没有 toolshim —— 工具调用是**原生**的（Spec.Tools=true），
// 响应解析直接复用 channel.ParseOpenAIChunk。
//
// 为什么必须伪装成 VS Code 插件：Copilot 的 API 只对「官方客户端」开放，上游按请求头识别
// 客户端（Copilot-Integration-Id / editor-version / user-agent / openai-intent / X-Initiator 等）。
// 缺了这些头会被判为第三方调用而拒绝。这些版本号会随上游客户端更新而失效，见下面的版本常量。
package copilot

import "time"

// 端点。
const (
	// githubBase 是 GitHub 主站：设备码申请与令牌轮询在这里（不用 api.github.com）。
	githubBase = "https://github.com"
	// githubAPIBase 是 GitHub REST API：用 githubToken 换 copilotToken、查账号与配额。
	githubAPIBase = "https://api.github.com"
	// copilotBase 是 Copilot API（individual 个人版）。
	// 团队/企业版是 api.<account_type>.githubcopilot.com（见 copilotBaseURL）。
	copilotBase = "https://api.githubcopilot.com"

	// epDeviceCode：申请设备码（POST JSON {client_id, scope}）。
	epDeviceCode = "/login/device/code"
	// epAccessToken：轮询换 githubToken（POST JSON {client_id, device_code, grant_type}）。
	epAccessToken = "/login/oauth/access_token"
	// epCopilotToken：用 githubToken 换 copilotToken（GET，header `authorization: token <gh>`）。
	epCopilotToken = "/copilot_internal/v2/token"
	// epUser：读登录名（做面板昵称与稳定 UID）。
	epUser = "/user"
	// epUsage：读订阅配额（余额）。与 epCopilotToken 同在 api.github.com。
	epUsage = "/copilot_internal/user"

	// clientID 是 GitHub Copilot 官方客户端的公开 client_id（public client，没有 secret）。
	// 设备码流程不需要 secret —— 这正是它比「把密码存在 NAS 上」更安全的原因。
	clientID = "Iv1.b507a08c87ecfe98"
	// scope 只申请 read:user：够识别是哪个账号，不够动用户的仓库。
	scope = "read:user"
	// deviceGrant 是设备码换令牌的 grant_type（RFC 8628 标准值）。
	deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

	// accountIndividual 是默认账号类型：个人版 Copilot 用 api.githubcopilot.com。
	accountIndividual = "individual"
)

// ---------------------------------------------------------------------------
// 伪装客户端的版本号 —— **会过期，需要时只改这里**
// ---------------------------------------------------------------------------

// copilotVersion 是伪装的 Copilot Chat 扩展版本，同时用于 user-agent
// （GitHubCopilotChat/<v>）与 editor-plugin-version（copilot-chat/<v>）两个头。
//
// 参考实现（copilot-api）把编辑器版本**联网去 AUR 抓**（PKGBUILD 里的 pkgver），抓不到才回落到常量。
// 我们**故意不联网抓**：一个渠道适配器不该在启动时去打第三个域名，而且抓来的版本照样会过期，
// 并没有解决根本问题。代价是这里的版本号会随时光推移显得旧 —— 上游若开始拒绝过旧客户端，
// 表现是 400/403 且报文里提到版本号，届时把下面两个常量改成当时的 VS Code / 插件版本即可。
const copilotVersion = "0.26.7"

// vsCodeVersion 是 editor-version 头里的 VS Code 版本（vscode/<v>）。
const vsCodeVersion = "1.104.3"

// githubAPIVersion 是 x-github-api-version 头的取值（GitHub REST 的版本标识，不是凭证）。
const githubAPIVersion = "2025-04-01"

// ── 版本常量的交叉验证（2026-09，任务补充）──────────────────────────────────
//
// 上面这三个版本常量取自 copilot-api（最后一次提交 2025-10-05）。任务补充里更新的实现取值
// **不一致**，按「各实现取值不同就【不】盲换」处理：原样保留 copilot-api 这一套，把各家取值与
// 依据强度记在这里，供上游收紧时决策。
//
//	Copilot 客户端版本（user-agent / editor-plugin-version）：
//	  copilot-api  0.26.7 / copilot-chat/0.26.7   ← 我们采用
//	  BYOKEY       0.67.0 / copilot-chat/0.67.0（配 VS Code 1.139，2026-09-25 提交）
//	  gpt4free     1.250.0 / copilot/1.250.0（注意是 copilot/，且明显过时）
//	editor-version：
//	  copilot-api  vscode/1.104.3  ← 我们采用（它启动时去 AUR 抓 PKGBUILD，抓不到才用这个兜底）
//	  BYOKEY       vscode/1.139.0
//	  gpt4free     vscode/1.95.0
//	x-github-api-version：
//	  copilot-api  两处（api.github.com 与 api.githubcopilot.com）都用 2025-04-01  ← 我们采用
//	  BYOKEY       **分开**——api.github.com 用 2025-04-01（与我们一致）、
//	               api.githubcopilot.com 用 2026-08-01
//	  gpt4free     2024-12-15
//	Copilot-Integration-Id：三家都是 vscode-chat（无分歧）。
//
// 依据强度：BYOKEY（crates/provider/src/executor/copilot/headers.rs:7-14,25-31）自称逐行对照
// microsoft/vscode 1.139 与 @vscode/copilot-api 的 CAPIClient，provenance 最硬；copilot-api 是
// 被广泛使用、仍在维护的代理，但它把同一份 GitHub 头也用在 chat 上，且 VS Code 版本是运行时抓的
// （services/get-vscode-version.ts:1-31）。两者都无法证明对方取值已失效，而我们没有真上游样本，
// 所以不换。上游若开始按客户端版本收紧（表现是 400/403 且报文提到版本），下一手是换成 BYOKEY 那一套
// （VS Code 1.139 / copilot-chat 0.67.0，并把 chat 的 x-github-api-version 提到 2026-08-01）。

// ---------------------------------------------------------------------------
// 超时与节奏
// ---------------------------------------------------------------------------

const (
	// chatTimeout 给足：Copilot 背后是各家旗舰模型，长思考 + 大上下文都可能很慢。
	chatTimeout = 10 * time.Minute
	// apiTimeout 用于登录/换令牌/目录这类小请求：卡住了要尽早失败，让用户看到原因而不是干等。
	apiTimeout = 30 * time.Second

	// defaultMinIntervalSec 是同账号两次请求的最小间隔出厂默认（秒）。
	//
	// Copilot 是**订阅额度池**（不是反爬敏感的网页端），且上游本来就按账号限流；
	// 取 1 秒：对单账号体验几乎无感，但能压住「一个号瞬间并发几十路」——那是最容易被风控盯上的用法
	// （见 docs/06 防封号设计）。
	defaultMinIntervalSec = 1

	// refreshLead 是 copilotToken 的提前刷新量。
	//
	// 上游在响应里给 refresh_in（秒），参考实现是 refresh_in - 60 秒时刷新。提前刷的理由：
	// token 一过期，每个请求都要先撞一次 401 才恢复，看起来像账号坏了。
	refreshLead = 60 * time.Second
	// copilotTokenFallback 是上游没给 refresh_in（或给得离谱）时的保守刷新周期。
	// Copilot token 实测有效期约 30 分钟，取 25 分钟留出提前量。
	copilotTokenFallback = 25 * time.Minute
)

// ---------------------------------------------------------------------------
// 请求头
// ---------------------------------------------------------------------------

// stdHeaders 是 GitHub 主站端点的最小 JSON 头。
// Accept 必须是 application/json：否则 /login/oauth/access_token 会回 form-encoded，解不出 access_token。
func stdHeaders() map[string]string {
	return map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"User-Agent":   "GitHubCopilotChat/" + copilotVersion,
	}
}

// githubHeaders 是打 api.github.com 用的头：用 `token <githubToken>` 鉴权（注意不是 Bearer）。
//
// 带上编辑器版本头是参考实现的实测要求：GitHub 侧对 copilot_internal 接口也看客户端标识。
func githubHeaders(githubToken string) map[string]string {
	return map[string]string{
		"Authorization":                       "token " + githubToken,
		"Accept":                              "application/json",
		"Content-Type":                        "application/json",
		"User-Agent":                          "GitHubCopilotChat/" + copilotVersion,
		"Editor-Version":                      "vscode/" + vsCodeVersion,
		"Editor-Plugin-Version":               "copilot-chat/" + copilotVersion,
		"X-GitHub-Api-Version":                githubAPIVersion,
		"X-Vscode-User-Agent-Library-Version": "electron-fetch",
	}
}

// copilotHeaders 是打 api.githubcopilot.com 用的「伪装成 VS Code 插件」的头。
//
// 这些头不是可选项：上游按它们判断「这是不是官方客户端」。逐个都有出处 ——
//   - copilot-integration-id: vscode-chat：标识走的是 VS Code 聊天面板这一路；
//   - editor-version / editor-plugin-version / user-agent：VS Code 与插件版本（见上面的版本注释）；
//   - openai-intent: conversation-panel：告诉上游这是聊天面板请求，而不是补全；
//   - x-request-id：每次请求一个 UUID（参考实现如此，上游用它做链路追踪）。
//
// 注意这里**没有** Copilot-Version 头。0.5.0 移植时曾按记忆加过一个，逐字段核对参考实现后删掉：
// copilot-api 的 copilotHeaders（src/lib/api-config.ts:20-37）和 gpt4free 的 get_headers
// （g4f/Provider/github/GithubCopilot.py:202-208）都没有它，客户端版本是靠 Copilot-Integration-Id
// 加 editor-plugin-version 一起表达的。多一个来源不明的头只会让请求看起来不像原版客户端 ——
// 不要再加回来，除非参考实现里出现它。
//
// 交叉验证（2026-09，任务补充）：更近的 BYOKEY（crates/provider/src/executor/copilot/headers.rs:100-158
// 配 device.rs:66-74）在 VS Code 档下**还多带**三个设备身份头 `vscode-sessionid`（uuid+毫秒）、
// `vscode-machineid`（MAC 的 SHA-256，64 hex）、`editor-device-id`（uuid4），外加
// `x-interaction-id` / `x-agent-task-id` / `x-interaction-type`；openai-intent 取 `conversation-agent`
// （我们与 copilot-api 都用 `conversation-panel`）。copilot-api 与 gpt4free 都不发这些头且工作正常，
// 说明**不是必需**；且伪造设备 ID 还牵扯「稳定、不与别号重复、不泄露凭证」的一堆细节
// （BYOKEY 用 SHA256(salt+凭证) 派生），所以**暂不实现**，留作上游收紧时的候补。
func copilotHeaders(copilotToken string) map[string]string {
	return map[string]string{
		"Authorization":                       "Bearer " + copilotToken,
		"Content-Type":                        "application/json",
		"Accept":                              "application/json",
		"Copilot-Integration-Id":              "vscode-chat",
		"Editor-Version":                      "vscode/" + vsCodeVersion,
		"Editor-Plugin-Version":               "copilot-chat/" + copilotVersion,
		"User-Agent":                          "GitHubCopilotChat/" + copilotVersion,
		"Openai-Intent":                       "conversation-panel",
		"X-GitHub-Api-Version":                githubAPIVersion,
		"X-Request-Id":                        newRequestID(),
		"X-Vscode-User-Agent-Library-Version": "electron-fetch",
	}
}
