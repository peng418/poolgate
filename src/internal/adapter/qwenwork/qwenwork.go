// Package qwenwork 是 PoolGate 的千问办公适配器：实现 channel.Channel。
//
// 与 wild-work 的旧实现不同，本适配器走**网页端协议**（2026-09-24 实测确认）：
//
//	网页域 qwenwork.cn 的 cookie token（与桌面端 token 同源，直接可用）
//	  → POST /api/chat-sessions 建会话
//	  → GET /api/chat-ws（WebSocket）join + new_prompt
//	  → 服务端推 session/update 帧（agent_thought_chunk / agent_message_chunk）
//
// 为什么换路径：桌面端网关 gateway.qwenwork.cn 在 2026-09 被上游加闸门，
// 正确签名与档位也返回信封 503 Model catalog unavailable（wild-work Issue #31）。
// 网页端走的是另一套服务，同账号可用 —— 这正是「上游改版就换路」的落点。
//
// 协议要点（实测）：
//   - 鉴权：Cookie: token=<JWT>。**这个 JWT 必须是 device_token**，不是 OAuth 授权码
//     换出来的 access token —— 后者打网页域一律 401
//     {"code":"invalid-credential","msg":"Invalid JWT token"}（2026-09-25 用户实测：
//     面板里「重新登录」多少次都一样）。拿 device_token 的唯一途径是桌面网关的
//     /api/v1/deviceToken/refresh（body {refresh_token, target:"c"}），
//     与 wild-work 的 RefreshToken 做法一致（它的备忘里也写着 OAuth 首 token 会 401）。
//   - 帧：JSON-RPC 2.0 over WebSocket 文本帧，外层 {"type":"control","payload":{...}}。
//   - 流式内容在 params.update.sessionUpdate：
//     agent_thought_chunk=思考、agent_message_chunk=正文、agent_tool_call=工具。
//   - 轮次结束：session_status.sessionStatus 由 running 回到 idle。
//   - 无签到接口（实测全抓包无 daily-claim / checkin）→ 标注「无活动」。
package qwenwork

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 上游端点。
const (
	WebBase = "https://qwenwork.cn"
	// GatewayBase 是桌面网关。chat 走它已被上游加闸门（信封 503），但
	// **deviceToken/refresh 仍然可用** —— 而它正是拿到「网页域认的那个 token」的入口。
	GatewayBase = "https://gateway.qwenwork.cn"
	// epDeviceToken 用 refresh_token 换 device_token（body {refresh_token,target:"c"}）。
	epDeviceToken = "/api/v1/deviceToken/refresh"

	epSessions = "/api/chat-sessions"
	epChatWS   = "/api/chat-ws"
	epBalance  = "/user/balance"
	epUserInfo = "/user/info"
	epChatMode = "/api/chat-modes"
	epWallets  = "/user/wallets"

	wsChannel = "qwenwork_web"

	// 与浏览器一致的 UA：上游对非浏览器 UA 可能另眼相待。
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
)

// staticModels 是网页端 /api/chat-modes 拿不到时的兜底档位表（实测三档）。
// Tools 一律 CapNo：不是「不知道」，是本渠道的协议位置确实放不下工具定义。
var staticModels = []channel.ModelInfo{
	{ID: "pro", DisplayName: "Pro", ContextWindow: 200000, Source: channel.SourceLocal,
		Tools: channel.CapNo, Reasoning: channel.CapYes, Images: channel.CapNo},
	{ID: "flash", DisplayName: "Flash", ContextWindow: 200000, Source: channel.SourceLocal,
		Tools: channel.CapNo, Reasoning: channel.CapYes, Images: channel.CapYes},
	{ID: "qwen3.8-max-preview", DisplayName: "Qwen3.8 Max (Preview)", ContextWindow: 200000,
		Source: channel.SourceLocal, Tools: channel.CapNo, Reasoning: channel.CapYes, Images: channel.CapYes},
}

