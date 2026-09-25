package qwen

// client.go 渠道实现：能力声明、模型表、对话、错误归一。
//
// 对话走 CLI 端点（portal.qwen.ai），它是纯 OpenAI 格式、无需浏览器指纹；
// 登录/授权走网页域（chat.qwen.ai），见 login.go。

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Adapter 实现 channel.Channel 与 channel.Authorizer。
type Adapter struct {
	// clients 按「有效出口」缓存客户端：全局出口代理能在运行中改，
	// 每次都新建 Transport 会丢掉连接复用（流式请求尤其吃亏）。
	mu      sync.Mutex
	clients map[string]*http.Client
	// chatBase / cliBase 字段化，便于测试指向 mock server。
	chatBase string
	cliBase  string
}

// New 建立适配器。
func New() *Adapter {
	return &Adapter{
		clients:  map[string]*http.Client{},
		chatBase: chatBase,
		cliBase:  cliBase,
	}
}

// clientFor 取本次调用要用的 HTTP 客户端。
//
// 没绑代理就复用默认客户端（共享连接池）；绑了代理**按凭证新建** —— 这就是
// 「一账号一出口」的落点（见 docs/06 防封号设计）。
func (a *Adapter) clientFor(c *channel.Credential) *http.Client {
	key := channel.EgressOf(c)
	a.mu.Lock()
	defer a.mu.Unlock()
	if cl, ok := a.clients[key]; ok {
		return cl
	}
	cl := channel.NewHTTPClient(c, chatTimeout)
	a.clients[key] = cl
	return cl
}

func (a *Adapter) Kind() channel.Kind { return channel.Qwen }

// Spec 能力声明。
//
// Tools=true 是**原生**：CLI 端接受 OpenAI 形态的 tools，并按标准返回 tool_calls 增量
// （含流式 arguments 分片、tool_choice 四态），所以不用 toolshim 模拟，可靠性高一档。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:        channel.Qwen,
		DisplayName: "通义（Qwen）",
		Status:      channel.Active,
		Category:    channel.CategoryChat, // 聊天平台类：与编程助手类隔离展示
		Tools:       true,
		Images:      false,
		Reasoning:   true, // 上游会给 reasoning_content（参考实现实测）
		SSEOnly:     true,
		CheckinCap:  false,
		// 网页/CLI 类账号同号并发猛打最容易被判异常；出厂就慢一点（设置可覆盖）。
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs:                  "Qwen Code CLI 的 OAuth + portal.qwen.ai/v1/chat/completions",
	}
}

// Models 返回 CLI 端的模型表。
//
// 这里**没有**目录接口可拉（参考实现也是硬编码），所以标注 SourceLocal ——
// 面板上要能看出「这是本地声明的清单，不是上游给的」，别把我们的常量说成上游的事实。
func (a *Adapter) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	out := make([]channel.ModelInfo, 0, len(cliModels))
	for _, m := range cliModels {
		name := m.Name
		if m.Note != "" {
			name = m.Name + "（" + m.Note + "）"
		}
		out = append(out, channel.ModelInfo{
			ID:            m.ID,
			DisplayName:   name,
			ContextWindow: m.Context,
			Source:        channel.SourceLocal,
			Tools:         channel.CapYes,
			Reasoning:     channel.CapYes,
			Images:        channel.CapNo,
		})
	}
	return out, nil
}

// Balance 余额未知：CLI 端没有余额接口，不猜数字（F4.2）。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin 无签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "通义没有签到活动"}, nil
}

// Chat 发起一次对话（CLI 端点）。错误归一成 errs.Error，由上层决定冷却与换号。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	body, err := buildBody(req)
	if err != nil {
		return nil, errs.New(errs.Parse, "构造请求体失败").WithChannel(string(channel.Qwen)).WithAccount(c.UID).WithCause(err)
	}
	url := a.cliBase + epChat
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.Qwen)).WithAccount(c.UID).WithCause(err)
	}
	for k, v := range cliHeaders {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.AccessToken)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := a.clientFor(c).Do(httpReq)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.Qwen)).
			WithAccount(c.UID).WithCause(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.Qwen)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, resp.Header.Get("Content-Type"), req.Model), nil
}

// Classify 把上游错误归一成有限枚举（D4）。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == 401 || status == 403:
		return errs.SessionDead // token 失效/被拒：重新授权或续期
	case status == 402:
		return errs.HardCredit
	case status == 429:
		for _, kw := range []string{"quota", "insufficient", "credit", "balance", "exhaust", "limit exceeded"} {
			if strings.Contains(s, kw) {
				return errs.HardCredit
			}
		}
		return errs.SoftRate
	case status == 404:
		return errs.ModelUnavailable
	case status == 400 || status == 422:
		switch {
		case strings.Contains(s, "context length"), strings.Contains(s, "too long"),
			strings.Contains(s, "max_tokens"), strings.Contains(s, "maximum context"):
			return errs.PromptTooLong
		case strings.Contains(s, "content"), strings.Contains(s, "safety"), strings.Contains(s, "filter"):
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

// cliHeaders 是 CLI 端要求的客户端头。
//
// 这些都是**公开客户端标识**（版本号、运行环境），不是凭证；参考实现实测全部可在服务端构造，
// 不需要浏览器指纹。版本号跟着上游客户端走，过旧可能被拒。
var cliHeaders = map[string]string{
	"Content-Type":                "application/json",
	"User-Agent":                  cliUA,
	"X-Dashscope-Useragent":       cliUA,
	"X-Stainless-Lang":            "js",
	"X-Stainless-Package-Version": "5.11.0",
	"X-Stainless-Os":              "MacOS",
	"X-Stainless-Arch":            "arm64",
	"X-Stainless-Runtime":         "node",
	"X-Stainless-Runtime-Version": "v22.17.0",
	"X-Stainless-Retry-Count":     "0",
	"X-Dashscope-Authtype":        "qwen-oauth",
	"X-Dashscope-Cachecontrol":    "enable",
	"Sec-Fetch-Mode":              "cors",
}
