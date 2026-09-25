package kiro

// client.go 渠道实现：能力声明、换令牌（两种鉴权模式）、对话编排、错误归一。
//
// 本渠道的工具调用是**原生**的（Spec.Tools=true）：工具定义直接放进
// userInputMessageContext.tools，上游按 toolUseEvent 流式回工具调用，
// 不需要 internal/toolshim 那层模拟。
//
// 鉴权按凭据里有没有 clientId+clientSecret 自动分流（见 constants.go 的文件头）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// ssoUIDPrefix 标记「这条凭据当初是 AWS SSO（企业版）签发的」。
//
// 为什么要把它写进 UID：凭据落盘的 schema 只保留少数几个渠道附加字段
// （见 store/creds.go 的 parseCredential），clientId/clientSecret **不在其中** ——
// 进程重启后它们会消失。把「本来是哪种鉴权」记在一定会被保留的 UID 上，
// 重启后就能给出「请重新粘贴三件套」这种可操作的错误，
// 而不是拿企业版 refreshToken 去桌面端点换、然后让用户看一个没头没脑的 400。
const ssoUIDPrefix = "kiro-sso-"

// isSSOUID 判断这条凭据当初是不是企业版（AWS SSO）签发的。
func isSSOUID(uid string) bool { return strings.HasPrefix(strings.TrimSpace(uid), ssoUIDPrefix) }

// Adapter 实现 channel.Channel（登录走 Authorizer + CallbackAcceptor，见 login.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	// 三个主机模板字段化，便于测试指向 mock server（生产用 constants.go 的模板）。
	authBase string // 桌面换令牌
	oidcBase string // 企业（AWS SSO OIDC）换令牌
	apiBase  string // 对话

	tokMu sync.Mutex
	// tokens 缓存「凭证 → access token」。key 取 UID：UID 由 refresh token 派生，
	// 同一账号重新登录会得到新 UID，旧缓存自然作废（与 kimi 的取键理由相同）。
	// value 里同时留着**最近一次换回来的 refresh token**：上游会轮换它，
	// 而凭证本身是面板写盘的对象、我们改不了，所以进程内要自己记住最新的那个。
	tokens map[string]cachedToken
}

type cachedToken struct {
	access  string
	refresh string
	exp     time.Time
}

func New() *Adapter {
	return &Adapter{
		clients:  map[string]*http.Client{},
		authBase: tmplAuthDesktop,
		oidcBase: tmplSSOOIDC,
		apiBase:  tmplRuntime,
		tokens:   map[string]cachedToken{},
	}
}

// 编译期断言：本渠道必须满足核心层的接口契约。
// 值这么写（而不是靠 boot.go 的注册行）是为了让「接口改了」在**本包**就编译失败 ——
// 注册行是外部加的，那边报错信息看不出是哪一个方法对不上。
var (
	_ channel.Channel          = (*Adapter)(nil)
	_ channel.Authorizer       = (*Adapter)(nil)
	_ channel.LoginSession     = (*session)(nil)
	_ channel.CallbackAcceptor = (*session)(nil)
)

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

func (a *Adapter) Kind() channel.Kind { return channel.Kiro }

// Spec 能力声明。
//
// Tools=true 且 ToolsShim=false：上游**原生**支持工具调用（toolSpecification / toolUseEvent），
// 声明成 shim 会白白多走一层提示词模拟，反而更差。
//
// Reasoning=false 是有意为之：上游**没有思考字段**。参考实现靠往提示词里注入
// <thinking_mode>…</thinking_mode> 标签、再从正文里把 <thinking>…</thinking> 抠出来，
// 变出一个「假思考」（它自己的原话就是 fake reasoning）。我们不做这件事：
// 那会改动用户可见的正文，而且一旦模型不服管教，抠出来的东西就混进正文里。
// 能力位说实话 —— 上游没有的字段就不声明。
//
// Category=coding：Kiro 是订阅额度池的 IDE 类（与 QwenWork「千问办公」那类聊天产品分开），
// 用户拿「额度」理解它的限速才不会拧。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.Kiro,
		DisplayName:           "AWS Kiro（Amazon Q Developer）",
		Status:                channel.Active,
		Category:              channel.CategoryCoding,
		Tools:                 true,
		ToolsShim:             false,
		Images:                false, // 见下方 Models 的说明：内部消息契约里没有图片部件
		Reasoning:             false, // 上游没有思考字段（见上面的说明）
		SSEOnly:               true,  // 上游只会流式返回，非流式由网关聚合
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs: "粘贴 refreshToken（桌面版）或 clientId/clientSecret/refreshToken 三件套（企业版 AWS SSO）；" +
			"上游是 AWS Event Stream 二进制协议，原生工具调用",
	}
}

