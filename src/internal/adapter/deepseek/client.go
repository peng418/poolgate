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

// powSolver 解一次 PoW 并给出对应的请求头值。
//
// 两种载荷形状是上游定的，别合并：
//   - powHeader      → `X-Ds-Pow-Response`（已登录的聊天接口，6 个字段）
//   - guestPowHeader → `X-DS-Guest-PoW-Response`（**没登录时的登录类接口**，只有 salt/answer）
//
// 哈希算法是同一种（DeepSeekHashV1），所以求解本身共用；差别只在往头里塞哪些字段。
type powSolver interface {
	powHeader(ctx context.Context, ch powChallenge) (string, error)
	guestPowHeader(ctx context.Context, ch powChallenge) (string, error)
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
			ID:          m.ID,
			DisplayName: name,
			// 参考实现 default 档 max_input_tokens = 1048576（src/config.rs:231-233）。
			// 之前填的 131072 是臆造值，对齐到参考实现。
			ContextWindow: 1048576,
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
	// parent_message_id：参考实现是 `Option<i64>` + `skip_serializing_if = "Option::is_none"`
	// （client.rs:252-253），首轮**整个字段省略**，而不是发一个 `null`。
	// 空值形态不一致可能被上游当成非法参数（豆包 710020202 common invalid param 的教训）；
	// 我们每轮都新建会话，parent_message_id 恒为首轮 → 不写这个键。
	body, _ := json.Marshal(map[string]any{
		"chat_session_id":  session,
		"model_type":       typ,
		"prompt":           packMessages(req.Messages),
		"ref_file_ids":     []any{},
		"thinking_enabled": think,
		"search_enabled":   false,
		"preempt":          false,
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

// ensureSolver 保证求解器就绪（WASM 按需下载并缓存；并发下只初始化一次）。
//
// 抽出来是因为现在有两个入口要解挑战：聊天（已登录，powHeader）和
// 登录类接口（游客，guestPowHeader）。WASM 只该下载一份。
func (a *Adapter) ensureSolver(ctx context.Context, c *channel.Credential) (powSolver, error) {
	a.solverMu.Lock()
	haveSolver := a.solver != nil
	a.solverMu.Unlock()
	if !haveSolver {
		raw, err := a.wasm.get(ctx, a.clientFor(c), a.wasmURL)
		if err != nil {
			return nil, err
		}
		a.solverMu.Lock()
		if a.solver == nil {
			s, err := newSolver(ctx, raw)
			if err != nil {
				a.solverMu.Unlock()
				return nil, errs.New(errs.Parse, "初始化 PoW 求解器失败："+err.Error()).
					WithChannel(string(channel.DeepSeek))
			}
			a.solver = s
		}
		a.solverMu.Unlock()
	}
	a.solverMu.Lock()
	s := a.solver
	a.solverMu.Unlock()
	return s, nil
}

// solvePow 解挑战并返回 X-Ds-Pow-Response 头（求解器按需下载 WASM 并缓存）。
func (a *Adapter) solvePow(ctx context.Context, c *channel.Credential, ch powChallenge) (string, error) {
	s, err := a.ensureSolver(ctx, c)
	if err != nil {
		return "", err
	}

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
	case env.Code == 1001, env.Code == 1201:
		// 参考实现 response.rs:644-648：信封级 1001/1201 是上游过载/限流 → 当归 SoftRate。
		kind = errs.SoftRate
	case env.Code == 40301:
		// 参考实现 response.rs:646：40301 = INVALID_POW_RESPONSE（PoW 没通过）。
		// 这是上游判定我们的求解结果无效（不是账号坏），归 UpstreamFault。
		kind = errs.UpstreamFault
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
		if role == "assistant" {
			// 客户端历史里的脏正文（上一轮模型自演的剧本）不能原样回灌：模型看到自己上一轮
			// 就是这么写的，就会接着演（真机同一会话连续 3 轮都脏）。用户/工具消息不动（红线一）。
			content = cutAtControlMarker(content)
		}
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
	// 出站哨兵：正文/思考各一个（思考被截断不该终止本轮，正文被截断就是本轮结束，见 guard.go）。
	gThink, gContent := &streamGuard{}, &streamGuard{}
	var event, data strings.Builder

	// raw 保留流开头的一小段原文，只用于「一个有效帧都没解析出来」时把上游原话带出去
	// —— 参考实现 request.rs:654-684 在空流时就是回头翻已收到的字节找 biz_code / JSON 信封。
	const rawCap = 8 << 10
	var raw strings.Builder
	sawSignal := false

	// flush 返回 true 表示应立刻停止读取（上游已通过 hint 明确报错）。
	flush := func() bool {
		ev := strings.TrimSpace(event.String())
		payload := strings.TrimSpace(data.String())
		event.Reset()
		data.Reset()
		if payload == "" {
			return false
		}
		var val map[string]any
		if json.Unmarshal([]byte(payload), &val) != nil {
			return false
		}
		// hint = 上游的提示/错误通道（限流、超长等都在这里）。参考实现把**任何** hint 帧
		// 都当错误、并立即终止（response.rs:143-146 + hint_to_error:511-526），不能静默放过。
		if ev == "hint" {
			s.ch <- chunkOrErr{err: hintError(val, payload)}
			return true
		}
		think, content, finished := state.feed(val)
		// 控制标记永不下发：出现标记即视为模型开始自演下一轮，本轮到此结束（guard.go）。
		tOut, _ := gThink.feed(think)
		cOut, cStop := gContent.feed(content)
		if cStop {
			if gContent.junkOnly() && !gThink.emitted {
				// 整轮都是自演：没有任何正文可给客户端 —— 明确报错（红线二），且记在上游头上
				// （不冷却账号：这不是账号的问题，是模型顺着我们拼的剧本往下写了）。
				dsLogf("本轮整轮都是模型自演（丢弃 %d 字节，起始 %q）", gContent.dropped, gContent.preview(120))
				s.ch <- chunkOrErr{err: errs.New(errs.UpstreamFault, guardUpstreamNote+"，本轮没有有效内容").
					WithChannel(string(channel.DeepSeek)).
					WithUpstream(gContent.preview(200))}
				return true
			}
			dsLogf("正文出现对话模板标记，已截断模型自演的后续轮次（丢弃 %d 字节，起始 %q）",
				gContent.dropped, gContent.preview(120))
			sawSignal = true
			s.ch <- chunkOrErr{chunk: chunkOf(s.model, tOut, cOut, true)}
			return true
		}
		if tOut != "" || cOut != "" || finished {
			sawSignal = true
			s.ch <- chunkOrErr{chunk: chunkOf(s.model, tOut, cOut, finished)}
		}
		return false
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			// 读流中断也必须给结构化错误（普通 error 会被网关换成通用文案，红线一）。
			s.ch <- chunkOrErr{err: errs.New(errs.Transport, "读取上游流失败").
				WithChannel(string(channel.DeepSeek)).WithCause(err)}
			return
		}
		if raw.Len() < rawCap {
			raw.WriteString(line)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			if flush() {
				return
			}
		case strings.HasPrefix(trimmed, "event:"):
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(trimmed, "event:")))
		case strings.HasPrefix(trimmed, "data:"):
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(strings.TrimPrefix(trimmed, "data:"))
		}
		if err == io.EOF {
			if flush() {
				return
			}
			// 收尾：把哨兵扣住的尾巴放行（走到这里说明它不含完整标记）。
			if out := gContent.flush(); strings.TrimSpace(out) != "" {
				sawSignal = true
				s.ch <- chunkOrErr{chunk: chunkOf(s.model, gThink.flush(), out, false)}
			} else if out := gThink.flush(); strings.TrimSpace(out) != "" {
				sawSignal = true
				s.ch <- chunkOrErr{chunk: chunkOf(s.model, out, "", false)}
			}
			// 一个有效帧都没产出：上游多半是「HTTP 200 + JSON 业务错误信封」（DeepSeek 的业务
			// 错误就藏在 200 里）。必须把上游原话（biz_code/biz_msg 或 code/msg）带出去，
			// 否则网关只能回一句通用的「上游返回空流」，原话整句丢失（红线一，TraeWork 教训）。
			if !sawSignal {
				s.ch <- chunkOrErr{err: streamEnvelopeError(raw.String())}
			}
			return
		}
	}
}

