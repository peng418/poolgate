package deepseek

// client.go 渠道实现：能力声明、对话编排（建会话 → 解 PoW → 打 completion → 消费 patch 流）、错误归一。
//
// 注意本渠道的「工具调用」是 **toolshim 模拟**：网页协议没有原生 tools，
// 所以 Spec 里是 Tools=false + ToolsShim=true —— 网关会把工具定义翻成提示词，
// 再把模型输出的标记解析回结构化 tool_calls（对客户端来说合同不变）。

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/toolshim"
)

// Adapter 实现 channel.Channel（登录走 Authorizer + CallbackAcceptor，见 login.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	base    string // API 基址（测试可换）
	wasmURL string
	wasm    wasmCache

	solverMu sync.Mutex
	solver   powSolver // 可注入（测试用桩；生产是 wazero 实现）
}

// powSolver 解一次 PoW 并给出 X-Ds-Pow-Response 头值。
type powSolver interface {
	powHeader(ctx context.Context, ch powChallenge) (string, error)
}

func New() *Adapter {
	return &Adapter{
		clients: map[string]*http.Client{},
		base:    apiBase,
		wasmURL: wasmURL,
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

// SetSolver 注入自定义求解器（测试用；传 nil 回到默认的 wazero 实现）。
func (a *Adapter) SetSolver(s powSolver) {
	a.solverMu.Lock()
	a.solver = s
	a.solverMu.Unlock()
}

func (a *Adapter) Kind() channel.Kind { return channel.DeepSeek }

// Spec 能力声明。
//
// ToolsShim=true 是本渠道的关键：网页协议**没有原生工具调用**（上游把工具定义当普通文本），
// 所以由网关代做模拟。声明成 native 就是撒谎，会让客户端在跑 agent 时撞到「模型把工具调用写成文本」。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.DeepSeek,
		DisplayName:           "DeepSeek（网页版）",
		Status:                channel.Active,
		Category:              channel.CategoryChat, // 登录式，与订阅额度池那类分开
		Tools:                 false,
		ToolsShim:             true,
		Images:                false,
		Reasoning:             true, // THINK 片段会转成 reasoning_content
		SSEOnly:               true,
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs:                  "网页登录 + 服务端解 PoW；工具调用由网关模拟（toolshim）",
	}
}

// Login 是「一次性登录」入口：DeepSeek 的登录需要真实浏览器设备指纹，必须用户参与。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"DeepSeek 需要你在浏览器登录一次：面板「添加账号 → DeepSeek」里粘贴 userToken（不必给密码）").
		WithChannel(string(channel.DeepSeek))
}

// Models 返回网页端档位（本地清单，如实标注；注明工具调用靠模拟）。
func (a *Adapter) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	out := make([]channel.ModelInfo, 0, len(webModels))
	for _, m := range webModels {
		name := m.Name
		if m.Note != "" {
			name += "（" + m.Note + "）"
		}
		out = append(out, channel.ModelInfo{
			ID:            m.ID,
			DisplayName:   name,
			ContextWindow: 131072,
			Source:        channel.SourceLocal,
			Tools:         channel.CapYes, // 客户端可用（由网关模拟）
			Reasoning:     channel.CapYes,
			Images:        channel.CapNo,
		})
	}
	return out, nil
}

// Balance 余额未知（网页端没有余额接口，不猜数字）。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin 无签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "DeepSeek 网页版没有签到"}, nil
}

// Chat 编排一次对话。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	typ, think, known := modelOf(req.Model)
	if !known {
		// 不认识的模型名：用默认档继续，但日志/流水里能看出来（不静默改行为到看不出来）。
		typ, think = "default", true
	}
	session, err := a.createSession(ctx, c)
	if err != nil {
		return nil, err
	}
	ch, err := a.powChallenge(ctx, c)
	if err != nil {
		a.deleteSession(c, session)
		return nil, err
	}
	header, err := a.solvePow(ctx, c, ch)
	if err != nil {
		a.deleteSession(c, session)
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{
		"chat_session_id":   session,
		"parent_message_id": nil,
		"model_type":        typ,
		"prompt":            packMessages(req.Messages),
		"ref_file_ids":      []any{},
		"thinking_enabled":  think,
		"search_enabled":    false,
		"preempt":           false,
	})
	resp, err := a.do(ctx, c, http.MethodPost, a.base+"/chat/completion", body, header)
	if err != nil {
		a.deleteSession(c, session)
		return nil, err
	}
	return newStream(resp.Body, req.Model, session, func() { a.deleteSession(c, session) }), nil
}

