package antigravity

// client.go 渠道实现：能力声明、模型表、令牌生命周期、对话（v1internal SSE）、错误归一。
//
// 三件事与隔壁 gemini 渠道**看起来像、实际上不同**，别照抄：
//  1. 请求要伪装成 Antigravity 桌面客户端（UA + 指纹），gemini 渠道不需要；
//  2. 基址是「daily 沙箱优先、生产回落」，gemini 渠道只有一个生产域；
//  3. 对话请求有 requestType/userAgent/requestId 这层外壳。共用的是 v1internal 的
//     方法名（loadCodeAssist / onboardUser / streamGenerateContent）与 Gemini 的 contents 形态。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Adapter 实现 channel.Channel（授权走 Authorizer + CallbackAcceptor，见 login.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	// 端点字段化，便于测试指向 mock server（生产用 constants.go 里的常量）。
	// 故意是**列表**而不是单值：回落是协议的一部分，不能只留一个基址。
	chatBases   []string
	loadBases   []string
	tokenURL    string
	userInfoURL string

	tokMu sync.Mutex
	// tokens 缓存「refresh token → access token」。
	// key 用 refresh token 而不是账号 UID：同一账号重新授权后 refresh token 会变，
	// 用旧 key 缓存到的东西必须作废。
	tokens map[string]cachedToken
}

type cachedToken struct {
	access string
	exp    time.Time
}

func New() *Adapter {
	return &Adapter{
		clients: map[string]*http.Client{},
		// 对话：daily 沙箱优先（参考实现实测更稳），生产域兜底。
		chatBases: []string{epDaily, epProd},
		// 开通：反过来，生产域优先 —— loadCodeAssist 在生产域上对「受管项目」的支持更好。
		loadBases:   []string{epProd, epDaily},
		tokenURL:    epToken,
		userInfoURL: epUserInfo,
		tokens:      map[string]cachedToken{},
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

func (a *Adapter) Kind() channel.Kind { return channel.Antigravity }

// Spec 能力声明。
//
// 逐条说依据，不照抄隔壁渠道：
//   - Tools=true 且 ToolsShim=false：上游是**原生** functionDeclarations / functionCall，
//     响应里的 functionCall 整包到达（不是分片）—— 不需要网关模拟层。
//   - Reasoning=true：思考型号确实单独吐思考块（Gemini 系是 `thought:true` + thoughtSignature，
//     Claude 思考系是 `thought:true`），映射成 reasoning_content 有据可依；
//     非思考型号不吐，所以模型表里逐档标了 CapYes/CapNo/CapUnknown，没有一刀切。
//   - Images=false：上游本身能收图片，但**本网关的 ChatRequest 不带图片**（Message 只有文本），
//     所以对客户端如实声明「不支持」——声明支持却传不过去，比不支持更糟。
//   - SSEOnly=true：本适配器只用 streamGenerateContent（流式），非流式由网关本地聚合。
//   - CheckinCap=false：Antigravity 没有签到。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.Antigravity,
		DisplayName:           "Google Antigravity",
		Status:                channel.Active,
		Category:              channel.CategoryChat, // 登录式（Google OAuth），不是订阅额度池那类 IDE 渠道
		Tools:                 true,
		ToolsShim:             false,
		Images:                false,
		Reasoning:             true,
		SSEOnly:               true,
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs: "⚠️ 风险自负：本渠道复用 Google Antigravity 桌面客户端的官方 OAuth（第三方客户端伪装）。" +
			"参考实现自述此举**违反 Google 服务条款**，社区已有账号被封 / 被 shadow-ban（限权但不通知）的实测记录。" +
			"别拿主力 Google 账号登录；封号与本项目无关，风险由使用者自担。" +
			" | 登录：Google OAuth 授权码 + PKCE（粘贴回码）。" +
			" | 模型：Google 统一网关，一个上游下发 Gemini / Claude / GPT-OSS 三类。" +
			" | 工具：原生 functionCall。",
	}
}

// Login 是 channel.Channel 要求的「一次性登录」：Antigravity 必须用户交互（浏览器授权 + 粘码），
// 所以走面板授权通道（channel.Authorizer）。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"Antigravity 需要交互式授权：请在面板「账号」页点「添加账号 → Google Antigravity」，"+
			"浏览器授权后把地址栏那一整条 URL 粘回面板").
		WithChannel(string(channel.Antigravity))
}