// modelKeys 客户端模型名 → 上游档位 key。兼容旧别名。
var modelKeys = map[string]string{
	"auto":                "pro",
	"qwork-advanced":      "pro",
	"qwork-auto":          "pro",
	"flash":               "flash",
	"qwork-lite":          "flash",
	"qwen3.8-max":         "qwen3.8-max-preview",
	"qmodel_latest":       "qwen3.8-max-preview",
	"qwen3.8-max-preview": "qwen3.8-max-preview",
	"pro":                 "pro",
}

// Adapter 实现 channel.Channel（千问办公渠道）。
type Adapter struct {
	http *http.Client
	base string
	// tokenURL 是 OAuth token 兑换端点；deviceURL 是 deviceToken 换发端点。
	// 两者都字段化，便于测试指向 mock server。
	tokenURL  string
	deviceURL string
	// wsDialer 供测试注入。
	wsDialer *websocket.Dialer

	// 网页端上游对同一账号只允许一个进行中的会话：
	// 并发建会话会返回 CONCURRENT_OPERATION「Already handling session start
	// request for this user」。因此按账号串行化对话 —— 这是协议约束，不是性能取舍。
	mu     sync.Mutex
	locked map[string]*sync.Mutex
}

// New 建立适配器。
func New() *Adapter { return NewWithTimeout(180 * time.Second) }

// NewWithTimeout 建立带超时的适配器。
func NewWithTimeout(timeout time.Duration) *Adapter {
	return &Adapter{
		http:      &http.Client{Timeout: timeout},
		base:      WebBase,
		tokenURL:  OAuthTokenURL,
		deviceURL: GatewayBase + epDeviceToken,
		wsDialer:  &websocket.Dialer{HandshakeTimeout: 20 * time.Second},
		locked:    map[string]*sync.Mutex{},
	}
}

// lockFor 取某账号的串行锁（懒建）。
func (a *Adapter) lockFor(uid string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.locked[uid]
	if !ok {
		l = &sync.Mutex{}
		a.locked[uid] = l
	}
	return l
}

// NewWithBase 供测试指定上游基址。
func NewWithBase(base string, hc *http.Client) *Adapter {
	a := New()
	if base != "" {
		a.base = base
	}
	if hc != nil {
		a.http = hc
	}
	return a
}

// Kind 返回渠道标识。
func (a *Adapter) Kind() channel.Kind { return "qwenwork" }

// Spec 返回能力声明。签到：实测无签到接口 → CheckinCap=false（UI 显示「无活动」）。
//
// Tools=false：网页端协议是 WebSocket 的 `new_prompt`（纯文本），没有放工具定义的
// 位置，也拿不到结构化 tool_calls —— 与其静默丢掉客户端传来的 tools（会让上游把
// 工具调用写成文本，coding agent 直接不可用），不如如实声明不支持，由网关明确拒绝。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:        "qwenwork",
		DisplayName: "千问办公",
		Status:      channel.Active,
		Category:    channel.CategoryCoding, // 编程助手类
		Tools:       false,
		Images:      true,
		Reasoning:   true,
		SSEOnly:     true, // 上游只有流式（WebSocket 推流），非流式由本地聚合
		CheckinCap:  false,
		Docs:        "网页端协议（chat-ws）；桌面网关已被上游闸门拦截",
	}
}

// Login 返回明确错误：本渠道不在面板里做交互式授权。
func (a *Adapter) Login(ctx context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse, "千问办公不支持面板授权，请用 poolgate migrate 从 wild-work 导入凭证")
}

// ---------------------------------------------------------------------------
// 凭证刷新
// ---------------------------------------------------------------------------

