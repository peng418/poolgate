// Package windsurf 是「Windsurf / Codeium（现 Devin Desktop）」渠道适配器 —— **登录式**。
//
// 协议取自公开参考实现 WindsurfAPI（纯 Node、零依赖、手搓 protobuf）里的
// **DEVIN_CONNECT 那条路**（src/devin-connect.js + src/connect.js）：纯 HTTP 直连
// `server.codeium.com`，不需要本地语言服务器（Cascade / LS 二进制）。
//
// 为什么只做这条路：参考实现还有一条走本地 LS 的路（StartCascade 流程）。我们的网关在
// NAS 上跑，不会为了一个渠道去常驻一个 IDE 的语言服务器（体积、架构、稳定性都不合），
// 所以**只**实现纯 HTTP 的 GetChatMessage。
//
// 三个「不看源码绝对猜不到」的协议要点（全部摘自参考实现，别凭记忆改）：
//
//  1. 鉴权头是 session token **双写、短横线连接**：`authorization: Basic <token>-<token>`。
//     单 token 会被上游回 permission_denied。但 protobuf 正文里的
//     `ClientMetadata.session_token`（字段 #3）仍是**单份** —— 复制的是 HTTP 头，不是正文。
//
//  2. `ClientMetadata` 字段 **#31 必须是 732 个 hex 字符（366 字节）**，短了上游会回一个
//     语焉不详的 `internal` 错误。这个值**只校验长度、不校验内容**，随机 hex 即可
//     （参考实现默认就是每请求随机 366 字节）。
//
//  3. 工具调用在这条路上有原生字段位（请求 `GetChatMessageRequest.tools` #10、响应
//     `ChatToolCall` #6），但参考实现**默认关闭**原生工具，走提示词模拟 —— 因为
//     `ToolDef` 的**内部子字段 tag 未标定**（recon 只知道「3 个字段、名字/描述/schema 之类」，
//     没有确认字段号）。猜错不会报错，只会静默失败（请求发出去、模型不调工具），
//     所以我们的 Spec 声明 `Tools=false + ToolsShim=true`，工具调用交给网关模拟层，
//     **绝不去猜那两个未标定的子 tag**。
//
// 风险（必读，也是 Spec.Docs 要写清的东西）：
//   - 本渠道复用第三方客户端（Windsurf/Devin）的登录态与私有 Connect-RPC 协议，
//     参考实现 README 自己声明「严禁商业使用、转售、代部署、挂后台对外提供服务」；
//     我们只把它当作个人自用的一条路，风险由使用者自担。
//   - 上游协议随时可能改（字段号是逆向出来的，没有官方 .proto 兜底），
//     失效是常态而不是意外。
package windsurf

import (
	"strings"
	"time"
)

// 端点（全部在 server.codeium.com 上）。
const (
	apiBase = "https://server.codeium.com"

	// epChat 是对话端点：Connect-RPC 单信封请求 + 多帧流式响应。
	epChat = "/exa.api_server_pb.ApiServerService/GetChatMessage"

	// epUserStatus 是额度/席位接口：用来「当场验一次凭证」，也是零推理、不烧额度的
	// 最便宜的活性探针（参考实现 checkSessionLiveness 就是拿它做活性检查的）。
	epUserStatus = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
)

// 客户端伪装常量。
//
// "chisel" 是 Windsurf/Devin CLI 内部对自己的自称；版本号跟随 CLI 构建。
// 这些值会随上游收紧而失效，改的时候只改这里一处。
const (
	clientName     = "chisel"
	clientVersion  = "2026.8.18"
	connectProtoV1 = "1"
	userAgent      = "connect-es/2.0.0"
	locale         = "en"
	platform       = "windows"
)

// Connect 传输用的两种 Content-Type。
//
// 对话走 Connect 流式协议（application/connect+proto：请求带 5 字节信封）；
// 额度接口是 unary，参考实现用 application/proto（裸 protobuf，无信封）。别用错。
const (
	ctConnectProto = "application/connect+proto"
	ctProto        = "application/proto"
)