// hintError 把 hint 帧归一成结构化错误（限流/超长最常见）。
//
// 对齐参考实现 hint_to_error（response.rs:511-526）：优先看 content、其次 finish_reason；
// 两者都取不到时也用**原文**兜底，而不是返回 nil 把这一帧丢掉 —— hint 帧本身就是失败信号。
func hintError(val map[string]any, payload string) error {
	content, _ := val["content"].(string)
	fr, _ := val["finish_reason"].(string)
	detail := firstNonEmpty(content, fr, truncate(payload, 200))
	lower := strings.ToLower(detail)
	kind := errs.UpstreamFault
	switch {
	case strings.Contains(lower, "rate_limit"), strings.Contains(lower, "rate limit"):
		kind = errs.SoftRate
	case strings.Contains(lower, "input_exceeds_limit"), strings.Contains(lower, "too long"):
		kind = errs.PromptTooLong
	}
	return errs.New(kind, "上游提示："+detail).WithChannel(string(channel.DeepSeek)).
		WithUpstream(truncate(payload, 200))
}

// streamEnvelopeError 在「整条流没有任何有效 patch 帧」时，从原文里挖出上游错误信封。
//
// 对齐参考实现 request.rs:654-684 的兜底：先逐行找 JSON（SSE 的 `data:` 行或整包 JSON），
// 有 biz_code/code 就用 bizError 归一（保留上游原话）；挖不到才认账为空流（Parse），
// 但同样把原文摘要带上 —— 不猜、不静默。这条路径专门解决「HTTP 200 + 业务错误码」：
// HTTP 层看不出失败（见 Classify 注释），以前会被当成「成功但没内容」。
func streamEnvelopeError(raw string) error {
	raw = strings.TrimSpace(raw)
	var cands []string
	for _, ln := range strings.Split(raw, "\n") {
		ln = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "data:"))
		if strings.HasPrefix(ln, "{") {
			cands = append(cands, ln)
		}
	}
	if strings.HasPrefix(raw, "{") {
		cands = append(cands, raw)
	}
	for _, c := range cands {
		var env struct {
			Code int `json:"code"`
			Data *struct {
				BizCode int `json:"biz_code"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(c), &env) != nil {
			continue
		}
		if env.Code != 0 || (env.Data != nil && env.Data.BizCode != 0) {
			return bizError([]byte(c), "上游流内错误")
		}
	}
	msg := "上游返回空流（HTTP 200 但没有任何内容）"
	if raw != "" {
		msg = "上游响应不是可解析的 patch 流"
	}
	return errs.New(errs.Parse, msg).WithChannel(string(channel.DeepSeek)).
		WithUpstream(truncate(raw, 200))
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