// Refresh 用 refresh_token 换 **device_token**（桌面网关的 deviceToken 换发端点）。
//
// 这里必须抄 wild-work 的做法，原因实测得很清楚：
//
//	OAuth 授权码兑换出来的 access token 拿去做网页域请求会 401
//	{"code":"invalid-credential","msg":"Invalid JWT token"}；
//	而 /api/v1/deviceToken/refresh（body {refresh_token, target:"c"}）换回的
//	device_token 才是网页域 JWT 校验认的那个（wild-work 的备忘里写得很直白：
//	「若 Poll 兑换的首个 token 无效（实测 OAuth 兑换 token 调 /user/info 会 401
//	invalid-credential），refreshIfSessionDead 会自动换新 token」）。
//
// 我们之前直接拿 OAuth access token 当 cookie 用，于是用户看到的永远是
// 「千问重新登录了，还是一样报错」—— 因为每次授权拿到的都是那个网页域不认的 token。
//
// 约定（与 pool.RefreshNow 配合）：拿不出新 token 时返回 nil，让调用方按 401 处理。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if c.RefreshToken == "" {
		return nil, nil // 没有 refresh token（如从 wild-work 导入的老凭证）：刷不了就说刷不了
	}
	dt, rotated, exp, err := a.fetchDeviceToken(ctx, c.RefreshToken)
	if err != nil {
		return nil, err
	}
	nc := *c
	nc.AccessToken = dt
	nc.RefreshToken = rotated
	nc.ExpiresAt = exp
	// device_token 里才有 username（OAuth 兑换的那个实测没有）：昵称空着就补上，
	// 免得面板显示一串 hex uid。
	if nc.Nickname == "" {
		if _, nick := parseJWTIdentity(dt); nick != "" {
			nc.Nickname = nick
		}
	}
	return &nc, nil
}

// fetchDeviceToken 调桌面网关的 deviceToken 换发端点，返回 (device_token, 轮换后的 refresh_token, 到期时间)。
//
// 上游错误形如 {"errorCode":"INVALID_REFRESH_TOKEN","errorMessage":"refresh token is invalid",...}，
// 原话照旧带回去（红线一）。
func (a *Adapter) fetchDeviceToken(ctx context.Context, refreshToken string) (string, string, time.Time, error) {
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken, "target": "c"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.deviceURL, bytes.NewReader(body))
	if err != nil {
		return "", "", time.Time{}, errs.New(errs.Transport, "构造 deviceToken 请求失败").WithCause(err).
			WithChannel(string(channel.QwenWork))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", "", time.Time{}, errs.New(errs.Transport, "deviceToken 请求失败").WithCause(err).
			WithChannel(string(channel.QwenWork))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := errs.UpstreamFault
		msg := "deviceToken 换发失败"
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// refresh_token 被拒 = 这个号必须重新授权。面板与客户端据此提示「重新登录」。
			kind = errs.SessionDead
			msg = "千问办公的 refresh_token 已失效，需要重新授权"
		}
		return "", "", time.Time{}, errs.New(kind, msg).
			WithChannel(string(channel.QwenWork)).WithUpstream(truncate(string(raw), 300))
	}
	var out struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"` // 毫秒（上游口径）
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", time.Time{}, errs.New(errs.Parse, "deviceToken 响应无法解析").
			WithCause(err).WithChannel(string(channel.QwenWork)).WithUpstream(truncate(string(raw), 300))
	}
	dt := out.DeviceToken
	if dt == "" {
		dt = out.Token
	}
	if dt == "" || out.RefreshToken == "" {
		// 半份 token 比没有更糟：只换了 access 不换 refresh，下次刷新必失败。
		return "", "", time.Time{}, errs.New(errs.Parse, "deviceToken 响应不完整（缺 device_token 或 refresh_token）").
			WithChannel(string(channel.QwenWork)).WithUpstream(truncate(string(raw), 300))
	}
	exp := time.Time{}
	switch {
	case out.ExpiresIn > 0:
		exp = time.Now().Add(time.Duration(out.ExpiresIn) * time.Millisecond)
	case out.ExpiresAt != "":
		if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
			exp = t
		}
	}
	if exp.IsZero() {
		// 上游没给存活时间：按观测寿命保守取 24 小时（wild-work 同做法）。
		exp = time.Now().Add(24 * time.Hour)
	}
	return dt, out.RefreshToken, exp, nil
}

// ---------------------------------------------------------------------------
// 目录 / 账务
// ---------------------------------------------------------------------------

