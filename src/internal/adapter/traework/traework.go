// Package traework 是 PoolGate 的 TraeWork 适配器：实现 channel.Channel。
// 协议移植自 wild-work internal/traework（SOLO 免费通道）——上游
// trae-api-cn.mchost.guru，Cloud-IDE-JWT 头族，SOLO SSE 自定义事件序列
// （metadata/output/token_usage/done/error）→ 标准 OpenAI chunk。
package traework

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

const (
	AgentHost = "https://trae-api-cn.mchost.guru"
	UgHost    = "https://api.trae.cn"
	OAuthHost = "https://api.trae.com.cn"

	EpChat         = "/api/agent/v3/llm_utils_chat"
	EpModels       = "/api/ide/v1/get_detail_param"
	EpEntUsage     = "/trae/api/v2/pay/web_user_ent_usage"
	EpExchange     = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	ClientID       = "en1oxy7wnw8j9n"
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion     = "0.1.52"
	IdeVersionCode = "20260811"
	DeviceBrand    = "20Y5A002XX"
	OSVersion      = "Windows 10 Pro"
	Function       = "solo_work_lite"
	DefaultModel   = "glm-5.2"
)

// Adapter 实现 channel.Channel（TraeWork 渠道）。
type Adapter struct {
	http  *http.Client
	agent string
	ug    string
	oauth string

	mu    sync.RWMutex
	cache []channel.ModelInfo
}

func New() *Adapter {
	tr := &http.Transport{MaxIdleConns: 100, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 120 * time.Second}
	return &Adapter{http: &http.Client{Timeout: 120 * time.Second, Transport: tr}, agent: AgentHost, ug: UgHost, oauth: OAuthHost}
}

func NewWithBase(agent, oauth string, hc *http.Client) *Adapter {
	a := New()
	a.agent = agent
	a.oauth = oauth
	if hc != nil {
		a.http = hc
	}
	return a
}

func Spec() channel.Spec {
	return channel.Spec{
		Kind:        channel.TraeWork,
		DisplayName: "TraeWork",
		Status:      channel.Active,
		Tools:       true,
		Reasoning:   true,
		SSEOnly:     true,
		CheckinCap:  true,
	}
}

func (a *Adapter) Kind() channel.Kind { return channel.TraeWork }
func (a *Adapter) Spec() channel.Spec { return Spec() }

func (a *Adapter) Login(ctx context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.AuthFailed, "渠道授权是异步交互流程（浏览器 + 本机回调），请在面板「渠道授权」页完成；本方法不阻塞等待").WithChannel(string(channel.TraeWork))
}