// Login 是「一次性登录」入口：Kiro 的凭据来自你本机的凭据文件，必须用户参与。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"Kiro 需要你粘一次凭据：面板「添加账号 → Kiro」里粘贴 refreshToken（企业版再带上 clientId/clientSecret）").
		WithChannel(string(channel.Kiro))
}

// Models 返回已知可用档位（本地清单）。
//
// 上游不提供目录：runtime.{region}.kiro.dev 上没有 /ListAvailableModels
// （参考实现专门判断「是不是 runtime 端点，是就直接用静态表」）。所以这里是
// 「已知可用档位」而不是「上游当场给出的目录」，Source 如实标 local。
//
// ContextWindow 留 0（未知）：上游不给窗口大小。参考实现统一按 200k 估算，
// 那是它自己记账用的默认值；面板显示「未知」比显示一个对 glm-5/qwen3-coder
// 并不成立的数字更诚实（F4.2）。
//
// Images=CapNo：上游的 userInputMessage 其实支持 images，但我们的内部消息契约
// （channel.Message）里没有图片部件，发不出去 —— 所以对客户端而言这个能力位是「不支持」。
func (a *Adapter) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	out := make([]channel.ModelInfo, 0, len(localModels))
	for _, m := range localModels {
		out = append(out, channel.ModelInfo{
			ID:            m.ID,
			DisplayName:   m.Name,
			ContextWindow: 0,
			Source:        channel.SourceLocal,
			Tools:         channel.CapYes, // 原生支持
			Reasoning:     channel.CapNo,
			Images:        channel.CapNo,
		})
	}
	return out, nil
}

// Balance 余额未知：上游没有余额接口（计费额度只在每次响应的 usage 事件里给），不猜数字。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin 无签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "Kiro 没有签到活动"}, nil
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// Chat 发一次对话：组装 Amz-Json 请求体，响应按 AWS Event Stream 帧流解析。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	// 先取令牌、再拼请求体：有些账号的 profileArn 是换令牌时下发的，
	// 先换令牌才能把 profileArn 放进这次的请求体（顺序反了，重启后的第一个请求就会缺它）。
	token, err := a.accessToken(ctx, c, false)
	if err != nil {
		return nil, err
	}
	raw, err := a.encodePayload(req, c)
	if err != nil {
		return nil, err
	}
	resp, err := a.sendGenerate(ctx, c, token, raw)
	if err != nil {
		return nil, err
	}
	// 上游把「令牌过期」判成 403（不是 401，参考实现据此换令牌重试）。两个都接住：
	// 换一次令牌再打一次，且只重试一次 —— 重试风暴会把限流坐实。
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		a.forgetToken(uidOf(c))
		if token, err = a.accessToken(ctx, c, true); err != nil {
			return nil, err
		}
		if resp, err = a.sendGenerate(ctx, c, token, raw); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		msg := "上游返回错误"
		if strings.Contains(strings.ToLower(string(body)), "improperly formed") {
			// 上游这句话什么都没说清，补一句能定位的
			msg = "上游拒绝了这个请求（Improperly formed request）：通常是工具定义里的 schema 字段不被接受，或历史消息形态不合法"
		}
		return nil, errs.New(a.Classify(resp.StatusCode, body), msg).
			WithChannel(string(channel.Kiro)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(body), 200))
	}
	return newStream(resp.Body, req.Model), nil
}