type chatModesResp struct {
	Qwork []struct {
		Key         string  `json:"key"`
		PriceFactor float64 `json:"price_factor"`
		Enable      bool    `json:"enable"`
		IsReasoning bool    `json:"is_reasoning"`
		IsVL        bool    `json:"is_vl"`
		// DisplayName 是**扁平**字段。实测上游 /api/chat-modes 返回的档位形如
		// {"key":"flash","display_name":"标准",...}（wild-work 备忘 §6.4 的实测报文，
		// 它的 modelEntry 也是按扁平 display_name 解析的）。只解析嵌套的
		// i18n.display_name 会取不到值，面板上就会把 key（pro/flash）当展示名。
		DisplayName   string `json:"display_name"`
		MaxInputToken int    `json:"max_input_tokens"`
		I18n          struct {
			DisplayName map[string]string `json:"display_name"`
		} `json:"i18n"`
	} `json:"qwork"`
}

// Models 拉取可用档位。来源标注为「上游」（这是上游实时返回的）。
//
// 拉不到时回退静态兜底表 —— 目录接口偶发失败不该让面板看起来「一个模型都没有」。
// 但回退项的 Source 标 local，不冒充上游值。
func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	raw, err := a.get(ctx, c, epChatMode)
	if err != nil {
		return staticModels, nil
	}
	var resp chatModesResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return staticModels, nil
	}
	out := []channel.ModelInfo{}
	for _, m := range resp.Qwork {
		if !m.Enable || m.Key == "" {
			continue
		}
		// 展示名优先级：i18n.zh（若上游确实给了）→ 扁平 display_name（实测口径）→ key。
		// 兜底顺序照 wild-work 的 displayName()：i18n 缺失时回退 display_name。
		name := m.Key
		if n := m.I18n.DisplayName["zh"]; n != "" {
			name = n
		} else if m.DisplayName != "" {
			name = m.DisplayName
		}
		mi := channel.ModelInfo{
			ID:            m.Key,
			DisplayName:   name,
			ContextWindow: m.MaxInputToken,
			Source:        channel.SourceUpstream,
			// 本渠道不实现工具调用（协议位置不存在），能力位如实标不支持；
			// 网关收到带 tools 的请求会明确拒绝，不会静默丢掉。
			Tools:     channel.CapNo,
			Reasoning: capOf(m.IsReasoning),
			Images:    capOf(m.IsVL),
		}
		if mi.ContextWindow <= 0 {
			// 上游没给窗口：不猜数字，标未知。
			mi.ContextWindow = 0
			mi.Source = channel.SourceUnknown
		}
		out = append(out, mi)
	}
	if len(out) == 0 {
		return staticModels, nil
	}
	return out, nil
}

func capOf(b bool) channel.Cap {
	if b {
		return channel.CapYes
	}
	return channel.CapNo
}

