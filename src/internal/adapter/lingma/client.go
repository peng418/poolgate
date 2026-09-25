// client.go 渠道实现：能力声明、对话编排、模型目录、错误归一。
//
// 登录走「粘贴 IDE 缓存」（见 login.go 的 Authorizer + CallbackAcceptor）。
//
// 与网页版聊天渠道最大的不同：灵码**支持原生 tools/tool_calls**（请求体透传、
// 响应里就是 OpenAI 形态的 delta.tool_calls），所以 Spec.Tools=true、ToolsShim=false ——
// 工具调用由网关原样转发，不经过模拟层。
package lingma

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Adapter 实现 channel.Channel（通义灵码渠道）。
type Adapter struct {
	base string // API 基址（测试可换）

	clientsMu sync.Mutex
	clients   map[string]*http.Client

	// 模型目录缓存：上次成功拉取即缓存（无静态兜底表）。
	modelsMu sync.RWMutex
	modelMap map[string]string // 客户端名 → 上游 model key
	cache    []modelEntry
}

// New 生产默认。
func New() *Adapter {
	return &Adapter{
		base:     baseURL,
		clients:  map[string]*http.Client{},
		modelMap: map[string]string{},
	}
}

// clientFor 按凭证的出口建/取 HTTP 客户端（一账号一出口，见 channel.NewHTTPClient）。
// key 用 EgressOf 而不是 UID：同一出口的账号可以共用连接池，不同出口必须分开。
func (a *Adapter) clientFor(c *channel.Credential) *http.Client {
	key := channel.EgressOf(c)
	a.clientsMu.Lock()
	defer a.clientsMu.Unlock()
	if cl, ok := a.clients[key]; ok {
		return cl
	}
	cl := channel.NewHTTPClient(c, httpTimeout)
	a.clients[key] = cl
	return cl
}

// Kind 实现 channel.Channel。
func (a *Adapter) Kind() channel.Kind { return channel.Lingma }

// Spec 实现 channel.Channel。能力位如实标注，宁少不多：
//
//   - Tools=true —— 上游原生支持，且我们确实把 tools 透传、把 delta.tool_calls 解析回来了；
//   - Images=false —— 不是「上游不支持」（它其实有 image_urls 字段），而是**内部契约里
//     channel.Message.Content 是纯字符串**，图片根本传不进来；声明 true 只会让客户端
//     以为传了图，实际被网关清洗掉；
//   - Reasoning=false —— 上游没有单独分流的思考通道（参考实现也没解析 reasoning_content），
//     思考内容会混在正文里。声明 true 会让客户端把正文当 reasoning 显示；
//   - SSEOnly=true —— 端点名字里就写着 sse，非流式由本地聚合。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.Lingma,
		DisplayName:           "通义灵码（Lingma）",
		Status:                channel.Active,
		Category:              channel.CategoryCoding, // 编程助手/IDE 类
		Tools:                 true,
		ToolsShim:             false,
		Images:                false,
		Reasoning:             false,
		SSEOnly:               true,
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs: "登录式：粘贴 IDE 的 cache/user 与 cache/id，由服务端解密出 COSY 凭证；" +
			"这是**第三方客户端复用官方 IDE 登录态**，上游风控针对此类用法，**有封号风险**；" +
			"工具调用为上游原生；不含图片与思考分流",
	}
}

// Login 是「一次性登录」入口：灵码的凭证来自你本地 IDE 的缓存，必须用户参与粘贴。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"通义灵码需要你粘贴 IDE 登录缓存：面板「添加账号 → 通义灵码」里按提示粘 cache/user 与 cache/id").
		WithChannel(string(channel.Lingma))
}

// Refresh 是账号级续期。灵码**没有续期机制**：cosy_key 由 IDE 登录时签发，
// 过期只能回 IDE 重新登录再粘一次缓存。所以这里明确返回 (nil, nil) 表示「刷不了」，
// 让上层按原样使用旧凭证、并在真失败时提示用户重新粘贴 —— 绝不假装刷新成功。
func (a *Adapter) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}

// Balance 余额未知：灵码没有可查的余额接口，不猜数字（F4.2）。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin 无签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "通义灵码没有签到活动"}, nil
}

// Chat 实现 channel.Channel：组装请求体 → 算 COSY 签名 → POST → 归一成 chunk 流。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	lc := credOf(c)
	if err := lc.validate(); err != nil {
		return nil, errs.New(errs.SessionDead, "凭证不完整："+err.Error()).
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c))
	}
	modelKey := a.modelKey(req.Model)

	body, err := buildChatBody(req, modelKey)
	if err != nil {
		return nil, errs.New(errs.Parse, "构造请求体失败").
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c)).WithCause(err)
	}
	rawURL := a.base + epChat + chatQuery
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c)).WithCause(err)
	}
	// 签名覆盖 body 原文，必须与本请求真正发送的字节逐字一致。
	if err := lc.signer().applyHeaders(httpReq, string(body), rawURL, "text/event-stream"); err != nil {
		return nil, errs.New(errs.Parse, "设置 COSY 头失败").
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c)).WithCause(err)
	}

	resp, err := a.clientFor(c).Do(httpReq)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c)).WithCause(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, req.Model), nil
}

// ---------------------------------------------------------------------------
// 错误归一：上游 HTTP status + body → 有限 errs.Kind（D4）
// ---------------------------------------------------------------------------

// hardMarkers 是「额度/权益耗尽」的文案特征。它们与限流都用 429，只能靠报文区分。
var hardMarkers = []string{
	"insufficient", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "not enough",
	"积分不足", "额度不足", "余额不足", "额度用尽", "没有积分", "欠费",
}

// Classify 按 HTTP 状态码 + body 判定错误类别（包级函数，便于单测）。
//
// 429 的判定顺序很关键：先认额度词、否则归限流 —— 反过来会把「等一会儿就好」
// 判成「没钱了，等多久都没用」。
func Classify(status int, body []byte) errs.Kind {
	lower := strings.ToLower(string(body))
	switch {
	case status == http.StatusPaymentRequired:
		return errs.HardCredit
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// 灵码的鉴权失败几乎都是签名/凭证问题（COSY 过期、machineID 对不上），
		// 提示用户重新粘贴比当成上游故障有用得多。
		return errs.SessionDead
	case status == http.StatusTooManyRequests:
		for _, m := range hardMarkers {
			if strings.Contains(lower, m) {
				return errs.HardCredit
			}
		}
		return errs.SoftRate
	case status == http.StatusNotFound:
		return errs.ModelUnavailable
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		switch {
		case strings.Contains(lower, "too long"), strings.Contains(lower, "context"),
			strings.Contains(lower, "max_tokens"):
			return errs.PromptTooLong
		case strings.Contains(lower, "sensitive"), strings.Contains(lower, "policy"),
			strings.Contains(lower, "moderation"), strings.Contains(lower, "风控"):
			return errs.ContentBlocked
		case strings.Contains(lower, "model"), strings.Contains(lower, "not found"):
			return errs.ModelUnavailable
		}
		return errs.Parse
	case status >= 500:
		return errs.UpstreamFault
	case status >= 400:
		for _, m := range hardMarkers {
			if strings.Contains(lower, m) {
				return errs.HardCredit
			}
		}
		return errs.Parse
	}
	// 2xx 但被判成错误（流内 statusCode 等）：未知不静默当成功 —— 返回可定位的分类。
	return errs.Parse
}

// Classify 实现 channel.Channel。
func (a *Adapter) Classify(status int, body []byte) errs.Kind { return Classify(status, body) }

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

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
