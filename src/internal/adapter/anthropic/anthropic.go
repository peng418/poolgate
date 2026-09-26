// Package anthropic 是「Anthropic 订阅（Claude Pro/Max）」渠道适配器 —— **登录式**。
//
// 与旧版的区别（本质性的）：旧版是 config 驱动的 **API Key 式来源**（面板「接入源」里填
// sk-ant-api…，没有账号、没有续期）；现在改成**订阅 OAuth 登录**：
//
//	面板点「添加账号 → Anthropic」→ 打开授权页 → 用订阅账号登录 → 把页面显示的授权码
//	粘回面板 → 网关拿授权码换 access/refresh token → 之后用 **Bearer + oauth beta 头**
//	调 Messages API，令牌过期用 refresh token 续。
//
// 为什么值得单列一个渠道而不是复用 API Key 形态：订阅令牌的**认证方式与 key 完全不同**
// （Authorization: Bearer + anthropic-beta: oauth-2025-04-20，而不是 x-api-key），
// 混用只会得到 401；而且它会过期，必须能自动续期 —— 这两点都是 Key 式来源没有的。
//
// 转换方向与网关的 /v1/messages 兼容层正好相反：
//   - 网关兼容层：Anthropic 形态（客户端）→ 内部 channel 契约；
//   - 本适配器：内部 channel 契约（OpenAI 形态）→ Anthropic 形态（上游），
//     响应侧再把 Anthropic SSE 归一成标准 chunk。
//
// 两边共用同一套字段名约定（input_schema ↔ function.parameters、tool_use ↔ tool_calls），
// 改一边时另一边要一起看。body.go 与 stream.go 是这两侧的换算，已验证，不动。
//
// OAuth 端点/参数来自活着的参考实现，出处见 constants.go 的文件头 —— 不凭记忆发明端点。
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Adapter 实现 channel.Channel（授权走 Authorizer + CallbackAcceptor，见 login.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	// 端点字段化，便于测试指向 mock server（生产用 constants.go 里的常量）。
	apiBase      string
	authorizeURL string
	tokenURL     string
}

// New 建立适配器。
func New() *Adapter {
	return &Adapter{
		clients:      map[string]*http.Client{},
		apiBase:      epAPI,
		authorizeURL: epAuthorize,
		tokenURL:     epToken,
	}
}

// clientFor 按「有效出口」缓存客户端（全局出口代理可在运行中改，所以按 key 缓存）。
//
// 出口对本渠道尤其重要：本机机房 IP 常被上游按 IP 拒（403），有出口才用得上。
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

func (a *Adapter) Kind() channel.Kind { return channel.Anthropic }

// Spec 能力声明。
//
// Tools=true 且 ToolsShim=false：Messages API **原生**支持工具调用（tools → input_schema、
// tool_use/tool_result），不需要 toolshim 那层模拟 —— 声明成模拟就是撒谎。
//
// Reasoning=false 是**有意**的、也是诚实的：内部契约里没有「本轮要不要思考」这个开关，
// 而 Anthropic 打开思考必须显式带 thinking 参数，且与 temperature 互斥（同时带会被判 400）。
// 擅自替客户端打开思考 = 让一部分本来能用的请求直接失败，所以这里不打开；
// 上游若仍然送来了 thinking 块（新版模型思考常开），stream.go 会照实转成 reasoning_content
// —— 不打开不等于丢掉。
//
// Images=false：本适配器不做图片上行；声明 true 会误导客户端。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:        channel.Anthropic,
		DisplayName: "Anthropic（Claude 订阅 OAuth）",
		Status:      channel.Active,
		// 订阅额度池归 coding（编程助手/IDE 类），与「API Key 式来源」「网页聊天类」分开：
		// 三者的额度模型、风控强度、能不能调工具都不同，混在一起用户会拿「余额」理解「限速」。
		Category:              channel.CategoryCoding,
		Tools:                 true,
		ToolsShim:             false,
		Images:                false,
		Reasoning:             false,
		SSEOnly:               true,  // 统一向上游要 stream，非流式由网关本地聚合
		CheckinCap:            false, // 订阅没有签到活动
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs:                  docsText,
	}
}

// Login 是 channel.Channel 要求的「一次性登录」：本渠道需要用户交互（授权码粘回），
// 所以走面板授权通道（channel.Authorizer，见 login.go）。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"Anthropic 订阅需要交互式授权：请在面板「账号」页点「添加账号 → Anthropic」，"+
			"浏览器登录后把授权码粘回面板").WithChannel(string(channel.Anthropic))
}

