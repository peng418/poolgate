// client.go QoderCN 适配器主体：实现 channel.Channel，把 COSY 签名对话、
// QoderEncoding 编码、嵌套 SSE 剥壳、错误 Classify 归一全部收敛在这里。
package qodercom

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Adapter 实现 channel.Channel（QoderCN 渠道）。
type Adapter struct {
	http    *http.Client
	base    string // 业务 API，默认 https://openapi.qoder.com.cn
	gateway string // 推理网关，默认 https://gateway.qoder.com.cn

	// modelMap 客户端名（display_name 规范化）→ 上游 model key；
	// cache 为最近一次成功拉取的完整模型表（无静态兜底，仅此缓存）。
	mu       sync.RWMutex
	modelMap map[string]string
	cache    []ModelEntry

	// userTypes 保护 uid→userType 实测缓存。
	utMu      sync.RWMutex
	userTypes map[string]string
}

// New 生产默认。Qoder gateway 对 HTTP/2 不友好（stream INTERNAL_ERROR），强制 HTTP/1.1。
func New() *Adapter {
	return NewWithTimeout(180 * time.Second)
}

// NewWithTimeout 指定上游 HTTP 超时；配置连接池。
func NewWithTimeout(timeout time.Duration) *Adapter {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // 强制 HTTP/1.1
	}
	return &Adapter{
		http:      &http.Client{Timeout: timeout, Transport: tr},
		base:      OpenAPIBase,
		gateway:   GatewayBase,
		modelMap:  map[string]string{},
		userTypes: map[string]string{},
	}
}

// NewWithBase 测试用：覆盖 base/gateway/http。
func NewWithBase(base, gateway string, hc *http.Client) *Adapter {
	a := New()
	a.base = base
	a.gateway = gateway
	if hc != nil {
		a.http = hc
	}
	return a
}

// Spec 返回 QoderCOM 能力声明。
// 签到：COM 无 legacy daily-check-in（实测 404），只有 campaigns 活动路径，
// 因此 CheckinCap=false，面板会显示「无活动」而不是一个点了没反应的按钮。
func Spec() channel.Spec {
	return channel.Spec{
		Kind:        channel.QoderCOM,
		DisplayName: "QoderCOM",
		Status:      channel.Active,
		Tools:       true,
		Reasoning:   true,
		SSEOnly:     true,
		CheckinCap:  false,
	}
}

// ---------------------------------------------------------------------------
// 凭证辅助：机器指纹与 userType 从 channel.Credential.Extra 读取。
// ---------------------------------------------------------------------------

// fingerprint 从凭证 Extra 读取机器指纹；缺失时生成并（由调用方）回填。
func (a *Adapter) fingerprint(c *channel.Credential) machineFingerprint {
	return machineFingerprint{
		MachineID:    c.Extra["machine_id"],
		MachineToken: c.Extra["machine_token"],
		MachineType:  c.Extra["machine_type"],
	}
}

// EnsureFingerprint 为凭证生成/保持 COSY 机器指纹（幂等）。
func (a *Adapter) EnsureFingerprint(c *channel.Credential) {
	if c.Extra == nil {
		c.Extra = map[string]string{}
	}
	if c.Extra["machine_id"] == "" {
		c.Extra["machine_id"] = uuid4()
	}
	if c.Extra["machine_token"] == "" {
		seed := []byte(uuid4() + uuid4())
		if len(seed) > 50 {
			seed = seed[:50]
		}
		c.Extra["machine_token"] = base64.RawURLEncoding.EncodeToString(seed)
	}
	if c.Extra["machine_type"] == "" {
		c.Extra["machine_type"] = strings.ReplaceAll(uuid4(), "-", "")[:18]
	}
}

// userTypeOf 返回 uid 的实测 userType；无缓存返回缺省 personal_standard。
func (a *Adapter) userTypeOf(c *channel.Credential) string {
	a.utMu.RLock()
	defer a.utMu.RUnlock()
	if ut := a.userTypes[c.UID]; ut != "" {
		return ut
	}
	return "personal_standard"
}

func (a *Adapter) setUserType(uid, ut string) {
	if uid == "" || ut == "" {
		return
	}
	a.utMu.Lock()
	a.userTypes[uid] = ut
	a.utMu.Unlock()
}

// ---------------------------------------------------------------------------
// channel.Channel 实现
// ---------------------------------------------------------------------------

// Kind 实现 channel.Channel。
func (a *Adapter) Kind() channel.Kind { return channel.QoderCOM }

// Spec 实现 channel.Channel。
func (a *Adapter) Spec() channel.Spec { return Spec() }

// Login 实现 channel.Channel。QoderCOM 是 OAuth 设备流，需要浏览器授权，
// 属于异步交互流程（M1 未接入控制台授权屏，见 M2）。这里返回明确错误，
// 不做静默成功 —— 红线一。
func (a *Adapter) Login(ctx context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.AuthFailed, "渠道授权是异步交互流程（浏览器设备流），请在面板「渠道授权」页完成；本方法不阻塞等待").WithChannel(string(channel.QoderCOM))
}

