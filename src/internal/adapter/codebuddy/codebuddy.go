// Package codebuddy 是 PoolGate 的腾讯 CodeBuddy（编程助手）适配器，
// 实现 channel.Channel —— **登录式**渠道，与 WorkBuddyCN 同源、差在出站身份。
//
// 为什么是「同源」：CodeBuddy（copilot.tencent.com / www.codebuddy.cn）与我们已经
// 接入的 WorkBuddy 是**同一套插件后端**，共享鉴权与聊天接口：
//
//	POST /v2/plugin/auth/state?platform=CLI   → {state, authUrl}
//	GET  /v2/plugin/auth/token?state=<state>  → 轮询登录态
//	POST /v2/plugin/auth/token/refresh        → 刷新令牌
//	POST /v2/chat/completions                 → OpenAI 兼容（原生 tools，只接受流式）
//
// 所以本包不是「另写一个协议」，而是把 WorkBuddy 那套流程里的**出站身份**做成
// 可切换的一档（见 Identity）：
//
//	身份档             X-Domain              刷新来源   出站头
//	CodeBuddy（默认）  www.codebuddy.cn      plugin     极简：Authorization / X-User-Id /
//	                                                       X-Enterprise-Id / X-Tenant-Id /
//	                                                       X-Domain + UA
//	WorkBuddy（备选）  copilot.tencent.com   workbuddy  完整头族 + 设备指纹（machine/session）
//
// 参考实现 codebuddy2openai（极简头版本）证明**最小头也能通过**；同一份
// workbuddy2api-hub 则给出了完整身份/指纹那一档。两档的差异集中在本文件，
// 复用的代码（SSE 解析、信封剥壳、错误归一、签到）按包内自持的原则**复制**过来，
// 不改动 internal/adapter/workbuddy/。
//
// 能力位说实话（Spec）：上游是 OpenAI 兼容且**原生支持工具调用** → Tools=true、ToolsShim=false；
// 但**后端只接受流式**（请求体强制 stream=true），非流式由网关聚合 → SSEOnly=true。
package codebuddy

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 上游域名与端点。BaseCN / EpChat 等与 WorkBuddy 完全相同 —— 同一套后端。
const (
	BaseCN   = "https://copilot.tencent.com"
	OriginCN = "https://www.codebuddy.cn"

	// DomainCB 是 CodeBuddy 身份档的 X-Domain 值；DomainWB 是 WorkBuddy 档的。
	// 参考实现比对结论：CodeBuddy 用 www.codebuddy.cn，WorkBuddy-CN 用 copilot.tencent.com。
	DomainCB = "www.codebuddy.cn"
	DomainWB = "copilot.tencent.com"

	EpChat      = "/v2/chat/completions"
	EpRefresh   = "/v2/plugin/auth/token/refresh"
	EpUserRes   = "/v2/billing/meter/get-user-resource"
	ProductCode = "p_tcaca"

	// clientUA 是 CodeBuddy 身份档的 UA。后端插件体系里 CLI 档的 UA 形如
	// `CLI/<v> CodeBuddy/<v>`；参考实现用的是自造串，也能过 —— 这里用与
	// CodeBuddy CLI 一致的形态，改 UA 时只改这一处。
	clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"
	// wbUA 是 WorkBuddy 身份档的 UA（桌面端形态，对齐 workbuddy2api-hub 实测值）。
	wbUA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
)

// Identity 是出站身份档 —— 本包与 WorkBuddy 的唯一实质差异。
//
// 做成显式枚举而不是散落的布尔：身份决定 x-domain、刷新来源、UA 与是否带设备指纹，
// 换身份时**四处必须一起换**，散着改一定会漏掉一处（刷新就会被打回）。
type Identity string

const (
	// IdentityCodeBuddy 是默认档：极简头，不带机器码/会话/IDE 头。
	IdentityCodeBuddy Identity = "codebuddy"
	// IdentityWorkBuddy 是备选档：WorkBuddy 桌面端完整头族 + 设备指纹。
	// 保留它是因为极简档一旦被上游收紧，切这一档即可续命，无需重写适配器。
	IdentityWorkBuddy Identity = "workbuddy"
)

