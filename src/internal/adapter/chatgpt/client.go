package chatgpt

// client.go 渠道实现：能力声明、凭证校验、目录、对话编排、错误归一。
//
// 本渠道的工具调用是 **toolshim 模拟**：ChatGPT 网页协议没有给第三方留 tools 字段，
// 所以 Spec 是 Tools=false + ToolsShim=true —— 网关把工具定义翻成提示词、再把模型
// 输出的 <tool_call> 标记解析回结构化 tool_calls（对客户端来说合同不变）。
// 声明成原生就是撒谎，会让 coding agent 撞上「模型把工具调用写成文本」。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Adapter 实现 channel.Channel；登录相关的接口见 login.go。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	// base 是 API 基址（测试可换）。
	base string
}

func New() *Adapter {
	return &Adapter{
		clients: map[string]*http.Client{},
		base:    apiBase,
	}
}

// clientFor 按凭证取 HTTP 客户端（一账号一出口，见 channel.NewHTTPClient）。
//
// 注意这是**标准库**的 TLS 栈：参考实现为了对抗 chatgpt.com 的 Cloudflare 用了
// curl-impersonate / curl_cffi 做 TLS 指纹伪装。我们这里没有（也不该在适配器里
// 塞一个假 TLS 栈），所以「真上游能不能过」取决于对方是否只按 IP/行为判风险。
// 这一点写在包注释里，属于已知边界，不要靠猜。
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

func (a *Adapter) Kind() channel.Kind { return channel.ChatGPT }

// Spec 能力声明。
//
// 逐项依据：
//   - Tools=false + ToolsShim=true：网页协议没有原生工具调用（与 Kimi/DeepSeek 同一处境）；
//   - Reasoning=true：思考单独分流在 /message/content/thoughts/*，我们映射成 reasoning_content；
//   - Images=false：图片要先上传再转 file-service 指针，本适配器没做，声明支持就是骗客户端；
//   - SSEOnly=true：上游只给 SSE（force_use_sse=true），非流式由本地聚合。
//
// DefaultMinIntervalSec 取 3：网页渠道反爬按账号算，且每个请求前都要多打一次首页 + sentinel，
// 同号猛打是进风控名单最直的路（参考两个实现都在 README 里写明了封号风险）。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.ChatGPT,
		DisplayName:           "ChatGPT（网页版）",
		Status:                channel.Active,
		Category:              channel.CategoryChat,
		Tools:                 false,
		ToolsShim:             true,
		Images:                false,
		Reasoning:             true,
		SSEOnly:               true,
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs: "浏览器登录后粘贴 accessToken；每个请求都要过 sentinel（引导 + 挑战 + PoW），" +
			"需要 turnstile 时由本地 VM 求解；accessToken 过期后无法自动续期，需要重新粘贴；" +
			"思考单独分流；工具调用由网关模拟（toolshim）。" +
			"注意：出口若被 Cloudflare 判风险（表现为引导首页 403 或挑战循环），" +
			"需要在出口层做 TLS 指纹伪装（参考实现用的是 curl-impersonate）",
	}
}

// Login 是「一次性登录」入口：凭证来自用户浏览器里的登录态，必须用户参与。
//
// 面板走 StartLogin + 粘贴（见 login.go），这个方法只在「调用方没走面板」时兜底报错 ——
// 给一个明确指引，而不是返回一个空凭证让后面的请求全都 401。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"ChatGPT 需要你在浏览器登录一次：面板「添加账号 → ChatGPT」里粘贴 accessToken（不需要密码）").
		WithChannel(string(channel.ChatGPT))
}

// Refresh 尝试续期 —— 这里**明确表示刷不了**。
//
// 为什么刷不了：网页端 accessToken 是靠 `__Secure-next-auth.session-token` cookie
// 去 /api/auth/session 换的，而用户只粘了 accessToken 本身，我们手里没有那个 cookie；
// 参考实现里能自动刷的是 **Codex/OAuth 账号**（有 refresh_token + client_id 走
// auth.openai.com/oauth/token），那是另一条接入路径，不是网页版。
//
// 返回 (nil, nil) 是「刷不了」的约定表达：上层据此按 401 处理（提示重新粘贴），
// 而不是重试到天荒地老，也不是假装刷新成功。
func (a *Adapter) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// 目录与账务
// ---------------------------------------------------------------------------

// Models 优先问上游要目录（slug 一直在漂，写死必然过期）。
//
// 上游拿不到时退回内置清单、**不报错**：目录接口抖动不该让一个健康账号在面板上变红，
// 用户要的是「能选模型」。真正的凭证/网络问题会在登录校验与 Chat 时明确报出来。
func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	if list, err := a.upstreamModels(ctx, c); err == nil && len(list) > 0 {
		return list, nil
	}
	out := make([]channel.ModelInfo, 0, len(webModels))
	for _, m := range webModels {
		out = append(out, channel.ModelInfo{
			ID:          m.ID,
			DisplayName: m.Name,
			// 上下文长度上游不给，不猜数字（F4.2）。
			ContextWindow: 0,
			Source:        channel.SourceLocal,
			Tools:         channel.CapYes, // 客户端可用（由网关模拟）
			Images:        channel.CapNo,
			Reasoning:     m.Reason,
		})
	}
	return out, nil
}

