package copilot

// client.go 渠道实现：能力声明、令牌链路（githubToken → copilotToken）、对话、错误归一。
//
// 令牌链路是本渠道唯一有点绕的地方，说明一次：
//
//	设备码授权 ──► githubToken（长期、不轮换，存进凭证的 AccessToken）
//	                └─► copilotToken（短期，约 30 分钟，只用来打 api.githubcopilot.com）
//
// copilotToken 每次调用都去换一遍太浪费（多一次 RTT + 多是无效调用），所以我们按
// refresh_in 在进程内缓存并按需提前刷新（见 copilotToken / exchangeCopilotToken）。
// 缓存 key 用 githubToken 而不是账号 UID：重新登录后 githubToken 会变，旧 key 自然作废。

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

// Adapter 实现 channel.Channel（登录走 Authorizer，见 login.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	// 三个端点基址，默认取 constants.go 的常量；测试注入 httptest 地址时只改这里。
	githubBase    string
	githubAPIBase string
	copilotBase   string

	tokMu sync.Mutex
	// tokens 缓存 githubToken → copilotToken（含下次刷新时刻）。
	tokens map[string]cachedCopilotToken
}

type cachedCopilotToken struct {
	token     string
	refreshAt time.Time
}

// 编译期断言：本适配器必须满足渠道契约与「能发起面板授权」的契约。
var (
	_ channel.Channel    = (*Adapter)(nil)
	_ channel.Authorizer = (*Adapter)(nil)
)

func New() *Adapter {
	return &Adapter{
		clients:       map[string]*http.Client{},
		githubBase:    githubBase,
		githubAPIBase: githubAPIBase,
		copilotBase:   copilotBase,
		tokens:        map[string]cachedCopilotToken{},
	}
}

// clientFor 按凭证（出口）与超时拿一个 HTTP 客户端 —— 「一账号一出口」的落点。
//
// key 里带超时：对话要 10 分钟，而登录/换令牌这类小请求卡住 10 分钟没有意义，
// 两者不能共用同一个 client（Client.Timeout 是整请求级的）。
func (a *Adapter) clientFor(c *channel.Credential, timeout time.Duration) *http.Client {
	key := channel.EgressOf(c) + "|" + timeout.String()
	a.mu.Lock()
	defer a.mu.Unlock()
	if cl, ok := a.clients[key]; ok {
		return cl
	}
	cl := channel.NewHTTPClient(c, timeout)
	a.clients[key] = cl
	return cl
}

func (a *Adapter) Kind() channel.Kind { return channel.Copilot }

// Spec 能力声明。**说实话**是硬要求：
//
//   - Tools=true / ToolsShim=false：上游就是 OpenAI 协议，工具调用是原生的（参考实现直接
//     透传 tools 与 tool_calls）。声明成 shim 是撒相反方向的谎，会让网关白白把工具定义
//     翻成提示词，还破坏上游原生的并行工具调用。
//   - Reasoning=false：上游**没有**独立的思考字段（响应里没有 reasoning_content 之类的约定），
//     参考实现也没有处理它 —— 没有就不声明，不猜。
//   - Images=false：Copilot 的个别模型支持视觉，但本适配器的 ChatRequest 只承载文本消息
//     （channel.Message.Content 是字符串），声明 true 会误导客户端发图然后失败。
//   - SSEOnly=true：我们统一向上游要 stream，非流式由本地聚合。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:        channel.Copilot,
		DisplayName: "GitHub Copilot",
		Status:      channel.Active,
		// 编程助手 / IDE 类：订阅额度池，设备码授权后按额度用（与聊天平台类分开）。
		Category:              channel.CategoryCoding,
		Tools:                 true,
		ToolsShim:             false,
		Images:                false,
		Reasoning:             false,
		SSEOnly:               true,
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs: "GitHub 设备码登录；上游即标准 OpenAI 协议（api.githubcopilot.com），" +
			"请求伪装成 VS Code 的 Copilot 插件",
	}
}

// Login 是 channel.Channel 要求的「一次性登录」入口。
//
// Copilot 的登录是**交互式**的（设备码要在浏览器里确认），所以走 channel.Authorizer 那条路
// （面板「添加账号 → GitHub Copilot」）；这里明确报错，别让调用方以为可以非交互登录。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"GitHub Copilot 需要交互式授权：请在面板「账号」页点「添加账号 → GitHub Copilot」，"+
			"按提示在 GitHub 授权页输入设备码").WithChannel(string(channel.Copilot))
}