// Models 返回可用模型（本地清单，如实标注 SourceLocal）。
func (a *Adapter) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	out := make([]channel.ModelInfo, 0, len(agModels))
	for _, m := range agModels {
		out = append(out, channel.ModelInfo{
			ID:            m.ID,
			DisplayName:   m.Name,
			ContextWindow: m.Context, // 未知就是 0，不猜数字（F4.2）
			Source:        channel.SourceLocal,
			Tools:         channel.CapYes, // 原生
			Images:        channel.CapNo,  // 见 Spec 的说明：上游能收，本网关传不过去
			Reasoning:     toCap(m.Reasoning),
		})
	}
	return out, nil
}

func toCap(f capFlag) channel.Cap {
	switch f {
	case capYes:
		return channel.CapYes
	case capNo:
		return channel.CapNo
	default:
		return channel.CapUnknown
	}
}

// Balance 余额未知：这条上游是按账号算配额（配额信息不在余额接口里），不猜数字。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin 无签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "Antigravity 没有签到活动"}, nil
}

// ---------------------------------------------------------------------------
// 令牌
// ---------------------------------------------------------------------------

// usableToken 取一个现在就能用的 access token。
//
// 三种情况：
//   - 令牌还在有效期内（或过期时间未知）→ 直接用；
//   - 快过期/已过期且有 refresh token → 换一次（带进程内缓存，避免每个请求都去换）；
//   - 只有 access token、没有 refresh token（手工导入的老凭证）→ 照用，
//     让 401 自己暴露 —— 手里没有 refresh token 就是刷不了，不能假装能刷。
//
// 注意：这里换回来的新 refresh token **无法回写凭证**（Chat 拿不到写权限），
// 只在本进程缓存里生效；跨重启的持久化由账号级 Refresh() 负责（池层会调它）。
func (a *Adapter) usableToken(ctx context.Context, c *channel.Credential) (string, error) {
	if c == nil {
		return "", errs.New(errs.SessionDead, "Antigravity 凭证为空：请在面板重新授权").
			WithChannel(string(channel.Antigravity))
	}
	if tok := strings.TrimSpace(c.AccessToken); tok != "" &&
		(c.ExpiresAt.IsZero() || time.Now().Before(c.ExpiresAt.Add(-refreshBuffer))) {
		return tok, nil
	}
	refresh := strings.TrimSpace(c.RefreshToken)
	if refresh == "" {
		if tok := strings.TrimSpace(c.AccessToken); tok != "" {
			return tok, nil
		}
		return "", errs.New(errs.SessionDead, "Antigravity 凭证里没有可用令牌：请在面板重新授权").
			WithChannel(string(channel.Antigravity)).WithAccount(uidOf(c))
	}

	a.tokMu.Lock()
	if v, ok := a.tokens[refresh]; ok && time.Now().Before(v.exp.Add(-refreshBuffer)) {
		a.tokMu.Unlock()
		return v.access, nil
	}
	a.tokMu.Unlock()

	out, err := a.refreshWith(ctx, c, refresh)
	if err != nil {
		return "", err
	}
	a.rememberRefresh(refresh, out)
	return out.AccessToken, nil
}

// forceRefreshToken 无视当前 access token 的「有效期」，强制换一次。
//
// 专给 401 重试用：凭证里的 access token 在我们看来还没过期，但上游已经拒了
// （时钟偏差、被吊销、服务端提前失效），这时只有真去换一次才知道行不行。
// 如果这里还走「有效期判断」，重试就会拿同一个死令牌再撞一次 401。
func (a *Adapter) forceRefreshToken(ctx context.Context, c *channel.Credential) (string, error) {
	refresh := strings.TrimSpace(c.RefreshToken)
	if refresh == "" {
		return "", errs.New(errs.SessionDead, "Antigravity 凭证无效且没有 refresh token：请在面板重新授权").
			WithChannel(string(channel.Antigravity)).WithAccount(uidOf(c))
	}
	a.forgetToken(refresh)
	out, err := a.refreshWith(ctx, c, refresh)
	if err != nil {
		return "", err
	}
	a.rememberRefresh(refresh, out)
	return out.AccessToken, nil
}

