package anthropic

// constants.go 端点、OAuth 常量、伪装头与模型兜底清单。
//
// 这里的每个值是**从活着的参考实现里抄来的**，不是凭记忆写的 —— OAuth 的端点/参数/头
// 只要错一个字母，表现都是「登录页打得开、兑换令牌 400」，而且报文不会告诉你是哪一步错：
//
//	claude-code-cli/constants/oauth.ts
//	    从 Claude Code 本体抽出的**官方配置**：授权地址、令牌地址、client_id、
//	    回调地址、scope 全集。最权威的一份。
//	claude-code-cli/utils/betas.ts、services/api/client.ts、services/oauth/client.ts
//	    官方 CLI 真正发出的 anthropic-beta 组合、伪装头，以及 refresh 的报文。
//	    Messages 请求里 beta 头不止一个值（见 cliBeta）。
//	KarpelesLab/teamclaude（src/oauth.js）
//	    完整跑通 PKCE + 手工粘回授权码的授权码流程，并且用 Bearer + oauth beta
//	    头调账号接口（usage/profile）。
//	AmazingAng/auth2api（src/auth/oauth.ts、src/upstream/anthropic-api.ts）
//	    令牌兑换的报文形态，**scope 里的冒号不能编码**这个坑，以及订阅令牌走
//	    Messages API 时的完整伪装头清单。
//
// 三份互相印证；冲突处以「从 Claude Code 抽出的官方配置」为准（见 epToken 的说明）。

import "time"

const (
	// epAPI 是官方 API 基址（对话、模型目录、账号档案都在这上面）。
	epAPI = "https://api.anthropic.com"

	// epMessages 是对话端点。`?beta=true` 是真实 Claude Code（以及两个会自建请求的
	// 参考实现）都会带的查询串；本适配器照带 —— 它是幂等的查询参数，
	// 不带才可能与官方客户端的行为分叉。
	epMessages = "/v1/messages?beta=true"

	// epModels 是模型目录。**它是要鉴权的**（Bearer 无效会 401），
	// 所以除了当目录用，还顺带承担「验凭证是否真的有效」——见 checkCredential。
	epModels = "/v1/models"

	// epProfile 是订阅账号档案（拿 email / uuid 作账号标识）。
	// 注意：只要 Authorization: Bearer，**不带** anthropic-beta（teamclaude 的做法）。
	epProfile = "/api/oauth/profile"

	// epAuthorize 是授权页。Claude Code 官方配置用的是 claude.com/cai/oauth/authorize，
	// 它 307 两跳到 claude.ai/oauth/authorize —— 这里直接用终点，少一跳、少一处失真。
	epAuthorize = "https://claude.ai/oauth/authorize"

	// epToken 是令牌端点（授权码兑换与 refresh 共用）。
	//
	// 参考实现里出现过两个写法：api.anthropic.com/v1/oauth/token（auth2api）与
	// platform.claude.com/v1/oauth/token（teamclaude + Claude Code 官方配置）。
	// 取后者 —— 官方配置是权威，且两份独立实现一致。真要回退，改这一行即可
	// （ApiBase、tokenURL 都是字段化的，测试也依赖这一点）。
	epToken = "https://platform.claude.com/v1/oauth/token"
)

