// Package workbuddyai 是 PoolGate 的 WorkBuddyAI（WorkBuddy 国际版）适配器。
//
// 与国内版 workbuddy 是**完全独立**的两家：独立域名、独立模型表、独立鉴权头。
// 实现由 workbuddy 代码级复制后改域名与端点 —— 协议同为 OpenAI 兼容 SSE 直通 +
// {code,msg,data} 信封剥壳，差异只在域名表与少量路径。
//
// 上游（2026-09-24 实测）：www.workbuddy.ai 单域名，chat/billing/catalog/auth 同域；
// 目录走 /v2/enterprises/personal/models（国内版是 /console/…）。
package workbuddyai

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
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/sanitize"
)

// 上游域名与端点（国内版）。
const (
	BaseAI   = "https://www.workbuddy.ai"
	OriginAI = "https://www.workbuddy.ai"
	DomainAI = "www.workbuddy.ai"

	EpChat      = "/v2/chat/completions"
	EpRefresh   = "/v2/plugin/auth/token/refresh"
	EpCatalog   = "/v2/enterprises/personal/models" // 国际版专用目录路径（实测 200）
	EpUserRes   = "/v2/billing/meter/get-user-resource"
	ProductCode = "p_tcaca"
	clientUA    = "CLI/2.63.2 CodeBuddy/2.63.2"

	// acceptLanguageIntl 国际版请求头语言。依据 wild-work internal/upstream/headers.go
	// acceptLanguageFor（global=en-US）与 hub wb_accounts.py（realm=="intl" → "en-US"）。
	acceptLanguageIntl = "en-US"
)

