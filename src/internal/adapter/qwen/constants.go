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
	// 版本号跟着上游客户端走，过旧可能被拒。
	//
	// 取值来自 BYOKEY crates/provider/src/executor/qwen.rs（DEFAULT_USER_AGENT，逐字一致）。
	// 注意参考实现之间**不一致**，未擅自改：AIClient2API src/providers/openai/qwen-core.js:672
	// 用 QwenCode/0.14.2（platform 动态），qwen-code-oai-proxy src/qwen/api.ts:109 用
	// QwenCode/0.14.3。两份较新的实现都是 0.14.x，我们/BYOKEY 是 0.10.3 —— 哪个被上游接受
	// 没有实测依据，保持现值 + 记在这里，等有真账号再定（见交接报告）。
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
//
// 档位清单来源：qwen-code-oai-proxy（走 portal.qwen.ai 的公开实现）README「Supported Models」
// 与 src/qwen/api.ts 的 QWEN_MODELS —— 六档：coder-model / qwen3.5-plus / qwen3.6-plus /
// qwen3-coder-plus / qwen3-coder-flash / vision-model。qwen-code 源码里 OAuth 只认
// coder-model，其余是它的别名或 Coding Plan 里也有的同名档（见该仓库 docs/model-discovery.md）。
var cliModels = []struct {
	ID      string
	Name    string
	Context int
	Note    string
}{
	// coder-model 是上游自己维护的别名（Qwen 团队会换基座，现指向 Qwen 3.6 Plus）：
	// 上下文 1,000,000 / 输出上限 65536，来源 qwen-code 的 tokenLimits（见 model-discovery.md）。
	{ID: "coder-model", Name: "Coder Model", Context: 1000000, Note: "Qwen 3.6 Plus（上游维护的别名，会跟随换基座），带推理链，1M 上下文"},
	{ID: "qwen3-coder-plus", Name: "Qwen3 Coder Plus", Context: 256000},
	{ID: "qwen3-coder-flash", Name: "Qwen3 Coder Flash", Context: 256000},
	{ID: "qwen3.6-plus", Name: "Qwen3.6 Plus（别名，请求时本地重定向到 coder-model）", Context: 1000000},
	{ID: "qwen3.5-plus", Name: "Qwen3.5 Plus（别名，请求时本地重定向到 coder-model）", Context: 1000000},
	// vision-model 是多模态档；本渠道 Spec.Images=false（不转发图片），所以只按文本用。
	{ID: "vision-model", Name: "Vision Model", Context: 256000, Note: "多模态档，本渠道未开图片（Spec.Images=false）"},
}

// modelRedirect 是上游要求的别名重定向（参考实现里写死的映射）。
//
// 来源：qwen-code-oai-proxy src/qwen/api.ts 的 MODEL_ALIASES —— qwen3.5-plus 与 qwen3.6-plus
// 都重定向到 coder-model。README 亦注明「两者均解析为 coder-model」。
var modelRedirect = map[string]string{
	"qwen3.5-plus": "coder-model",
	"qwen3.6-plus": "coder-model",
}

// modelMaxTokens 是上游对各档 max_tokens 的硬上限，超过会被上游拒或截断，先本地夹住。
//
// 来源：qwen-code-oai-proxy src/qwen/api.ts 的 MODEL_LIMITS（clampMaxTokens）。注意这里是
// **输出** token 上限，与 cliModels 的 Context（输入上下文）是两回事。
var modelMaxTokens = map[string]int{
	"vision-model":  32768,
	"qwen3-vl-plus": 32768,
	"qwen3-vl-max":  32768,
	"qwen3.5-plus":  65536,
	"qwen3.6-plus":  65536,
	"coder-model":   65536,
}

// unsupportedFields 是 CLI 端不接受的请求体字段：留着会被上游拒或行为不可预期。
var unsupportedFields = []string{
	"frequency_penalty", "presence_penalty", "logit_bias", "logprobs",
	"top_logprobs", "n", "seed", "service_tier", "user",
}
