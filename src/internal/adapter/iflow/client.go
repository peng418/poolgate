package iflow

// client.go 渠道实现：能力声明、对话编排、凭证探活、错误归一、HTTP 与请求头。
//
// 本渠道是**登录式**：凭证是你从自己机器上粘过来的 apiKey（见 login.go）。
// 与 Kimi/DeepSeek 不同，iFlow 上游是标准 OpenAI 接口、**原生支持 tools**，
// 所以这里没有任何协议翻译 —— Spec.Tools=true / ToolsShim=false，工具定义原样透传。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Adapter 实现 channel.Channel（登录走 Authorizer + CallbackAcceptor，见 login.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	base string // API 基址（测试可换）
}

func New() *Adapter {
	return &Adapter{
		clients: map[string]*http.Client{},
		base:    apiBase,
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

func (a *Adapter) Kind() channel.Kind { return channel.IFlow }

// Spec 能力声明。
//
// Tools=true + ToolsShim=false：上游**原生**吃 OpenAI 的 tools/tool_calls，
// 工具定义原样放进请求体即可，不需要 internal/toolshim 那层模拟。
// 声明成 shim 是撒谎（会让网关多绕一圈、还可能把原生 tool_calls 当文本解析）；
// 声明成不支持更糟（客户端一用工具就被网关拒）。
//
// Category=chat：与其它**登录式**渠道（Gemini / 通义 / Kimi）一致归到「聊天平台类」，
// 与订阅额度池那类编程助手分开展示 —— 它们的额度模型与风控强度不同，混在一起会误导用户。
// SSEOnly=false：上游流式/非流式都支持（参考实现两条路都实现了），所以不需要本地聚合。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.IFlow,
		DisplayName:           "iFlow CLI（心流）",
		Status:                channel.Active,
		Category:              channel.CategoryChat,
		Tools:                 true,  // 上游原生支持
		ToolsShim:             false, // 不需要网关模拟
		Images:                false, // 见下：本适配器契约里没有图片字段
		Reasoning:             true,  // 思考模型会回 reasoning_content
		SSEOnly:               false, // 流式/非流式上游都支持
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs: "凭证是 ~/.iflow/settings.json 里的 apiKey（粘贴）；请求带 HMAC-SHA256 签名，" +
			"UA 必须是 iFlow-Cli 才能解锁 GLM/DeepSeek/Kimi 高级模型；原生 tools。",
	}
}

// Login 是 channel.Channel 要求的「一次性登录」：apiKey 只能由用户从自己机器上取，必须交互。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"iFlow 需要你在装了 iFlow CLI 的机器上取一次 apiKey：面板「添加账号 → iFlow」里粘贴").
		WithChannel(string(channel.IFlow))
}

// Models 返回本地模型清单（上游没有公开 /models，Source=local，如实标注）。
//
// 上游的模型名我们不认识的（比如新增档位）会原样透传给上游 —— 见 Chat 的注释。
func (a *Adapter) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	out := make([]channel.ModelInfo, 0, len(localModels))
	for _, m := range localModels {
		out = append(out, channel.ModelInfo{
			ID:          m.ID,
			DisplayName: m.Name,
			// 上下文长度：上游没给可信数字，不猜（F4.2）。
			ContextWindow: 0,
			Source:        channel.SourceLocal,
			Tools:         channel.CapYes, // 上游原生支持，所有档位都能用
			Reasoning:     capOf(m.Think),
			// 参考实现称「所有模型都支持图像输入」，但**本适配器的请求契约
			// （channel.Message）根本没有图片字段**，图片到不了上游。
			// 标成支持会诱导客户端发图、然后被网关清洗掉，表现成「图丢了」；
			// 所以如实标「不支持」，与 Kimi 渠道同一个处理原则。
			Images: channel.CapNo,
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

// Balance 余额未知：iFlow 没有公开的余额接口，不猜数字（F4.2）。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin 无签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "iFlow 没有签到活动"}, nil
}

// Refresh 不支持续期：apiKey 长期有效，没有可换的东西。
//
// 返回 (nil, nil) 明确表示「刷不了」—— 让上层按 401 走 SessionDead（提示重新粘贴），
// 而不是假装成功、把一个死凭证继续留在池子里。
func (a *Adapter) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// Chat 发一次对话。上游是 OpenAI 兼容接口，所以这里只做三件事：
// 组请求体（含签名头）、判错、把响应交给流解析器。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	key, err := apiKeyOf(c)
	if err != nil {
		return nil, err
	}
	// 模型名原样透传，不认识的也不替换成默认档：我们的清单是**本地快照**，
	// 上游随时可能新增档位；偷偷换掉会让用户以为「点的模型真的跑了」。
	// 上游认不出自然会回 ModelUnavailable（Classify 会归一）。
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = defaultModel
	}
	body, err := json.Marshal(buildBody(req, model))
	if err != nil {
		return nil, errs.New(errs.Parse, "构造请求失败").WithChannel(string(channel.IFlow)).WithCause(err)
	}

	resp, err := a.send(ctx, c, key, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		// 注意：这里**不重试**。apiKey 没有刷新链路，401 就是凭证真的失效了，
		// 重试只会把同一个死凭证再打一遍（不会变好，只会多一条上游错误）。
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.IFlow)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, resp.Header.Get("Content-Type"), model), nil
}