// rememberRefresh 把换到的令牌写进进程内缓存（含 Google 可能轮换的新 refresh token）。
func (a *Adapter) rememberRefresh(refresh string, out tokenResp) {
	exp := out.expiry()
	a.cacheToken(refresh, out.AccessToken, exp)
	if nr := strings.TrimSpace(out.RefreshToken); nr != "" && nr != refresh {
		a.cacheToken(nr, out.AccessToken, exp)
	}
}

func (a *Adapter) cacheToken(refresh, access string, exp time.Time) {
	a.tokMu.Lock()
	a.tokens[refresh] = cachedToken{access: access, exp: exp}
	a.tokMu.Unlock()
}

// forgetToken 丢掉某个 refresh token 的缓存（401 之后必须丢，否则会一直用死 token 重试）。
func (a *Adapter) forgetToken(refresh string) {
	a.tokMu.Lock()
	delete(a.tokens, strings.TrimSpace(refresh))
	a.tokMu.Unlock()
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// Chat 发起一次流式对话。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	// 先拿令牌，再开通：开通请求也要带令牌，而 usableToken 可能会顺手刷一次
	// （凭证里的 access token 过期时）。顺序反了的话，开通会用那个旧令牌去打，
	// 表现成「明明能刷新的凭证却报 401」。
	token, err := a.usableToken(ctx, c)
	if err != nil {
		return nil, err
	}
	project := projectOf(c)
	if project == "" {
		// 凭证里没有项目（老凭证 / 手工导入）：就地开通一次，失败就说清原因（不静默）。
		p, err := a.ensureProject(ctx, c, token)
		if err != nil {
			return nil, err
		}
		project = p
	}

	body, err := buildRequest(req, project, "agent-"+randHex(16))
	if err != nil {
		return nil, errs.New(errs.Parse, "构造请求体失败").WithChannel(string(channel.Antigravity)).
			WithAccount(uidOf(c)).WithCause(err)
	}

	resp, err := a.sendChat(ctx, c, token, body, req.Model)
	if err != nil {
		return nil, err
	}
	// 401 是「access token 过期了」的常见表现：丢缓存换一次再试，只重试一次。
	if resp.StatusCode == http.StatusUnauthorized && strings.TrimSpace(c.RefreshToken) != "" {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if token, err = a.forceRefreshToken(ctx, c); err != nil {
			return nil, err
		}
		if resp, err = a.sendChat(ctx, c, token, body, req.Model); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.Antigravity)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, req.Model), nil
}

// sendChat 依序尝试候选基址发对话请求。
//
// 回落判据只看两件：**连不上**（Transport 错）与 **5xx**。4xx 是上游给出的明确答案
// （401 是凭证、400 是请求体、404 是模型不存在），换一个域名问一遍结果一样，
// 还会把真正的错误藏起来 —— 所以 4xx 直接返回，交给上层 Classify。
func (a *Adapter) sendChat(ctx context.Context, c *channel.Credential, token string, body []byte, model string) (*http.Response, error) {
	bases := a.chatBases
	if len(bases) == 0 {
		bases = []string{epDaily}
	}
	fp := fingerprintOf(seedOf(c))

	var lastErr error
	for i, base := range bases {
		hdrs := map[string]string{
			"Authorization": "Bearer " + token,
			"Content-Type":  "application/json",
			"Accept":        "text/event-stream",
			// 对话请求只带 UA（参考实现实测：Antigravity 模式的正文请求不发
			// X-Goog-Api-Client / Client-Metadata，发了反而与客户端指纹不一致）。
			"User-Agent": fp.ua(),
		}
		// Claude 思考型号：要实时思考流，否则思考会攒成一大块最后才吐。
		if isClaudeThinking(model) {
			hdrs["anthropic-beta"] = interleavedThinking
		}
		resp, err := a.doOnce(ctx, c, http.MethodPost, base+"/v1internal:streamGenerateContent?alt=sse", body, hdrs)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 && i < len(bases)-1 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			lastErr = errors.New("上游 HTTP " + itoa(resp.StatusCode))
			continue
		}
		return resp, nil
	}
	return nil, errs.New(errs.Transport, "上游不可达").
		WithChannel(string(channel.Antigravity)).WithAccount(uidOf(c)).WithCause(lastErr)
}