// profile 是一个身份档的落地参数。
type profile struct {
	domain        string
	refreshSource string
	ua            string
	// full 为 true 时带完整头族与设备指纹（machineId/sessionId/IDE 头）。
	full bool
}

func profileOf(id Identity) profile {
	if id == IdentityWorkBuddy {
		return profile{domain: DomainWB, refreshSource: "workbuddy", ua: wbUA, full: true}
	}
	return profile{domain: DomainCB, refreshSource: "plugin", ua: clientUA}
}

// Adapter 实现 channel.Channel（CodeBuddy 渠道）。
type Adapter struct {
	http *http.Client
	// noFollow 不自动跟随重定向：授权页地址需要自己读 Location 解析成最终地址。
	noFollow *http.Client
	base     string
	ident    profile
}

// 编译期断言：适配器必须满足渠道契约与授权契约。
var (
	_ channel.Channel    = (*Adapter)(nil)
	_ channel.Authorizer = (*Adapter)(nil)
)

func New() *Adapter { return NewWithTimeout(120 * time.Second) }

func NewWithTimeout(timeout time.Duration) *Adapter {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	// 带 cookie jar：登录流程的 state 与浏览器会话绑定，多账号授权不能串会话。
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: timeout, Transport: tr, Jar: jar}
	return &Adapter{http: client, noFollow: noFollowClient(client), base: BaseCN, ident: profileOf(IdentityCodeBuddy)}
}

// NewWithIdentity 按指定身份档构造（默认档 CodeBuddy）。
func NewWithIdentity(id Identity) *Adapter {
	a := New()
	a.ident = profileOf(id)
	return a
}

// NewWithBase 面向测试/自建网关：换上游 base 与 http 客户端，身份档保持默认。
func NewWithBase(base string, hc *http.Client) *Adapter {
	return NewWithBaseIdentity(base, hc, IdentityCodeBuddy)
}

// NewWithBaseIdentity 是 NewWithBase 的身份档版本。
func NewWithBaseIdentity(base string, hc *http.Client, id Identity) *Adapter {
	a := New()
	a.base = base
	a.ident = profileOf(id)
	if hc != nil {
		a.http = hc
		a.noFollow = noFollowClient(hc)
	}
	return a
}