// Classify 把上游错误归一成有限枚举。
//
// DeepSeek 的业务错误藏在 200 的信封里（`code` / `biz_code`），HTTP 状态码经常是 200 ——
// 所以信封里的码由 stream/challenge 解析路径单独判（见 jsonBizError），这里只管 HTTP 层。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == 401 || status == 403:
		return errs.SessionDead // token 失效：让用户重新粘一次
	case status == 402:
		return errs.HardCredit
	case status == 429:
		return errs.SoftRate
	case status == 418, status == 202:
		// 参考实现里 202 + x-amzn-waf-action 是 WAF 挑战；我们这条出口实测首页 200，
		// 但换出口后仍可能撞上 —— 归到 UpstreamFault 并带上原话（让用户知道是网络/风控层）。
		return errs.UpstreamFault
	case status == 400 || status == 422:
		switch {
		case strings.Contains(s, "user is muted"):
			return errs.SessionDead
		case strings.Contains(s, "is_banned"):
			return errs.SessionDead
		case strings.Contains(s, "input_exceeds_limit"), strings.Contains(s, "too long"):
			return errs.PromptTooLong
		case strings.Contains(s, "rate_limit"):
			return errs.SoftRate
		}
		return errs.Parse
	case status >= 500:
		return errs.UpstreamFault
	}
	return errs.Parse
}

// ── 内部：建会话 / 挑战 / 求解 / 请求 ──────────────────────────────

func (a *Adapter) createSession(ctx context.Context, c *channel.Credential) (string, error) {
	resp, err := a.do(ctx, c, http.MethodPost, a.base+"/chat_session/create", []byte("{}"), "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Data struct {
			BizData struct {
				ChatSession struct {
					ID string `json:"id"`
				} `json:"chat_session"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Data.BizData.ChatSession.ID == "" {
		return "", bizError(raw, "建会话失败")
	}
	return out.Data.BizData.ChatSession.ID, nil
}

func (a *Adapter) deleteSession(c *channel.Credential, session string) {
	if session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"chat_session_id": session})
	if resp, err := a.do(ctx, c, http.MethodPost, a.base+"/chat_session/delete", body, ""); err == nil {
		resp.Body.Close()
	}
}

func (a *Adapter) powChallenge(ctx context.Context, c *channel.Credential) (powChallenge, error) {
	body, _ := json.Marshal(map[string]any{"target_path": powTargetPath})
	resp, err := a.do(ctx, c, http.MethodPost, a.base+"/chat/create_pow_challenge", body, "")
	if err != nil {
		return powChallenge{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Data struct {
			BizData struct {
				Challenge powChallenge `json:"challenge"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Data.BizData.Challenge.Challenge == "" {
		return powChallenge{}, bizError(raw, "取 PoW 挑战失败")
	}
	return out.Data.BizData.Challenge, nil
}

// solvePow 解挑战并返回 X-Ds-Pow-Response 头（求解器按需下载 WASM 并缓存）。
func (a *Adapter) solvePow(ctx context.Context, c *channel.Credential, ch powChallenge) (string, error) {
	a.solverMu.Lock()
	haveSolver := a.solver != nil
	a.solverMu.Unlock()
	if !haveSolver {
		raw, err := a.wasm.get(ctx, a.clientFor(c), a.wasmURL)
		if err != nil {
			return "", err
		}
		a.solverMu.Lock()
		if a.solver == nil {
			s, err := newSolver(ctx, raw)
			if err != nil {
				a.solverMu.Unlock()
				return "", errs.New(errs.Parse, "初始化 PoW 求解器失败："+err.Error()).
					WithChannel(string(channel.DeepSeek))
			}
			a.solver = s
		}
		a.solverMu.Unlock()
	}
	a.solverMu.Lock()
	s := a.solver
	a.solverMu.Unlock()

	powCtx, cancel := context.WithTimeout(ctx, powTimeout)
	defer cancel()
	h, err := s.powHeader(powCtx, ch)
	if err != nil {
		return "", errs.New(errs.Parse, "解 PoW 失败："+err.Error()).WithChannel(string(channel.DeepSeek))
	}
	return h, nil
}

// do 发一个带客户端标识的请求（powHeader 非空时带上 X-Ds-Pow-Response）。
func (a *Adapter) do(ctx context.Context, c *channel.Credential, method, url string, body []byte, powHeader string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.DeepSeek)).WithCause(err)
	}
	for k, v := range clientHeaders(c) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.Contains(url, "/chat/completion") {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if powHeader != "" {
		req.Header.Set("X-Ds-Pow-Response", powHeader)
	}
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.DeepSeek)).
			WithAccount(c.UID).WithCause(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.DeepSeek)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	return resp, nil
}

// clientHeaders 是网页/App 客户端标识（公开常量；UA 必须是 App 身份，桌面 Chrome UA 会被 WAF 拦）。
func clientHeaders(c *channel.Credential) map[string]string {
	h := map[string]string{
		"User-Agent":               ua,
		"X-Client-Version":         clientVersion,
		"X-Client-Platform":        clientPlatform,
		"X-Client-Locale":          clientLocale,
		"X-Client-Bundle-Id":       clientBundleID,
		"X-Client-Timezone-Offset": clientTimezoneOfset,
		"X-Device-Model":           deviceModel,
	}
	if c != nil {
		if v := strings.TrimSpace(c.Extra["device_id"]); v != "" {
			h["X-Device-Id"] = v
		}
		if t := strings.TrimSpace(c.AccessToken); t != "" {
			h["Authorization"] = "Bearer " + t
		}
	}
	return h
}