// encodePayload 拼请求体并做体积检查。
func (a *Adapter) encodePayload(req channel.ChatRequest, c *channel.Credential) ([]byte, error) {
	payload, err := buildPayload(req, resolveModel(req.Model), profileArnOf(c))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, errs.New(errs.Parse, "序列化请求体失败").
			WithChannel(string(channel.Kiro)).WithAccount(uidOf(c)).WithCause(err)
	}
	if len(raw) > maxPayloadBytes {
		// 上游在 ~615KB 起报 "Improperly formed request"（文案完全看不出是体积问题）。
		// 本地先拦，并把「怎么办」写清楚。
		return nil, errs.New(errs.PromptTooLong, fmt.Sprintf(
			"请求体 %d 字节，超过 Kiro 上游约 %d 字节上限：请精简历史消息或减少工具定义",
			len(raw), maxPayloadBytes)).WithChannel(string(channel.Kiro)).WithAccount(uidOf(c))
	}
	return raw, nil
}

// sendGenerate 打对话端点：Amz-Json 的头一整套（少一个上游都会按别的形态路由）。
func (a *Adapter) sendGenerate(ctx context.Context, c *channel.Credential, token string, body []byte) (*http.Response, error) {
	url := hostURL(a.apiBase, apiRegionOf(c)) + pathGenerate
	headers := map[string]string{
		"Authorization":               "Bearer " + token,
		"Content-Type":                amzContentType,
		"Accept":                      "*/*",
		"x-amz-target":                xAmzTarget,
		"x-amzn-codewhisperer-optout": codewhispererOptOut,
		"x-amzn-kiro-agent-mode":      kiroAgentMode,
		"amz-sdk-invocation-id":       uuid4(),
		"amz-sdk-request":             amzSDKRequest,
		"User-Agent":                  a.userAgentOf(c),
		"x-amz-user-agent":            a.amzUserAgentOf(c),
		"Accept-Encoding":             "identity", // 别让中间层压缩二进制帧（半包判断依赖字节原样）
	}
	return a.do(ctx, c, http.MethodPost, url, body, headers)
}

// Classify 把上游错误归一成有限枚举（D4）。
//
// 403 归 SessionDead 是本渠道的要点：上游用它表达「令牌过期」，
// 判成别的会让好账号被无谓冷却（或反过来一直重试一个死令牌）。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return errs.SessionDead
	case status == http.StatusPaymentRequired:
		// 402：订阅/额度层面要付费
		return errs.HardCredit
	case status == http.StatusTooManyRequests:
		// 「限流」与「额度耗尽」都可能是 429，靠报文区分。
		// 注意别用 "limit" 这种宽词：限流的原话里也常带（"rate limited"），
		// 认它就会把「等一会儿」判成「没钱了，等多久都没用」——正好判反。
		for _, kw := range []string{"quota exceeded", "exceeded your", "out of credits",
			"insufficient", "exhaust", "额度"} {
			if strings.Contains(s, kw) {
				return errs.HardCredit
			}
		}
		return errs.SoftRate
	case status == http.StatusNotFound:
		return errs.ModelUnavailable
	case status == http.StatusRequestEntityTooLarge:
		return errs.PromptTooLong
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		switch {
		case strings.Contains(s, "invalid_grant"), strings.Contains(s, "invalid_token"),
			strings.Contains(s, "invalid token"), strings.Contains(s, "refresh token"):
			// AWS SSO 的 CreateToken 用 400 + invalid_grant 表达「refresh token 废了」，
			// 这是要重新登录，不是我们请求拼错了。
			return errs.SessionDead
		case strings.Contains(s, "context"), strings.Contains(s, "too long"),
			strings.Contains(s, "too many tokens"), strings.Contains(s, "maximum"):
			return errs.PromptTooLong
		case strings.Contains(s, "content"), strings.Contains(s, "blocked"),
			strings.Contains(s, "prohibited"), strings.Contains(s, "harmful"):
			return errs.ContentBlocked
		case strings.Contains(s, "model"):
			return errs.ModelUnavailable
		}
		return errs.Parse
	case status >= 500:
		return errs.UpstreamFault
	}
	return errs.Parse
}

// ---------------------------------------------------------------------------
// 令牌
// ---------------------------------------------------------------------------

