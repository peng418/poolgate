package chatglm

// client.go 渠道实现：能力声明、凭证换取（refresh_token → access_token）、对话编排、错误归一。
//
// 本渠道的「工具调用」是 **toolshim 模拟**：网页协议没有原生 tools，所以 Spec 是
// Tools=false + ToolsShim=true —— 网关把工具定义翻成提示词，再把模型输出的标记解析回
// 结构化 tool_calls（对客户端来说合同不变）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// refreshBuffer 是 access_token 的提前刷新量。
const refreshBuffer = 5 * time.Minute

// Adapter 实现 channel.Channel（登录走 Authorizer + CallbackAcceptor，见 login.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	base string // 站点基址（测试可换）

	tokMu  sync.Mutex
	tokens map[string]cachedToken // refresh token → access token（进程内缓存）
}

type cachedToken struct {
	access string
	exp    time.Time
}

func New() *Adapter {
	return &Adapter{
		clients: map[string]*http.Client{},
		base:    apiBase,
		tokens:  map[string]cachedToken{},
	}
}

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

func (a *Adapter) Kind() channel.Kind { return channel.ChatGLM }

// Spec 能力声明。
//
// Reasoning=true：上游把思考放在 item.type=="think" 里单独发，我们映射成 reasoning_content。
// ToolsShim=true：没有原生工具调用，由网关代做模拟（声明成 native 就是撒谎）。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.ChatGLM,
		DisplayName:           "智谱清言（网页版）",
		Status:                channel.Active,
		Category:              channel.CategoryChat,
		Tools:                 false,
		ToolsShim:             true,
		Images:                false,
		Reasoning:             true,
		SSEOnly:               true,
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs:                  "浏览器登录后粘贴 chatglm_refresh_token；每请求带自算签名；工具调用由网关模拟（toolshim）",
	}
}

// Login 是「一次性登录」入口：凭证来自你浏览器里的登录态。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"智谱清言需要你在浏览器登录一次：面板「添加账号 → 智谱清言」里粘贴 chatglm_refresh_token（不必给密码）").
		WithChannel(string(channel.ChatGLM))
}

// Models 返回网页端档位（本地清单，如实标注）。
//
// 上游没有目录接口（模型/智能体是靠 assistant_id 与 chat_mode 决定的），所以只能本地列。
// 名字里带 think / deepresearch 的档位是**同一个模型的不同模式**，不是不同模型 ——
// 面板的说明文案要把这点说清楚，否则用户会以为模型变多了。
func (a *Adapter) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	out := make([]channel.ModelInfo, 0, len(webModels))
	for _, m := range webModels {
		out = append(out, channel.ModelInfo{
			ID:          m.ID,
			DisplayName: m.Name,
			// 上下文长度：上游没给，不猜数字（F4.2）。
			ContextWindow: 0,
			Source:        channel.SourceLocal,
			Tools:         channel.CapYes, // 客户端可用（由网关模拟）
			Reasoning:     capOf(m.Think || m.Deep),
			Images:        channel.CapNo,
		})
	}
	return out, nil
}

func capOf(b bool) channel.Cap {
	if b {
		return channel.CapYes
	}
	return channel.CapNo
}

// Balance 余额未知：网页端没有余额接口，不猜数字。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin 无签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "智谱清言没有签到活动"}, nil
}

// ---------------------------------------------------------------------------
// 凭证
// ---------------------------------------------------------------------------

// secretOf 取本次请求要用的签名密钥：凭证可覆盖，否则用编译进来的默认值。
func secretOf(c *channel.Credential) string {
	if c != nil && c.Extra != nil {
		if v := strings.TrimSpace(c.Extra["sign_secret"]); v != "" {
			return v
		}
	}
	return signSecret
}

// refreshOf 取 refresh token。
//
// 上游的两种令牌都是不透明串（不是 JWT），没法从形态上区分，所以约定：
// RefreshToken 字段优先；只有 AccessToken 时，把它当 refresh token 用（用户直接粘了 access_token
// 也能跑 —— 反正过期后要重新粘，这一点在面板引导语里写明）。
func refreshOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	if v := strings.TrimSpace(c.RefreshToken); v != "" {
		return v
	}
	return strings.TrimSpace(c.AccessToken)
}