// ---------------------------------------------------------------------------
// 令牌链路
// ---------------------------------------------------------------------------

// copilotToken 取一个可用的 copilotToken：缓存里没过期就直接用，否则去上游换一个。
func (a *Adapter) copilotToken(ctx context.Context, c *channel.Credential) (string, error) {
	gh := strings.TrimSpace(tokenOf(c))
	if gh == "" {
		return "", errs.New(errs.SessionDead,
			"GitHub Copilot 凭证为空：请在面板里重新登录一次").WithChannel(string(channel.Copilot))
	}
	a.tokMu.Lock()
	v, ok := a.tokens[gh]
	a.tokMu.Unlock()
	if ok && time.Now().Before(v.refreshAt) {
		return v.token, nil
	}
	return a.exchangeCopilotToken(ctx, c, gh)
}

// forgetToken 丢掉某个 githubToken 的缓存（401 之后必须丢，否则会一直用死 token 重试）。
func (a *Adapter) forgetToken(githubToken string) {
	a.tokMu.Lock()
	delete(a.tokens, strings.TrimSpace(githubToken))
	a.tokMu.Unlock()
}

// exchangeCopilotToken 用 githubToken 换 copilotToken，并按 refresh_in 记下刷新时刻。
//
// 响应形如 {token, expires_at, refresh_in}。我们**只信 refresh_in**（秒，距下次该刷新还有多久）：
// expires_at 的类型在两份参考实现里就不一致（copilot-api 当数字、gpt4free 当 ISO 字符串），
// 去兼容一个各家都说不准的字段不如直接用上游明确给的 refresh_in（任务要求也是这么说的）。
//
// 交叉验证（2026-09，任务补充）：更近的 BYOKEY 反而用 expires_at（当 unix 秒整数）算 TTL，
// 且**不减提前量**、只在缺失时回落到 25 分钟（crates/provider/src/executor/copilot/mod.rs:230-242，
// 与我们的 copilotTokenFallback 巧合一致）。三家对 expires_at 的类型都不统一，正好印证了
// 「别去信它」这个判断；提前量仍按 copilot-api 的 refresh_in - 60 秒。
//
// 另：BYOKEY 还会读响应里的 `endpoints.api` 并**用它覆盖 API 基址**（mod.rs:244-250，VS Code 就是
// 这么做的）。我们与 copilot-api/gpt4free 一样按账号类型拼 api.<type>.githubcopilot.com，
// 没有采信该字段 —— 后果是只有 business/enterprise 账号可能打到错误 host（个人版两个域等价）。
// 真上游样本到位后若发现端点不对，这里就是落点。
func (a *Adapter) exchangeCopilotToken(ctx context.Context, c *channel.Credential, githubToken string) (string, error) {
	status, raw, err := a.do(ctx, c, http.MethodGet, a.githubAPIBase+epCopilotToken, githubHeaders(githubToken), nil)
	if err != nil {
		return "", errs.New(errs.Transport, "换取 Copilot token 失败").WithChannel(string(channel.Copilot)).
			WithAccount(uidOf(c)).WithCause(err)
	}
	if status >= 400 {
		// 401/403 会被 Classify 归成 SessionDead（githubToken 失效）或 HardCredit（没有 Copilot 权益）。
		return "", errs.New(a.Classify(status, raw),
			"换取 Copilot token 失败：GitHub 拒绝了这个 token，或该账号未开通 Copilot").
			WithChannel(string(channel.Copilot)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	var out struct {
		Token     string `json:"token"`
		RefreshIn int    `json:"refresh_in"`
	}
	_ = json.Unmarshal(raw, &out)
	token := strings.TrimSpace(out.Token)
	if token == "" {
		// 200 却没有 token：绝不能当成功（否则后面每个请求都 401，看起来像别的问题）。
		return "", errs.New(errs.SessionDead,
			"上游没有返回 Copilot token：GitHub 授权可能已失效，请重新登录").
			WithChannel(string(channel.Copilot)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}

	ttl := time.Duration(out.RefreshIn) * time.Second
	if ttl <= 0 {
		ttl = copilotTokenFallback
	}
	now := time.Now()
	refreshAt := now.Add(ttl - refreshLead)
	if !refreshAt.After(now) {
		// 上游给了一个比 refreshLead 还小的 refresh_in（不常见，但别因此每个请求都换一次 token）。
		refreshAt = now.Add(ttl / 2)
	}
	a.tokMu.Lock()
	a.tokens[githubToken] = cachedCopilotToken{token: token, refreshAt: refreshAt}
	a.tokMu.Unlock()
	return token, nil
}

// buildCredential 用 githubToken 组装凭证，并**当场验一次**。
//
// 为什么当场验：只验「token 有效」不够 —— 一个没开通 Copilot 的 GitHub 号也能通过设备码授权。
// 换一次 copilotToken 同时回答了「token 有效吗」和「这个号能用 Copilot 吗」。
// 不验的后果是用户看到「装上了，一对话就报错」，分不清是自己哪一步做错了还是渠道坏了。
func (a *Adapter) buildCredential(ctx context.Context, githubToken string) (*channel.Credential, error) {
	if _, err := a.exchangeCopilotToken(ctx, nil, githubToken); err != nil {
		return nil, err
	}
	// 拿登录名做昵称与稳定 UID；这一步失败不影响凭证可用性，只是少个好看的名字。
	login := a.githubLogin(ctx, githubToken)
	uid := "gh-" + shortHash(githubToken)
	nick := "GitHub Copilot"
	if login != "" {
		uid = "gh-" + login
		nick = "Copilot · " + login
	}
	return &channel.Credential{
		UID:      uid,
		Nickname: nick,
		// githubToken 长期有效、不发散，存 AccessToken；本渠道没有 refresh token（设备码流程不给）。
		AccessToken: githubToken,
		Extra: map[string]string{
			"account_type": accountIndividual,
			"github_login": login,
		},
	}, nil
}

// githubLogin 读 GitHub 登录名（GET /user）。失败返回空串 —— 非致命信息，不值得让登录失败。
func (a *Adapter) githubLogin(ctx context.Context, githubToken string) string {
	status, raw, err := a.do(ctx, nil, http.MethodGet, a.githubAPIBase+epUser, githubHeaders(githubToken), nil)
	if err != nil || status >= 400 {
		return ""
	}
	var out struct {
		Login string `json:"login"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return ""
	}
	return strings.TrimSpace(out.Login)
}

// Refresh 是账号级续期。
//
// Copilot 的 githubToken 不会过期、也不轮换，所以这里没有「换新凭证」这回事；但**验证**仍然是
// 有意义的：重新换一次 copilotToken 就能确认「这个号现在还能不能用」（token 被吊销 / 订阅到期
// 都会在这一步暴露）。因此续期成功返回原凭证的副本，失败返回错误（让上层按 SessionDead 处理），
// 而不是像纯 key 式来源那样返回 (nil, nil) 假装「刷不了」—— 我们确实有可以重新验证的东西。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	gh := strings.TrimSpace(tokenOf(c))
	if gh == "" {
		return nil, nil // 没有凭证可续：交给上层按 401 处理
	}
	if _, err := a.exchangeCopilotToken(ctx, c, gh); err != nil {
		return nil, err
	}
	nc := *c
	return &nc, nil
}

// ---------------------------------------------------------------------------
// 目录与账务
// ---------------------------------------------------------------------------

// Models 拉上游的模型目录（GET {base}/models），如实标注每一项的来源与能力位。
//
// 这里是**动态**目录（任务要求）：Copilot 的可用模型随订阅等级变化，写死会下发一堆用户其实
// 没有权限的模型，或者漏掉新上线的。拿不到就报错，不静默给空列表（红线一）。
func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	token, err := a.copilotToken(ctx, c)
	if err != nil {
		return nil, err
	}
	status, raw, err := a.do(ctx, c, http.MethodGet, a.copilotBaseURL(c)+"/models", copilotHeaders(token), nil)
	if err != nil {
		return nil, errs.New(errs.Transport, "拉取模型目录失败").WithChannel(string(channel.Copilot)).
			WithAccount(uidOf(c)).WithCause(err)
	}
	if status >= 400 {
		return nil, errs.New(a.Classify(status, raw), "拉取模型目录失败").
			WithChannel(string(channel.Copilot)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	var out modelsResp
	if json.Unmarshal(raw, &out) != nil {
		return nil, errs.New(errs.Parse, "模型目录不是合法 JSON").
			WithChannel(string(channel.Copilot)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	list := make([]channel.ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		id := strings.TrimSpace(m.ID)
		// 上游把非对话模型（embeddings 等）也列在同一张表里；下发给客户端会得到一个
		// 「点了就报错」的模型，所以按上游自己给的 model_picker_enabled 过滤。
		// 字段缺失时**不过滤**（不拿默认值当事实）。
		if id == "" || (m.ModelPickerEnabled != nil && !*m.ModelPickerEnabled) {
			continue
		}
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = id
		}
		list = append(list, channel.ModelInfo{
			ID:          id,
			DisplayName: name,
			// 上游明确给出上下文窗口；没给就是 0 = 未知，不猜数字（F4.2）。
			ContextWindow: m.Capabilities.Limits.MaxContextWindowTokens,
			Source:        channel.SourceUpstream,
			Tools:         capFromBool(m.Capabilities.Supports.ToolCalls),
			// 上游目录里没有「是否支持图片/思考」的能力位 —— 保持未知，不替它下结论。
			Images:    channel.CapUnknown,
			Reasoning: channel.CapUnknown,
		})
	}
	if len(list) == 0 {
		return nil, errs.New(errs.UpstreamFault, "上游返回了空的模型目录（没有可下发的模型）").
			WithChannel(string(channel.Copilot)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return list, nil
}

// modelsResp 是 /models 的响应（只取我们真正会用的字段）。
type modelsResp struct {
	Data []struct {
		ID                 string `json:"id"`
		Name               string `json:"name"`
		ModelPickerEnabled *bool  `json:"model_picker_enabled"`
		Capabilities       struct {
			Limits struct {
				MaxContextWindowTokens int `json:"max_context_window_tokens"`
			} `json:"limits"`
			Supports struct {
				ToolCalls *bool `json:"tool_calls"`
			} `json:"supports"`
		} `json:"capabilities"`
	} `json:"data"`
}

// Balance 读订阅配额（GET api.github.com/copilot_internal/user）。
//
// 取 premium_interactions（最稀缺的那份额度）的剩余量。**不猜数字**：
// 上游说 unlimited、或没给出配额快照时，Known=false，面板显示「未知」而不是一个假的 0。
func (a *Adapter) Balance(ctx context.Context, c *channel.Credential) (channel.Balance, error) {
	gh := strings.TrimSpace(tokenOf(c))
	if gh == "" {
		return channel.Balance{Known: false}, nil
	}
	status, raw, err := a.do(ctx, c, http.MethodGet, a.githubAPIBase+epUsage, githubHeaders(gh), nil)
	if err != nil {
		return channel.Balance{Known: false}, errs.New(errs.Transport, "查询 Copilot 配额失败").
			WithChannel(string(channel.Copilot)).WithAccount(uidOf(c)).WithCause(err)
	}
	if status >= 400 {
		return channel.Balance{Known: false}, errs.New(a.Classify(status, raw), "查询 Copilot 配额失败").
			WithChannel(string(channel.Copilot)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	var out struct {
		QuotaSnapshots struct {
			Premium struct {
				Remaining int64 `json:"remaining"`
				Unlimited bool  `json:"unlimited"`
			} `json:"premium_interactions"`
		} `json:"quota_snapshots"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return channel.Balance{Known: false}, nil
	}
	if out.QuotaSnapshots.Premium.Unlimited {
		// 无限额度没有「剩余多少」这个数 —— 给 0 会被看成「用完了」。
		return channel.Balance{Known: false}, nil
	}
	return channel.Balance{Credits: out.QuotaSnapshots.Premium.Remaining, Known: true}, nil
}

// Checkin 无签到活动：Copilot 是订阅制，没有「每日签到领额度」这回事。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "GitHub Copilot 是订阅制，没有签到活动"}, nil
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// Chat 发一次对话：换 copilotToken → 带上伪装头 POST /chat/completions → 按 SSE 解析。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	token, err := a.copilotToken(ctx, c)
	if err != nil {
		return nil, err
	}
	body := buildBody(req)

	resp, err := a.sendChat(ctx, c, token, body, req.Messages)
	if err != nil {
		return nil, err
	}
	// 401 = copilotToken 过期（常见）或 githubToken 失效。前者换一次就好，只重试一次。
	// 判断依据不能只看状态码：丢掉缓存、重新走一遍 exchangeCopilotToken，失败它会自己报 SessionDead。
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		a.forgetToken(strings.TrimSpace(tokenOf(c)))
		if token, err = a.copilotToken(ctx, c); err != nil {
			return nil, err
		}
		if resp, err = a.sendChat(ctx, c, token, body, req.Messages); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.Copilot)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	// 上游可能无视 stream:true 直接回整包 JSON —— 交给流解析器按 Content-Type 判断。
	return newStream(resp.Body, resp.Header.Get("Content-Type"), req.Model), nil
}

// sendChat 发一个对话请求，带齐伪装头与 X-Initiator。
func (a *Adapter) sendChat(ctx context.Context, c *channel.Credential, copilotToken string, body []byte, msgs []channel.Message) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.copilotBaseURL(c)+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.Copilot)).WithCause(err)
	}
	for k, v := range copilotHeaders(copilotToken) {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Initiator", initiatorOf(msgs))
	req.Header.Set("Accept", "text/event-stream")

	resp, err := a.clientFor(c, chatTimeout).Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.Copilot)).
			WithAccount(uidOf(c)).WithCause(err)
	}
	return resp, nil
}