// Balance 读网页域余额（实测 /user/balance 返回 {balance, freeze_credit}）。
func (a *Adapter) Balance(ctx context.Context, c *channel.Credential) (channel.Balance, error) {
	raw, err := a.get(ctx, c, epBalance)
	if err != nil {
		return channel.Balance{}, err
	}
	var resp struct {
		Code string `json:"code"`
		Data struct {
			Balance float64 `json:"balance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		// 带上游原话（红线一：账户类错误必须能定位到上游说了什么）。
		return channel.Balance{}, errs.New(errs.Parse, "余额响应无法解析").
			WithCause(err).WithChannel("qwenwork").WithUpstream(truncate(string(raw), 300))
	}
	if resp.Code != "ok" {
		return channel.Balance{Known: false}, nil
	}
	return channel.Balance{Credits: int64(resp.Data.Balance), Known: true}, nil
}

// Checkin 千问办公没有签到活动 —— 如实标注，不假装成功（F1.8）。
func (a *Adapter) Checkin(ctx context.Context, c *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{
		OK:         true,
		NoActivity: true,
		Message:    "该渠道无签到活动（实测网页端不存在 daily-claim / checkin 接口）",
	}, nil
}

// ---------------------------------------------------------------------------
// 对话：WebSocket + JSON-RPC
// ---------------------------------------------------------------------------

// Chat 发起一次对话，返回归一化的标准 OpenAI chunk 流。
//
// 实现：建会话 → 连 chat-ws → join → new_prompt → 把 session/update 帧翻译成 chunk。
//
// 同一账号的对话按顺序串行：上游一次只允许一个进行中的会话，
// 并发会直接返回 CONCURRENT_OPERATION。锁持有到流关闭为止。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	model := a.resolveModel(req.Model)

	lk := a.lockFor(c.UID)
	lk.Lock()

	sessionID, err := a.createSession(ctx, c, model)
	if err != nil {
		lk.Unlock()
		return nil, err
	}

	conn, _, err := a.wsDialer.DialContext(ctx,
		wsURL(a.base)+epChatWS+"?token="+urlQueryEscape(c.AccessToken),
		http.Header{
			"Cookie":     {"token=" + c.AccessToken},
			"User-Agent": {userAgent},
			"Origin":     {a.base},
		})
	if err != nil {
		lk.Unlock()
		return nil, errs.New(errs.Transport, "连接上游 chat-ws 失败").WithCause(err).WithChannel("qwenwork")
	}

	s := &wsStream{
		conn:      conn,
		ctx:       ctx,
		sessionID: sessionID,
		model:     model,
		chunks:    make(chan channel.ChatCompletionChunk, 64),
		errc:      make(chan error, 1),
		release:   func() { lk.Unlock() },
	}
	go s.run(a.promptText(req))
	return s, nil
}

// resolveModel 把客户端模型名映射到上游档位 key。
func (a *Adapter) resolveModel(m string) string {
	m = strings.TrimSpace(m)
	if m == "" {
		return "pro"
	}
	if k, ok := modelKeys[m]; ok {
		return k
	}
	return m
}

// promptText 把 messages 拼成单条 prompt。上游服务端无状态，多轮必须带全量上下文。
func (a *Adapter) promptText(req channel.ChatRequest) string {
	if len(req.Messages) == 1 {
		return req.Messages[0].Content
	}
	var b strings.Builder
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			b.WriteString("[系统] " + m.Content + "\n\n")
		case "assistant":
			b.WriteString("[助手] " + m.Content + "\n\n")
		default:
			b.WriteString(m.Content + "\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// createSession 建一个对话会话，返回 session id。
func (a *Adapter) createSession(ctx context.Context, c *channel.Credential, mode string) (string, error) {
	body, _ := json.Marshal(map[string]any{"mode": mode})
	raw, _, err := a.do(ctx, c, http.MethodPost, epSessions, body)
	if err != nil {
		return "", err
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.ID == "" {
		return "", errs.New(errs.Parse, "建会话失败：响应缺少 id").
			WithChannel("qwenwork").WithUpstream(truncate(string(raw), 300))
	}
	return resp.ID, nil
}

// ---------------------------------------------------------------------------
// wsStream：把 WebSocket 帧翻译成 OpenAI chunk 流
// ---------------------------------------------------------------------------

// wsFrame 是上游的外层帧：{"type":"control"|"data","payload":{...}}。
type wsFrame struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// rpcEnvelope 是 payload 里的 JSON-RPC 信封。
type rpcEnvelope struct {
	ID     any             `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Params json.RawMessage `json:"params"`
}

type wsStream struct {
	conn      *websocket.Conn
	ctx       context.Context
	sessionID string
	model     string

	chunks chan channel.ChatCompletionChunk
	errc   chan error

	mu     sync.Mutex
	closed bool
	id     int64
	// release 释放账号级串行锁；Close 与读循环结束都会调用（只生效一次）。
	release   func()
	releaseMu sync.Once
	// seq 是 new_prompt 的 JSON-RPC id。
	seq int
	// started 表示已收到过 running 状态；idle 只在 running 之后才算结束，
	// 否则 join 时那一下 idle 会把流提前关掉。
	started bool
	// wroteContent 记录是否真的收到过内容（判据用）。
	wroteContent bool
}

// Next 实现 channel.Stream：返回下一个归一化 chunk。
func (s *wsStream) Next() (channel.ChatCompletionChunk, error) {
	select {
	case c, ok := <-s.chunks:
		if !ok {
			return channel.ChatCompletionChunk{}, io.EOF
		}
		return c, nil
	case err := <-s.errc:
		return channel.ChatCompletionChunk{}, err
	case <-s.ctx.Done():
		// 超时/取消要如实报成 Transport，不能落成 Parse ——
		// 「响应解析不了」和「等超时了」对排查是两件完全不同的事。
		return channel.ChatCompletionChunk{}, errs.New(errs.Transport, "上游响应超时或被取消").
			WithCause(s.ctx.Err()).WithChannel("qwenwork")
	}
}

// Close 关闭底层连接并释放账号级串行锁。
func (s *wsStream) Close() error {
	s.releaseOnce()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.conn.Close()
}

// releaseOnce 释放串行锁（幂等）。
func (s *wsStream) releaseOnce() {
	if s.release != nil {
		s.releaseMu.Do(s.release)
	}
}

// run 是读循环：join → new_prompt → 读帧直到 idle。
func (s *wsStream) run(prompt string) {
	defer close(s.chunks)
	defer s.conn.Close()
	defer s.releaseOnce()

	// 1) join
	if err := s.sendRPC("join", map[string]any{"sessionId": s.sessionID, "lastSeqId": 0}); err != nil {
		s.fail(errs.New(errs.Transport, "join 发送失败").WithCause(err).WithChannel("qwenwork"))
		return
	}
	// 2) new_prompt
	if err := s.sendRPC("new_prompt", map[string]any{
		"sessionId": s.sessionID,
		"text":      prompt,
		"_meta":     map[string]any{"channel": wsChannel},
	}); err != nil {
		s.fail(errs.New(errs.Transport, "new_prompt 发送失败").WithCause(err).WithChannel("qwenwork"))
		return
	}

	// 3) 读帧
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(180 * time.Second))
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			if s.wroteContent {
				return // 已出内容，连接关闭视作正常结束
			}
			s.fail(errs.New(errs.Transport, "上游连接中断且未收到内容").
				WithCause(err).WithChannel("qwenwork"))
			return
		}
		if done := s.handleFrame(raw); done {
			return
		}
	}
}

