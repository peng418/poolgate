// Package workbuddy 是 PoolGate 的 WorkBuddyCN（CodeBuddy 国内版）适配器：
// 实现 channel.Channel。协议移植自 wild-work internal/workbuddyai（CN 分支）——
// 上游 copilot.tencent.com / www.codebuddy.cn，OpenAI 兼容 SSE 直接透传，
// 信封 {code,msg,data} 剥壳，Bearer + 会话 ID 头族。
package workbuddy

import (
	"bytes"
	"context"
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

// 上游域名与端点（国内版）。
const (
	BaseCN   = "https://copilot.tencent.com"
	OriginCN = "https://www.codebuddy.cn"
	DomainCN = "www.codebuddy.cn"

	EpChat      = "/v2/chat/completions"
	EpRefresh   = "/v2/plugin/auth/token/refresh"
	EpUserRes   = "/v2/billing/meter/get-user-resource"
	ProductCode = "p_tcaca"
	clientUA    = "CLI/2.63.2 CodeBuddy/2.63.2"
)

// Adapter 实现 channel.Channel（WorkBuddyCN 渠道）。
type Adapter struct {
	http *http.Client
	// noFollow 不自动跟随重定向：授权页地址需要自己读 Location 解析成最终地址。
	noFollow *http.Client
	base     string
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
	return &Adapter{http: client, noFollow: noFollowClient(client), base: BaseCN}
}

func NewWithBase(base string, hc *http.Client) *Adapter {
	a := New()
	a.base = base
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
var staticModels = []channel.ModelInfo{
	{ID: "hy3", DisplayName: "Hy3", ContextWindow: 131072},
	{ID: "hy3-preview", DisplayName: "Hy3 Preview", ContextWindow: 131072},
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
	req.Header.Set("Origin", OriginCN)
	req.Header.Set("Referer", OriginCN+"/")
	req.Header.Set("User-Agent", clientUA)
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
	mid := messageID()
	req.Header.Set("X-Conversation-Request-ID", mid)
	req.Header.Set("X-Conversation-Message-ID", mid)
	req.Header.Set("X-Request-ID", mid)
	req.Header.Set("X-Root-Request-ID", mid)
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
	body, _ := json.Marshal(map[string]any{"PageNumber": 1, "PageSize": 100, "ProductCode": ProductCode, "Status": []int{0, 3}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base+EpUserRes, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", c.UID)
	req.Header.Set("X-Domain", DomainCN)
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
		msgs[i] = map[string]any{"role": role, "content": m.Content}
	}
	obj := map[string]any{
		"model":          req.Model,
		"messages":       msgs,
		"stream":         true,
		"max_tokens":     req.MaxTokens,
		"stream_options": map[string]any{"include_usage": true},
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