// Adapter 实现 channel.Channel（WorkBuddyAI 渠道）。
type Adapter struct {
	http *http.Client
	// noFollow 不自动跟随重定向：授权页地址需要自己读 Location 解析成最终地址。
	noFollow *http.Client
	base     string

	// mu/cache 保存最近一次成功拉取的目录（ModelsSnapshot 兜底用）。
	mu    sync.RWMutex
	cache []channel.ModelInfo
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
	return &Adapter{http: client, noFollow: noFollowClient(client), base: BaseAI}
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

// staticModels 是国际版的兜底模型表。目录接口可用时以动态结果为准，此表只在拉取失败时兜底。
//
// 表项与窗口对齐 wild-work internal/workbuddyai/constants.go staticModels（已实测），
// 并与 hub v1.5.8 wb_catalog.py 的国际版目录交叉核对（hub 覆盖其中 23 项，窗口值一致；
// minimax-m3 / deepseek-v3 / glm-5.1 / glm-5v-turbo / kimi-k2.7 是 wild-work extraModels
// 「目录不返回但实测可用」的补充项）。此前只搬了 13 项、窗口一律写 131072，属移植时缩水。
// 注意：ContextWindow 是本地声明（上游目录不回该字段）→ Source=local，不冒充上游值。
var staticModels = []channel.ModelInfo{
	{ID: "hy3", DisplayName: "Hy3", ContextWindow: 192000, Source: channel.SourceLocal},
	{ID: "hy4-preview", DisplayName: "Hy4 preview", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "deepseek-v4.1-flash", DisplayName: "Deepseek-V4.1-Flash", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "hy4-preview-f", DisplayName: "Hy4 preview F", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "default-model", DisplayName: "Auto", ContextWindow: 176000, Source: channel.SourceLocal},
	{ID: "fast-model", DisplayName: "Fast", ContextWindow: 200000, Source: channel.SourceLocal},
	{ID: "balanced-model", DisplayName: "Balanced", ContextWindow: 256000, Source: channel.SourceLocal},
	{ID: "primary-model", DisplayName: "Primary", ContextWindow: 272000, Source: channel.SourceLocal},
	{ID: "deep-model", DisplayName: "Deep", ContextWindow: 176000, Source: channel.SourceLocal},
	{ID: "gpt-5.6-sol", DisplayName: "GPT-5.6-Sol", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "gpt-5.6-terra", DisplayName: "GPT-5.6-Terra", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "gpt-5.6-luna", DisplayName: "GPT-5.6-Luna", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "gpt-6-astra", DisplayName: "GPT-6-Astra", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "gpt-5.5", DisplayName: "GPT-5.5", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "gpt-5.4", DisplayName: "GPT-5.4", ContextWindow: 272000, Source: channel.SourceLocal},
	{ID: "gpt-5.3-codex", DisplayName: "GPT-5.3-Codex", ContextWindow: 272000, Source: channel.SourceLocal},
	{ID: "gemini-3.5-flash", DisplayName: "Gemini-3.5-Flash", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "glm-5.3", DisplayName: "GLM-5.3", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "glm-5.2", DisplayName: "GLM-5.2", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "kimi-k3", DisplayName: "Kimi-K3", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "kimi-k2.6", DisplayName: "Kimi-K2.6", ContextWindow: 256000, Source: channel.SourceLocal},
	{ID: "kimi-k2.8-preview", DisplayName: "Kimi-K2.8-Preview", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "kimi-k2.7", DisplayName: "Kimi-K2.7", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "minimax-m3", DisplayName: "MiniMax-M3", ContextWindow: 256000, Source: channel.SourceLocal},
	{ID: "deepseek-v3", DisplayName: "Deepseek-V3", ContextWindow: 192000, Source: channel.SourceLocal},
	{ID: "glm-5.1", DisplayName: "GLM-5.1", ContextWindow: 1000000, Source: channel.SourceLocal},
	{ID: "glm-5v-turbo", DisplayName: "GLM-5V-Turbo", ContextWindow: 256000, Source: channel.SourceLocal},
}

// Spec 返回能力声明。
// 签到：国际版 daily-checkin 实测返回 code=10001（未开启）→ CheckinCap=false。
func Spec() channel.Spec {
	return channel.Spec{
		Kind:        channel.WorkBuddyAI,
		DisplayName: "WorkBuddyAI",
		Status:      channel.Active,
		Category:    channel.CategoryCoding, // 编程助手/IDE 类
		Tools:       true,
		Reasoning:   true,
		SSEOnly:     true,
		CheckinCap:  false,
	}
}

func (a *Adapter) Kind() channel.Kind { return channel.WorkBuddyAI }
func (a *Adapter) Spec() channel.Spec { return Spec() }

// ---------------------------------------------------------------------------
// 头与信封
// ---------------------------------------------------------------------------

func commonHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// X-CodeBuddy-Request / Accept-Language：wild-work internal/upstream/headers.go
	// CommonHeaders 与 hub wb_accounts.py Account.headers 都固定带。
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", acceptLanguageIntl)
	req.Header.Set("Origin", OriginAI)
	req.Header.Set("Referer", OriginAI+"/")
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
	req.Header.Set("X-Domain", DomainAI)
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", "5.5.4")
	req.Header.Set("X-Product", "WorkBuddy")
	// X-Enterprise-Id：wild-work workbuddyai chatHeaders（有则发、无则 X-No-Enterprise-Id）
	// 与 hub 都对；此前漏掉，企业账号身份信息丢失。
	if ent := enterpriseID(c); ent != "" {
		req.Header.Set("X-Enterprise-Id", ent)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if c.UID != "" {
		req.Header.Set("X-Machine-ID", stableID(c.UID, "machine"))
		req.Header.Set("X-Session-ID", stableID(c.UID, "session"))
	}
	// 会话头族：X-Conversation-Request-ID 与 X-Request-ID 是两个不同的 id
	// （wild-work internal/upstream/headers.go injectConversationHeaders）。
	mid := messageID()
	cid := messageID()
	req.Header.Set("X-Conversation-Request-ID", cid)
	req.Header.Set("X-Conversation-Message-ID", mid)
	req.Header.Set("X-Request-ID", mid)
	req.Header.Set("X-Root-Request-ID", cid)
	// B3 链路头（wild-work headers.go 同款）。
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
	return nil, errs.New(errs.AuthFailed, "渠道授权是异步交互流程（浏览器 + state 轮询），请在面板「渠道授权」页完成；本方法不阻塞等待").WithChannel(string(channel.WorkBuddyAI))
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
	// X-Enterprise-Id：wild-work workbuddyai refreshHeaders 与 hub Account._refresh_locked 都带。
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

// Models 拉取国际版动态模型目录（/v2/enterprises/personal/models，实测可用）。
// 拉不到时回退静态表 —— 但静态表标注 Source=local，不冒充上游值。
func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.base+EpCatalog, nil)
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Domain", DomainAI)
	req.Header.Set("User-Agent", clientUA)
	data, err := a.doJSON(req)
	if err != nil {
		return staticModels, nil
	}
	ids := extractCatalogModels(data)
	if len(ids) == 0 {
		return staticModels, nil
	}
	out := make([]channel.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, channel.ModelInfo{
			ID:          id,
			DisplayName: id,
			// 上游目录不回上下文窗口：标 unknown，不猜数字（F4.2）。
			ContextWindow: 0,
			Source:        channel.SourceUnknown,
			Tools:         channel.CapYes,
		})
	}
	a.mu.Lock()
	a.cache = out
	a.mu.Unlock()
	return out, nil
}