// ---------------------------------------------------------------------------
// 凭证：OAuth 令牌的取得、续期、可用性
// ---------------------------------------------------------------------------

// Refresh 用 refresh_token 换新的 access token（上游可能轮换 refresh_token，新的要落盘）。
//
// 拿不到 refresh token 时返回 (nil, nil) —— 明确表示「刷不了」，让上层按 401 处理，
// 而不是假装刷新成功（那会让每个请求都带着死令牌发出，看起来像上游故障）。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if c == nil || strings.TrimSpace(c.RefreshToken) == "" {
		return nil, nil
	}
	tok, err := a.refreshToken(ctx, c)
	if err != nil {
		return nil, err
	}
	nc := *c
	nc.AccessToken = tok.AccessToken
	if got := strings.TrimSpace(tok.RefreshToken); got != "" {
		nc.RefreshToken = got
	}
	nc.ExpiresAt = tok.expiry()
	return &nc, nil
}

// usableToken 取一个现在就能用的 access token。
//
// 三种情况：
//   - 只有 refresh token（比如刚完成授权、或上层清了 access token）→ 先换一次；
//   - access token 快过期（凭证里记了 ExpiresAt）且有 refresh token → 先换，省一次
//     注定 401 的往返；换取失败**不报错**，继续用手里的 access token 试一次 ——
//     本机时钟偏差不该让一个上游其实还认的令牌被我们自己拒掉；
//   - 两个都没有 → 明确报 SessionDead，让面板提示「重新授权」。
func (a *Adapter) usableToken(ctx context.Context, c *channel.Credential) (string, error) {
	if c == nil {
		return "", errs.New(errs.SessionDead, "Anthropic 凭证为空：请在面板重新授权").
			WithChannel(string(channel.Anthropic))
	}
	access := strings.TrimSpace(c.AccessToken)
	refresh := strings.TrimSpace(c.RefreshToken)

	if access == "" {
		if refresh == "" {
			return "", errs.New(errs.SessionDead, "Anthropic 凭证为空：请在面板重新授权").
				WithChannel(string(channel.Anthropic))
		}
		tok, err := a.refreshToken(ctx, c)
		if err != nil {
			return "", err
		}
		return tok.AccessToken, nil
	}
	if refresh != "" && !c.ExpiresAt.IsZero() && time.Now().Add(refreshBuffer).After(c.ExpiresAt) {
		if tok, err := a.refreshToken(ctx, c); err == nil {
			return tok.AccessToken, nil
		}
		// 刷不动就照用旧的：让上游自己表态（见函数头第 2 点）。
	}
	return access, nil
}

// ---------------------------------------------------------------------------
// 目录与账务
// ---------------------------------------------------------------------------

