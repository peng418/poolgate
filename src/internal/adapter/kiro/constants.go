// Package kiro 是「AWS Kiro（Amazon Q Developer / AWS CodeWhisperer）」渠道适配器
// —— **登录式**（不要 API key，粘一次长期 refresh token 即可）。
//
// 协议取自公开参考实现 kiro-gateway（Python/FastAPI，2026-09 仍在使用）对
// prod.{region}.auth.desktop.kiro.dev / oidc.{region}.amazonaws.com /
// runtime.{region}.kiro.dev 的实测结论。三处与别家渠道不同，照抄时别想当然：
//
//  1. **鉴权有两种模式，按凭据里有没有 clientId+clientSecret 自动判**：
//     - Kiro Desktop（个人版 / Builder ID）：POST
//     https://prod.{region}.auth.desktop.kiro.dev/refreshToken，
//     body 是 {"refreshToken": "..."}；
//     - AWS SSO OIDC（企业版 / IAM Identity Center / kiro-cli）：
//     POST https://oidc.{region}.amazonaws.com/token，**JSON + camelCase**
//     （grantType=refresh_token、clientId、clientSecret、refreshToken）。
//     两者都返回 accessToken / refreshToken / expiresIn（可能还有 profileArn）。
//
//  2. 对话端点是 **Amz-Json** 风格（不是 OpenAI 的 /v1/chat/completions）：
//     POST {api_host}/generateAssistantResponse，Content-Type 为
//     application/x-amz-json-1.0，靠 **x-amz-target 头**选操作。
//
//  3. 响应体是 **AWS Event Stream 二进制帧**（既不是 SSE 也不是整包 JSON）：
//     每帧 =「4 字节总长 + 4 字节头长 + 4 字节前导 CRC + 头 + 载荷 + 4 字节消息 CRC」，
//     真正的内容（{"content":…} 这类 JSON）在**载荷**里。所以必须按帧切（见 protocol.go）——
//     按行读/按整包读都会得到乱码或一直转圈。
//
// 工具调用上游**原生支持**（toolSpecification / toolUseEvent），所以 Spec.Tools=true，
// 不走 toolshim 模拟层。
package kiro

import "time"

// ---------------------------------------------------------------------------
// 端点
// ---------------------------------------------------------------------------

// 路径部分。
const (
	pathDesktopRefresh = "/refreshToken"
	pathOIDCToken      = "/token"
	pathGenerate       = "/generateAssistantResponse"
)

// 主机模板。{region} 在运行期替换（见 client.go 的 hostURL）。
//
// 三个主机分别对应三种用途：桌面换令牌、企业换令牌、对话。参考实现早期用
// codewhisperer.{region}.amazonaws.com 做 api_host，但那个域名在 us-east-1 之外不存在，
// 已统一改成 runtime.{region}.kiro.dev —— 这条别退回去。
const (
	tmplAuthDesktop = "https://prod.{region}.auth.desktop.kiro.dev"
	tmplSSOOIDC     = "https://oidc.{region}.amazonaws.com"
	tmplRuntime     = "https://runtime.{region}.kiro.dev"
)

// defaultRegion 是凭据里没写 region 时的兜底（与参考实现一致）。
const defaultRegion = "us-east-1"

// kiroVersion 参与 UA（KiroIDE-<版本>-<指纹>）。参考实现写死 0.7.45；
// 上游若开始校验版本号，只改这一处。
const kiroVersion = "0.7.45"

// ---------------------------------------------------------------------------
// Amz-Json 协议固定值
// ---------------------------------------------------------------------------

const (
	amzContentType    = "application/x-amz-json-1.0"
	xAmzTarget        = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"
	amzSDKRequest     = "attempt=1; max=3"
	amzSDKJSVersion   = "aws-sdk-js/1.0.27"
	amzSDKAPIVersion  = "api/codewhispererstreaming#1.0.27"
	originAIEditor    = "AI_EDITOR"
	chatTriggerManual = "MANUAL"
	// codewhispererOptOut=true 是「不拿我的数据做训练」的开关。参考实现固定 true，
	// 我们对用户也照此默认 —— 这是对用户有利的一侧，没有理由关掉。
	codewhispererOptOut = "true"
	// kiroAgentMode=vibe 是 Kiro IDE 的默认智能体模式；缺这个头上游会按别的形态路由。
	kiroAgentMode = "vibe"
)

