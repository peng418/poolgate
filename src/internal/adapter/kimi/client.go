package kimi

// client.go 渠道实现：能力声明、凭证换取（refresh → access）、对话编排、错误归一。
//
// 本渠道的「工具调用」是 **toolshim 模拟**：网页协议没有原生 tools，只有内置联网搜索开关，
// 所以 Spec 是 Tools=false + ToolsShim=true —— 网关把工具定义翻成提示词，再把模型输出的
// 标记解析回结构化 tool_calls（对客户端来说合同不变）。

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

// refreshBuffer 是 access token 的提前刷新量：少于它就觉得「快过期了」。
// 不提前刷的代价是每个请求都可能先撞一次 401，看起来像凭证坏了。
const refreshBuffer = 5 * time.Minute

// Adapter 实现 channel.Channel（登录走 Authorizer + CallbackAcceptor，见 login.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	base string // API 基址（测试可换）

	tokMu sync.Mutex
	// tokens 缓存「refresh token → access token」，避免每个请求都去上游换一次。
	// key 用 refresh token 而不是账号 UID：同一账号重新登录后 refresh token 会变，
	// 用旧 key 缓存到的东西必须作废。
	tokens map[string]cachedToken

	// catMu 保护模型目录的两样东西：给面板/网关的快照（catalog），
	// 以及「模型名 → 档位参数」的表（specs，Chat 用）。
	// specs 在建表时就用兜底表填了一遍（见 New），所以即使目录从来没拉成功过，
	// 兜底表里那些档位也认得出、发得对。
	catMu     sync.RWMutex
	catalog   []channel.ModelInfo
	catalogAt time.Time
	specs     map[string]modelSpec
}

type cachedToken struct {
	access string
	exp    time.Time
}

func New() *Adapter {
	a := &Adapter{
		clients: map[string]*http.Client{},
		base:    apiBase,
		tokens:  map[string]cachedToken{},
		specs:   map[string]modelSpec{},
	}
	// 兜底表先入库：列表里下发的档位与 Chat 认得的档位保持一致（见 localModel 的说明）。
	for _, m := range fallbackModels {
		a.specs[m.ID] = m.Spec
	}
	return a
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

func (a *Adapter) Kind() channel.Kind { return channel.Kimi }

// Spec 能力声明。
//
// ToolsShim=true 是本渠道的关键：网页协议没有原生工具调用，由网关代做模拟。
// 声明成 native 就是撒谎，会让客户端在跑 agent 时撞到「模型把工具调用写成文本」。
// Reasoning=true 是有依据的：上游把思考阶段单独发（STAGE_NAME_THINKING / block.think），
// 我们把它映射成 reasoning_content。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.Kimi,
		DisplayName:           "Kimi（网页版）",
		Status:                channel.Active,
		Category:              channel.CategoryChat, // 登录式，与订阅额度池那类分开
		Tools:                 false,
		ToolsShim:             true,
		Images:                false,
		Reasoning:             true,
		SSEOnly:               true,
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs:                  "浏览器登录后粘贴 refresh token；思考单独分流；工具调用由网关模拟（toolshim）",
	}
}

// Login 是「一次性登录」入口：Kimi 的凭证来自你浏览器里的登录态，必须用户参与。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"Kimi 需要你在浏览器登录一次：面板「添加账号 → Kimi」里粘贴凭证（不必给密码）").
		WithChannel(string(channel.Kimi))
}

// Models 返回可用档位：**优先用凭证向上游问目录**（GetAvailableModels），
// 拉不到才退到本地兜底表。上游目录是权威来源 —— 它按账号订阅给出真正可用的档位，
// 而且 id 是上游自己那套开关组合（见 models.go 的说明）。
//
// 为什么失败也不返回 error：这个函数在面板、boot 探针、/v1/models 三处被调用，
// 返回错误会让「模型列表」整块消失（网关只在取不到时回退快照，快照也没有就下发空）。
// 退到兜底表并如实标注 SourceLocal，比空着好；真正的失败由对话请求暴露。
func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	if cached, ok := a.cachedCatalog(); ok {
		return cached, nil
	}
	if models, specs, err := a.fetchModelCatalog(ctx, c); err == nil && len(models) > 0 {
		a.storeCatalog(models, specs)
		return models, nil
	}
	return toModelInfos(fallbackModels, channel.SourceLocal), nil
}