// extractCatalogModels 从目录响应里取出模型 id 列表（去重、保序）。
//
// 注意 doJSON 已经把外层 {code,msg,data} 剥掉，传进来的是 data 本身，
// 因此这里按「data 的内容」解析；同时兼容 data 里再嵌一层 data 的形态。
// 实测形态：{"agents":[{"models":["default-model",...]}]}。
func extractCatalogModels(raw []byte) []string {
	var resp struct {
		Agents []struct {
			Models []string `json:"models"`
		} `json:"agents"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
		Data *struct {
			Agents []struct {
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, ag := range resp.Agents {
		for _, m := range ag.Models {
			add(m)
		}
	}
	for _, m := range resp.Models {
		add(m.ID)
	}
	if resp.Data != nil {
		for _, ag := range resp.Data.Agents {
			for _, m := range ag.Models {
				add(m)
			}
		}
	}
	return out
}

// ModelsSnapshot 返回最近一次成功拉取的目录；没有则回退静态表。
func (a *Adapter) ModelsSnapshot() []channel.ModelInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.cache) > 0 {
		out := make([]channel.ModelInfo, len(a.cache))
		copy(out, a.cache)
		return out
	}
	out := make([]channel.ModelInfo, len(staticModels))
	copy(out, staticModels)
	return out
}

func (a *Adapter) Balance(ctx context.Context, c *channel.Credential) (channel.Balance, error) {
	// PackageEndTimeRangeBegin/End：两份参考都固定带（wild-work internal/upstream
	// client.go 与 internal/workbuddyai client.go 的 userResource 请求体）。
	// 此前漏掉，上游可能因缺范围参数少回套餐条目。
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              ProductCode,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	// 国际版 chat/billing 同域（www.workbuddy.ai），故仍用 a.base —— 与
	// wild-work ChatBaseGlobal=BillingBaseGlob=www.workbuddy.ai、hub REALM_CONFIGS["intl"] 一致。
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base+EpUserRes, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", c.UID)
	req.Header.Set("X-Domain", DomainAI)
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", acceptLanguageIntl)
	// X-Enterprise-Id / X-Tenant-Id：wild-work billingHeaders 与 hub
	// Account.headers(purpose="billing") 都带。
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
		// 三档口径与参考一致（wild-work internal/workbuddyai client.go userResource）：
		// 周期总量 >0 用周期剩余；否则周期剩余或已用非 0 也用周期剩余；
		// 都没有才回退容量剩余。此前只判 CycleCapacitySize>0，中间档会错取常为 0 的
		// CapacityRemain，把真实余额读成 0。剩余负数钳 0。
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

func (a *Adapter) Checkin(ctx context.Context, c *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: true, NoActivity: true, Message: "签到调度 M4 接入"}, nil
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
		return nil, errs.New(kind, "上游返回错误").WithChannel(string(channel.WorkBuddyAI)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
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
	// 依据两份参考一致：wild-work internal/upstream/payload.go 从不注入 max_tokens，
	// hub v1.5.8 translate_max_completion_tokens 只在值 >0 时设置；
	// 同族 codebuddy 适配器 buildBody 同样是 >0 才发。
	if req.MaxTokens > 0 {
		obj["max_tokens"] = req.MaxTokens
	}
	// temperature 透传（客户端给了才发）：wild-work 整包透传、hub build_upstream_body
	// body=dict(payload) 都保留该字段。
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	if tools := req.ForwardTools(); len(tools) > 0 {
		obj["tools"] = tools
		// tool_choice 必须归一成**字符串**再发（上游把该字段声明为 string，
		// 发对象会 400 code=11101）。依据 wild-work payload.go normalizeToolChoice
		// 与 hub v1.5.8 wb_proxy.py normalize_tool_choice。
		if tc := normalizeToolChoice(req.ToolChoice); tc != nil {
			obj["tool_choice"] = tc
		}
	}
	if len(msgs) == 0 || msgs[0]["role"] != "system" {
		obj["messages"] = append([]map[string]any{{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
	}
	raw, _ := json.Marshal(obj)
	// 出站脱敏（body 组装完、发出去之前，落点与 wild-work internal/workbuddyai/wb_utils.go
	// PrepareBody 一致）：剥离上游内容审核黑名单指纹（Claude Code / Codex CLI 注入的模板句、
	// billing header、裸 11128 等）。上游对指纹做逐字精确匹配，命中即回
	// HTTP 400 code=11128 "Illegal API invocation from an unapproved channel" —— 从
	// Claude Code / Studio 调本渠道会被原样挡掉，见 internal/sanitize 文件头依据。
	return sanitize.Messages(raw)
}

// normalizeToolChoice 把 OpenAI tool_choice 归一成上游认的字符串形态。
// 语义照 hub v1.5.8 wb_proxy.py normalize_tool_choice + wild-work payload.go normalizeToolChoice：
//   - "auto"/"required" → 原样字符串
//   - {"type":"function","function":{"name":"x"}}（或 {"name":"x"}）→ 字符串 "x"，无名则 "auto"
//   - {"type":"auto"/"required"} → 对应字符串
//   - {"type":"none"} / 其他对象 / 非标量 → 删（返回 nil）
//
// "none" 的取舍见 CN 适配器同名列注释（tools 由 channel.ForwardTools() 决定）。
func normalizeToolChoice(v any) any {
	switch t := v.(type) {
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		if s == "auto" || s == "required" {
			return s
		}
		return nil
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
// 依据 wild-work internal/workbuddyai/models.go 与 internal/upstream/client.go
// 同款 sessionDeadMarkers。
var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// contentBlockedMarkers 内容审核拦截标记（wild-work internal/upstream/client.go 同款）。
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
		// 专项分类在通用 4xx 之前（标记来自 wild-work 两处 Classify）。
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return errs.ContentBlocked
			}
		}
		if strings.Contains(lower, "prompt is too long") || strings.Contains(body, `"code":11115`) {
			return errs.PromptTooLong
		}
		// 11102 service info not found：模型在该账号不可用
		// （wild-work workbuddyai constants.go brokenModels 注释实测）。
		if strings.Contains(body, `"code":11102`) || strings.Contains(lower, "service info not found") {
			return errs.ModelUnavailable
		}
		return errs.UpstreamFault
	}
	return errs.Parse
}
