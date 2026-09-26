// Package workbuddy 是 PoolGate 的 WorkBuddyCN（CodeBuddy 国内版）适配器：
// 实现 channel.Channel。协议移植自 wild-work internal/workbuddyai（CN 分支）——
// 上游 copilot.tencent.com / www.codebuddy.cn，OpenAI 兼容 SSE 直接透传，
// 信封 {code,msg,data} 剥壳，Bearer + 会话 ID 头族。
package workbuddy

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"poolgate/internal/sanitize"
)

// 上游域名与端点（国内版）。
const (
	BaseCN   = "https://copilot.tencent.com"
	OriginCN = "https://www.codebuddy.cn"
	DomainCN = "www.codebuddy.cn"

	// BillingCN 是账单/签到专用基址，与 chat 基址**不同域**：
	// chat/refresh/login 走 copilot.tencent.com，billing（余额/签到）走 www.codebuddy.cn。
	// 依据两份参考一致：wild-work internal/upstream/client.go（ChatBaseCN="https://copilot.tencent.com"、
	// BillingBaseCN="https://www.codebuddy.cn"）与 workbuddy2api-hub v1.5.8
	// wb_accounts.py REALM_CONFIGS["cn"]（chat_upstream=copilot.tencent.com、
	// billing_upstream=www.codebuddy.cn）。此前两家共用 a.base（copilot.tencent.com），
	// 账单请求打错了域。
	BillingCN = "https://www.codebuddy.cn"

	EpChat      = "/v2/chat/completions"
	EpRefresh   = "/v2/plugin/auth/token/refresh"
	EpUserRes   = "/v2/billing/meter/get-user-resource"
	ProductCode = "p_tcaca"
	clientUA    = "CLI/2.63.2 CodeBuddy/2.63.2"

	// acceptLanguageCN 国内版请求头语言。依据 wild-work internal/upstream/headers.go
	// acceptLanguageFor（CN=zh-CN、global=en-US）与 hub wb_accounts.py Account.headers
	// （realm=="intl" ? "en-US" : "zh-CN"）。
	acceptLanguageCN = "zh-CN"
)

// Adapter 实现 channel.Channel（WorkBuddyCN 渠道）。
type Adapter struct {
	http *http.Client
	// noFollow 不自动跟随重定向：授权页地址需要自己读 Location 解析成最终地址。
	noFollow *http.Client
	base     string
	// billing 是账单/签到基址（见 BillingCN）。单独成字段而不是直接用常量，
	// 是为了让 NewWithBase 的测试替身同时覆盖 chat 与 billing 两条路。
	billing string
}

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
	return &Adapter{http: client, noFollow: noFollowClient(client), base: BaseCN, billing: BillingCN}
}