// 设备指纹长度：366 字节 = 732 个 hex 字符。见包注释要点 2。
const (
	fingerprintBytes = 366
	fingerprintHex   = fingerprintBytes * 2
)

// CompletionConfig 的默认值（对齐参考实现实测的 CLI 请求）。
const (
	// defaultMaxTokens 是调用方没指定时的输出上限。
	//
	// 为什么是 8192 而不是 4096：字段 #2 在参考实现里是**真正生效**的输出上限
	// （曾经 #2/#3 标反过，那时 #2 只是个空转字段），所以默认值不能给太小 ——
	// 否则「没指定 max_tokens」的调用会被悄悄截断，而参考实现的仓库约定就是 8192。
	defaultMaxTokens     = 8192
	defaultContextWindow = 128000
	defaultTemperature   = 1.0
	defaultTopK          = 40
	defaultTopP          = 0.95
)

// minTemperature 是上游能接受的最小温度。
//
// Live 实测：temperature 正好 0（贪心解码）会让上游回 `an internal error occurred`，
// 0.001 正常。而 OpenAI/Anthropic 客户端发 temperature=0 是常规操作，所以这里
// 把 0 夹到 0.001（尽可能接近贪心），而不是把「客户端要确定性输出」变成硬失败。
const minTemperature = 0.001

// defaultModel 是模型名缺省时的兜底 selector。
//
// 免费账号在这条路上**只跑得动 swe-1-6-slow**，其它 selector 会被上游用升级提示拒掉
// （参考实现实测结论）。所以缺省档就选它 —— 至少保证免费账号开箱可用。
const defaultModel = "swe-1-6-slow"

const (
	httpTimeout = 10 * time.Minute

	// defaultMinIntervalSec 是同账号两次请求的最小间隔出厂默认（秒）。
	//
	// 这是个订阅额度池型的 IDE 渠道（不是网页反爬那类），但同号猛打仍会撞上
	// 上游的 message rate limit（实测会返回带 `Resets in: 3h` 的硬限流），
	// 所以给一个温和的 2 秒节奏。
	defaultMinIntervalSec = 2
)

// modelEntry 是面板下发的模型档位。
type modelEntry struct {
	ID    string
	Name  string
	Free  bool // 免费账号能不能跑
	Think bool // 上游会不会单独吐思考（映射成 reasoning_content）
}

// webModels 是本地模型清单。
//
// 为什么是本地硬编码而不是拉上游目录：上游确实有 `GetCliModelConfigs` 目录接口，
// 但它的 selector 名册有一百多个、且随上游增删（参考实现是拿一份快照 + 运行时同步来兜的）。
// 我们只列**参考实现里确认存在**的这一小撮 SWE（Cognition 自研）档位，
// 并逐档标注「免费可用 / 付费」，避免用户点了才发现要升级。
//
// 不在清单里的 selector 不会「回落到别的模型」（那会让用户拿到一个不是自己点的模型），
// 而是原样转发给上游 —— 上游认就认，不认就回升级提示或 internal 错误，如实归一。
var webModels = []modelEntry{
	{ID: "swe-1-6-slow", Name: "SWE-1.6 Slow（免费可用）", Free: true, Think: true},
	{ID: "swe-1-6", Name: "SWE-1.6（付费）", Think: true},
	{ID: "swe-1-6-fast", Name: "SWE-1.6 Fast（付费）", Think: true},
	{ID: "swe-1-7", Name: "SWE-1.7（付费）", Think: true},
	{ID: "swe-1-7-lightning", Name: "SWE-1.7 Lightning（付费）", Think: true},
}

// resolveModel 解析客户端给的模型名。
//
// 规则尽量少：空 → 默认档；其余原样（只 trim）。**不做别名猜测、不做静默替换** ——
// 把 claude/x 翻译成 swe-1-6-slow 会让用户以为自己在用另一个模型，那比报错更糟。
func resolveModel(id string) string {
	m := strings.TrimSpace(id)
	if m == "" {
		return defaultModel
	}
	return m
}