// accessToken 取一个可用的 access token：先看进程内缓存，再看凭证上的过期时间，最后去换。
func (a *Adapter) accessToken(ctx context.Context, c *channel.Credential) (string, error) {
	refresh := refreshOf(c)
	if refresh == "" {
		return "", errs.New(errs.SessionDead, "智谱清言凭证为空：请在面板里重新粘贴 chatglm_refresh_token").
			WithChannel(string(channel.ChatGLM))
	}
	a.tokMu.Lock()
	if v, ok := a.tokens[refresh]; ok && time.Now().Before(v.exp.Add(-refreshBuffer)) {
		a.tokMu.Unlock()
		return v.access, nil
	}
	a.tokMu.Unlock()

	// 凭证里已经带了 access token 且没过期（上次登录/续期换来的）就直接用，省一次往返。
	if c != nil && strings.TrimSpace(c.AccessToken) != "" && c.AccessToken != refresh {
		if !c.ExpiresAt.IsZero() && time.Now().Before(c.ExpiresAt.Add(-refreshBuffer)) {
			a.remember(refresh, c.AccessToken, c.ExpiresAt)
			return c.AccessToken, nil
		}
	}

	access, exp, err := a.exchange(ctx, c, refresh)
	if err != nil {
		return "", err
	}
	a.remember(refresh, access, exp)
	return access, nil
}

func (a *Adapter) remember(refresh, access string, exp time.Time) {
	if exp.IsZero() {
		exp = time.Now().Add(accessTokenTTL)
	}
	a.tokMu.Lock()
	a.tokens[refresh] = cachedToken{access: access, exp: exp}
	a.tokMu.Unlock()
}

// forget 丢掉缓存（401 之后必须丢，否则会一直拿死 token 重试）。
func (a *Adapter) forget(refresh string) {
	a.tokMu.Lock()
	delete(a.tokens, strings.TrimSpace(refresh))
	a.tokMu.Unlock()
}

// exchange 用 refresh token 换 access token。
//
// 注意：上游会**轮换** refresh token（返回值里有新的），所以我们要把它一起带上 ——
// 只留 access token 的话，下一次刷新就用旧 refresh token，迟早失效。
// 参考实现丢掉了新 refresh token（只缓存在内存里），那是它的缺陷，我们照实存。
func (a *Adapter) exchange(ctx context.Context, c *channel.Credential, refresh string) (string, time.Time, error) {
	sign := makeSign(secretOf(c))
	resp, err := a.do(ctx, c, http.MethodPost, epRefresh, refresh, []byte("{}"), sign)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", time.Time{}, errs.New(a.Classify(resp.StatusCode, raw), "换 access token 失败").
			WithChannel(string(channel.ChatGLM)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	var out struct {
		Result struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", time.Time{}, errs.New(errs.Parse, "上游没有返回可解析的令牌").
			WithChannel(string(channel.ChatGLM)).WithUpstream(truncate(string(raw), 200))
	}
	access := strings.TrimSpace(out.Result.AccessToken)
	if access == "" {
		// 200 但没有令牌 = 凭证无效（上游习惯用业务码表达）。绝不能当成功。
		return "", time.Time{}, errs.New(errs.SessionDead,
			"上游没有返回 access token：chatglm_refresh_token 可能已失效，请重新粘贴").
			WithChannel(string(channel.ChatGLM)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return access, time.Now().Add(accessTokenTTL), nil
}

// Refresh 是账号级续期：拿 refresh token 再换一次 access token。
// 拿不到就返回 (nil, nil)，让上层按 401 处理，而不是假装成功。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if c == nil {
		return nil, nil
	}
	refresh := refreshOf(c)
	if refresh == "" {
		return nil, nil
	}
	access, exp, err := a.exchange(ctx, c, refresh)
	if err != nil {
		return nil, err
	}
	nc := *c
	nc.AccessToken = access
	nc.RefreshToken = refresh
	nc.ExpiresAt = exp
	a.remember(refresh, access, exp)
	return &nc, nil
}

// checkCredential 当场验一次凭证（登录用）：能换出 access token 就算有效。
func (a *Adapter) checkCredential(ctx context.Context, c *channel.Credential) error {
	access, exp, err := a.exchange(ctx, c, refreshOf(c))
	if err != nil {
		return err
	}
	c.AccessToken = access
	c.ExpiresAt = exp
	a.remember(refreshOf(c), access, exp)
	return nil
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// Chat 发一次对话：拼成单条 user 文本 → 带签名请求 → SSE 里按 parts 快照求增量。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	spec := specOf(req.Model)

	token, err := a.accessToken(ctx, c)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{
		"assistant_id":    spec.assistantID,
		"conversation_id": "",
		"project_id":      "",
		"chat_type":       "user_chat",
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": packMessages(req.Messages)}},
		}},
		"meta_data": map[string]any{
			"channel":             "",
			"chat_mode":           spec.chatMode,
			"draft_id":            "",
			"if_plus_model":       true,
			"input_question_type": "xxxx",
			// is_networking 照参考实现开着：上游的联网检索结果会被它塞进 parts 的
			// tool_result/quote_result 里，我们把这些结果**只放进思考流**（不污染正文）。
			"is_networking": true,
			"is_test":       false,
			"platform":      "pc",
			"quote_log_id":  "",
			"cogview":       map[string]any{"rm_label_watermark": false},
		},
	})

	resp, err := a.do(ctx, c, http.MethodPost, epStream, token, body, makeSign(secretOf(c)))
	if err != nil {
		return nil, err
	}
	// 401 多半是 access token 过期：丢掉缓存换一次再试，只重试一次。
	if resp.StatusCode == 401 {
		resp.Body.Close()
		a.forget(refreshOf(c))
		if token, err = a.accessToken(ctx, c); err != nil {
			return nil, err
		}
		if resp, err = a.do(ctx, c, http.MethodPost, epStream, token, body, makeSign(secretOf(c))); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.ChatGLM)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	// 上游应当回 SSE；若回的是整包 JSON，由流解析器按内容判断（避免「客户端拿到空回复」）。
	return newStream(resp.Body, resp.Header.Get("Content-Type"), req.Model), nil
}