func NewWithBase(base string, hc *http.Client) *Adapter {
	a := New()
	a.base = base
	a.billing = base
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

// staticModels WorkBuddyCN 静态兜底模型表（对齐 wild-work internal/server/handler.go 的
// workbuddyStaticModels）。国内版无可用动态目录端点（/console 被网关拒，/v2/enterprises
// 是国际版专用），故模型目录以静态表为准；对话仍走 /v2/chat/completions（实测可用）。
//
// hy3-x / hy3-preview-agent 是移植时漏掉的条目：
//   - hy3-x 见 hub v1.5.8 wb_catalog.py STATIC_CN_MODELS，且本仓 docs/03 抽样实测
//     「hy3 / hy3-x / kimi-k3-1」全部 ✅；
//   - hy3-preview-agent 见 wild-work workbuddyStaticModels，且同族 codebuddy 适配器
//     staticModels 也带它（codebuddy2openai DEFAULT_MODELS）。
//
// kimi-k3-1 是 wild-work 静态表里没有、但 hub CN 目录与本仓 docs/03 实测都支持的条目，
// 保留（不因「对齐 wild-work」而删掉实测可用的模型）。
var staticModels = []channel.ModelInfo{
	{ID: "hy3", DisplayName: "Hy3", ContextWindow: 131072},
	{ID: "hy3-x", DisplayName: "Hy3-X", ContextWindow: 131072},
	{ID: "hy3-preview", DisplayName: "Hy3 Preview", ContextWindow: 131072},
	{ID: "hy3-preview-agent", DisplayName: "Hy3-Preview-Agent", ContextWindow: 131072},
	{ID: "glm-5.2", DisplayName: "GLM-5.2", ContextWindow: 131072},
	{ID: "glm-5.1", DisplayName: "GLM-5.1", ContextWindow: 131072},
	{ID: "glm-5v-turbo", DisplayName: "GLM-5V-Turbo", ContextWindow: 131072},
	{ID: "kimi-k2.7", DisplayName: "Kimi-K2.7", ContextWindow: 131072},
	{ID: "kimi-k3-1", DisplayName: "Kimi-K3-1", ContextWindow: 131072},
	{ID: "minimax-m3", DisplayName: "MiniMax-M3", ContextWindow: 131072},
	{ID: "deepseek-v4-pro", DisplayName: "DeepSeek-V4-Pro", ContextWindow: 131072},
	{ID: "deepseek-v4-flash", DisplayName: "DeepSeek-V4-Flash", ContextWindow: 131072},
}

// Spec 返回能力声明。
func Spec() channel.Spec {
	return channel.Spec{
		Kind:        channel.WorkBuddyCN,
		DisplayName: "WorkBuddyCN",
		Status:      channel.Active,
		Category:    channel.CategoryCoding, // 编程助手/IDE 类
		Tools:       true,
		Reasoning:   true,
		SSEOnly:     true,
		CheckinCap:  true,
	}
}

func (a *Adapter) Kind() channel.Kind { return channel.WorkBuddyCN }
func (a *Adapter) Spec() channel.Spec { return Spec() }

// ---------------------------------------------------------------------------
// 头与信封
// ---------------------------------------------------------------------------

func commonHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// X-CodeBuddy-Request / Accept-Language 是两份参考都固定带的字段：
	// wild-work internal/upstream/headers.go CommonHeaders 与 hub wb_accounts.py
	// Account.headers（"X-CodeBuddy-Request": "1"、Accept-Language）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", acceptLanguageCN)
	req.Header.Set("Origin", OriginCN)
	req.Header.Set("Referer", OriginCN+"/")
	req.Header.Set("User-Agent", clientUA)
}

// enterpriseID 取授权时落下的企业 ID（可能为空，空则不猜）。
func enterpriseID(c *channel.Credential) string {
	if c == nil || c.Extra == nil {
		return ""
	}
	return strings.TrimSpace(c.Extra["enterprise_id"])
}

// stableID 由 uid 稳定派生 36 位 hex 设备/会话标识（X-Machine-ID / X-Session-ID）。
// 依据 wild-work internal/upstream/headers.go injectAccountStableHeaders +
// deriveStableID（sha256("wb2a:"+purpose+":"+uid) 取前 18 字节 hex）与 hub
// wb_fingerprint.py derive_id（同账号恒同值，防上游风控关联）。两参考算法不同，
// 但都要求「同一 uid 恒定、36 hex、账号间隔离」——这里采用 wild-work 的实现。
func stableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

