package windsurf

// client.go 渠道实现：能力声明、对话编排、额度校验、错误归一。
//
// 本渠道的「工具调用」走**原生**（Spec: Tools=true）：请求 #10 下发 ToolDef、响应 #6
// 回收 ChatToolCall。参考实现已用付费实弹标定了 ToolDef 的内部 tag（见 protocol.go
// toolDefFieldName 处说明），所以不再走网关模拟。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Adapter 实现 channel.Channel（登录走 Authorizer + CallbackAcceptor，见 login.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	base string // 站点基址（测试可换）
}

func New() *Adapter {
	return &Adapter{
		clients: map[string]*http.Client{},
		base:    apiBase,
	}
}

func (a *Adapter) clientFor(c *channel.Credential) *http.Client {
	key := channel.EgressOf(c)
	a.mu.Lock()
	defer a.mu.Unlock()
	if cl, ok := a.clients[key]; ok {
		return cl
	}
	cl := channel.NewHTTPClient(c, httpTimeout)
	a.clients[key] = cl
	return cl
}

func (a *Adapter) Kind() channel.Kind { return channel.Windsurf }

// Spec 能力声明（说实话，逐条有依据）。
//
//   - Tools=true：上游原生工具调用（请求 #10 ToolDef、响应 #6 ChatToolCall）。
//     ToolDef 的内部 tag 已被参考实现付费实弹标定（devin-connect.js:394-397），
//     所以走原生透传，不再声明 ToolsShim。
//   - Reasoning=true：上游把思考与正文**原生分流**（正文 #3、思考 #9），我们映射成
//     reasoning_content。这是一个真实的独立通道，不是把正文切一半。
//   - Images=false：上游能收图（请求 ChatMessage #10），但本网关的 ChatRequest 里
//     Message 只有文本，传不过去 —— 声明支持却不转发，比不支持更糟。
//   - SSEOnly=true：只有流式多帧响应，非流式由网关本地聚合。
//   - CheckinCap=false：没有签到。
//   - Category=Coding：订阅额度池型的 IDE 渠道，与网页聊天那类分开。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.Windsurf,
		DisplayName:           "Windsurf（Codeium）",
		Status:                channel.Active,
		Category:              channel.CategoryCoding,
		Tools:                 true,
		ToolsShim:             false,
		Images:                false,
		Reasoning:             true,
		SSEOnly:               true,
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs: "⚠️ 风险自负：本渠道复用 Windsurf/Devin 桌面客户端的私有 Connect-RPC 协议与登录态" +
			"（第三方客户端伪装）。参考实现（WindsurfAPI）README 自述**严禁商业使用、转售、代部署、" +
			"挂后台对外提供服务**；别拿主力账号登录，封号与本项目无关。" +
			" | 登录：粘贴 session token（形如 devin-session-token$…），我们**不做**邮箱密码登录。" +
			" | 模型：免费账号只跑得动 swe-1-6-slow，其它 selector 上游返回升级提示。" +
			" | 工具：原生透传（请求 #10 / 响应 #6）；上游会对工具描述做 MCP 指纹匹配，命中即整请求" +
			" permission_denied，所以工具描述会被替换成工具名、参数 schema 里的描述会被去掉。",
	}
}

// Login 是「一次性登录」入口：凭证来自你已有的 Windsurf/Devin 登录态，必须用户参与。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"Windsurf 需要你在面板里粘贴一次 session token（形如 devin-session-token$…），不必给密码").
		WithChannel(string(channel.Windsurf))
}

// Models 返回本地模型清单（详见 constants.go 的 webModels 与理由）。
func (a *Adapter) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	out := make([]channel.ModelInfo, 0, len(webModels))
	for _, m := range webModels {
		out = append(out, channel.ModelInfo{
			ID:          m.ID,
			DisplayName: m.Name,
			// 上下文长度：上游没给可信值，不猜数字（F4.2）。
			ContextWindow: 0,
			Source:        channel.SourceLocal,
			Tools:         channel.CapYes, // 原生工具调用（请求 #10 / 响应 #6）
			Reasoning:     capOf(m.Think),
			Images:        channel.CapNo,
		})
	}
	return out, nil
}

func capOf(b bool) channel.Cap {
	if b {
		return channel.CapYes
	}
	return channel.CapNo
}

// Balance 余额未知：额度接口给的是 plan 与配额百分比，不是可直接展示的余额数字；
// 不猜数字（F4.2）。真要看 plan，登录校验那一步会把 plan 写进账号昵称。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin 无签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "Windsurf 没有签到活动"}, nil
}