// handleFrame 处理一帧；返回 true 表示本轮结束。
func (s *wsStream) handleFrame(raw []byte) bool {
	var f wsFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return false
	}
	var env rpcEnvelope
	if err := json.Unmarshal(f.Payload, &env); err != nil {
		return false
	}
	// 心跳：服务端 ping 必须回 pong，否则连接会被断。
	if env.Method == "ping" {
		_ = s.sendRaw(map[string]any{
			"type":    "control",
			"payload": map[string]any{"jsonrpc": "2.0", "method": "pong"},
		})
		return false
	}
	// JSON-RPC 的错误有两种帧形态：错误通知（method="error"）与错误响应
	// （只有 id + error，没有 method）。这里按 spec 在 method 分派之前统一判 error，
	// 否则错误响应会被当成无关帧丢掉，用户最终只等到一条通用的「连接中断/超时」文案，
	// 上游原话（红线一）就没了 —— 与 TraeWork 流内错误未归一导致文案被网关换掉是同一类问题。
	if env.Error != nil {
		s.fail(s.classifyRPC(env.Error.Code, env.Error.Message))
		return true
	}
	switch env.Method {
	case "session/update":
		s.emitUpdate(env.Params)
	case "session_status":
		var p struct {
			SessionStatus string `json:"sessionStatus"`
		}
		if json.Unmarshal(env.Params, &p) == nil {
			if p.SessionStatus == "running" {
				s.started = true
			}
			if p.SessionStatus == "idle" && s.started {
				return true
			}
		}
	}
	return false
}

// updateParams 是 session/update 的 params。
type updateParams struct {
	Update struct {
		Content struct {
			Text string `json:"text"`
			Type string `json:"type"`
		} `json:"content"`
		SessionUpdate string `json:"sessionUpdate"`
		MessageID     string `json:"messageId"`
	} `json:"update"`
}

// emitUpdate 把一条 session/update 翻成 OpenAI chunk。
//
// 实测的 sessionUpdate 取值：
//
//	agent_thought_chunk —— 思考（映射到 reasoning_content）
//	agent_message_chunk —— 正文（映射到 content）
func (s *wsStream) emitUpdate(params json.RawMessage) {
	var p updateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	text := p.Update.Content.Text
	switch p.Update.SessionUpdate {
	case "agent_message_chunk", "agent_thought_chunk":
		if text == "" {
			return
		}
		delta := channel.ChunkChoice{Index: 0}
		delta.Delta.Role = "assistant"
		if p.Update.SessionUpdate == "agent_thought_chunk" {
			delta.Delta.ReasoningContent = text
		} else {
			delta.Delta.Content = text
			s.wroteContent = true
		}
		s.push(channel.ChatCompletionChunk{
			ID:      "chatcmpl-" + p.Update.MessageID,
			Model:   s.model,
			Choices: []channel.ChunkChoice{delta},
		})
	}
}