// accessToken 取一个可用的 access token。
//
// force=true 表示「令牌已被上游拒了，必须换一个再来」（401/403 之后的第二次尝试）：
// 这时不能再看凭证里的 access token，哪怕它还没到期 —— 上游说它不行了。
func (a *Adapter) accessToken(ctx context.Context, c *channel.Credential, force bool) (string, error) {
	if c == nil {
		return "", errs.New(errs.SessionDead, "Kiro 凭证为空：请在面板里重新粘贴 refreshToken").
			WithChannel(string(channel.Kiro))
	}
	access := strings.TrimSpace(c.AccessToken)
	refresh := strings.TrimSpace(c.RefreshToken)
	if refresh == "" {
		if access == "" {
			return "", errs.New(errs.SessionDead, "Kiro 凭证里既没有 refreshToken 也没有 accessToken：请重新粘贴").
				WithChannel(string(channel.Kiro)).WithAccount(uidOf(c))
		}
		// 只粘了 access token：没有可续期的东西，过期只能靠上游 403 暴露出来
		//（我们手里没有 refresh token，刷不了，就不能假装能刷）。
		return access, nil
	}

	key := uidOf(c)
	if !force {
		if v, ok := a.cached(key); ok && time.Now().Before(v.exp.Add(-refreshBuffer)) {
			return v.access, nil
		}
		// 凭证自带的 access token 还新鲜、且该有的上下文（profileArn）都在时，
		// 直接用它，不多打一次上游。profileArn 缺失时**有意**走一次换令牌：
		// 有些账号的 profileArn 是换令牌时下发的，而它在落盘时会被丢掉（见 ssoUIDPrefix 的说明），
		// 换一次就能自愈，否则请求会一直 400。
		if access != "" && !expiring(c) && profileArnOf(c) != "" {
			return access, nil
		}
	}

	use := refresh
	if v, ok := a.cached(key); ok && v.refresh != "" {
		use = v.refresh // 上游轮换过 refresh token：用最新的那个
	}
	acc, newRefresh, exp, arn, err := a.exchangeRefresh(ctx, c, use)
	if err != nil {
		return "", err
	}
	a.remember(key, cachedToken{access: acc, refresh: firstNonEmpty(newRefresh, use), exp: exp})
	if arn != "" {
		// 换令牌时下发的 profileArn 只放在内存里给本次请求用（落盘的那份由面板管）。
		setExtra(c, "profile_arn", arn)
	}
	return acc, nil
}

// Refresh 是账号级续期：拿 refreshToken 换一组新令牌。核心层目前不会主动调它
// （Chat 内部会按需换），这里实现完整是为了接口语义正确、也方便将来接状态巡检。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if c == nil {
		return nil, nil
	}
	refresh := strings.TrimSpace(c.RefreshToken)
	if refresh == "" {
		return nil, nil // 没得续期，明确表示「刷不了」，让上层按 403 处理
	}
	access, newRefresh, exp, arn, err := a.exchangeRefresh(ctx, c, refresh)
	if err != nil {
		return nil, err
	}
	nc := *c
	nc.AccessToken = access
	if newRefresh != "" {
		nc.RefreshToken = newRefresh
	}
	nc.ExpiresAt = exp
	if arn != "" {
		setExtra(&nc, "profile_arn", arn)
	}
	a.remember(uidOf(c), cachedToken{access: access, refresh: firstNonEmpty(newRefresh, refresh), exp: exp})
	return &nc, nil
}