// copilotBaseURL 决定本次对话/目录请求打哪个域。
//
//   - 凭证 Extra["copilot_base"] 优先（同来源多账号指向不同网关时用，测试也用它）；
//   - 否则按账号类型：个人版 api.githubcopilot.com，团队/企业版 api.<type>.githubcopilot.com
//     （参考实现的规则）；
//   - 都没有就用默认值（accountIndividual）。
func (a *Adapter) copilotBaseURL(c *channel.Credential) string {
	if v := extraOf(c, "copilot_base"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if at := extraOf(c, "account_type"); at != "" && at != accountIndividual {
		return "https://api." + at + ".githubcopilot.com"
	}
	return a.copilotBase
}

// Classify 把上游错误归一成有限枚举（D4）。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == 401:
		// copilotToken 过期或 githubToken 被吊销 —— 两者都要重新登录（或换一次 token，由调用方做）。
		return errs.SessionDead
	case status == 403:
		// GitHub 的 403 有两副面孔，必须分开：
		//   · 「没有 Copilot 权益 / 组织策略禁止」—— 重新登录多少遍都没用，是账号权益问题，
		//     归 HardCredit（冷却但不禁用，也不会让用户白白重登）；
		//   · 其它 403 —— 身份不被接受，归 SessionDead（提示重新登录）。
		for _, kw := range []string{
			"subscription", "entitlement", "seat", "copilot is not enabled",
			"not enabled for copilot", "no access to copilot", "copilot access",
		} {
			if strings.Contains(s, kw) {
				return errs.HardCredit
			}
		}
		return errs.SessionDead
	case status == 402:
		return errs.HardCredit
	case status == 429:
		// 429 同样有两副面孔：限流（等一会儿就好）与额度耗尽（换号或等结算周期）。
		// 关键词要具体：宽词（如 "limit"）会命中 "rate limited"，把「等一会儿」判成「没钱了」，
		// 正好判反（kimi/qwen 的注释里有同样的教训）。
		for _, kw := range []string{
			"quota", "premium request", "entitlement", "insufficient", "exceeded your monthly",
		} {
			if strings.Contains(s, kw) {
				return errs.HardCredit
			}
		}
		return errs.SoftRate
	case status == 400 || status == 422:
		switch {
		case strings.Contains(s, "context"), strings.Contains(s, "too long"),
			strings.Contains(s, "max_tokens"), strings.Contains(s, "token limit"):
			return errs.PromptTooLong
		case strings.Contains(s, "content_filter"), strings.Contains(s, "content policy"),
			strings.Contains(s, "responsible ai"), strings.Contains(s, "safety"):
			return errs.ContentBlocked
		case strings.Contains(s, "model"), strings.Contains(s, "not found"),
			strings.Contains(s, "unsupported"):
			return errs.ModelUnavailable
		}
		return errs.Parse
	case status == 404:
		// 对话接口的 404 基本是模型名写错；目录接口的 404 会走 Models 里的报错路径。
		return errs.ModelUnavailable
	case status >= 500:
		return errs.UpstreamFault
	}
	return errs.Parse
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// do 发一个简单请求（GET/POST + JSON 体），返回状态码与原始报文。
//
// 语义：只有「连不通/构造失败」才算 Go 层错误；HTTP >= 400 是上游的业务错误，
// 交给调用方按状态码与 body 判读（与 qwen 的 doForm 同一约定）。
func (a *Adapter) do(ctx context.Context, c *channel.Credential, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := a.clientFor(c, apiTimeout).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}