// probeCredential 用一个**最小对话请求**确认这串 apiKey 现在真的能用。
//
// 为什么用对话探活，而不是 GET /models：
//   - 参考实现自己说得很清楚「iFlow API 没有公开的 /models 端点」，清单是本地硬编码的；
//     拿一个上游不保证存在的接口当凭证校验，会在上游改版时把好凭证判死。
//   - 对话探活同时验证了 401 之外的另一个关键点：**签名是否被接受**
//     （签名错上游也只回 401/403，只有真打一次对话才能确认签名算法接上了）。
//
// 代价是登录时会多花一次极短对话的额度 —— 一次性成本，换来「装进池子的凭证一定可用」。
// 用默认档 glm-4.6、不压 max_new_tokens（压太小部分上游会直接 400，
// 那就把「凭证问题」伪装成了「参数问题」，更难查）。
func (a *Adapter) probeCredential(ctx context.Context, c *channel.Credential) error {
	key, err := apiKeyOf(c)
	if err != nil {
		return err
	}
	body, err := json.Marshal(buildBody(channel.ChatRequest{
		Model:    defaultModel,
		Messages: []channel.Message{{Role: "user", Content: "ping"}},
		Stream:   false,
	}, defaultModel))
	if err != nil {
		return errs.New(errs.Parse, "构造校验请求失败").WithChannel(string(channel.IFlow)).WithCause(err)
	}
	resp, err := a.send(ctx, c, key, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return errs.New(a.Classify(resp.StatusCode, raw), "凭证校验失败").
			WithChannel(string(channel.IFlow)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	// 200 但信封里带 error：上游有「HTTP 200 + 业务错误」的先例，不能当成功（红线二）。
	var env map[string]any
	if json.Unmarshal(raw, &env) == nil {
		if e, ok := env["error"].(map[string]any); ok {
			msg, _ := e["message"].(string)
			if strings.TrimSpace(msg) == "" {
				msg = truncate(string(raw), 200)
			}
			// 默认归 UpstreamFault 而不是 SessionDead：只有报文里明确是身份/鉴权问题时
			// 才算凭证死了；否则会把「上游临时抽风」误判成「key 坏了」，逼用户白重登一次。
			return errs.New(classifyBody(errs.UpstreamFault, msg), "凭证校验失败："+msg).
				WithChannel(string(channel.IFlow)).WithAccount(uidOf(c)).
				WithUpstream(truncate(string(raw), 200))
		}
	}
	return nil
}

// Classify 把上游错误归一成有限枚举（D4）。
//
// 401/403 都算凭证问题；此外 iFlow 的**签名错误**也可能以 400/403 返回，
// 但语义同样是「这串凭证/它的签名不被接受」→ 也归 SessionDead（errs 的注释就把
// Signature invalid 列在 SessionDead 名下），这样面板才会提示「重新粘贴」，
// 而不是让人去查网络。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == 401 || status == 403:
		return errs.SessionDead
	case status == 429:
		// 「限流」与「额度耗尽」上游都用 429，只能靠报文区分。
		// 别把 "limit" 当关键词：限流原话里也常带 limit，认它会把「等一会儿就好」
		// 判成「没钱了」，正好判反。
		for _, kw := range []string{"quota", "credit", "balance", "exhaust", "insufficient", "额度"} {
			if strings.Contains(s, kw) {
				return errs.HardCredit
			}
		}
		return errs.SoftRate
	case status == 400 || status == 422:
		if strings.Contains(s, "signature") {
			return errs.SessionDead // 签名失效 = 凭证侧问题
		}
		switch {
		case strings.Contains(s, "context"), strings.Contains(s, "too long"),
			strings.Contains(s, "max_tokens"), strings.Contains(s, "max_new_tokens"):
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

// classifyBody 是 Classify 的「报文版」：HTTP 200 的信封里带 error、或 SSE 帧内带 error 时用
// （这两种场景看不到有意义的 HTTP 状态码）。只有报文里明确是身份/鉴权问题才判 SessionDead，
// 否则归 def（调用方按场景给 UpstreamFault）—— 不能因为「有个 error」就把好凭证判死。
func classifyBody(def errs.Kind, msg string) errs.Kind {
	s := strings.ToLower(msg)
	for _, kw := range []string{"token", "auth", "unauthorized", "forbidden", "signature", "登录", "凭证"} {
		if strings.Contains(s, kw) {
			return errs.SessionDead
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// send 发一个对话请求，带齐官方 CLI 的头与签名。
//
// 头名大小写说明：参考实现里全小写，那是 httpx 自动归一的结果（HTTP 头本身大小写不敏感），
// 所以在 Go 里用标准写法即可 —— 上游不会因为 `User-Agent` 与 `user-agent` 的差别拒请求。
// 但**参与签名的原文**必须用常量 cliUserAgent，不能用被改写过的头值。
func (a *Adapter) send(ctx context.Context, c *channel.Credential, key string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+epChat, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.IFlow)).WithCause(err)
	}
	seed := seedOf(c, key)
	session := sessionIDFor(seed)
	ts := time.Now().UnixMilli() // 毫秒；签名与头必须用同一个值

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", cliUserAgent)
	req.Header.Set("Session-Id", session)
	req.Header.Set("Conversation-Id", conversationIDFor(seed))
	req.Header.Set("X-Iflow-Signature", signature(key, session, ts))
	req.Header.Set("X-Iflow-Timestamp", strconv.FormatInt(ts, 10))

	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.IFlow)).
			WithAccount(uidOf(c)).WithCause(err)
	}
	return resp, nil
}

// apiKeyOf 取凭证里的 apiKey（存在 AccessToken 字段里，见 login.go）。
func apiKeyOf(c *channel.Credential) (string, error) {
	if c != nil {
		if v := strings.TrimSpace(c.AccessToken); v != "" {
			return v, nil
		}
	}
	return "", errs.New(errs.SessionDead, "iFlow 凭证为空：请在面板里重新粘贴 apiKey").
		WithChannel(string(channel.IFlow))
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