// noFollowClient 复制一个客户端的传输与 cookie，但拒绝自动跟随重定向。
func noFollowClient(src *http.Client) *http.Client {
	return &http.Client{
		Timeout:   src.Timeout,
		Jar:       src.Jar,
		Transport: src.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// staticModels CodeBuddy 静态兜底模型表。
//
// 与 WorkBuddy 共用同一后端，因此目录同源；这里取参考实现 codebuddy2openai 的
// DEFAULT_MODELS 作为基线（含 `auto` 路由档）。国内版没有可用的动态目录端点，
// 静态表是唯一来源 —— 认不出的模型名仍原样透传，由上游判定。
var staticModels = []channel.ModelInfo{
	{ID: "auto", DisplayName: "Auto", ContextWindow: 131072},
	{ID: "glm-5.2", DisplayName: "GLM-5.2", ContextWindow: 131072},
	{ID: "glm-5.1", DisplayName: "GLM-5.1", ContextWindow: 131072},
	{ID: "glm-5v-turbo", DisplayName: "GLM-5V-Turbo", ContextWindow: 131072},
	{ID: "kimi-k2.7", DisplayName: "Kimi-K2.7", ContextWindow: 131072},
	{ID: "kimi-k2.6", DisplayName: "Kimi-K2.6", ContextWindow: 131072},
	{ID: "kimi-k2.5", DisplayName: "Kimi-K2.5", ContextWindow: 131072},
	{ID: "deepseek-v4-pro", DisplayName: "DeepSeek-V4-Pro", ContextWindow: 131072},
	{ID: "deepseek-v4-flash", DisplayName: "DeepSeek-V4-Flash", ContextWindow: 131072},
	{ID: "minimax-m3-pay", DisplayName: "MiniMax-M3-Pay", ContextWindow: 131072},
	{ID: "hy3-preview-agent", DisplayName: "Hy3-Preview-Agent", ContextWindow: 131072},
}

// Spec 返回能力声明。能力位说实话：原生工具调用（Tools=true、ToolsShim=false），
// 但上游只接受流式（SSEOnly=true）。
func Spec() channel.Spec {
	return channel.Spec{
		Kind:        channel.CodeBuddy,
		DisplayName: "CodeBuddy",
		Status:      channel.Active,
		Category:    channel.CategoryCoding, // 编程助手 / IDE 类
		Tools:       true,
		ToolsShim:   false,
		Reasoning:   true,
		SSEOnly:     true,
		CheckinCap:  true,
	}
}

func (a *Adapter) Kind() channel.Kind { return channel.CodeBuddy }
func (a *Adapter) Spec() channel.Spec { return Spec() }

// ---------------------------------------------------------------------------
// 头与信封
// ---------------------------------------------------------------------------

// commonHeaders 是登录/轮询等插件端点的公共头（对齐 workbuddy2api-hub 实测）。
func (a *Adapter) commonHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", OriginCN)
	req.Header.Set("Referer", OriginCN+"/")
	req.Header.Set("User-Agent", a.ident.ua)
}

// authHeaders 按当前身份档组装带鉴权的出站头。
//
// CodeBuddy 档刻意保持**极简**（只有 Authorization / X-User-Id / X-Enterprise-Id /
// X-Tenant-Id / X-Domain + UA）—— 参考实现证明最小头即可通过；多带机器码/会话头
// 反而会引入不一致的指纹。WorkBuddy 档才补齐完整头族与设备指纹。
func (a *Adapter) authHeaders(req *http.Request, c *channel.Credential) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", a.ident.ua)
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("X-User-Id", c.UID)
	req.Header.Set("X-Domain", a.ident.domain)

	// X-Enterprise-Id / X-Tenant-Id 两档都带：参考实现里它们是固定出现的字段，
	// 值来自授权时落下的 enterprise_id；没有就留空（不猜、不编造）。
	ent := ""
	if c.Extra != nil {
		ent = c.Extra["enterprise_id"]
	}
	req.Header.Set("X-Enterprise-Id", ent)
	req.Header.Set("X-Tenant-Id", ent)

	if a.ident.full {
		// WorkBuddy 档：完整头族 + 按 uid 稳定派生的设备指纹（多账号天然隔离）。
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", OriginCN)
		req.Header.Set("Referer", OriginCN+"/")
		req.Header.Set("X-Agent-Purpose", "conversation")
		req.Header.Set("X-IDE-Name", "WorkBuddy")
		req.Header.Set("X-IDE-Type", "WorkBuddy")
		req.Header.Set("X-IDE-Version", "5.5.6")
		req.Header.Set("X-Product", "WorkBuddy")
		req.Header.Set("X-Machine-ID", deriveID(c.UID, "machine"))
		req.Header.Set("X-Session-ID", deriveID(c.UID, "session"))
		mid := messageID()
		req.Header.Set("X-Conversation-Request-ID", mid)
		req.Header.Set("X-Conversation-Message-ID", mid)
		req.Header.Set("X-Request-ID", mid)
		req.Header.Set("X-Root-Request-ID", mid)
	}
}

func messageID() string {
	var b [16]byte
	n := time.Now().UnixNano()
	for i := 0; i < 8; i++ {
		b[i] = byte(n >> (i * 8))
		b[15-i] = byte(n >> ((7 - i) * 8))
	}
	return hex.EncodeToString(b[:])
}

// deriveID 由 uid + salt 稳定派生一个设备/会话标识（对齐 wb_fingerprint.derive_id）。
// 幂等：同一账号每次都得到同一个值，避免随机机器码被上游风控关联。
func deriveID(uid, salt string) string {
	if uid == "" {
		uid = "anonymous"
	}
	sum := md5.Sum([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:])
}

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// ---------------------------------------------------------------------------
// channel.Channel 实现
// ---------------------------------------------------------------------------

func (a *Adapter) Login(ctx context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.AuthFailed, "渠道授权是异步交互流程（浏览器 + state 轮询，或粘贴凭据），请在面板「渠道授权」页完成；本方法不阻塞等待").WithChannel(string(channel.CodeBuddy))
}