// doOnce 构造并发送一个请求（带上伪装头）。
func (a *Adapter) doOnce(ctx context.Context, c *channel.Credential, method, urlStr string, body []byte, hdrs map[string]string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, rdr)
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.Antigravity)).WithCause(err)
	}
	applyHeaders(req, hdrs)
	return a.clientFor(c).Do(req)
}

// applyHeaders 设置请求头。
//
// 关于 x-api-key / x-goog-user-project：**我们从不设置它们**，这里仍显式 Del 一遍是防线 ——
// 这两个头（分别由 OpenAI SDK 与 Google SDK 的调用方加上）会让上游按**项目级**鉴权校验，
// 而本渠道没有项目级 API key、也不该带项目，带上就是 403（参考实现同样会删掉它们）。
// 放在这里是给未来的重构兜底：哪天有人改成「复用外部传入的头集合」，不至于把 403 带回来。
func applyHeaders(req *http.Request, hdrs map[string]string) {
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-user-project")
}

// Classify 把上游错误归一成有限枚举（D4）。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == 401:
		return errs.SessionDead
	case status == 403:
		// 403 有两种完全不同的含义，报文里有区分线索，必须分开：
		//   - 凭证失效（unauthenticated / invalid token）→ 重新授权能解决；
		//   - 账号资格问题（地区不支持 / 未开通 / 无许可）→ 重新授权也没用，是账号本身不行。
		// 分错的后果：用户一直在重登一个注定登不上的账号。
		for _, kw := range []string{"unauthenticated", "invalid_token", "invalid token", "expired"} {
			if strings.Contains(s, kw) {
				return errs.SessionDead
			}
		}
		return errs.AuthFailed
	case status == 412:
		// Precondition Failed：典型是「账号没开通」（loadCodeAssist/onboardUser 没走完）。
		return errs.AuthFailed
	case status == 429:
		// 关键词要具体：「limit」这种宽词会命中 "rate limited"（那其实是限流），
		// 分错的后果是把「等一会儿就好」判成「没钱了」，或者反过来无休止重试。
		for _, kw := range []string{"quota exceeded", "exceeded your quota", "resource_exhausted",
			"out of credits", "insufficient", "billing", "exhaust"} {
			if strings.Contains(s, kw) {
				return errs.HardCredit
			}
		}
		return errs.SoftRate
	case status == 404:
		return errs.ModelUnavailable
	case status == 400:
		switch {
		case strings.Contains(s, "token"), strings.Contains(s, "maximum"), strings.Contains(s, "too long"):
			return errs.PromptTooLong
		case strings.Contains(s, "safety"), strings.Contains(s, "blocked"), strings.Contains(s, "prohibited"):
			return errs.ContentBlocked
		case strings.Contains(s, "model"):
			return errs.ModelUnavailable
		}
		return errs.Parse
	case status >= 500:
		return errs.UpstreamFault
	}
	return errs.Parse
}

// ---------------------------------------------------------------------------
// SSE 流
// ---------------------------------------------------------------------------

// stream 把 v1internal 的 SSE 转成 chunk 流。
//
// 与 gemini 渠道同形：每帧外面包了一层 `response`，要先剥壳；
// SSE 允许 `data:` 跨多行、空行才算一帧（按行直接 parse 会解析失败）。
type stream struct {
	rc    io.ReadCloser
	model string
	ch    chan chunkOrErr
	seq   int
}

type chunkOrErr struct {
	chunk channel.ChatCompletionChunk
	err   error
}

func newStream(rc io.ReadCloser, model string) *stream {
	s := &stream{rc: rc, model: model, ch: make(chan chunkOrErr, 16)}
	go s.pump()
	return s
}

func (s *stream) pump() {
	defer close(s.ch)
	br := bufio.NewReaderSize(s.rc, 256*1024)
	var data []string
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			s.ch <- chunkOrErr{err: err}
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if payload := strings.Join(data, "\n"); payload != "" {
				if !s.emit(payload) {
					return
				}
			}
			data = data[:0]
		}
		if err == io.EOF {
			// 收尾：最后一帧可能没有空行结尾。
			if payload := strings.Join(data, "\n"); payload != "" {
				_ = s.emit(payload)
			}
			return
		}
	}
}