// Refresh 尝试续期。
//
// session token 是**长期凭证**：参考实现拿它去上游并不存在「用 refresh token 换新
// session token」的公开路径（它的续期靠邮箱密码重新 Auth1 登录，那条路我们不做）。
// 所以这里明确返回 (nil, nil) 表示「刷不了」，让 401 按 SessionDead 处理、提示重新粘贴，
// 而不是假装 refresh 成功。
func (a *Adapter) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// Chat 发一次对话：构造 GetChatMessageRequest → 包 Connect 信封 → 按帧流式解析。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	token := tokenOf(c)
	if token == "" {
		return nil, errs.New(errs.SessionDead, "Windsurf 凭证为空：请在面板里重新粘贴 session token").
			WithChannel(string(channel.Windsurf))
	}
	model := resolveModel(req.Model)
	// 记录**实际写进 wire 的**输出上限：结束原因要靠它判断有没有被截断
	// （参考实现 resolveFinishReason 的判据，见 stream.go）。
	encodedMax := req.MaxTokens
	if encodedMax <= 0 {
		encodedMax = defaultMaxTokens
	}

	// ForwardTools 已按 tool_choice:"none" 过滤（见 channel.ChatRequest）。
	tools := req.ForwardTools()
	proto := buildChatRequest(token, model, "", req.Messages, tools, req.MaxTokens, req.Temperature)
	framed := wrapRequest(proto)

	resp, err := a.send(ctx, c, epChat, ctConnectProto, framed)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.Windsurf)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, req.Model, encodedMax, toolNamesOf(tools)), nil
}