// ModelsSnapshot 返回上次成功拉取的目录快照（网关 /v1/models 取不到时兜底用）；
// 一次都没成功过就返回兜底表 —— 与 WorkBuddy/CodeBuddy 那边的做法一致。
func (a *Adapter) ModelsSnapshot() []channel.ModelInfo {
	if cached, ok := a.cachedCatalog(); ok {
		return cached
	}
	return toModelInfos(fallbackModels, channel.SourceLocal)
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
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "Kimi 网页版没有签到活动"}, nil
}

// ---------------------------------------------------------------------------
// 凭证：refresh token → access token
// ---------------------------------------------------------------------------

// seedOf 取派生根（设备号/会话号都从它派生）。
//
// 优先用账号 UID：access token 会轮换，用 token 派生会让设备号跟着变
// （风控看起来就是「同一账号换了设备」）。只有登录途中还没有 UID 时才退回用 token。
func seedOf(c *channel.Credential) string {
	if c == nil {
		return "kimi-anonymous"
	}
	if strings.TrimSpace(c.UID) != "" {
		return c.UID
	}
	return c.AccessToken
}

// accessToken 取一个可用的 access token。
//
// 三种情况：
//   - 凭证本身就是 access token（用户直接粘了 JWT）→ 直接用；快过期就照用，
//     让 401 自己暴露出来（我们手里没有 refresh token，刷不了，不能假装能刷）；
//   - 凭证是 refresh token → 先查进程内缓存，再向上游换；
//   - 空凭证 → 明确报 SessionDead，让面板提示「重新粘贴」。
func (a *Adapter) accessToken(ctx context.Context, c *channel.Credential) (string, error) {
	raw := strings.TrimSpace(tokenOf(c))
	if raw == "" {
		return "", errs.New(errs.SessionDead, "Kimi 凭证为空：请在面板里重新粘贴 refresh token").
			WithChannel(string(channel.Kimi))
	}
	if isAccessToken(raw) {
		return raw, nil
	}
	a.tokMu.Lock()
	if v, ok := a.tokens[raw]; ok && time.Now().Before(v.exp.Add(-refreshBuffer)) {
		a.tokMu.Unlock()
		return v.access, nil
	}
	a.tokMu.Unlock()

	access, exp, err := a.exchangeRefresh(ctx, c, raw)
	if err != nil {
		return "", err
	}
	a.tokMu.Lock()
	a.tokens[raw] = cachedToken{access: access, exp: exp}
	a.tokMu.Unlock()
	return access, nil
}

// forgetToken 丢掉某个 refresh token 的缓存（401 之后必须丢，否则会一直用死 token 重试）。
func (a *Adapter) forgetToken(refresh string) {
	a.tokMu.Lock()
	delete(a.tokens, strings.TrimSpace(refresh))
	a.tokMu.Unlock()
}