func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if strings.TrimSpace(c.RefreshToken) == "" {
		return nil, errs.New(errs.SessionDead, "缺少 refresh token，需重新授权").WithAccount(c.UID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+EpRefresh, nil)
	if err != nil {
		return nil, errs.New(errs.Transport, "构造刷新请求失败").WithAccount(c.UID).WithCause(err)
	}
	// 刷新头的身份字段与聊天一致；来源串随身份档切换（CodeBuddy=plugin，WorkBuddy=workbuddy）。
	a.authHeaders(req, c)
	req.Header.Set("X-Refresh-Token", c.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", a.ident.refreshSource)

	data, err := a.doJSON(req)
	if err != nil {
		return nil, err
	}
	tok := parseToken(data)
	if tok.AccessToken == "" {
		return nil, errs.New(errs.SessionDead, "刷新失败，需重新授权").WithAccount(c.UID).
			WithUpstream(truncate(string(data), 200))
	}
	nc := *c
	nc.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		nc.RefreshToken = tok.RefreshToken
	}
	if exp := tokenExpiry(tok.ExpiresIn, tok.ExpiresAt); !exp.IsZero() {
		nc.ExpiresAt = exp
	}
	return &nc, nil
}

// tokenResp 是 auth/token 与 refresh 端点共用的令牌响应体（信封内的 data）。
//
// 上游有时把令牌再套一层 data（workbuddy2api-hub 的实测：`data.data or data`），
// 所以用嵌套解一次，两处都覆盖。
type tokenResp struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresIn        int64  `json:"expiresIn"` // 秒
	ExpiresAt        int64  `json:"expiresAt"` // 毫秒（上游既有秒也有毫秒，见 tokenExpiry）
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
	Data             *struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
	} `json:"data"`
}

func parseToken(raw json.RawMessage) tokenResp {
	var t tokenResp
	if json.Unmarshal(raw, &t) != nil {
		return tokenResp{}
	}
	if t.AccessToken == "" && t.Data != nil {
		return tokenResp{
			AccessToken:  t.Data.AccessToken,
			RefreshToken: t.Data.RefreshToken,
			ExpiresIn:    t.Data.ExpiresIn,
			ExpiresAt:    t.Data.ExpiresAt,
			Domain:       t.Data.Domain,
		}
	}
	return t
}

// tokenExpiry 把上游的两种过期表达归一成绝对时间。
// expiresAt 既可能是毫秒时间戳（auth 文件里的形态），也可能是秒；>1e12 视为毫秒。
func tokenExpiry(expiresIn, expiresAt int64) time.Time {
	if expiresAt > 0 {
		ms := expiresAt
		if ms < 1e12 {
			ms *= 1000
		}
		return time.UnixMilli(ms)
	}
	if expiresIn > 0 {
		return time.Now().Add(time.Duration(expiresIn) * time.Second)
	}
	return time.Time{}
}

func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	// 国内版无可用动态目录端点，模型目录以静态表为准（与 WorkBuddy 同源）。
	return staticModels, nil
}

func (a *Adapter) ModelsSnapshot() []channel.ModelInfo {
	out := make([]channel.ModelInfo, len(staticModels))
	copy(out, staticModels)
	return out
}