// push 投递一个 chunk；流已关闭时静默丢弃。
func (s *wsStream) push(c channel.ChatCompletionChunk) {
	select {
	case s.chunks <- c:
	case <-s.ctx.Done():
	}
}

// fail 投递一个终止错误。
func (s *wsStream) fail(err error) {
	select {
	case s.errc <- err:
	case <-s.ctx.Done():
	}
}

// sendRPC 发一个 JSON-RPC 请求帧。
func (s *wsStream) sendRPC(method string, params map[string]any) error {
	s.mu.Lock()
	s.seq++
	id := s.seq
	s.mu.Unlock()
	return s.sendRaw(map[string]any{
		"type": "control",
		"payload": map[string]any{
			"jsonrpc": "2.0", "method": method, "params": params, "id": id,
		},
	})
}

func (s *wsStream) sendRaw(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	return s.conn.WriteMessage(websocket.TextMessage, raw)
}

// classifyRPC 把上游 JSON-RPC 错误归一成 errs.Kind。
// 标记词与 Classify 共用同一套（wild-work 的 hardMarkers 等），
// 避免流内错误和 HTTP 错误对同一句上游原话给出不同分类。
func (s *wsStream) classifyRPC(code int, msg string) error {
	low := strings.ToLower(msg)
	k := errs.UpstreamFault
	switch {
	case hasAnyMarker(low, hardMarkers):
		k = errs.HardCredit
	case hasAnyMarker(low, rateMarkers):
		k = errs.SoftRate
	case hasAnyMarker(low, contentBlockMarkers):
		k = errs.ContentBlocked
	case strings.Contains(low, "unauthor") || strings.Contains(low, "token") ||
		strings.Contains(low, "expired") || code == 4001:
		k = errs.SessionDead
	}
	return errs.New(k, "上游返回错误："+msg).WithChannel("qwenwork").WithUpstream(msg)
}

// ---------------------------------------------------------------------------
// HTTP 小工具
// ---------------------------------------------------------------------------

func (a *Adapter) get(ctx context.Context, c *channel.Credential, path string) ([]byte, error) {
	raw, _, err := a.do(ctx, c, http.MethodGet, path, nil)
	return raw, err
}

// do 发一个带 cookie 鉴权的请求。网页域认 Cookie: token，不认 Bearer。
func (a *Adapter) do(ctx context.Context, c *channel.Credential, method, path string, body []byte) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rdr)
	if err != nil {
		// 构造请求失败属「本地/传输」范畴，不是「响应解析不了」。
		// 归 Transport 与同文件 fetchDeviceToken（及全仓 http.NewRequestWithContext
		// 失败的通行做法，如 chatglm/copilot/deepseek）一致；此前错标 Parse 会把
		// 这类错误说成「上游回了看不懂的东西」，误导排查方向。
		return nil, 0, errs.New(errs.Transport, "构造上游请求失败").WithCause(err).WithChannel("qwenwork")
	}
	req.Header.Set("Cookie", "token="+c.AccessToken)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", a.base)
	req.Header.Set("Referer", a.base+"/app/chat")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, 0, errs.New(errs.Transport, "请求上游失败").WithCause(err).WithChannel("qwenwork")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, errs.New(errs.Transport, "读取上游响应失败").WithCause(err).WithChannel("qwenwork")
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, errs.New(a.Classify(resp.StatusCode, raw), "上游返回 HTTP "+
			fmt.Sprint(resp.StatusCode)).WithChannel("qwenwork").
			WithUpstream(truncate(string(raw), 400))
	}
	return raw, resp.StatusCode, nil
}