const (
	httpTimeout = 10 * time.Minute

	// refreshBuffer 是 access token 的提前刷新量：少于它就觉得「快过期了」。
	// 不提前刷的代价是每个请求都可能先撞一次 403（上游把过期令牌判 403，不是 401）。
	refreshBuffer = 5 * time.Minute

	// defaultMinIntervalSec 是同账号最小请求间隔出厂默认（秒）。
	//
	// Kiro 是**订阅额度池**（IDE 类），比网页版渠道宽松，但同号猛打会撞 429；
	// 参考实现默认重试退避 1s。取 1s：给面板一个安全节奏的起点，用户可在设置里覆盖。
	defaultMinIntervalSec = 1

	// maxToolNameLen 是上游硬限制：工具名超过 64 字符，请求会被上游 400 拒掉
	//（错误文案是模糊的 "Improperly formed request"，所以我们在本地先拦）。
	maxToolNameLen = 64

	// maxPayloadBytes 是上游载荷上限（实测约 615KB 起报 "Improperly formed request"），
	// 留 15KB 余量。超了在本地就说清楚 —— 否则用户只会看到上游那句没法定位的 400。
	maxPayloadBytes = 600000

	// maxFrameLen / maxBufferBytes 是**本地**保护：帧头说多长就等多少字节，
	// 报一个 4GB 的假长度会把内存吃光。合法帧远小于这两个数。
	maxFrameLen    = 16 << 20
	maxBufferBytes = 8 << 20
)

// ---------------------------------------------------------------------------
// 模型清单
// ---------------------------------------------------------------------------

// modelDesc 是本地清单里的一档。
type modelDesc struct {
	ID   string
	Name string
}

// localModels 是面板下发的档位清单（Source=local）。
//
// 为什么是写死的清单而不是向上游要目录：**runtime.{region}.kiro.dev 不提供
// /ListAvailableModels**（参考实现里专门有一段判断「是不是 runtime 端点，是就用静态表」）。
// 老端点上的 /ListAvailableModels 我们够不着，所以这里列的是「已知可用档位」，
// 不是「上游当场给出的目录」—— 面板要能看出这个区别，所以 Source=local。
//
// 「auto」这条被故意去掉了：它是个路由别名，很多 IDE（如 Cursor）也有同名的模型，
// 直接下发会撞名；想要它的走别名 `auto-kiro`（见 modelAliases）。
var localModels = []modelDesc{
	{ID: "auto-kiro", Name: "Kiro Auto（由上游自动选档）"},
	{ID: "claude-sonnet-4.6", Name: "Claude Sonnet 4.6"},
	{ID: "claude-sonnet-4.5", Name: "Claude Sonnet 4.5"},
	{ID: "claude-sonnet-4", Name: "Claude Sonnet 4"},
	{ID: "claude-haiku-4.5", Name: "Claude Haiku 4.5"},
	{ID: "claude-opus-4.7", Name: "Claude Opus 4.7"},
	{ID: "claude-opus-4.6", Name: "Claude Opus 4.6"},
	{ID: "claude-opus-4.5", Name: "Claude Opus 4.5"},
	{ID: "glm-5", Name: "GLM-5"},
	{ID: "deepseek-3.2", Name: "DeepSeek 3.2"},
	{ID: "minimax-m2.5", Name: "MiniMax M2.5"},
	{ID: "minimax-m2.1", Name: "MiniMax M2.1"},
	{ID: "qwen3-coder-next", Name: "Qwen3 Coder Next"},
}

// modelAliases 是「客户端常见写法 → Kiro 认识的档位名」的对照表。
//
// 参考实现的 normalize_model_name 用一串正则把 claude-haiku-4-5 / claude-haiku-4-5-20251001
// 这类写法归一成 claude-haiku-4.5。我们没有照搬那些正则（它们还会处理日期后缀、
// 倒装写法等一堆边角），只把**我们现在列出来的档位**的点/横线两种写法留成显式对照 ——
// 认不出的一律原样透传（Kiro API 才是最终裁判，参考实现的原话是
// "we are a gateway, not a gatekeeper"）。
var modelAliases = map[string]string{
	"auto-kiro":         "auto",
	"auto":              "auto",
	"claude-sonnet-4-6": "claude-sonnet-4.6",
	"claude-sonnet-4-5": "claude-sonnet-4.5",
	"claude-haiku-4-5":  "claude-haiku-4.5",
	"claude-opus-4-7":   "claude-opus-4.7",
	"claude-opus-4-6":   "claude-opus-4.6",
	"claude-opus-4-5":   "claude-opus-4.5",
}