// upstreamModels 问上游要一次目录。
func (a *Adapter) upstreamModels(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	fp := fingerprintOf(c)
	// 首页引导是尽力而为：拿不到也照样试目录（目录接口本身不吃 sentinel）。
	_, _ = a.bootstrap(ctx, c, fp)

	resp, err := a.send(ctx, c, http.MethodGet, a.base+epModels, fp, nil,
		map[string]string{"Accept": "application/json"})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, stepErr(a.Classify(resp.StatusCode, raw), "拉取模型目录失败", c, raw)
	}
	var payload struct {
		Models []struct {
			Slug    string `json:"slug"`
			Title   string `json:"title"`
			MaxToks int    `json:"max_tokens"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errs.New(errs.Parse, "模型目录不是可解析的 JSON").
			WithChannel(string(channel.ChatGPT)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	seen := map[string]bool{}
	out := make([]channel.ModelInfo, 0, len(payload.Models))
	for _, m := range payload.Models {
		slug := strings.TrimSpace(m.Slug)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		name := strings.TrimSpace(m.Title)
		if name == "" {
			name = slug
		}
		out = append(out, channel.ModelInfo{
			ID:          slug,
			DisplayName: name,
			// 上游的 max_tokens 是「单次上限」不是窗口大小，当成窗口会误导用户，
			// 所以这里仍然不填（Source=Upstream 只标「ID 来自上游」）。
			ContextWindow: 0,
			Source:        channel.SourceUpstream,
			Tools:         channel.CapYes, // 网关模拟
			Images:        channel.CapNo,
			Reasoning:     channel.CapUnknown, // 哪个 slug 会思考由上游决定，不猜
		})
	}
	if len(out) == 0 {
		return nil, errs.New(errs.Parse, "上游模型目录为空").
			WithChannel(string(channel.ChatGPT)).WithAccount(uidOf(c))
	}
	return out, nil
}

// Balance：网页端没有可信任的余额接口，不猜数字（F4.2）。
//
// /backend-api/me 给的是套餐与限流窗口（plan_type、rate limits），不是一个「余额」；
// 把它折算成 Credits 就是编数字，面板上会显示一个看起来精确、实际无意义的数。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin：ChatGPT 没有签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "ChatGPT 网页版没有签到活动"}, nil
}

// ---------------------------------------------------------------------------
// 凭证与模型档位
// ---------------------------------------------------------------------------

// accessTokenOf 取凭证里的 accessToken。
func accessTokenOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.AccessToken)
}

// modelSpec 是一个模型档位解析后的形态。
type modelSpec struct {
	slug   string // 发给上游的 slug
	effort string // thinking_effort（空 = 不带）
	known  bool   // 是否是本地认得的档位
}

// normalizeModel 解析客户端给的模型名。
//
// ChatGPT 网页端的「不同模型」其实是同一个 slug 配不同的思考强度，
// gpt-5-1/2/3 这三个别名就是思考强度（低/中/高）—— 这是参考实现实测出来的映射，
// 不要按字面理解成「GPT-5 的第 1/2/3 版」。
//
// 认不出的名字**原样透传**（而不是回落到 auto）：客户端可能是从
// /backend-api/models 里选的真 slug，替它改成 auto 会让它拿到一个「不是它要的模型」
// 却看不出原因。透传的最坏结果是上游报模型不存在 —— 那是准确的报错。
func normalizeModel(id string) modelSpec {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "":
		return modelSpec{slug: defaultModel, known: true}
	case "auto":
		return modelSpec{slug: "auto", known: true}
	case "gpt-5-1", "gpt-5-mini":
		return modelSpec{slug: "gpt-5", effort: "low", known: true}
	case "gpt-5-2":
		return modelSpec{slug: "gpt-5", effort: "medium", known: true}
	case "gpt-5-3", "gpt-5-3-mini":
		return modelSpec{slug: "gpt-5", effort: "high", known: true}
	default:
		return modelSpec{slug: strings.TrimSpace(id)}
	}
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// Chat 发一次对话：先过 sentinel 三道门，再带着挑战头打 SSE。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	token := accessTokenOf(c)
	if token == "" {
		return nil, errs.New(errs.SessionDead,
			"ChatGPT 凭证为空：请在面板里重新粘贴 accessToken").
			WithChannel(string(channel.ChatGPT))
	}
	fp := fingerprintOf(c)
	// 未知 slug 也照发，但要让上游的报错说清楚是模型名的问题。
	spec := normalizeModel(req.Model)

	cr, err := a.sentinelFlow(ctx, c, fp)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(buildConversationPayload(req, spec))
	if err != nil {
		return nil, errs.New(errs.Parse, "构造对话请求体失败").
			WithChannel(string(channel.ChatGPT)).WithCause(err)
	}
	resp, err := a.send(ctx, c, http.MethodPost, a.base+epConversation, fp, body, conversationHeaders(cr))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		e := stepErr(a.Classify(resp.StatusCode, raw), "对话请求被上游拒绝", c, raw)
		if resp.StatusCode == 401 {
			// 这里是本渠道唯一「用户必须动手」的失败，说清楚该做什么。
			e.WithMessage("ChatGPT 的 accessToken 已失效（本渠道没有 refresh token，无法自动续期）：" +
				"请在浏览器重新登录 chatgpt.com 并重新粘贴 accessToken")
		}
		return nil, e
	}
	return newStream(resp.Body, req.Model), nil
}

// checkCredential 用 /backend-api/me 确认「这串 accessToken 现在还能用吗」。
//
// 登录时当场验一次：把一个已失效的 token 塞进池子，表现是「装上就 401」，
// 用户分不清是自己粘错了还是渠道坏了。
func (a *Adapter) checkCredential(ctx context.Context, c *channel.Credential) error {
	fp := fingerprintOf(c)
	resp, err := a.send(ctx, c, http.MethodGet, a.base+epMe, fp, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return stepErr(a.Classify(resp.StatusCode, raw), "凭证校验失败（accessToken 可能已失效）", c, raw)
	}
	return nil
}

// Classify 把上游错误归一成有限枚举（D4）。
//
// 403 的处理是本渠道最需要想清楚的一处：它既可能是「accessToken 不被接受」，
// 也可能是「人机校验没通过/风控拦了」。前者该判 SessionDead（禁用账号、要求重登），
// 后者判 SessionDead 就是**误杀好号**（策略表里 SessionDead 是 Disable，不是冷却）。
// 所以只能看报文：带 verification/challenge/turnstile/arkose 这类词的按上游故障处理。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == 401:
		return errs.SessionDead
	case status == 403:
		for _, kw := range []string{
			"verification", "cloudflare", "challenge", "captcha", "turnstile", "arkose",
			"unusual activity", "risk", "人机",
		} {
			if strings.Contains(s, kw) {
				return errs.UpstreamFault
			}
		}
		return errs.SessionDead
	case status == 429:
		// 「限流」与「额度耗尽」上游都用 429，只能靠报文区分。
		// 不要拿 "limit" 当关键词：限流的原话里也常带 limit（"rate limited"），
		// 认它就会把「等一会儿就好」判成「等多久都没用」，正好判反。
		for _, kw := range []string{
			"quota", "credit", "balance", "exhaust", "insufficient",
			"usage limit", "limit reached", "额度",
		} {
			if strings.Contains(s, kw) {
				return errs.HardCredit
			}
		}
		return errs.SoftRate
	case status == 400 || status == 422:
		switch {
		case strings.Contains(s, "context"), strings.Contains(s, "too long"),
			strings.Contains(s, "maximum length"):
			return errs.PromptTooLong
		case strings.Contains(s, "moderation"), strings.Contains(s, "sensitive"),
			strings.Contains(s, "policy"), strings.Contains(s, "content"):
			return errs.ContentBlocked
		case strings.Contains(s, "model"):
			return errs.ModelUnavailable
		}
		return errs.Parse
	case status == 404:
		return errs.ModelUnavailable
	case status >= 500:
		return errs.UpstreamFault
	}
	return errs.Parse
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// send 发一个 API 请求（XHR 形态头 + 账号指纹 + Bearer）。
func (a *Adapter) send(
	ctx context.Context,
	c *channel.Credential,
	method, rawURL string,
	fp fingerprint,
	body []byte,
	extra map[string]string,
) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").
			WithChannel(string(channel.ChatGPT)).WithCause(err)
	}
	req.Header = apiHeaders(targetPath(rawURL), fp)
	if tok := accessTokenOf(c); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").
			WithChannel(string(channel.ChatGPT)).WithAccount(uidOf(c)).WithCause(err)
	}
	return resp, nil
}

// sendDocument 发导航类请求（首页引导）：头必须换成「浏览器打开页面」那一套。
func (a *Adapter) sendDocument(ctx context.Context, c *channel.Credential, rawURL string, fp fingerprint) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errs.New(errs.Transport, "构造引导请求失败").
			WithChannel(string(channel.ChatGPT)).WithCause(err)
	}
	for k, v := range documentHeaders(fp) {
		req.Header.Set(k, v)
	}
	if tok := accessTokenOf(c); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "引导首页请求失败").
			WithChannel(string(channel.ChatGPT)).WithAccount(uidOf(c)).WithCause(err)
	}
	return resp, nil
}

// targetPath 从完整 URL 里取出 X-OpenAI-Target-Path 要的值（**不含 query**）。
//
// 带上 query（如 ?history_and_training_disabled=false）会被上游判为路径不匹配：
// 参考实现是 strings.Split(path, "?")[0]，我们也这么做。
func targetPath(rawURL string) string {
	path := rawURL
	if i := strings.Index(path, "://"); i >= 0 {
		rest := path[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			path = rest[j:]
		} else {
			path = "/"
		}
	}
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
	if path == "" {
		path = "/"
	}
	return path
}