// 下列标记词表逐条对照 wild-work internal/qwenwork/client.go 的 Classify/hardMarkers
// （真上游实测过的清单）。早期移植只留了几个英文宽匹配词，上游用中文措辞
// （「积分不足」「余额不足」）或其它英文说法（"not enough credit"）时会被误判成 Parse
// ——「上游故障」与「账号额度耗尽」在冷却策略上处置完全不同，分类错了会误杀好号（红线一）。
var (
	// hardMarkers 余额/权益不足。
	hardMarkers = []string{
		// wild-work hardMarkers 原表（含中文）。
		"insufficient credit", "no credit", "credit exhausted", "out of credit",
		"quota exceeded", "quota exhaust", "payment required", "credit not enough",
		"not enough credit", "credit is not enough",
		"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
		// 移植时原有的宽匹配词，保留以免回归。
		"credits", "quota", "insufficient", "balance",
	}
	// rateMarkers 限流。429 之外，上游也常用 200/400 带这些文案回。
	rateMarkers = []string{
		"rate limit", "too many requests", "too many", "usage limit", "请求过于频繁",
		// 同账号并发建会话被拒：限流性质，短冷却后重试即可，不算账号故障。
		"concurrent_operation",
	}
	// contentBlockMarkers 内容拦截。marker 取自 wild-work Classify 的 400 分支。
	contentBlockMarkers = []string{
		"blocked by security policy", "content filter", "content blocked",
		"检测到敏感内容", "敏感内容",
	}
	// promptTooLongMarkers 超出上下文窗口（wild-work Classify 同款判据）。
	promptTooLongMarkers = []string{"prompt is too long", "context length", "maximum context"}
)

// hasAnyMarker 报告低文化后的 body 是否含任一标记词。
func hasAnyMarker(low string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// Classify 把上游错误归一成有限枚举（D4）。
// 判定顺序与 wild-work 的 Classify 对齐：状态码先定「明确的」，再看 body 标记；
// 403 先排除内容拦截，避免把「内容违规」当成账号失效去冷却好号。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	low := strings.ToLower(string(body))
	// 402 Payment Required 直接是额度不足（wild-work client.go:622 首判）。
	if status == http.StatusPaymentRequired {
		return errs.HardCredit
	}
	// 401 是明确的登录态失效。
	if status == http.StatusUnauthorized {
		return errs.SessionDead
	}
	// 403 不一定是账号问题：内容拦截也常用 403（wild-work 会先看 body 标记再归类）。
	if status == http.StatusForbidden {
		if hasAnyMarker(low, contentBlockMarkers) {
			return errs.ContentBlocked
		}
		if hasAnyMarker(low, hardMarkers) {
			return errs.HardCredit
		}
		return errs.SessionDead
	}
	// 429 优先于额度标记：限流响应常带 quota exceeded（wild-work 备忘 §6.15 的取舍）。
	if status == http.StatusTooManyRequests {
		return errs.SoftRate
	}
	// 5xx 是上游故障，先于 body 标记判定：body 里偶然出现 "balance"/"quota" 这类宽匹配词
	// 也不能把服务端故障算到账号头上（记错账会冷却好号）。
	if status >= 500 {
		return errs.UpstreamFault
	}
	if hasAnyMarker(low, hardMarkers) {
		return errs.HardCredit
	}
	if hasAnyMarker(low, rateMarkers) {
		return errs.SoftRate
	}
	if hasAnyMarker(low, contentBlockMarkers) {
		return errs.ContentBlocked
	}
	if hasAnyMarker(low, promptTooLongMarkers) {
		return errs.PromptTooLong
	}
	if strings.Contains(low, "not available for this user") {
		return errs.ModelUnavailable
	}
	if status == 404 {
		return errs.ModelUnavailable
	}
	return errs.Parse
}

// wsURL 把 https 基址换成 wss。
func wsURL(base string) string {
	return strings.Replace(strings.Replace(base, "https://", "wss://", 1), "http://", "ws://", 1)
}

// urlQueryEscape 只转义 token 里的保留字符，避免引入 net/url 的整串重编码。
func urlQueryEscape(s string) string {
	r := strings.NewReplacer(
		"%", "%25", "&", "%26", "=", "%3D", "?", "%3F", "#", "%23", "+", "%2B", " ", "%20",
	)
	return r.Replace(s)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