// exchangeRefresh 用 refresh token 换 access token。
//
// 注意上游这里**不轮换 refresh token**（同一个 refresh token 可以一直换），
// 所以换回来的只有 access token；这一点对我们的凭证存储很关键：不需要回写凭证。
func (a *Adapter) exchangeRefresh(ctx context.Context, c *channel.Credential, refresh string) (string, time.Time, error) {
	resp, err := a.send(ctx, c, http.MethodGet, epRefresh, refresh, nil, false, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", time.Time{}, errs.New(a.Classify(resp.StatusCode, raw), "换 access token 失败").
			WithChannel(string(channel.Kimi)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Token       string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", time.Time{}, errs.New(errs.Parse, "上游没有返回可解析的令牌").
			WithChannel(string(channel.Kimi)).WithUpstream(truncate(string(raw), 200))
	}
	access := strings.TrimSpace(out.AccessToken)
	if access == "" {
		access = strings.TrimSpace(out.Token)
	}
	if access == "" {
		// 拿到 200 却没有令牌：绝不能当成功（否则后面每个请求都 401，看起来像别的问题）。
		return "", time.Time{}, errs.New(errs.SessionDead,
			"上游没有返回 access token：refresh token 可能已失效，请重新粘贴").
			WithChannel(string(channel.Kimi)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return access, tokenExpiry(access), nil
}

// Refresh 是账号级续期：拿手里的 refresh token 再换一次 access token。
//
// 返回更新后的凭证（AccessToken 换成新 access token、RefreshToken 保留原 refresh token）；
// 拿不到新令牌时返回 (nil, nil) —— 明确表示「刷不了」，让上层按 401 处理，而不是假装成功。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if c == nil {
		return nil, nil
	}
	refresh := strings.TrimSpace(c.RefreshToken)
	if refresh == "" && !isAccessToken(strings.TrimSpace(c.AccessToken)) {
		refresh = strings.TrimSpace(c.AccessToken)
	}
	if refresh == "" {
		return nil, nil // 只粘了 access token：没有可续期的东西
	}
	access, exp, err := a.exchangeRefresh(ctx, c, refresh)
	if err != nil {
		return nil, err
	}
	nc := *c
	nc.AccessToken = access
	nc.RefreshToken = refresh
	nc.ExpiresAt = exp
	a.tokMu.Lock()
	a.tokens[refresh] = cachedToken{access: access, exp: exp}
	a.tokMu.Unlock()
	return &nc, nil
}

// checkCredential 用订阅接口确认「这串凭证现在还能用吗」。
//
// 登录时用它当场验一把：把一个已失效的凭证塞进池子，表现是「装上就报 401」，
// 用户根本不知道是自己粘错了还是渠道坏了。
func (a *Adapter) checkCredential(ctx context.Context, c *channel.Credential) error {
	token, err := a.accessToken(ctx, c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// 订阅校验是**普通 JSON 调用**，不是 Connect 信封：参考实现传 json={}（client.py:255-261），
	// 只有 Connect-Protocol-Version 这个头照旧由统一的头构造函数带上（client.py:207-210）。
	resp, err := a.send(ctx, c, http.MethodPost, epSubscription, token, []byte("{}"), false,
		map[string]string{"Connect-Protocol-Version": "1"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return errs.New(a.Classify(resp.StatusCode, raw), "凭证校验失败").
			WithChannel(string(channel.Kimi)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return nil
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// Chat 发一次对话：把内部契约拼成上游要的单轮文本 + Connect 信封，响应按帧流式解析。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	// 档位参数（scenario / 思考 / 联网 / agent 的两个产品字段）先查模型表 ——
	// 表里的 id 是上游目录给的，参数也是照目录生成的，不会出现「名字是 agent 档、
	// 发出去却是普通场景」这种错配。表里没有的名字再走语法解析。
	spec, ok := a.specFor(req.Model)
	if !ok {
		spec = specOf(req.Model)
	}
	if !spec.known {
		// 不认识的模型名：按默认档走，但**不静默**——日志与流水里能看到用的是哪一档
		// （与 DeepSeek 渠道同一个处理原则：宁可给默认档，也不让请求失败，但要留痕）。
		spec = specOf(defaultModel)
	}

	token, err := a.accessToken(ctx, c)
	if err != nil {
		return nil, err
	}
	body, err := encodeConnect(buildChatPayload(req, spec))
	if err != nil {
		return nil, errs.New(errs.Parse, "构造请求失败").WithChannel(string(channel.Kimi)).WithCause(err)
	}

	resp, err := a.send(ctx, c, http.MethodPost, epChat, token, body, true, nil)
	if err != nil {
		return nil, err
	}
	// 401 是「access token 过期了」的常见表现：换一次再试，只重试一次。
	if resp.StatusCode == 401 && !isAccessToken(strings.TrimSpace(tokenOf(c))) {
		resp.Body.Close()
		a.forgetToken(strings.TrimSpace(tokenOf(c)))
		if token, err = a.accessToken(ctx, c); err != nil {
			return nil, err
		}
		if resp, err = a.send(ctx, c, http.MethodPost, epChat, token, body, true, nil); err != nil {
			return nil, err
		}
	}
	// 非 200 一律当错误：参考实现的对话路径就是这么判的（只有 200 才当流读，
	// 其余交给 _raise_for_response，client.py:340-351）。若把 204/3xx 当流读，
	// 客户端看到的是「转圈到最后空回复」，而真正的问题是响应根本不是流。
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.Kimi)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, req.Model), nil
}

// buildChatPayload 组装上游请求体（形态照参考实现）。
func buildChatPayload(req channel.ChatRequest, spec modelSpec) map[string]any {
	content := packMessages(req.Messages)
	message := map[string]any{
		"role": "user",
		"blocks": []any{
			map[string]any{"message_id": "", "text": map[string]any{"content": content}},
		},
		"scenario": spec.scenario,
	}
	tools := []any{}
	if spec.search {
		tools = append(tools, map[string]any{"type": "TOOL_TYPE_SEARCH", "search": map[string]any{}})
	}
	payload := map[string]any{
		"scenario": spec.scenario,
		"tools":    tools,
		"message":  message,
		"options":  map[string]any{"thinking": spec.thinking},
	}
	// agent 档多两个产品字段，且**只在非空时才写**（参考实现 client.py:332-335）：
	// 普通档带上它们上游会当成别的产品请求。
	if spec.kimiPlusID != "" {
		payload["kimiplusId"] = spec.kimiPlusID
	}
	if spec.agentMode != "" {
		payload["agentMode"] = spec.agentMode
	}
	return payload
}

// Classify 把上游错误归一成有限枚举（D4）。
//
// 401/403 都算凭证问题：Kimi 用 403 表示「这个身份不被接受」（与 Anthropic 那种
// 「出口 IP 被拦」不同 —— 这里的 403 带上明确的鉴权语义）。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == 401 || status == 403:
		return errs.SessionDead
	case status == 429:
		// 「限流」与「额度耗尽」上游都用 429，只能靠报文区分。
		// 注意别把 "limit" 当关键词：限流的原话里也常带 limit（"rate limited"），
		// 认它就会把「等一会儿就好」判成「没钱了，等多久都没用」——正好判反。
		for _, kw := range []string{"quota", "credit", "balance", "exhaust", "insufficient", "额度"} {
			if strings.Contains(s, kw) {
				return errs.HardCredit
			}
		}
		return errs.SoftRate
	case status == 400 || status == 422:
		switch {
		case strings.Contains(s, "context"), strings.Contains(s, "too long"),
			strings.Contains(s, "max_tokens"):
			return errs.PromptTooLong
		case strings.Contains(s, "sensitive"), strings.Contains(s, "policy"),
			strings.Contains(s, "moderation"):
			return errs.ContentBlocked
		case strings.Contains(s, "model"), strings.Contains(s, "not found"):
			return errs.ModelUnavailable
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

// send 发一个请求，带上伪装头与设备号。
//
// connectFramed=true 时按 Connect 协议设置 Content-Type（请求体也是信封形态）；
// false 用于普通的 JSON 接口（换令牌、订阅校验、模型目录）。
// extra 是逐调用覆盖的头（参考实现也是这么分层的：统一头 + 每处 extra，protocol.py:117-132）。
func (a *Adapter) send(ctx context.Context, c *channel.Credential, method, path, token string, body []byte, connectFramed bool, extra map[string]string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rdr)
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.Kimi)).WithCause(err)
	}
	for k, v := range fakeHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	seed := seedOf(c)
	req.Header.Set("X-Msh-Device-Id", deriveDeviceID(seed))
	req.Header.Set("X-Msh-Session-Id", deriveSessionID(seed))
	if connectFramed {
		req.Header.Set("Content-Type", "application/connect+json")
		req.Header.Set("Connect-Protocol-Version", "1")
	} else if body != nil {
		// 无 body 的 GET（换令牌）不带 Content-Type：参考实现那条路也是裸 GET
		// （token_manager.py:135-147，头里没有 Content-Type）。
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.Kimi)).
			WithAccount(uidOf(c)).WithCause(err)
	}
	return resp, nil
}

// tokenOf 取凭证里的令牌（AccessToken 优先，其次 RefreshToken）。
func tokenOf(c *channel.Credential) string {
	if c == nil {
		return ""
	}
	if v := strings.TrimSpace(c.AccessToken); v != "" {
		return v
	}
	return strings.TrimSpace(c.RefreshToken)
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