// emit 处理一帧。返回 false 表示流内出现了不可继续的错误（已投递给调用方）。
func (s *stream) emit(payload string) bool {
	var frame map[string]any
	if json.Unmarshal([]byte(payload), &frame) != nil {
		return true // 非 JSON 帧（注释/心跳）跳过
	}
	// 流内错误：上游会在帧里带 error 对象（HTTP 已经是 200 了，错误只能从这里出）。
	if e, ok := frame["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		code := intOf(e["code"])
		if strings.TrimSpace(msg) == "" {
			msg = truncate(payload, 200)
		}
		kind := errs.UpstreamFault
		// 帧内错误若是令牌/身份问题，那就是凭证失效（要重新授权），不是上游故障 ——
		// 分错了用户会去查网络，而真正该做的是重新登录。
		if code == 401 || code == 403 || looksLikeAuthError(msg) {
			kind = errs.SessionDead
		}
		s.ch <- chunkOrErr{err: errs.New(kind, "上游返回错误："+msg).
			WithChannel(string(channel.Antigravity)).WithUpstream(truncate(payload, 200))}
		return false
	}
	resp, _ := frame["response"].(map[string]any)
	if resp == nil {
		resp = frame // 容错：万一某帧没包那层
	}
	if c, ok := chunksFromFrame(resp, s.model, &s.seq); ok {
		s.ch <- chunkOrErr{chunk: c}
	}
	return true
}

// looksLikeAuthError 判断错误文案是不是「凭证问题」。
func looksLikeAuthError(msg string) bool {
	s := strings.ToLower(msg)
	for _, kw := range []string{"token", "auth", "unauthorized", "forbidden", "credential", "登录", "凭证"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }

// ---------------------------------------------------------------------------
// 指纹与凭证小工具
// ---------------------------------------------------------------------------

// 伪装用的平台池。与参考实现的候选集一致（Windows / macOS，Intel 与 ARM）。
var agPlatforms = []struct {
	uaPlatform   string
	metaPlatform string
}{
	{"windows/amd64", "WINDOWS"},
	{"darwin/arm64", "MACOS"},
	{"darwin/amd64", "MACOS"},
}

// X-Goog-Api-Client 的候选集（开通流程会带它）。
var agAPIClients = []string{
	"google-cloud-sdk vscode_cloudshelleditor/0.1",
	"google-cloud-sdk vscode/1.96.0",
	"google-cloud-sdk vscode/1.95.0",
}

// fingerprint 是一个账号对外呈现的客户端指纹。
type fingerprint struct {
	uaPlatform   string
	metaPlatform string
	apiClient    string
}

func (f fingerprint) ua() string {
	return "antigravity/" + antigravityVersion + " " + f.uaPlatform
}

// fingerprintOf 由凭证派生出**稳定**的客户端指纹。
//
// 为什么派生而不是每次随机（参考实现是随机的）：上游把这套头当作客户端身份的一部分，
// 同一账号一会儿 Windows、一会儿 macOS、UA 版本乱跳，在风控看来就是「一个账号被多台机器轮着用」——
// 正是最该被盯上的形态。由凭证派生同时满足三件事：同账号恒定、不同账号天然不同、
// 不需要额外落盘（与 kimi 渠道派生设备号是同一个思路）。
func fingerprintOf(seed string) fingerprint {
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	sum := h.Sum64()
	p := agPlatforms[sum%uint64(len(agPlatforms))]
	return fingerprint{
		uaPlatform:   p.uaPlatform,
		metaPlatform: p.metaPlatform,
		apiClient:    agAPIClients[(sum/uint64(len(agPlatforms)))%uint64(len(agAPIClients))],
	}
}

// seedOf 取派生根（指纹的稳定来源）。
//
// 优先账号 UID：access token 会轮换，用它派生会让指纹跟着变
// （风控看起来就是「同一账号换了设备」）。只有登录途中还没有 UID 时才退回用令牌。
func seedOf(c *channel.Credential) string {
	if c == nil {
		return "antigravity-anonymous"
	}
	if strings.TrimSpace(c.UID) != "" {
		return c.UID
	}
	if t := strings.TrimSpace(c.AccessToken); t != "" {
		return t
	}
	return "antigravity-anonymous"
}

// projectOf 取凭证里记的项目 id（可能为空：老凭证 / 手工导入）。
func projectOf(c *channel.Credential) string {
	if c == nil || c.Extra == nil {
		return ""
	}
	return strings.TrimSpace(c.Extra["project"])
}

func uidOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	return c.UID
}