func (a *Adapter) Balance(ctx context.Context, c *channel.Credential) (channel.Balance, error) {
	body, _ := json.Marshal(map[string]any{"PageNumber": 1, "PageSize": 100, "ProductCode": ProductCode, "Status": []int{0, 3}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+EpUserRes, bytes.NewReader(body))
	if err != nil {
		return channel.Balance{}, errs.New(errs.Transport, "构造余额请求失败").WithAccount(c.UID).WithCause(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", a.ident.ua)
	req.Header.Set("X-User-Id", c.UID)
	req.Header.Set("X-Domain", a.ident.domain)
	data, err := a.doJSON(req)
	if err != nil {
		return channel.Balance{}, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					CycleCapacitySize   int64 `json:"CycleCapacitySize"`
					CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
					CapacitySize        int64 `json:"CapacitySize"`
					CapacityRemain      int64 `json:"CapacityRemain"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return channel.Balance{}, errs.New(errs.Parse, "余额解析失败").WithAccount(c.UID).WithCause(err)
	}
	var total int64
	for _, acct := range resp.Response.Data.Accounts {
		if acct.CycleCapacitySize > 0 {
			total += acct.CycleCapacityRemain
		} else {
			total += acct.CapacityRemain
		}
	}
	return channel.Balance{Credits: total, Known: true}, nil
}

// Chat 发对话请求，返回标准 OpenAI SSE 流（上游已是 OpenAI 兼容 SSE，直接透传）。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	body := buildBody(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+EpChat, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithAccount(c.UID).WithCause(err)
	}
	a.authHeaders(httpReq, c)
	resp, err := a.http.Do(httpReq)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithAccount(c.UID).WithCause(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := a.Classify(resp.StatusCode, raw)
		return nil, errs.New(kind, "上游返回错误").WithChannel(string(channel.CodeBuddy)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	// 上游是标准 OpenAI SSE，直接逐行透传。
	return newStream(resp.Body, req.Model), nil
}

func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	return Classify(status, string(body))
}

func (a *Adapter) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, errs.New(Classify(resp.StatusCode, string(raw)), "上游错误").WithUpstream(truncate(string(raw), 200))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, errs.New(errs.Parse, "信封解析失败").WithCause(err)
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		return nil, errs.New(kind, fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160)))
	}
	return env.Data, nil
}

// buildBody 构造 OpenAI chat.completions 请求体。
//
// 关键约束：**后端只接受流式**，所以 stream 恒为 true（非流式由网关聚合）；
// stream_options.include_usage 一并带上，让用量帧能回传。
func buildBody(req channel.ChatRequest) []byte {
	msgs := make([]map[string]any, len(req.Messages))
	for i, m := range req.Messages {
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		msg := map[string]any{"role": role, "content": m.Content}
		if role == "assistant" && len(m.ToolCalls) > 0 {
			msg["tool_calls"] = m.ToolCalls
		}
		if role == "tool" {
			if m.ToolCallID != "" {
				msg["tool_call_id"] = m.ToolCallID
			}
			if m.Name != "" {
				msg["name"] = m.Name
			}
		}
		msgs[i] = msg
	}
	obj := map[string]any{
		"model":          req.Model,
		"messages":       msgs,
		"stream":         true,
		"max_tokens":     req.MaxTokens,
		"stream_options": map[string]any{"include_usage": true},
	}
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	if tools := req.ForwardTools(); len(tools) > 0 {
		obj["tools"] = tools
		if req.ToolChoice != nil {
			obj["tool_choice"] = req.ToolChoice
		}
	}
	if len(msgs) == 0 || msgs[0]["role"] != "system" {
		obj["messages"] = append([]map[string]any{{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
	}
	raw, _ := json.Marshal(obj)
	return raw
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// quotaCodes 是上游表示「额度已耗尽」的业务码（实测 14018 来自 HTTP 429 的信封）。
// 按码判定比按文案判定可靠：文案会随语言和措辞变。
var quotaCodes = []string{`"code":14018`, `"code": 14018`}

func codeQuotaExhausted(lowerBody string) bool {
	for _, c := range quotaCodes {
		if strings.Contains(lowerBody, c) {
			return true
		}
	}
	return false
}

// Classify 按 HTTP 状态 + body 判定错误类别（与 WorkBuddy 同源，枚举映射一致）。
func Classify(status int, body string) errs.Kind {
	lower := strings.ToLower(body)
	// 额度类判据必须先于 429：上游把「额度耗尽」也回成 HTTP 429 +
	// 信封 code 14018。若按限流处理（冷却 60s + 换号重试），会一直拿这个没额度的号去试。
	if codeQuotaExhausted(lower) || strings.Contains(lower, "insufficient") ||
		strings.Contains(lower, "credit") || strings.Contains(lower, "exhausted") ||
		strings.Contains(lower, "quota") ||
		strings.Contains(lower, "余额不足") || strings.Contains(lower, "额度不足") ||
		strings.Contains(lower, "额度已用尽") || strings.Contains(lower, "额度用尽") {
		return errs.HardCredit
	}
	if status == http.StatusUnauthorized {
		return errs.SessionDead
	}
	if status == http.StatusTooManyRequests {
		return errs.SoftRate
	}
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many") {
		return errs.SoftRate
	}
	if status >= 500 {
		return errs.UpstreamFault
	}
	if status >= 400 {
		return errs.UpstreamFault
	}
	return errs.Parse
}