func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if strings.TrimSpace(c.RefreshToken) == "" {
		return nil, errs.New(errs.SessionDead, "缺少 refresh token，需重新授权").WithAccount(c.UID)
	}
	body := map[string]any{"ClientID": ClientID, "RefreshToken": c.RefreshToken, "ClientSecret": "-", "UserID": ""}
	raw, _ := json.Marshal(body)
	oauthHost := a.oauth
	if h := c.Extra["api_host"]; h != "" {
		oauthHost = h
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, oauthHost+EpExchange, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Trae/"+IdeVersion)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "刷新失败").WithAccount(c.UID).WithCause(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, errs.New(errs.SessionDead, "刷新被拒，需重新授权").WithAccount(c.UID)
	}
	var out struct {
		Result struct {
			Token         string `json:"Token"`
			RefreshToken  string `json:"RefreshToken"`
			TokenExpireAt int64  `json:"TokenExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Result.Token == "" {
		return nil, errs.New(errs.SessionDead, "刷新响应不完整，需重新授权").WithAccount(c.UID)
	}
	nc := *c
	nc.AccessToken = out.Result.Token
	if out.Result.RefreshToken != "" {
		nc.RefreshToken = out.Result.RefreshToken
	}
	if out.Result.TokenExpireAt > 0 {
		nc.ExpiresAt = time.Unix(normalizeExpires(out.Result.TokenExpireAt), 0)
	}
	return &nc, nil
}

func normalizeExpires(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	dyn, err := a.fetchModels(ctx, c)
	if err != nil {
		a.mu.RLock()
		defer a.mu.RUnlock()
		if len(a.cache) > 0 {
			return a.cache, nil
		}
		return nil, err
	}
	a.mu.Lock()
	a.cache = dyn
	a.mu.Unlock()
	return dyn, nil
}

func (a *Adapter) ModelsSnapshot() []channel.ModelInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]channel.ModelInfo, len(a.cache))
	copy(out, a.cache)
	return out
}

func (a *Adapter) Balance(ctx context.Context, c *channel.Credential) (channel.Balance, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.ug+EpEntUsage, bytes.NewReader([]byte(`{"require_usage":true}`)))
	ugHeaders(req, c)
	resp, err := a.http.Do(req)
	if err != nil {
		return channel.Balance{}, errs.New(errs.Transport, "查询余额失败").WithAccount(c.UID).WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return channel.Balance{}, errs.New(a.Classify(resp.StatusCode, raw), "查询余额失败").WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	var out struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				AvailableEndpoint int `json:"available_endpoint"`
				Quota             struct {
					CreditsLimit float64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return channel.Balance{}, errs.New(errs.Parse, "余额解析失败").WithAccount(c.UID).WithCause(err)
	}
	var usable int64
	for _, p := range out.UserEntitlementPackList {
		if p.EntitlementBaseInfo.AvailableEndpoint == 0 {
			limit := int64(p.EntitlementBaseInfo.Quota.CreditsLimit)
			used := int64(p.Usage.CreditsAmount)
			if r := limit - used; r > 0 {
				usable += r
			}
		}
	}
	return channel.Balance{Credits: usable, Known: true}, nil
}

// Chat 发对话请求，返回 SOLO SSE → 标准 OpenAI chunk 流。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	body := buildBody(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.agent+EpChat, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithAccount(c.UID).WithCause(err)
	}
	soloHeaders(httpReq, c, true)
	resp, err := a.http.Do(httpReq)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithAccount(c.UID).WithCause(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := a.Classify(resp.StatusCode, raw)
		return nil, errs.New(kind, "上游返回错误").WithChannel(string(channel.TraeWork)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, req.Model), nil
}

func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	return Classify(status, string(body))
}

func (a *Adapter) fetchModels(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	body := map[string]any{"function": Function, "config_names": nil, "need_prompt": false, "current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.agent+EpModels, bytes.NewReader(raw))
	soloHeaders(req, c, false)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, errs.New(a.Classify(resp.StatusCode, data), "目录失败")
	}
	var out struct {
		ConfigInfoList []struct {
			ConfigName    string `json:"config_name"`
			DisplayConfig struct {
				DisplayName   string `json:"display_name"`
				IsCustomModel bool   `json:"is_custom_model"`
			} `json:"display_config"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, errs.New(errs.Parse, "目录解析失败").WithCause(err)
	}
	seen := map[string]bool{}
	models := make([]channel.ModelInfo, 0, len(out.ConfigInfoList))
	for _, cfg := range out.ConfigInfoList {
		name := strings.TrimSpace(cfg.ConfigName)
		if name == "" || seen[name] || cfg.DisplayConfig.IsCustomModel || strings.HasPrefix(name, "custom_model_") {
			continue
		}
		seen[name] = true
		models = append(models, channel.ModelInfo{
			ID:          name,
			DisplayName: cfg.DisplayConfig.DisplayName,
			Source:      channel.SourceUpstream,
		})
	}
	if len(models) == 0 {
		return nil, errs.New(errs.UpstreamFault, "目录返回空列表")
	}
	return models, nil
}

// buildBody 构造 SOLO llm_utils_chat 请求体（强制 stream，content 转多模态数组）。
func buildBody(req channel.ChatRequest) []byte {
	msgs := make([]map[string]any, len(req.Messages))
	for i, m := range req.Messages {
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		msgs[i] = map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": m.Content}}}
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = DefaultModel
	}
	obj := map[string]any{
		"model":       model,
		"config_name": model,
		"stream":      true,
		"function":    Function,
		"messages":    msgs,
	}
	raw, _ := json.Marshal(obj)
	return raw
}

func soloHeaders(req *http.Request, c *channel.Credential, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", "Trae/"+IdeVersion)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+c.AccessToken)
	req.Header.Set("X-Cloudide-Token", c.AccessToken)
	req.Header.Set("X-Ide-Token", c.AccessToken)
	req.Header.Set("X-Uid", c.UID)
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", IdeVersion)
	req.Header.Set("X-Ide-Version-Code", IdeVersionCode)
	req.Header.Set("X-App-Version-Code", IdeVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if mid := c.Extra["machine_id"]; mid != "" {
		req.Header.Set("X-Machine-Id", mid)
	}
	if did := c.Extra["device_id"]; did != "" {
		req.Header.Set("x-device-id", did)
	}
}

func ugHeaders(req *http.Request, c *channel.Credential) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Trae/"+IdeVersion)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+c.AccessToken)
	req.Header.Set("X-User-Region", "CN")
	req.Header.Set("x-device-brand", DeviceBrand)
	req.Header.Set("x-device-type", "windows")
	req.Header.Set("x-os-version", OSVersion)
	req.Header.Set("x-app-version", IdeVersion)
	if did := c.Extra["device_id"]; did != "" {
		req.Header.Set("x-device-id", did)
	}
}

func Classify(status int, body string) errs.Kind {
	lower := strings.ToLower(body)
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
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

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func capOf(b bool) channel.Cap {
	if b {
		return channel.CapYes
	}
	return channel.CapNo
}
