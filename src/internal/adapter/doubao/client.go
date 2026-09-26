package doubao

// client.go 渠道实现：能力声明、对话编排、错误归一。
//
// 本渠道的「工具调用」是 **toolshim 模拟**：网页协议没有原生 tools（参考实现的 README 也
// 明确写「豆包客户端模型不支持 Function Calling」）→ Spec 是 Tools=false + ToolsShim=true。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// errNoSession 是「粘贴内容里没有 sessionid」的归一化错误。
var errNoSession = errs.New(errs.Parse,
	"没识别出 sessionid：请复制浏览器里豆包的整行 Cookie（至少包含 sessionid），"+
		"也可以粘 JSON：{\"cookie\":\"…\",\"device_id\":\"…\",\"web_id\":\"…\"}").
	WithChannel(string(channel.Doubao))

// Adapter 实现 channel.Channel（登录走 Authorizer + CallbackAcceptor，见 login.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client

	base string // 站点基址（测试可换）
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

func (a *Adapter) Kind() channel.Kind { return channel.Doubao }

// Spec 能力声明。
//
// Docs 里写清楚 a_bogus 这件事：本渠道能用，但上游风控随时可能要求人工验证码
// （我们不去伪造签名）。把这个前提摆在面板上，比让用户遇到 710022004 时不明所以好。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.Doubao,
		DisplayName:           "豆包（网页版）",
		Status:                channel.Active,
		Category:              channel.CategoryChat,
		Tools:                 false,
		ToolsShim:             true,
		Images:                false,
		Reasoning:             true, // 10040 开关块之间的内容 → reasoning_content
		SSEOnly:               true,
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs: "粘贴浏览器 Cookie 登录；请求签名 a_bogus 由浏览器现算，本渠道不带它 —— " +
			"可用但可能被风控要求人工验证码；工具调用由网关模拟（toolshim）",
	}
}

// Login 是「一次性登录」入口：凭证来自你浏览器里的登录态。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"豆包需要你在浏览器登录一次：面板「添加账号 → 豆包」里粘贴整行 Cookie（不必给密码）").
		WithChannel(string(channel.Doubao))
}

// Models 返回档位（本地清单，如实标注：上游不是按模型名选，而是靠 need_deep_think）。
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
			Reasoning:     capOf(m.Think > 0),
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
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "豆包没有签到活动"}, nil
}

// Refresh 表示「刷不了」：豆包的登录态是一串 Cookie，没有刷新接口，失效只能重新粘贴。
// 返回 (nil, nil) 让上层按 401 处理，而不是假装刷新成功。
func (a *Adapter) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}

// Chat 发一次对话：拼成单条文本 → 带全套安全参数的 GET 查询串 + POST body → 解析事件流。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	cr, err := parseCred(tokenOf(c))
	if err != nil {
		// 凭证格式不对：把账号带上，面板里能定位到是哪一个账号粘坏了。
		if e, ok := err.(*errs.Error); ok {
			return nil, e.WithAccount(uidOf(c))
		}
		return nil, err
	}
	think, ok := thinkOf(req.Model)
	if !ok {
		think = 0 // 认不出的档位回落到默认档（由网关的模型校验先挡一道）
	}

	body := buildBody(packMessages(req.Messages), think, cr)
	url := a.base + epCompletion + "?" + queryParams(cr, randHex16())
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.Doubao)).WithCause(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	httpReq.Header.Set("Origin", apiBase)
	httpReq.Header.Set("Referer", apiBase+"/chat")
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "+
		"(KHTML, like Gecko) Chrome/"+chromeVersion+" Safari/537.36")
	httpReq.Header.Set("Cookie", cr.cookie)
	if cr.csrf != "" {
		httpReq.Header.Set("x-tt-passport-csrf-token", cr.csrf)
	}

	resp, err := a.clientFor(c).Do(httpReq)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.Doubao)).
			WithAccount(uidOf(c)).WithCause(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.Doubao)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, resp.Header.Get("Content-Type"), req.Model), nil
}

// Classify 把上游错误归一成有限枚举（D4）。
//
// 401/403 都算凭证问题：豆包没有刷新机制，失效只能重新粘 Cookie。
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

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// packMessages 把内部消息拼成上游要的一段文本（上游只吃单轮）。
//
// 标记形态沿用仓库里其它网页渠道的约定（[function_calls] / [TOOL_RESULT for id]），
// 这样 toolshim 在两侧的形态是一致的，换渠道不用改提示词模板。
func packMessages(msgs []channel.Message) string {
	var lines []string
	for _, m := range msgs {
		text := strings.TrimSpace(m.Content)
		role := m.Role
		if role == "assistant" && len(m.ToolCalls) > 0 {
			calls := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, "[call:"+tc.Function.Name+"]"+tc.Function.Arguments+"[/call]")
			}
			if len(calls) > 0 {
				text = "[function_calls]\n" + strings.Join(calls, "\n") + "\n[/function_calls]"
			}
		}
		if role == "tool" {
			role = "user"
			if m.ToolCallID != "" {
				text = "[TOOL_RESULT for " + m.ToolCallID + "] " + text
			}
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if role == "system" || role == "developer" {
			role = "system"
		}
		lines = append(lines, role+":"+text)
	}
	return strings.Join(lines, "\n")
}

// tokenOf 取凭证里的 Cookie 串（AccessToken 存的就是它）。
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

// randHex16 生成一个 16 字节随机十六进制串（web_tab_id 用）。
func randHex16() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// randUUID4 生成一个 UUIDv4 串。
//
// 上游对 local_message_id / block_id / unique_key 这几个字段要的是**UUID 形态**的字面量，
// 参考实现（doubao2api `_build_completion_payload`）就是 `str(uuid.uuid4())`，
// 空串会让上游把这次请求当成非法客户端。形态必须是带连字符的 8-4-4-4-12。
func randUUID4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// 断言：Adapter 必须实现 channel.Channel（编译期检查，避免接口漂移后才发现）。
var _ channel.Channel = (*Adapter)(nil)