// Refresh 实现 channel.Channel：用 drt- 走 /api/v1/deviceToken/refresh 轮换 dt/drt。
// 401/403 → SessionDead（需重新 OAuth 登录）。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if strings.TrimSpace(c.RefreshToken) == "" {
		return nil, errs.New(errs.SessionDead, "缺少 refresh token，需重新授权").WithAccount(c.UID)
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": c.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+EpDTRefresh, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "刷新令牌请求失败").WithAccount(c.UID).WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errs.New(errs.SessionDead, "refresh token 已失效，需重新授权").
			WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	if resp.StatusCode >= 400 {
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "刷新令牌失败").
			WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	var out struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"` // ms
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errs.New(errs.Parse, "刷新令牌响应无法解析").WithAccount(c.UID).WithCause(err)
	}
	dt := out.Token
	if dt == "" {
		dt = out.DeviceToken
	}
	if dt == "" || out.RefreshToken == "" {
		return nil, errs.New(errs.Parse, "刷新令牌返回不完整").WithAccount(c.UID)
	}
	nc := *c
	nc.AccessToken = dt
	nc.RefreshToken = out.RefreshToken
	now := time.Now()
	if out.ExpiresIn > 0 {
		nc.ExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Millisecond)
	} else if out.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
			nc.ExpiresAt = t
		}
	}
	if nc.ExpiresAt.IsZero() {
		nc.ExpiresAt = now.Add(30 * 24 * time.Hour)
	}
	return &nc, nil
}

// Models 实现 channel.Channel。
func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	return a.models(ctx, c)
}

// Balance 实现 channel.Channel：查询账号当前可花费积分（基础 + 赠送聚合）。
func (a *Adapter) Balance(ctx context.Context, c *channel.Credential) (channel.Balance, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+EpQuotaUsage, nil)
	if err != nil {
		return channel.Balance{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return channel.Balance{}, errs.New(errs.Transport, "查询余额失败").WithAccount(c.UID).WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return channel.Balance{}, errs.New(a.Classify(resp.StatusCode, raw), "查询余额失败").
			WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	var q struct {
		UserQuota struct {
			Remaining float64 `json:"remaining"`
		} `json:"userQuota"`
		AddOnQuota struct {
			Remaining float64 `json:"remaining"`
		} `json:"addOnQuota"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return channel.Balance{}, errs.New(errs.Parse, "余额响应无法解析").WithAccount(c.UID).WithCause(err)
	}
	return channel.Balance{
		Credits: int64(q.UserQuota.Remaining + q.AddOnQuota.Remaining),
		Known:   true,
	}, nil
}

// Chat 实现 channel.Channel：构造 agent 体 → QoderEncode → COSY 签名 POST →
// 返回归一化 channel.Stream。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	clientName := req.Model
	modelKey := a.modelKey(clientName)
	if modelKey == "" {
		modelKey = clientName
	}

	msgs := make([]channel.Message, len(req.Messages))
	copy(msgs, req.Messages)

	rawBody, err := buildAgentBody(msgs, a.modelEntry(modelKey), false, req.MaxTokens, a.userTypeOf(c))
	if err != nil {
		return nil, errs.New(errs.Parse, "构造请求体失败").WithAccount(c.UID).WithCause(err)
	}
	encoded := qoderEncode(rawBody)
	url := a.gateway + EpChat

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithAccount(c.UID).WithCause(err)
	}
	sess, err := newCosySession(a.fingerprint(c), c.Nickname, c.UID, c.AccessToken, c.RefreshToken, a.userTypeOf(c))
	if err != nil {
		return nil, errs.New(errs.Transport, "构建 COSY 会话失败").WithAccount(c.UID).WithCause(err)
	}
	if err := sess.ApplyHeaders(httpReq, encoded, url, c.UID, "text/event-stream", true, modelKey); err != nil {
		return nil, errs.New(errs.Transport, "设置 COSY 头失败").WithAccount(c.UID).WithCause(err)
	}

	resp, err := a.http.Do(httpReq)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithAccount(c.UID).WithCause(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := a.Classify(resp.StatusCode, raw)
		return nil, errs.New(kind, "上游返回错误").
			WithChannel(string(channel.QoderCOM)).
			WithAccount(c.UID).
			WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, clientName), nil
}

// ---------------------------------------------------------------------------
// 错误归一：上游 HTTP status + body → 有限 errs.Kind（D4）
// ---------------------------------------------------------------------------

var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "isquotaexceeded\":true",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// Classify 实现 channel.Channel：按 HTTP 状态码 + body 判定错误类别。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	return Classify(status, string(body))
}

// Classify 按 HTTP 状态码 + body 判定错误类别（包级函数，便于单测）。
func Classify(status int, body string) errs.Kind {
	if status == http.StatusPaymentRequired {
		return errs.HardCredit
	}
	lower := strings.ToLower(body)
	// TOKEN_EXPIRE 优先于通用 401
	if status == http.StatusUnauthorized && strings.Contains(body, "TOKEN_EXPIRE") {
		return errs.SessionDead
	}
	if status == http.StatusUnauthorized {
		return errs.SessionDead
	}
	// 429 优先于 hardRule：限流 body 高频带 "quota exceeded"。
	if status == http.StatusTooManyRequests {
		return errs.SoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return errs.HardCredit
		}
	}
	// 非 429 但 body 含限流文案 → 软限流
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "usage limit") || strings.Contains(lower, "请求过于频繁") {
		return errs.SoftRate
	}
	if status == http.StatusNotFound {
		return errs.ModelUnavailable
	}
	if status >= 500 {
		return errs.UpstreamFault
	}
	if status >= 400 {
		if strings.Contains(lower, "blocked by security policy") ||
			strings.Contains(lower, "unapproved channel") ||
			strings.Contains(lower, "illegal api invocation") {
			return errs.ContentBlocked
		}
		if (status == 400 || status == 404) &&
			(strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "11115")) {
			return errs.PromptTooLong
		}
		if status == 403 && strings.TrimSpace(body) == "" {
			return errs.ContentBlocked
		}
		if strings.Contains(lower, "request illegal") || strings.Contains(lower, "trial not activated") {
			return errs.UpstreamFault
		}
		return errs.UpstreamFault
	}
	return errs.Parse // 未知不静默当成功 —— 返回可定位分类
}