func chatHeaders(req *http.Request, c *channel.Credential) {
	commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("X-User-Id", c.UID)
	req.Header.Set("X-Domain", DomainCN)
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", "5.5.4")
	req.Header.Set("X-Product", "WorkBuddy")
	// X-Enterprise-Id：wild-work（两处 chatHeaders）与 hub 都按「有则发、无则 X-No-*」
	// 的约定处理，此前完全漏掉，等于把企业账号的身份信息丢了。
	if ent := enterpriseID(c); ent != "" {
		req.Header.Set("X-Enterprise-Id", ent)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 设备/会话指纹：wild-work headers.go 与 hub wb_accounts.py 都带，按 uid 稳定派生。
	if c.UID != "" {
		req.Header.Set("X-Machine-ID", stableID(c.UID, "machine"))
		req.Header.Set("X-Session-ID", stableID(c.UID, "session"))
	}
	// 会话头族：X-Conversation-Request-ID 与 X-Request-ID 是两个不同的 id。
	// 依据 wild-work internal/upstream/headers.go injectConversationHeaders
	// （cid := newMessageID()；Message/Request-ID 用传入的 messageID，Request-ID 用 cid）。
	mid := messageID()
	cid := messageID()
	req.Header.Set("X-Conversation-Request-ID", cid)
	req.Header.Set("X-Conversation-Message-ID", mid)
	req.Header.Set("X-Request-ID", mid)
	req.Header.Set("X-Root-Request-ID", cid)
	// B3 链路头（wild-work headers.go 同款；hub 不带，属补充性追踪头，不影响业务）。
	req.Header.Set("X-B3-TraceId", mid)
	req.Header.Set("X-B3-SpanId", mid[:16])
	req.Header.Set("X-B3-Sampled", "1")
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

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// ---------------------------------------------------------------------------
// channel.Channel 实现
// ---------------------------------------------------------------------------

func (a *Adapter) Login(ctx context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.AuthFailed, "渠道授权是异步交互流程（浏览器 + state 轮询），请在面板「渠道授权」页完成；本方法不阻塞等待").WithChannel(string(channel.WorkBuddyCN))
}

func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if strings.TrimSpace(c.RefreshToken) == "" {
		return nil, errs.New(errs.SessionDead, "缺少 refresh token，需重新授权").WithAccount(c.UID)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base+EpRefresh, nil)
	commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Refresh-Token", c.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	// X-Enterprise-Id：wild-work RefreshHeaders（internal/upstream 与 workbuddyai 两处）
	// 都带；企业账号刷新缺它会与 chat 身份不一致。
	if ent := enterpriseID(c); ent != "" {
		req.Header.Set("X-Enterprise-Id", ent)
	}
	data, err := a.doJSON(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, errs.New(errs.SessionDead, "刷新失败，需重新授权").WithAccount(c.UID)
	}
	nc := *c
	nc.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		nc.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		nc.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return &nc, nil
}

func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	// WorkBuddyCN 国内版无可用动态目录端点，模型目录以静态表为准（对齐 wild-work）。
	return staticModels, nil
}

func (a *Adapter) ModelsSnapshot() []channel.ModelInfo {
	out := make([]channel.ModelInfo, len(staticModels))
	copy(out, staticModels)
	return out
}

func (a *Adapter) Balance(ctx context.Context, c *channel.Credential) (channel.Balance, error) {
	// PackageEndTimeRangeBegin/End：两份参考都固定带（wild-work internal/upstream
	// client.go getUserResource 与 internal/workbuddyai client.go userResource；
	// workbuddy2api-hub 同域账单）。此前漏掉，上游可能因缺范围参数少回套餐条目。
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              ProductCode,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	// 账单走 BillingCN（www.codebuddy.cn），不是 chat 的 copilot.tencent.com。
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.billing+EpUserRes, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", c.UID)
	req.Header.Set("X-Domain", DomainCN)
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", acceptLanguageCN)
	// X-Enterprise-Id / X-Tenant-Id：wild-work BillingHeaders（两处）与 hub
	// Account.headers(purpose="billing") 都带，值来自授权时落下的 enterprise_id。
	if ent := enterpriseID(c); ent != "" {
		req.Header.Set("X-Enterprise-Id", ent)
		req.Header.Set("X-Tenant-Id", ent)
	}
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
					CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
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
		// 三档口径与两份参考一致（wild-work resourceAccount.bill() /
		// internal/workbuddyai userResource）：周期总量 > 0 用周期剩余；否则只要
		// 周期已用或剩余非 0 就用周期剩余；都没有才回退容量剩余。
		// 此前只判 CycleCapacitySize>0，中间档（Size=0 但 Remain>0）会错取 CapacityRemain（常为 0），
		// 把真实余额读成 0。剩余负数钳 0（与参考一致）。
		var remain int64
		switch {
		case acct.CycleCapacitySize > 0:
			remain = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			remain = acct.CycleCapacityRemain
		default:
			remain = acct.CapacityRemain
		}
		if remain < 0 {
			remain = 0
		}
		total += remain
	}
	return channel.Balance{Credits: total, Known: true}, nil
}

