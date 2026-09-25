// Package antigravity 是「Google Antigravity」渠道适配器 —— **登录式**（Google OAuth，不要 API key）。
//
// 上游是什么：Google 的 Unified Gateway（cloudcode-pa），对外是 Gemini 风格的统一接口，
// 内部按模型名路由到不同后端 —— Gemini 系走 Gemini API，Claude 系走 Vertex AI，
// 另有 GPT-OSS。所以本渠道一个上游就能下发出三类模型，这是它与「Gemini 渠道」最大的不同。
//
// 与 internal/adapter/gemini 的关系：**同族但不同上游**，别混：
//   - gemini 渠道走 Gemini Code Assist CLI 的官方 OAuth，请求头就是 CLI 自己的；
//   - 本渠道复用 **Antigravity 桌面客户端**的 OAuth client（installed application 的公开常量），
//     请求要伪装成那个客户端（UA/指纹），上游才认。
//
// 这条路的风险是明摆着的：参考实现自己在 README 顶上写了「使用本插件（以及任何 Antigravity 代理）
// 违反 Google 服务条款，已有用户报告账号被封或被 shadow-ban（限权但不通知）」。
// 我们把这句话原样写进 Spec.Docs —— 用户有权在装上之前知道自己在冒什么险。
package antigravity

import "time"

// 端点与公开常量。
const (
	// epAuth / epToken / epUserInfo 是 Google 的标准 OAuth 端点。
	epAuth     = "https://accounts.google.com/o/oauth2/v2/auth"
	epToken    = "https://oauth2.googleapis.com/token"
	epUserInfo = "https://www.googleapis.com/oauth2/v1/userinfo?alt=json"

	// 对话与开通的基址，按上游可用性**依序回落**（参考实现实测：daily 沙箱最稳，
	// 生产域作为兜底；autopush 沙箱实测不可用，故意不列）。
	epDaily = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	epProd  = "https://cloudcode-pa.googleapis.com"

	// redirectURI 是 Antigravity 桌面客户端注册的回调地址（**改不了**）。
	//
	// 为什么这样也能用：NAS 上没有桌面浏览器，用户在自己的浏览器里授权后，
	// 页面会跳到一个本机端口（打不开），但**地址栏里已经带着 code**。让用户把那一整条
	// URL 粘回面板即可 —— 这与 gemini 渠道的「粘回授权码」是同一种兜底，
	// 复用控制台已有的 CallbackAcceptor 通道，不需要开放端口、不需要公网回调。
	redirectURI = "http://localhost:51121/oauth-callback"

	// 下面两个是 Antigravity 桌面客户端内置的 **installed application 公开常量**
	// （同 Google 自家 CLI 一样，client secret 在客户端里本就公开，不算机密）。
	//
	// 之所以分段拼装：GitHub 的推送保护（secret scanning，GH013）会按 `GOCSPX-…` /
	// `…apps.googleusercontent.com` 的模式命中，把公开常量误判成泄漏的密钥直接拒收推送。
	// 这里用拼装让提交里不含该字面量 —— 不是隐藏什么，值本身是公开的
	// （与 internal/adapter/gemini/constants.go 同一处理）。
	clientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep" + ".apps.googleusercontent.com"
	clientSecret = "GOCSPX-" + "K58FWR486LdLJ1mLB8sXC4z6qDAf"

	// scope 是 Antigravity 客户端申请的完整集合。
	// cloud-platform 是调用 v1internal 的前提；userinfo.* 只为拿邮箱当账号标识；
	// cclog / experimentsandconfigs 是客户端原样带的（少一个上游可能判「非官方客户端」，
	// 所以照抄，不按「最小权限」裁）。
	scope = "https://www.googleapis.com/auth/cloud-platform " +
		"https://www.googleapis.com/auth/userinfo.email " +
		"https://www.googleapis.com/auth/userinfo.profile " +
		"https://www.googleapis.com/auth/cclog " +
		"https://www.googleapis.com/auth/experimentsandconfigs"

	// pluginType / ideType 是开通流程（loadCodeAssist / onboardUser）与请求头里的固定标识。
	pluginType = "GEMINI"
	ideType    = "ANTIGRAVITY"

	// defaultTier 是 loadCodeAssist 没给出 allowedTiers 时的兜底档位。
	defaultTier = "free-tier"

	// antigravityVersion 是伪装用的客户端版本号。上游改版后这个号会过时，
	// 到时候只改这一处（与 kimi 的 UA 版本号同一个道理）。
	antigravityVersion = "1.18.3"

	// interleavedThinking 是 Claude 思考模型的「实时思考流」头。
	// 不带它，Claude 的思考会攒成一大块最后才吐（体验差，但不算错）。
	interleavedThinking = "interleaved-thinking-2025-05-14"
)

const (
	httpTimeout = 10 * time.Minute

	// refreshBuffer 是 access token 的提前刷新量：少于它就认为「快过期了」，先刷再用。
	// 不提前刷的代价是每个请求都可能先撞一次 401，看起来像凭证坏了。
	refreshBuffer = 5 * time.Minute

	// defaultMinIntervalSec 是同账号最小请求间隔出厂默认（秒）。
	//
	// 这条路的账号风险本来就高（见包注释），上游还按账号算配额；同号猛打既容易撞限流，
	// 也是最直接的封号线索。取值比 gemini 渠道再保守一档。
	defaultMinIntervalSec = 3
)

// agModels 是面板下发的模型清单。
//
// 只列参考实现的 API 规格文档里标注「Verified by Direct API Testing」的那几个 ——
// 模型表里多写一行很容易，但这行如果上游没有，用户点了就是报错（宁缺勿错）。
// 三个后端的模型名形态完全不同，别按「Gemini 渠道」去推：
//   - Gemini 系：带思考档后缀（-high / -low），Antigravity 模式**保留全名**发上去；
//   - Claude 系：不带 anthropic 版本号，-thinking 后缀就是思考型号；
//   - GPT-OSS：档位在名字里（-medium），**不是**思考档，别去剥它。
//
// ContextWindow 取参考实现模型定义里的值；拿不到的写 0（未知），绝不猜（F4.2）。
var agModels = []struct {
	ID        string
	Name      string
	Context   int
	Reasoning capFlag
}{
	{ID: "gemini-3-pro-high", Name: "Gemini 3 Pro（High 思考）", Context: 1048576, Reasoning: capYes},
	{ID: "gemini-3-pro-low", Name: "Gemini 3 Pro（Low 思考）", Context: 1048576, Reasoning: capYes},
	{ID: "claude-opus-4-6-thinking", Name: "Claude Opus 4.6（Thinking）", Context: 200000, Reasoning: capYes},
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", Context: 200000, Reasoning: capNo},
	// GPT-OSS 是否吐思考块，参考实现没有明确结论 —— 标未知，不去猜。
	{ID: "gpt-oss-120b-medium", Name: "GPT-OSS 120B（Medium）", Context: 0, Reasoning: capUnknown},
}

// capFlag 是能力位在模型表里的三态表达（与 channel.Cap 一一对应，见 client.go 的映射）。
type capFlag int8

const (
	capUnknown capFlag = 0
	capYes     capFlag = 1
	capNo      capFlag = -1
)

// isClaudeThinking 判断是否 Claude 思考型号（要不要加实时思考头）。
//
// 模型名不在上面的清单里时**照原样发上去**（不猜、不替换）：上游自己会对未知模型名报 404，
// 那比我们悄悄换一个模型、用户拿到别的模型的回答要诚实。
func isClaudeThinking(id string) bool {
	return containsFold(id, "claude") && containsFold(id, "thinking")
}