// bizError 把业务信封里的错误变成可读错误（上游大量错误是 HTTP 200 + code/biz_code）。
func bizError(raw []byte, what string) error {
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			BizCode int    `json:"biz_code"`
			BizMsg  string `json:"biz_msg"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	kind := errs.Parse
	switch {
	case env.Code == 40003:
		// 信封级 40003「Authorization Failed (invalid token)」= userToken 已失效
		// （改密码/被下线都会让老 token 变这样）→ SessionDead，让用户重新粘一次。
		kind = errs.SessionDead
	case env.Data.BizCode == 10:
		kind = errs.SessionDead // USER_IS_BANNED
	case env.Data.BizCode == 5:
		kind = errs.SessionDead // user is muted（临时禁言，重登也解不了）
	case env.Data.BizCode == 11:
		kind = errs.AuthFailed // RISK_DEVICE_DETECTED（设备指纹不对）
	}
	msg := firstNonEmpty(env.Data.BizMsg, env.Msg)
	if msg == "" {
		msg = truncate(string(raw), 200)
	}
	return errs.New(kind, what+"："+msg).WithChannel(string(channel.DeepSeek)).
		WithUpstream(truncate(string(raw), 200))
}

// packMessages 把多轮消息打成一个 ChatML 提示词（网页协议只有 prompt 一个字段，靠标签表达角色）。
//
// 参考实现实测：用户消息前要补 <｜end▁of▁sentence｜>，末尾必须补 <｜Assistant｜> 锚点。
func packMessages(msgs []channel.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		content := m.Content
		if len(m.ToolCalls) > 0 {
			// 工具调用历史（shim 通常已经转成文本了，这里兜底）
			var parts []string
			for _, tc := range m.ToolCalls {
				parts = append(parts, tc.Function.Name+"("+tc.Function.Arguments+")")
			}
			content = strings.TrimSpace(content + "\n" + strings.Join(parts, "\n"))
		}
		switch role {
		case "system":
			b.WriteString("<｜System｜>" + content + "<｜end▁of▁sentence｜>")
		case "assistant":
			b.WriteString("<｜Assistant｜>" + content + "<｜end▁of▁sentence｜>")
		case "tool":
			b.WriteString("<｜User｜>[工具执行结果] " + content + "<｜end▁of▁sentence｜>")
		default:
			b.WriteString("<｜User｜>" + content + "<｜end▁of▁sentence｜>")
		}
	}
	b.WriteString("<｜Assistant｜>")
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── 流 ────────────────────────────────────────────────────────────

type chunkOrErr struct {
	chunk channel.ChatCompletionChunk
	err   error
}

type stream struct {
	rc      io.ReadCloser
	model   string
	session string
	onClose func()
	ch      chan chunkOrErr
	once    sync.Once
}

func newStream(rc io.ReadCloser, model, session string, onClose func()) *stream {
	s := &stream{rc: rc, model: model, session: session, onClose: onClose, ch: make(chan chunkOrErr, 16)}
	go s.pump()
	return s
}

// pump 解析 SSE：帧以空行分隔，帧内可能有 event:/data: 行。
func (s *stream) pump() {
	defer close(s.ch)
	defer s.once.Do(s.onClose)
	br := bufio.NewReaderSize(s.rc, 256*1024)
	state := &patchState{}
	var event, data strings.Builder
	flush := func() {
		ev := strings.TrimSpace(event.String())
		payload := strings.TrimSpace(data.String())
		event.Reset()
		data.Reset()
		if payload == "" {
			return
		}
		var val map[string]any
		if json.Unmarshal([]byte(payload), &val) != nil {
			return
		}
		// hint = 上游的提示/错误通道（限流、超长等都在这里）
		if ev == "hint" {
			if h := hintError(val); h != nil {
				s.ch <- chunkOrErr{err: h}
			}
			return
		}
		think, content, finished := state.feed(val)
		if think != "" || content != "" || finished {
			s.ch <- chunkOrErr{chunk: chunkOf(s.model, think, content, finished)}
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			s.ch <- chunkOrErr{err: err}
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			flush()
		case strings.HasPrefix(trimmed, "event:"):
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(trimmed, "event:")))
		case strings.HasPrefix(trimmed, "data:"):
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(strings.TrimPrefix(trimmed, "data:"))
		}
		if err == io.EOF {
			flush()
			return
		}
	}
}

// hintError 把 hint 帧里的错误转成可读错误（限流/超长最常见）。
func hintError(val map[string]any) error {
	content, _ := val["content"].(string)
	fr, _ := val["finish_reason"].(string)
	if content == "" && fr == "" {
		return nil
	}
	kind := errs.UpstreamFault
	switch {
	case strings.Contains(fr, "rate_limit"), strings.Contains(content, "rate limit"):
		kind = errs.SoftRate
	case strings.Contains(fr, "input_exceeds_limit"), strings.Contains(content, "too long"):
		kind = errs.PromptTooLong
	}
	return errs.New(kind, "上游提示："+firstNonEmpty(content, fr)).WithChannel(string(channel.DeepSeek))
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error {
	s.once.Do(s.onClose)
	return s.rc.Close()
}

var _ = toolshim.OpenTag // 工具标记由 toolshim 统一定义（本渠道用它做模拟），此处仅作编译期关联