const (
	// oauthClientID 是 Claude Code 自己的 OAuth 客户端 id。
	//
	// 为什么能用它的：订阅授权的 scope（user:inference 等）只对这个客户端开放，
	// 自己注册的 client 拿不到「用订阅额度推理」的权限。这也是本渠道的性质来源:
	// 它借用的就是 Claude Code 这条官方链路。
	oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	// oauthScopes 是**全集**（Console 的 org:create_api_key 与 Claude.ai 订阅的
	// user:* 都带上）。官方注释说这是为了兼容「Console 账号跳 claude.ai」的跳转，
	// 少一个都可能让某类账号授权后拿不到可用的令牌。
	oauthScopes = "org:create_api_key user:profile user:inference " +
		"user:sessions:claude_code user:mcp_servers user:file_upload"

	// oauthRedirect 是**手工授权**用的回调地址：授权完成后浏览器落到这个地址，
	// 页面把授权码显示出来给用户复制（形如 `code#state`）。
	//
	// 这是本渠道能在 NAS 上跑起来的关键：不需要浏览器与网关同机、不需要开本机端口、
	// 不需要公网回调 —— 与 gemini 的 user-code 流同一个思路（见 login.go）。
	oauthRedirect = "https://platform.claude.com/oauth/code/callback"

	// oauthBeta 是订阅式 OAuth 令牌**必须**带的 beta 头。
	//
	// 这是本渠道与「API Key 式」的硬分界：OAuth 令牌走 Authorization: Bearer + 这个头；
	// API key 走 x-api-key + anthropic-version。两者用反了上游只回 401，
	// 看起来像「令牌无效」，实际是认证方式搞混了。
	oauthBeta = "oauth-2025-04-20"

	// cliBeta 是官方 CLI 在**非 haiku** 模型上固定追加的 beta 头。
	//
	// 依据：claude-code-cli/utils/betas.ts:240-252 的 getAllModelBetas —— 非 haiku
	// 模型先 push 'claude-code-20250219'（constants/betas.ts:3，betas.ts:241），
	// 订阅账号再 push OAUTH_BETA_HEADER（betas.ts:252）；auth2api 的 buildBetaHeader
	// （src/upstream/anthropic-api.ts:29-32）也是这两项并列。所以真实 Claude Code 的
	// Messages 请求头是「claude-code-20250219,oauth-2025-04-20,…」而不是只带 oauth 一项。
	//
	// 我们只跟这两项，**不带**参考实现里属于具体功能的 beta（interleaved-thinking /
	// redact-thinking / effort / structured-outputs / context-management 等）：
	// 那些是「打开某个功能」，不是认证所需，带上等于替客户端改了行为（红线一）。
	cliBeta = "claude-code-20250219"

	// anthropicVersion 是 Messages API 的版本头。Anthropic 要求每个请求都带，
	// 语义是「按日期锁定行为」—— 不能省，也不能乱升。
	anthropicVersion = "2023-06-01"

	// cliUA 与 x-app 是**伪装成官方 CLI** 的头。
	//
	// 参考实现都带（auth2api 从 mitmproxy 抓真实 Claude Code 得到 UA 形态；
	// BYOKEY 硬编码 claude-cli/2.1.109）。上游对订阅令牌的接受度与「像不像官方客户端」
	// 相关，带上成本为零。UA 里的版本号会随上游放宽/收紧而失效 —— 换版本只改这一处。
	cliUA = "claude-cli/2.1.109 (external, cli)"
)

const (
	// httpTimeout 给足：长思考 + 长输出，首字之前不设死。
	httpTimeout = 10 * time.Minute

	// refreshBuffer 是 access token 的提前刷新量：剩余寿命少于它就认为「快过期」。
	// 不提前刷的代价是每个请求都先撞一次 401，看起来像凭证坏了。
	refreshBuffer = 5 * time.Minute

	// defaultMinIntervalSec 是同账号最小请求间隔出厂默认（秒）。
	//
	// 取比网页聊天渠道更保守的值：订阅额度是**按账号**算的，同号猛打既是限流，
	// 也是最直接的封号线索 —— 而本渠道的官方定性本来就是「第三方客户端不该用订阅令牌」，
	// 再叠上高频调用，风险是叠加的（见 docsText）。
	defaultMinIntervalSec = 3
)

// docsText 是面板上展示的渠道说明。
//
// 硬性要求：**必须写明封号风险**，而且要写成用户看得懂的结论，不是免责声明走形式。
// Anthropic 的条款明确禁止第三方客户端拿订阅令牌调 API —— 用这个渠道是有代价的，
// 用户有权在加账号之前就知道。
const docsText = "Anthropic 订阅（Claude Pro/Max）OAuth 登录，不用 API Key。" +
	"⚠️ 封号风险：Anthropic 明确禁止第三方客户端使用订阅令牌调用 Messages API；" +
	"多账号轮换、高频调用、对外转售更容易被判定违规并**封禁账号**。" +
	"请自己评估风险，别拿主力账号试；在意风险就改用官方 API Key 的「接入源」形态。" +
	"本渠道不改写对话内容，思考参数不主动打开（见 Spec 注释）。"

// defaultModels 是**官方地址**的兜底清单（上游 /v1/models 拿不到时用）。
//
// 只在渠道上线时 /v1/models 不可用的情况下顶一下，来源统一标 SourceLocal（本地清单 ≠
// 上游确认）。上下文长度取官方公布值，不猜数字。
var defaultModels = []struct {
	ID   string
	Name string
	Ctx  int
}{
	{ID: "claude-opus-5", Name: "Claude Opus 5", Ctx: 1_000_000},
	{ID: "claude-sonnet-5", Name: "Claude Sonnet 5", Ctx: 1_000_000},
	{ID: "claude-opus-4-8", Name: "Claude Opus 4.8", Ctx: 1_000_000},
	{ID: "claude-opus-4-7", Name: "Claude Opus 4.7", Ctx: 1_000_000},
	{ID: "claude-opus-4-6", Name: "Claude Opus 4.6", Ctx: 1_000_000},
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", Ctx: 1_000_000},
	{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5", Ctx: 200_000},
}