// exchangeRefresh 用 refresh token 换 access token，按凭据自动分流桌面版/企业版。
//
// 返回（access、可能轮换的新 refresh、过期时间、可能下发的 profileArn）。
func (a *Adapter) exchangeRefresh(ctx context.Context, c *channel.Credential, refresh string) (string, string, time.Time, string, error) {
	fail := func(err error) (string, string, time.Time, string, error) {
		return "", "", time.Time{}, "", err
	}
	region := regionOf(c)
	headers := map[string]string{"Content-Type": "application/json"}
	var url string
	var body []byte

	switch {
	case useOIDC(c):
		url = hostURL(a.oidcBase, region) + pathOIDCToken
		// AWS SSO 的 CreateToken：JSON + **camelCase**（不是表单、也不是 snake_case）。
		body, _ = json.Marshal(map[string]any{
			"grantType":    "refresh_token",
			"clientId":     clientIDOf(c),
			"clientSecret": clientSecretOf(c),
			"refreshToken": refresh,
		})
	case isSSOUID(uidOf(c)):
		// 曾经是企业版，但 clientId/clientSecret 不在了（落盘 schema 不含这两个键）。
		// 不用企业版凭据去撞桌面端点 —— 直接告诉用户怎么修。
		return fail(errs.New(errs.SessionDead,
			"这是 AWS SSO（企业版）账号，但 clientId/clientSecret 没有随凭据保留下来："+
				"请在面板重新粘贴三件套（refreshToken + clientId + clientSecret）").
			WithChannel(string(channel.Kiro)).WithAccount(uidOf(c)))
	default:
		url = hostURL(a.authBase, region) + pathDesktopRefresh
		body, _ = json.Marshal(map[string]any{"refreshToken": refresh})
		headers["User-Agent"] = "KiroIDE-" + kiroVersion + "-" + a.fingerprintOf(c)
	}

	resp, err := a.do(ctx, c, http.MethodPost, url, body, headers)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fail(errs.New(a.Classify(resp.StatusCode, raw), "换 access token 失败").
			WithChannel(string(channel.Kiro)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200)))
	}
	var out struct {
		AccessToken  string  `json:"accessToken"`
		RefreshToken string  `json:"refreshToken"`
		ExpiresIn    float64 `json:"expiresIn"`
		ProfileArn   string  `json:"profileArn"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fail(errs.New(errs.Parse, "上游没有返回可解析的令牌").
			WithChannel(string(channel.Kiro)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200)))
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		// 拿到 200 却没有令牌：绝不能当成功（否则后面每个请求都 403，看起来像别的问题）。
		return fail(errs.New(errs.SessionDead,
			"上游没有返回 accessToken：refreshToken 可能已失效或与当前鉴权模式不匹配，请重新粘贴").
			WithChannel(string(channel.Kiro)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200)))
	}
	secs := out.ExpiresIn
	if secs <= 0 {
		secs = 3600 // 上游没给就当 1 小时（参考实现的默认值）
	}
	// 过期时间留 60 秒余量（与参考实现一致）：掐着点用会在网络抖动时撞 403。
	exp := time.Now().Add(time.Duration(secs)*time.Second - time.Minute)
	return strings.TrimSpace(out.AccessToken), strings.TrimSpace(out.RefreshToken), exp, strings.TrimSpace(out.ProfileArn), nil
}

func (a *Adapter) cached(key string) (cachedToken, bool) {
	a.tokMu.Lock()
	defer a.tokMu.Unlock()
	v, ok := a.tokens[key]
	return v, ok
}

func (a *Adapter) remember(key string, v cachedToken) {
	if key == "" {
		return
	}
	a.tokMu.Lock()
	a.tokens[key] = v
	a.tokMu.Unlock()
}

// forgetToken 丢掉某个凭证的令牌缓存（被上游拒了之后必须丢，否则会一直用死令牌重试）。
func (a *Adapter) forgetToken(key string) {
	a.tokMu.Lock()
	delete(a.tokens, key)
	a.tokMu.Unlock()
}

// ---------------------------------------------------------------------------
// 凭证字段 / 端点 / 指纹
// ---------------------------------------------------------------------------

func regionOf(c *channel.Credential) string {
	if v := extraOf(c, "region"); v != "" {
		return v
	}
	return defaultRegion
}

// apiRegionOf 取对话端点的区域。
//
// 为什么和换令牌的区域分开：SSO/IAM Identity Center 的区域与 Q Developer 运行时的
// 可用区域**不一定是同一个**（参考实现为此专门做了自动探测 + KIRO_API_REGION 覆盖）。
// 我们允许凭据里用 api_region 显式指定；没写就跟随 region。
func apiRegionOf(c *channel.Credential) string {
	if v := extraOf(c, "api_region"); v != "" {
		return v
	}
	return regionOf(c)
}

func profileArnOf(c *channel.Credential) string { return extraOf(c, "profile_arn") }
func clientIDOf(c *channel.Credential) string   { return extraOf(c, "client_id") }
func clientSecretOf(c *channel.Credential) string {
	return extraOf(c, "client_secret")
}

// useOIDC 判据：**凭据里同时有 clientId 和 clientSecret** 就走 AWS SSO OIDC，
// 否则走桌面版（与参考实现的 _detect_auth_type 完全一致）。
func useOIDC(c *channel.Credential) bool {
	return clientIDOf(c) != "" && clientSecretOf(c) != ""
}

func extraOf(c *channel.Credential, key string) string {
	if c == nil || c.Extra == nil {
		return ""
	}
	return strings.TrimSpace(c.Extra[key])
}

// setExtra 往凭证 Extra 里写一个值（Extra 为 nil 时建出来）。凭证可能是 nil 安全跳过。
func setExtra(c *channel.Credential, key, val string) {
	if c == nil || val == "" {
		return
	}
	if c.Extra == nil {
		c.Extra = map[string]string{}
	}
	c.Extra[key] = val
}

func uidOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	return c.UID
}

// expiring 判断凭证自带的 access token 是不是「已经过期或快过期」。
// 过期时间未知（零值）时按「需要刷新」处理 —— 宁可多换一次，也不要撞 403。
func expiring(c *channel.Credential) bool {
	if c.ExpiresAt.IsZero() {
		return true
	}
	return time.Now().After(c.ExpiresAt.Add(-refreshBuffer))
}

// hostURL 把模板里的 {region} 换成实际区域。
func hostURL(tmpl, region string) string {
	if strings.TrimSpace(region) == "" {
		region = defaultRegion
	}
	return strings.ReplaceAll(tmpl, "{region}", region)
}

// machineSeed 是「这台机器」的原始标识（参考实现用的是 hostname + username）。
func machineSeed() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "poolgate"
	}
	user := strings.TrimSpace(os.Getenv("USER"))
	if user == "" {
		user = strings.TrimSpace(os.Getenv("USERNAME"))
	}
	if user == "" {
		user = "poolgate"
	}
	return host + "-" + user
}

// fingerprintOf 由凭证派生一个稳定指纹，参与 UA 的 KiroIDE-<版本>-<指纹> 段。
//
// 与参考实现的**有意差别**：参考实现用 hostname-user，即「整机一个指纹」。
// 一个网关装多个 Kiro 账号时，那样会让所有账号在 AWS 侧长得像同一台机器 ——
// 而「同一台机器挂着一串账号」正是风控最容易拿来关联的特征。
// 我们按账号派生（seed 取 UID），同时满足三件事：同账号恒定、不同账号天然不同、
// 不需要额外落盘。与别家渠道（Kimi/DeepSeek 的设备号）同一个原则。
func (a *Adapter) fingerprintOf(c *channel.Credential) string {
	seed := uidOf(c)
	if seed == "" {
		seed = strings.TrimSpace(c.AccessToken)
	}
	if seed == "" {
		seed = "anonymous"
	}
	sum := sha256.Sum256([]byte(machineSeed() + "-" + seed + "-kiro-gateway"))
	return hex.EncodeToString(sum[:])
}

// userAgentOf 伪装成 Kiro IDE 自带的 AWS SDK（参考实现实测的形态）。
// os/win32 那几个字段是刻意写死的：上游看到的就是一个 Windows 上的 Kiro IDE。
func (a *Adapter) userAgentOf(c *channel.Credential) string {
	return amzSDKJSVersion + " ua/2.1 os/win32#10.0.19044 lang/js md/nodejs#22.21.1 " +
		amzSDKAPIVersion + " m/E KiroIDE-" + kiroVersion + "-" + a.fingerprintOf(c)
}

func (a *Adapter) amzUserAgentOf(c *channel.Credential) string {
	return amzSDKJSVersion + " KiroIDE-" + kiroVersion + "-" + a.fingerprintOf(c)
}

// do 发一个普通 JSON 请求（换令牌用）。
func (a *Adapter) do(ctx context.Context, c *channel.Credential, method, url string, body []byte, headers map[string]string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").
			WithChannel(string(channel.Kiro)).WithAccount(uidOf(c)).WithCause(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").
			WithChannel(string(channel.Kiro)).WithAccount(uidOf(c)).WithCause(err)
	}
	return resp, nil
}

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