// Models 拉上游模型目录（GET /v1/models）。
//
// 顺带承担「鉴权自检」：/v1/models 是**要鉴权的**，令牌无效会 401 —— 面板的连通性测试
// 因此能真的验一把凭证，而不是只看格式对不对。
//
// 回退策略（红线一：不做会撒谎的目录，也不把上游故障说成凭证坏了）：
//   - 401/403 → 明确报错（这是凭证的判决，必须让用户看到）；
//   - 其它错误（5xx/超时/解析失败）→ 回落本地清单，但来源标 SourceLocal（本地清单 ≠ 上游确认）。
func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	token, err := a.usableToken(ctx, c)
	if err != nil {
		return nil, err
	}
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, a.apiBase+epModels+"?limit=1000", nil)
	if rerr != nil {
		return fallbackModels(), nil
	}
	a.setAPIAuth(req, token)
	resp, derr := a.clientFor(c).Do(req)
	if derr != nil {
		return fallbackModels(), nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		if k := a.Classify(resp.StatusCode, raw); k == errs.SessionDead {
			return nil, errs.New(k, "模型目录拒绝了这枚凭证").
				WithChannel(string(channel.Anthropic)).WithAccount(uidOf(c)).
				WithUpstream(truncate(string(raw), 200))
		}
		return fallbackModels(), nil
	}
	var out struct {
		Data []struct {
			ID        string `json:"id"`
			Name      string `json:"display_name"`
			MaxInput  int    `json:"max_input_tokens"`
			MaxOutput int    `json:"max_tokens"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Data) == 0 {
		return fallbackModels(), nil
	}
	models := make([]channel.ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		models = append(models, channel.ModelInfo{
			ID:            m.ID,
			DisplayName:   m.Name,
			ContextWindow: m.MaxInput, // 上游不给就是 0（未知），不猜数字
			Tools:         channel.CapYes,
			Reasoning:     channel.CapYes, // 模型本身会思考；本条目只描述能力，不代表网关打开了它
			Images:        channel.CapNo,
			Source:        channel.SourceUpstream,
		})
	}
	if len(models) == 0 {
		return fallbackModels(), nil
	}
	return models, nil
}

// fallbackModels 用内置清单顶一下（来源标 SourceLocal，不冒充上游）。
func fallbackModels() []channel.ModelInfo {
	out := make([]channel.ModelInfo, 0, len(defaultModels))
	for _, m := range defaultModels {
		out = append(out, channel.ModelInfo{
			ID:            m.ID,
			DisplayName:   m.Name,
			ContextWindow: m.Ctx,
			Tools:         channel.CapYes,
			Reasoning:     channel.CapYes,
			Images:        channel.CapNo,
			Source:        channel.SourceLocal, // 内置清单 ≠ 上游确认
		})
	}
	return out
}

// checkCredential 当场验一次凭证是否真的能推理（登录/导入后立刻调用）。
//
// 为什么必须验：把一个已失效的令牌塞进池子，用户看到的是「装上了但一调用就报错」，
// 分不清是自己授权错了还是渠道坏了。这里直接拿 /v1/models 问一句（它要鉴权），
// 错了当场说清楚 —— 与 kimi/chatglm 的 checkCredential 同一个约定。
func (a *Adapter) checkCredential(ctx context.Context, c *channel.Credential) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.apiBase+epModels+"?limit=1", nil)
	if err != nil {
		return errs.New(errs.Transport, "构造校验请求失败").
			WithChannel(string(channel.Anthropic)).WithCause(err)
	}
	a.setAPIAuth(req, strings.TrimSpace(c.AccessToken))
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return errs.New(errs.Transport, "凭证校验失败（连不上上游）").
			WithChannel(string(channel.Anthropic)).WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return errs.New(a.Classify(resp.StatusCode, raw), "凭证校验失败").
			WithChannel(string(channel.Anthropic)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return nil
}

// Balance 余额未知：订阅是按额度/限速而非余额计量，上游也没有统一的余额接口，
// 不猜数字（F4.2）—— 拿限速当余额展示，用户会以为「省着点用就能多用」。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin 无签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "订阅渠道没有签到活动"}, nil
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// Chat 发一次对话。channel 契约（OpenAI 形态）→ Anthropic Messages 请求体；
// 上游 SSE 事件流 → 标准 chunk 流（换算在 body.go / stream.go）。
//
// 401 的处理是本方法唯一有状态的地方：access token 过期是**常态**（订阅令牌寿命短），
// 撞到 401 就用 refresh token 换一次再试，**只重试一次** —— 重试更多次只会把
// 一个已经失效的 refresh token 反复送去上游，看起来像在被暴力尝试。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	token, err := a.usableToken(ctx, c)
	if err != nil {
		return nil, err
	}
	body := buildBody(req)

	resp, err := a.doMessages(ctx, c, token, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 && c != nil && strings.TrimSpace(c.RefreshToken) != "" {
		resp.Body.Close()
		tok, rerr := a.refreshToken(ctx, c)
		if rerr != nil {
			return nil, rerr // 刷新失败的原因比「401」更具体，照实上报
		}
		if resp, err = a.doMessages(ctx, c, tok.AccessToken, body); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.Anthropic)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, resp.Header.Get("Content-Type"), channel.Anthropic, req.Model), nil
}

// doMessages 发一次 /v1/messages 请求，返回原始响应（错误分类交给调用方）。
func (a *Adapter) doMessages(ctx context.Context, c *channel.Credential, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiBase+epMessages, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").
			WithChannel(string(channel.Anthropic)).WithCause(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	a.setAPIAuth(req, token)
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").
			WithChannel(string(channel.Anthropic)).WithAccount(uidOf(c)).WithCause(err)
	}
	return resp, nil
}

// setAPIAuth 设置订阅式鉴权与版本头。
//
// 与旧版（按 sk-ant-oat 前缀在两套认证间自动切换）不同：本渠道**只用订阅令牌**，
// 一律 Bearer + oauth beta。这一点没有「看情况」的余地 —— 官方把订阅令牌与 API key
// 当成两套认证，送错头只会得到 401，且报文不会告诉你是认证方式错了。
//
// beta 头是**两个值**（cliBeta + oauthBeta），顺序照官方 CLI：它先 push claude-code
// beta 再 push oauth beta（claude-code-cli/utils/betas.ts:241,252）。只带 oauth
// 是「少了真实客户端固定会带的一项」，见 constants.go 的 cliBeta 说明。
func (a *Adapter) setAPIAuth(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("anthropic-beta", cliBeta+","+oauthBeta)
	// 伪装成官方 CLI：参考实现里真实 Claude Code 都带这几个头（见 constants.go）。
	req.Header.Set("User-Agent", cliUA)
	req.Header.Set("x-app", "cli")
	// 官方 CLI 的 getAnthropicClient 传 dangerouslyAllowBrowser: true
	// （claude-code-cli/services/api/client.ts:145），@anthropic-ai/sdk 据此自动注入
	// 这个头；auth2api 绕过 SDK 后手工补上它（src/upstream/anthropic-api.ts:147）。
	// 也就是说真实订阅令牌请求一定带它 —— 属于「照抄真实客户端」而不是额外发明。
	req.Header.Set("anthropic-dangerous-direct-browser-access", "true")
}

// postJSON 发一个 JSON 请求（令牌端点用），返回状态码与原始报文。
func (a *Adapter) postJSON(ctx context.Context, c *channel.Credential, urlStr string, payload any, out any) (int, []byte, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode, raw, nil
}

// Classify 把 Anthropic 的错误归一成有限枚举（D4）。
//
// 403 要分两种（只有报文能区分，也是唯一能区分的地方）：
//   - 权限/密钥类（permission_error…）→ SessionDead：这枚凭证确实不能用了；
//   - 其它（Cloudflare / WAF / 出口 IP 被拦）→ UpstreamFault：**不是账号的问题**，
//     判死会让用户以为凭证坏了去重新授权，而真正该做的是换出口。
//
// 关键词表里还有一条 `oauth token has been revoked`：官方 CLI 的 withOAuth401Retry
// 把**403 + 该报文**也当成认证失败（强制刷新后重试一次，claude-code-cli/utils/http.ts:112-129），
// 说明上游对「令牌被撤销」有时用 403 而不是 401 表达。漏了它，被撤销的令牌会被判成
// 「上游故障」而继续留在池子里，每个请求都白跑一趟。
//
// 实测（2026-09-26，本机机房 IP）官方返回的就是后一种，报文是：
//
//	{"error":{"type":"forbidden","message":"Request not allowed"}}
//
// 所以关键词只能认 `permission_error` 这类**类型名**，不能认 "forbidden"/"not allowed"
// 这种泛词 —— 认泛词会把「出口 IP 被拦」判成「凭证坏了」，正好判反。
//
// 429 同样有两副面孔：限流（等一会儿）与额度耗尽（等多久都没用），靠关键词区分。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == 401:
		return errs.SessionDead // authentication_error：令牌无效/过期（刷新也救不回来时）
	case status == 403:
		for _, kw := range []string{"permission_error", "does not have permission", "not authorized", "oauth authentication", "oauth token has been revoked"} {
			if strings.Contains(s, kw) {
				return errs.SessionDead
			}
		}
		return errs.UpstreamFault
	case status == 429:
		for _, kw := range []string{"quota", "credit", "balance", "billing", "exhaust"} {
			if strings.Contains(s, kw) {
				return errs.HardCredit
			}
		}
		return errs.SoftRate
	case status == 400 || status == 422:
		switch {
		case strings.Contains(s, "credit"), strings.Contains(s, "balance"), strings.Contains(s, "billing"):
			return errs.HardCredit
		case strings.Contains(s, "prompt is too long"), strings.Contains(s, "context"),
			strings.Contains(s, "too long"), strings.Contains(s, "max_tokens"):
			return errs.PromptTooLong
		case strings.Contains(s, "model"), strings.Contains(s, "not found"):
			return errs.ModelUnavailable
		}
		return errs.Parse
	case status == 413:
		return errs.PromptTooLong // request_too_large
	case status == 404:
		// not_found_error：多数是模型名写错，或 base_url 少了 /v1。
		return errs.ModelUnavailable
	case status >= 500:
		return errs.UpstreamFault // 500 api_error / 529 overloaded_error 都算上游故障
	}
	return errs.Parse
}

func uidOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.UID)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