// Chat 发对话请求，返回标准 OpenAI SSE 流（WorkBuddy 已是 OpenAI 兼容 SSE，直接透传）。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	body := buildBody(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+EpChat, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithAccount(c.UID).WithCause(err)
	}
	chatHeaders(httpReq, c)
	resp, err := a.http.Do(httpReq)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithAccount(c.UID).WithCause(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := a.Classify(resp.StatusCode, raw)
		return nil, errs.New(kind, "上游返回错误").WithChannel(string(channel.WorkBuddyCN)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	// WorkBuddy 是标准 OpenAI SSE，直接逐行透传。
	return newStream(resp.Body, req.Model), nil
}

func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	return Classify(status, string(body))
}

func (a *Adapter) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
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
		"stream_options": map[string]any{"include_usage": true},
	}
	// max_tokens 只在客户端真的给了（>0）时才发：网关在客户端没写时传 0，
	// 而 `"max_tokens": 0` 在 OpenAI 语义里是「一个 token 都不许生成」，上游可能直接拒。
	// 依据两份参考一致：wild-work internal/upstream/payload.go 从不注入 max_tokens
	// （只把别名 max_completion_tokens 翻译成它），hub v1.5.8 translate_max_completion_tokens
	// 也只在值 >0 时设置；同族 codebuddy 适配器 buildBody 同样是 >0 才发。
	if req.MaxTokens > 0 {
		obj["max_tokens"] = req.MaxTokens
	}
	// temperature 透传（客户端给了才发）：wild-work 整包透传、hub build_upstream_body
	// body=dict(payload) 都保留该字段；此前丢失，采样参数被静默改回上游默认。
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	if tools := req.ForwardTools(); len(tools) > 0 {
		obj["tools"] = tools
		// tool_choice 必须归一成**字符串**再发（上游把该字段声明为 string，
		// 发对象会 400 code=11101）。依据 wild-work internal/upstream/payload.go
		// normalizeToolChoice 与 hub v1.5.8 wb_proxy.py normalize_tool_choice。
		// 此前完全没转发，function 型强制选择被丢弃。
		if tc := normalizeToolChoice(req.ToolChoice); tc != nil {
			obj["tool_choice"] = tc
		}
	}
	if len(msgs) == 0 || msgs[0]["role"] != "system" {
		obj["messages"] = append([]map[string]any{{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
	}
	raw, _ := json.Marshal(obj)
	// 出站脱敏（body 组装完、发出去之前，落点与 wild-work internal/upstream/payload.go
	// PrepareBody 一致）：剥离上游内容审核黑名单指纹（Claude Code / Codex CLI 注入的模板句、
	// billing header、裸 11128 等）。上游对指纹做逐字精确匹配，命中即回
	// HTTP 400 code=11128 "Illegal API invocation from an unapproved channel" —— 也就是说
	// 从 Claude Code / Studio 调本渠道会被原样挡掉，见 internal/sanitize 文件头依据。
	return sanitize.Messages(raw)
}

// normalizeToolChoice 把 OpenAI tool_choice 归一成上游认的字符串形态。
// 逻辑照 hub v1.5.8 wb_proxy.py normalize_tool_choice + wild-work payload.go
// normalizeToolChoice（两者语义一致，只在 "none" 的 tools 处置上不同）：
//   - "auto"/"required" → 原样字符串
//   - {"type":"function","function":{"name":"x"}}（或 {"name":"x"}）→ 字符串 "x"，无名则 "auto"
//   - {"type":"auto"/"required"} → 对应字符串
//   - {"type":"none"} / 其他对象 / 非标量 → 删（返回 nil）
//
// "none" 的取舍：hub 会保留 tools 并发字符串 "none"（实测上游并不真遵守 none，
// 但删 tools 会让模型把调用降级成文本、Agent 空转）；wild-work 则连 tools 一起删。
// 这里的 tools 是否转发由 channel.ForwardTools() 决定（none 时不转发），
// 因此本函数只管把值归一成字符串。
func normalizeToolChoice(v any) any {
	switch t := v.(type) {
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		if s == "auto" || s == "required" {
			return s
		}
		return nil // "none"/"any" 未知值：不转发
	case map[string]any:
		typ, _ := t["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "auto", "required":
			return strings.ToLower(strings.TrimSpace(typ))
		case "function":
			name := ""
			if fn, ok := t["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = t["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				return name
			}
			return "auto"
		}
		return nil
	}
	return nil
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

// sessionDeadMarkers 会话失效标记（需重新登录而非换号重试）。
// 依据 wild-work internal/upstream/client.go 与 internal/workbuddyai/models.go
// 同款 sessionDeadMarkers：业务码 12153 与 "Offline user session not found" 表示登录态已死。
var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// contentBlockedMarkers 内容审核拦截标记。依据 wild-work internal/upstream/client.go
// contentBlockedMarkers（这三条是上游内容安全拦截的原话）。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// Classify 按 HTTP 状态 + body 判定错误类别（对齐 QoderCN 的枚举映射）。
func Classify(status int, body string) errs.Kind {
	lower := strings.ToLower(body)
	// 额度类判据必须先于 429：上游把「额度耗尽」也回成 HTTP 429 +
	// 信封 code 14018（实测：{"error":{"data":{"code":14018,"msg":"额度已用尽…"}}}）。
	// 若按限流处理（冷却 60s + 换号重试），会一直拿这个没额度的号去试。
	// 全部用 lower 比较，「Credits exhausted」是大写开头，逐字匹配会漏。
	if codeQuotaExhausted(lower) || strings.Contains(lower, "insufficient") ||
		strings.Contains(lower, "credit") || strings.Contains(lower, "exhausted") ||
		strings.Contains(lower, "quota") ||
		strings.Contains(lower, "余额不足") || strings.Contains(lower, "额度不足") ||
		strings.Contains(lower, "额度已用尽") || strings.Contains(lower, "额度用尽") {
		return errs.HardCredit
	}
	// 402 Payment Required 一律判额度不足（wild-work 两处 Classify 的首个判据）。
	if status == http.StatusPaymentRequired {
		return errs.HardCredit
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return errs.SessionDead
		}
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
		// 专项分类在通用 4xx 之前：区分「内容拦截 / 上下文超限 / 模型不可用」与普通上游故障。
		// 标记与判据来自 wild-work internal/upstream/client.go Classify 与
		// internal/workbuddyai/models.go Classify。
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return errs.ContentBlocked
			}
		}
		if strings.Contains(lower, "prompt is too long") || strings.Contains(body, `"code":11115`) {
			return errs.PromptTooLong
		}
		// 11102 service info not found：模型在该账号不可用（wild-work workbuddyai
		// constants.go brokenModels 注释实测），按「该账号无此模型权限」处理，
		// 不把好号当上游故障冷却。
		if strings.Contains(body, `"code":11102`) || strings.Contains(lower, "service info not found") {
			return errs.ModelUnavailable
		}
		return errs.UpstreamFault
	}
	return errs.Parse
}
