// Package qwen 是「通义（Qwen）」渠道适配器 —— 走 Qwen Code CLI 的 OAuth + 官方 CLI 端点。
//
// 为什么走 CLI 端而不是网页端（协议来自公开参考实现 Rfym21/Qwen2API 的实测结论）：
//   - CLI 端 `portal.qwen.ai/v1/chat/completions` 是**纯 OpenAI 格式**，请求头全部可在服务端构造，
//     **不需要浏览器指纹**（无 bx-ua、无 ssxmod、无签名）；
//   - 网页端 `chat.qwen.ai/api/v2/chat/completions` 需要伪造 ssxmod 指纹 Cookie，而且它的原生工具调用
//     会被服务端 agent loop 拦截（返回 "Tool X does not exists"），参考实现只能退化成文本标记 ——
//     对我们这种「给 coding agent 当后端」的用途是硬伤。
//
// 所以：**登录走设备码（用户在浏览器/手机点一下），对话走 CLI 端**。凭 Web JWT 自动授权那一步
// 我们不做（那需要存用户的邮箱密码），改成让用户自己在授权页完成 —— 更安全，也更符合本项目的做法。
package qwen

import "time"

// 端点与常量。协议取值来自公开参考实现的实测；这些都是**公开客户端常量**，不是凭证。
const (
	// chatBase 是网页域：登录/授权/设备码都在这里。
	chatBase = "https://chat.qwen.ai"
	// cliBase 是 CLI 端：对话在这里（与网页域不是同一套模型空间）。
	cliBase = "https://portal.qwen.ai"

	epSignin     = "/api/v1/auths/signin"
	epDeviceCode = "/api/v1/oauth2/device/code"
	epToken      = "/api/v1/oauth2/token"
	epAuthorize  = "/api/v2/oauth2/authorize"
	epChat       = "/v1/chat/completions"

	// clientID 是 Qwen Code CLI 的公开 client_id（public client，无 secret）。
	clientID = "f0304373b74a44d2b584a3fb70ca9e56"
	// scope 是设备码要申请的权限：openid/profile/email 之外，model.completion 才是对话权限。
	scope = "openid profile email model.completion"

	// deviceGrant 是设备码换令牌的 grant_type（RFC 8628 标准值）。
	deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

	// cliUA 是 CLI 端认的客户端标识：形如 QwenCode/<版本> (<os>; <arch>)。
	// 版本号跟着上游客户端走，过旧可能被拒；这里与参考实现对齐。
	cliUA = "QwenCode/0.10.3 (darwin; arm64)"
)

// defaultMinIntervalSec 是「同一账号两次请求的最小间隔」出厂默认（秒）。
//
// 通义是订阅/免费额度类账号，同号并发猛打是最容易被判异常的用法；2 秒对单账号体验影响很小，
// 但能把「像脚本一样刷」这件事压下去（见 docs/06 防封号设计）。
const defaultMinIntervalSec = 2

// chatTimeout 给足：长思考 + 大上下文。
const chatTimeout = 10 * time.Minute

// cliModels 是 CLI 端的模型表。
//
// 为什么是写死的：CLI 端没有目录接口（参考实现也是硬编码），只有这几个档位。
// 标注 SourceLocal 是诚实：这是本地声明的清单，不是上游给的。
var cliModels = []struct {
	ID      string
	Name    string
	Context int
	Note    string
}{
	{ID: "coder-model", Name: "Coder Model", Context: 256000, Note: "Qwen3.5-Plus 基座，带推理链，256K"},
	{ID: "qwen3-coder-plus", Name: "Qwen3 Coder Plus", Context: 256000},
	{ID: "qwen3-coder-flash", Name: "Qwen3 Coder Flash", Context: 256000},
	{ID: "qwen3.6-plus", Name: "Qwen3.6 Plus", Context: 256000},
	{ID: "qwen3.5-plus", Name: "Qwen3.5 Plus（别名，上游会重定向到 coder-model）", Context: 256000},
}

// modelRedirect 是上游要求的别名重定向（参考实现里写死的映射）。
var modelRedirect = map[string]string{
	"qwen3.5-plus": "coder-model",
}

// unsupportedFields 是 CLI 端不接受的请求体字段：留着会被上游拒或行为不可预期。
var unsupportedFields = []string{
	"frequency_penalty", "presence_penalty", "logit_bias", "logprobs",
	"top_logprobs", "n", "seed", "service_tier", "user",
}