// Classify 把上游错误归一成有限枚举（D4）。
//
// 智谱的习惯是把错误塞在**业务码**里（HTTP 可能是 200），所以 40102（refresh_token 过期）
// 这类只能靠报文关键词认；HTTP 状态码那条路留给网关/CDN 层。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == 401 || status == 403:
		return errs.SessionDead
	case status == 429:
		for _, kw := range []string{"quota", "credit", "balance", "exhaust", "insufficient", "额度"} {
			if strings.Contains(s, kw) {
				return errs.HardCredit
			}
		}
		return errs.SoftRate
	case status == 400 || status == 422:
		switch {
		case strings.Contains(s, "10102"), strings.Contains(s, "40102"), strings.Contains(s, "token"):
			// 业务码类：refresh_token 过期 → 需要重新粘贴
			return errs.SessionDead
		case strings.Contains(s, "context"), strings.Contains(s, "too long"), strings.Contains(s, "长度"):
			return errs.PromptTooLong
		case strings.Contains(s, "sensitive"), strings.Contains(s, "intervene"),
			strings.Contains(s, "审核"), strings.Contains(s, "不合规"), strings.Contains(s, "违规"):
			return errs.ContentBlocked
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

// do 发一个带伪装的请求。
//
// token 为空表示这是「用 refresh token 换 access token」的调用（Authorization 就是 refresh token）。
func (a *Adapter) do(ctx context.Context, c *channel.Credential, method, path, token string, body []byte, sign requestSign) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.ChatGLM)).WithCause(err)
	}
	for k, v := range fakeHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgents[rand.IntN(len(userAgents))])
	req.Header.Set("Authorization", "Bearer "+token)
	// 签名三件套 + 每请求新的设备/请求 id（参考实现就是每请求新生成）。
	req.Header.Set("X-Sign", sign.sign)
	req.Header.Set("X-Timestamp", sign.timestamp)
	req.Header.Set("X-Nonce", sign.nonce)
	req.Header.Set("X-Device-Id", uuidNoDash())
	req.Header.Set("X-Request-Id", uuidNoDash())

	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.ChatGLM)).
			WithAccount(uidOf(c)).WithCause(err)
	}
	return resp, nil
}

func uidOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	return c.UID
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