// checkCredential 用额度接口当场验一次凭证（登录时调用）。
//
// 为什么用它：GetUserStatus 是零推理、不烧额度的席位接口 —— 一个 200 就说明
// session token 现在还能用。把已失效的凭证塞进池子，用户看到的是「装上就 401」，
// 分不清是自己粘错了还是渠道坏了；这里当场问一句，错了立刻说清楚。
//
// 顺带把 plan 名解出来写进凭证昵称（免费/付费一眼可见），但**不**解配额百分比：
// 百分比在 protobuf 响应的 plan_status 子消息里，参考实现自己没有标定那几个 tag
// （它是从 proto3-JSON 变体里读的），我们不猜未标定的字段号。
func (a *Adapter) checkCredential(ctx context.Context, c *channel.Credential) (string, error) {
	body := buildUserStatusRequest(tokenOf(c))
	resp, err := a.send(ctx, c, epUserStatus, ctProto, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", errs.New(a.Classify(resp.StatusCode, raw), "凭证校验失败").
			WithChannel(string(channel.Windsurf)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return decodePlanName(raw), nil
}

// decodePlanName 从 GetUserStatusResponse 里取 plan 名。
//
// 字段号来自参考实现 devin-connect-catalog.js（calibrated）：
//
//	顶层 #1 = UserStatus，顶层 #2 = PlanInfo
//	PlanInfo #2 = plan_name（字符串，如 "Free" / "Pro"）
//
// 解不出来就返回空串 —— 这是「未知」，不是错误（旧账号可能没有这个字段）。
func decodePlanName(raw []byte) string {
	top, err := parseFields(raw)
	if err != nil {
		return ""
	}
	pi := getBytes(top, 2)
	if pi == nil {
		return ""
	}
	fields, err := parseFields(pi)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(getString(fields, 2))
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// send 发一个请求，带上伪装头与「双写」鉴权头。
//
// contentType 只有两种取值：对话用 application/connect+proto（body 是信封），
// 额度接口用 application/proto（body 是裸 protobuf）。别混 —— 用错 Content-Type
// 上游的回法完全不同（前者按帧、后者整包）。
func (a *Adapter) send(ctx context.Context, c *channel.Credential, path, contentType string, body []byte) (*http.Response, error) {
	token := tokenOf(c)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.Windsurf)).WithCause(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Connect-Protocol-Version", connectProtoV1)
	req.Header.Set("Connect-Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	// ★ 鉴权：session token 双写、短横线连接。单 token 会被回 permission_denied。
	req.Header.Set("Authorization", "Basic "+token+"-"+token)

	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.Windsurf)).
			WithAccount(uidOf(c)).WithCause(err)
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// 错误归一（D4）
// ---------------------------------------------------------------------------

// Classify 把上游错误归一成有限枚举。
//
// ★ 判据顺序是本函数的核心，有两个「排错位置就会误杀账号」的地方：
//
//  1. **瞬时故障优先于鉴权**。上游会把容量/后端故障（"high demand"、
//     "an internal error occurred"）包在一个 401/403 的「鉴权壳」里返回。如果先看
//     状态码，一次瞬时抖动就会被读成「token 死了」→ 触发无意义的重新登录、
//     把好号冷却掉。所以凡是瞬时文本判据，都排在 401/403 之前。
//  2. **内容策略 / 升级墙优先于鉴权**。这两个上游也用 permission_denied 表达，
//     但它们不是凭证问题（内容被拦换个 prompt 就好；升级墙是模型没权限）。
//     排在鉴权之前，才不会把「内容被拦」报成「账号被封」。
//
// 与本仓约定一致的一处处理：上游把「限流」和「额度耗尽」都用 429 表达，
// 我们用正文关键词把额度耗尽拆出来判 HardCredit（等多久都没用）而不是 SoftRate
// （等一会儿就好）—— 与 internal/adapter/kimi 同一条判据。判反了要么白等 12 小时，
// 要么把一个还能用的账号当成没钱了。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	return classify(status, body)
}

func classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	// 内容策略拦截：token 是好的（换个 prompt 立刻能用），不罚账号、不要求重登。
	case containsAny(s, "blocked by our content policy", "remove sensitive", "remove unsafe", "content_policy", "content policy"):
		return errs.ContentBlocked
	// 额度/权益不足：账号态，冷却账号。必须排在 429 分支之前，否则「没钱了」被读成「限流了」。
	case containsAny(s, "insufficient credit", "insufficient quota", "insufficient balance", "out of credit", "out of quota", "quota exceeded", "credit exhausted", "exceeded your quota"):
		return errs.HardCredit
	// 限流（含带重置窗口的硬限流 "…Resets in: 3h0m0s"）：等一会儿，交给池级冷却，
	// 不做同 token 立即重试（重试只会加重限流）。
	case containsAny(s, "rate limit", "rate_limit", "resource_exhausted", "too many requests"), status == 429:
		return errs.SoftRate
	// 升级墙：免费账号点了付费 selector。是「该账号没这个模型的权限」，
	// 既不是凭证问题也不是余额问题。上游同样用 permission_denied 表达。
	case containsAny(s, "/upgrade", "upgrade to access", "insufficient entitlement", "requires a paid", "requires paid", "requires a pro", "requires a team", "requires an enterprise"):
		return errs.ModelUnavailable
	// 后端瞬时故障：容量/高需求/overloaded/"an internal error occurred"。
	// **必须排在鉴权之前**（见上）。这些不算账号错误
	// （errs.UpstreamFault.AccountBlamed()==false），否则一次上游抖动会冷却好号。
	case containsAny(s, "internal error occurred", "high demand", "try again later",
		"at capacity", "currently busy", "currently overloaded", "temporarily unavailable",
		"server is busy", "service unavailable", "backend unavailable", "model unavailable", "overloaded"):
		return errs.UpstreamFault
	// 鉴权失败：401/403 状态码，或正文里的鉴权语义。
	case status == 401 || status == 403,
		containsAny(s, "permission_denied", "unauthenticated", "invalid token", "invalid_token", "token is invalid"):
		return errs.SessionDead
	case status == 400 || status == 422:
		if containsAny(s, "context", "too long", "max_tokens", "maximum context") {
			return errs.PromptTooLong
		}
		return errs.Parse
	case status == 404:
		return errs.ModelUnavailable
	case status >= 500:
		return errs.UpstreamFault
	}
	return errs.Parse
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// tokenOf 取凭证里的 session token。
//
// 约定放在 AccessToken：这串是长期凭证、没有 refresh 形态，不占 RefreshToken
// （省得上层看到 RefreshToken 非空却刷不动而困惑）。
func tokenOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	if v := strings.TrimSpace(c.AccessToken); v != "" {
		return v
	}
	return strings.TrimSpace(c.RefreshToken)
}

func uidOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	return c.UID
}

// toolNamesOf 抽出本轮下发的工具名，供响应侧工具调用「解不到 name 时反查」用
// （上游响应侧 ChatToolCall 的 name 字段参考实现自己存疑，见 protocol.go 的说明）。
func toolNamesOf(tools []map[string]any) []string {
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		if name, _ := fn["name"].(string); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func trimSpace(s string) string { return strings.TrimSpace(s) }

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
