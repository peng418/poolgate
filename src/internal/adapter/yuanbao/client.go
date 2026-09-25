package yuanbao

// client.go 渠道实现：能力声明、会话创建、对话编排、错误归一。
//
// 本渠道的「工具调用」是 **toolshim 模拟**：元宝的网页协议没有原生 tools
// （两份参考实现都没有把 tools 传上去）→ Spec 是 Tools=false + ToolsShim=true。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// errNoUskey 是「粘贴内容里没有 x-uskey」的归一化错误。
var errNoUskey = errs.New(errs.Parse,
	"没识别出 x-uskey：请在浏览器登录元宝后，F12 → Network → 点一个 yuanbao.tencent.com/api 的请求，"+
		"复制 Request Headers 里 `x-uskey` 的值（整段请求头直接粘过来也行）").
	WithChannel(string(channel.Yuanbao))

// Adapter 实现 channel.Channel（登录走 Authorizer + CallbackAcceptor，见 login.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	base string
}

func New() *Adapter {
	return &Adapter{clients: map[string]*http.Client{}, base: apiBase}
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

func (a *Adapter) Kind() channel.Kind { return channel.Yuanbao }

// Spec 能力声明。
//
// Reasoning=true 有依据：上游把思考单独发（`{"type":"think","content":…}`），
// 我们映射成 reasoning_content —— 这一点比豆包那种「靠开关块推断」更干净。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.Yuanbao,
		DisplayName:           "腾讯元宝（网页版）",
		Status:                channel.Active,
		Category:              channel.CategoryChat,
		Tools:                 false,
		ToolsShim:             true,
		Images:                false,
		Reasoning:             true,
		SSEOnly:               true,
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs:                  "粘贴浏览器请求头里的 x-uskey 登录；历史拼成单轮 prompt；工具调用由网关模拟（toolshim）",
	}
}

// Login 是「一次性登录」入口：凭证来自你浏览器里的登录态。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"腾讯元宝需要你在浏览器登录一次：面板「添加账号 → 腾讯元宝」里粘贴 x-uskey（不必给密码）").
		WithChannel(string(channel.Yuanbao))
}

// Models 返回档位（本地清单：上游没有目录接口）。
func (a *Adapter) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	out := make([]channel.ModelInfo, 0, len(webModels))
	for _, m := range webModels {
		out = append(out, channel.ModelInfo{
			ID:          m.ID,
			DisplayName: m.Name,
			// 上游没给上下文长度，不猜数字（F4.2）。
			ContextWindow: 0,
			Source:        channel.SourceLocal,
			Tools:         channel.CapYes, // 客户端可用（由网关模拟）
			// 这两档是推理模型：上游会把思考单独发过来。
			Reasoning: reasonCap(m.Upward),
			Images:    channel.CapNo,
		})
	}
	return out, nil
}

// reasonCap 判断档位是不是推理档（DeepSeek R1 与混元 T1 是）。
func reasonCap(upward string) channel.Cap {
	switch upward {
	case "deep_seek", "hunyuan_t1":
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
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "腾讯元宝没有签到活动"}, nil
}

// Refresh 表示「刷不了」：x-uskey 是长期凭证，没有刷新接口，失效只能重新粘贴。
func (a *Adapter) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}

// Chat 发一次对话：建会话 → 打对话接口 → 解析事件流。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	cr, err := parseCred(tokenOf(c))
	if err != nil {
		if e, ok := err.(*errs.Error); ok {
			return nil, e.WithAccount(uidOf(c))
		}
		return nil, err
	}
	spec := specOf(req.Model)

	convID, err := a.createConversation(ctx, c, cr)
	if err != nil {
		return nil, err
	}
	body := buildBody(packMessages(req.Messages), spec.upward, spec.search)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.base+epChatPrefix+convID, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.Yuanbao)).WithCause(err)
	}
	a.setHeaders(httpReq, cr, "text/event-stream")

	resp, err := a.clientFor(c).Do(httpReq)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.Yuanbao)).
			WithAccount(uidOf(c)).WithCause(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.Yuanbao)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, resp.Header.Get("Content-Type"), req.Model), nil
}

// createConversation 建一个会话并取回它的 id。
//
// 上游没有「无会话直接聊」的入口，所以这一步不能省；id 也只是本次请求用一下就丢
// （我们每次都把完整历史拼进 prompt，不依赖服务端会话记忆）。
func (a *Adapter) createConversation(ctx context.Context, c *channel.Credential, cr *cred) (string, error) {
	body, _ := json.Marshal(map[string]any{"agentId": agentID})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+epCreate, bytes.NewReader(body))
	if err != nil {
		return "", errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.Yuanbao)).WithCause(err)
	}
	a.setHeaders(httpReq, cr, "application/json")

	resp, err := a.clientFor(c).Do(httpReq)
	if err != nil {
		return "", errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.Yuanbao)).
			WithAccount(uidOf(c)).WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", errs.New(a.Classify(resp.StatusCode, raw), "建会话失败").
			WithChannel(string(channel.Yuanbao)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || strings.TrimSpace(out.ID) == "" {
		// 拿不到会话 id 就没法聊：明确报错，不要带着空 id 去请求（那会得到更难懂的错误）。
		return "", errs.New(errs.Parse, "上游没有返回会话 id").
			WithChannel(string(channel.Yuanbao)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return strings.TrimSpace(out.ID), nil
}

// setHeaders 设置伪装头。
//
// 元宝的鉴权就是「浏览器那一次请求的头」，所以我们照浏览器形态补齐；
// 用户粘回来的 UA 优先（他要是从手机浏览器复制的，用他的更一致）。
func (a *Adapter) setHeaders(req *http.Request, cr *cred, accept string) {
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", apiBase)
	req.Header.Set("Referer", apiBase+"/chat/")
	req.Header.Set("x-uskey", cr.uskey)
	if cr.cookie != "" {
		req.Header.Set("Cookie", cr.cookie)
	}
	ua := cr.ua
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) " +
			"Chrome/131.0.0.0 Safari/537.36"
	}
	req.Header.Set("User-Agent", ua)
}

// Classify 把上游错误归一成有限枚举（D4）。
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
		case strings.Contains(s, "context"), strings.Contains(s, "too long"), strings.Contains(s, "长度"):
			return errs.PromptTooLong
		case strings.Contains(s, "sensitive"), strings.Contains(s, "审核"), strings.Contains(s, "违规"):
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

// tokenOf 取凭证里的 uskey（或整段请求头原文）。
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

// 断言：Adapter 必须实现 channel.Channel（编译期检查）。
var _ channel.Channel = (*Adapter)(nil)
