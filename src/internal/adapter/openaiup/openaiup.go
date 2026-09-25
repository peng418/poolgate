// Package openaiup 是「通用 OpenAI 兼容上游」适配器 —— 一个适配器覆盖一整类来源。
//
// 用途：把官方 API（Google AI Studio / 阿里百炼 / OpenRouter / DeepSeek / 智谱 / 火山方舟 …）
// 按 PoolGate 的渠道契约接进来。它们的线上格式本来就是 OpenAI 兼容（/chat/completions、
// /models、choices[].delta），所以**不需要一家写一个适配器** —— 这正是 docs/05 §6 的结论：
// 「配置级」接入，填 base_url + key + 模型名即可。
//
// 与登录授权式渠道的区别（面板会把两类并到一张表里，但生命周期完全不同）：
//   - 凭证：这里是用户填的一串 key（配置），不是登录换来的会话；没有续期、没有「重新登录」；
//   - 工具调用：由配置声明（SupportsTools）。声明不支持时网关会**明确拒绝**带 tools 的请求，
//     不会静默丢弃 —— 0.4.2 之前静默丢弃 tools 导致客户端拿到一堆乱码，那条教训写进了红线一。
package openaiup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Config 是一个 API Key 式来源的配置（由装配层从 store.ProviderConfig 映射过来）。
type Config struct {
	Name          string   // 模型前缀，同时是 channel.Kind
	DisplayName   string   // 面板显示名
	BaseURL       string   // 不含 /chat/completions，如 https://api.deepseek.com/v1
	APIKey        string   //
	Models        []string // 手填清单（上游没有 /models 时兜底）
	SupportsTools bool     // 声明能力位；false 时网关明确拒绝带 tools 的请求
	ToolsMode     string   // native | shim | none（空 = 按 SupportsTools 推）
	Notes         string   // 免费额度/实名等提示（面板展示）
}

// Adapter 实现 channel.Channel。
type Adapter struct {
	cfg  Config
	http *http.Client
}

// New 建立适配器。base_url 会去掉尾部斜杠。
func New(cfg Config) *Adapter {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	return &Adapter{
		cfg: cfg,
		// 上游是第三方 API，超时给足；首字之前不设死，长思考模型靠客户端自己取消。
		http: &http.Client{Timeout: 10 * time.Minute},
	}
}

// Config 返回配置副本（面板编辑时要用）。
func (a *Adapter) Config() Config { return a.cfg }

func (a *Adapter) Kind() channel.Kind { return channel.Kind(a.cfg.Name) }

// Spec 能力声明。
//
// Tools 由配置声明 —— 声明 true 却上游不支持是**撒谎**，会让客户端在跑 agent 时撞到
// 「模型把工具调用写成文本」；反之声明 false 时网关会明确拒绝，客户端能立刻看出原因。
func (a *Adapter) Spec() channel.Spec {
	name := a.cfg.DisplayName
	if name == "" {
		name = a.cfg.Name
	}
	mode := a.cfg.ToolsMode
	if mode == "" {
		if a.cfg.SupportsTools {
			mode = "native"
		} else {
			mode = "none"
		}
	}
	return channel.Spec{
		Kind:        channel.Kind(a.cfg.Name),
		DisplayName: name,
		Status:      channel.Active,
		Tools:       mode == "native",
		ToolsShim:   mode == "shim",
		Images:      false, // 本适配器不做图片上行；声明 true 会误导客户端
		Reasoning:   false, // 未知就不猜（上游各家字段不一，reasoning_content 仍会透传）
		SSEOnly:     true,  // 统一向上游要 stream，非流式由本地聚合
		CheckinCap:  false, // API 计费没有签到
		Docs:        a.cfg.BaseURL,
	}
}

// Credential 合成一条「账号」给池子用。
//
// key 式来源没有多账号概念，但它必须进池 —— 否则路由层选不到号（NoCandidate）。
// UID 用来源名，AccessToken 就是 key，base_url 走 Extra 传给 Chat。
func (a *Adapter) Credential() channel.Credential {
	return channel.Credential{
		UID:         a.cfg.Name,
		Nickname:    a.cfg.DisplayName,
		AccessToken: a.cfg.APIKey,
		Extra:       map[string]string{"base_url": a.cfg.BaseURL, "auth": "api_key"},
	}
}

// baseURL 取本次调用要用的 base_url（凭证里带优先，便于同来源多 key 指向不同网关）。
func (a *Adapter) baseURL(c *channel.Credential) string {
	if c != nil && c.Extra != nil {
		if v := strings.TrimRight(strings.TrimSpace(c.Extra["base_url"]), "/"); v != "" {
			return v
		}
	}
	return a.cfg.BaseURL
}

// Login 返回明确错误：key 式来源不走面板授权流程。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"API Key 式来源不需要授权：在面板「接入源」里填 key 即可").WithChannel(string(a.Kind()))
}

// Refresh 表示「刷不了」：key 不会自动续期，失效了要去上游重新签发。
// 返回 (nil, nil) 让上层按 401 逻辑处理（不要假装刷新成功）。
func (a *Adapter) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}

// Balance 余额未知 —— API 计费来源不提供统一余额接口，不猜数字（F4.2）。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin 无签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "API 计费来源没有签到"}, nil
}

// Models 拉上游模型目录；拿不到就用手填清单；两者都空则报错（不静默给空列表）。
func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL(c)+"/models", nil)
	if err != nil {
		return a.fallbackModels()
	}
	req.Header.Set("Authorization", "Bearer "+tokenOf(c, a.cfg.APIKey))
	resp, err := a.http.Do(req)
	if err != nil {
		return a.fallbackModels()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return a.fallbackModels()
	}
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Data) == 0 {
		return a.fallbackModels()
	}
	models := make([]channel.ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		models = append(models, channel.ModelInfo{
			ID:    m.ID,
			Tools: capFromBool(a.cfg.SupportsTools),
			// 上下文窗口上游多不提供，留 0 表示未知 —— 不猜数字。
			Source: channel.SourceUpstream,
		})
	}
	if len(models) == 0 {
		return a.fallbackModels()
	}
	return models, nil
}

// fallbackModels 用手填清单兜底；为空时返回明确错误。
func (a *Adapter) fallbackModels() ([]channel.ModelInfo, error) {
	if len(a.cfg.Models) == 0 {
		return nil, errs.New(errs.UpstreamFault,
			"拿不到模型目录：上游 /models 不可用，且没有手填模型清单").
			WithChannel(string(a.Kind())).WithAccount(a.cfg.Name)
	}
	out := make([]channel.ModelInfo, 0, len(a.cfg.Models))
	for _, id := range a.cfg.Models {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		out = append(out, channel.ModelInfo{
			ID:     id,
			Tools:  capFromBool(a.cfg.SupportsTools),
			Source: channel.SourceLocal, // 来源标注：手填，不是上游给的
		})
	}
	return out, nil
}

func capFromBool(b bool) channel.Cap {
	if b {
		return channel.CapYes
	}
	return channel.CapNo
}

// Chat 发一次对话。统一向上游要流式（SSEOnly=true），非流式由网关本地聚合。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	body := buildBody(req)
	url := a.baseURL(c) + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithAccount(c.UID).WithCause(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+tokenOf(c, a.cfg.APIKey))

	resp, err := a.http.Do(httpReq)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithAccount(c.UID).WithCause(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(a.Kind())).WithAccount(c.UID).
			WithUpstream(truncate(string(raw), 200))
	}
	// 上游可能无视 stream:true 直接回整包 JSON —— 交给流解析器按 Content-Type 判断。
	return newStream(resp.Body, resp.Header.Get("Content-Type"), req.Model), nil
}

// Classify 把上游错误归一成有限枚举（D4）。
//
// 关键区分：**429 有两副面孔** —— 限流（等一会儿就好）与额度耗尽（等多久都没用，要换 key/充值）。
// 上游普遍都用 429，只能靠报文关键词区分；分错了会把「没钱了」当成「稍后重试」，
// 于是客户端看到的是无休止的重试而不是明确的失败原因。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == 401 || status == 403:
		return errs.SessionDead // key 失效/无权限：去上游重新签发
	case status == 402:
		return errs.HardCredit
	case status == 429:
		for _, kw := range []string{"quota", "insufficient", "credit", "balance", "exhaust", "billing"} {
			if strings.Contains(s, kw) {
				return errs.HardCredit
			}
		}
		return errs.SoftRate
	case status == 400 || status == 422:
		switch {
		case strings.Contains(s, "context length"), strings.Contains(s, "too long"),
			strings.Contains(s, "maximum context"), strings.Contains(s, "max_tokens"):
			return errs.PromptTooLong
		case strings.Contains(s, "content_filter"), strings.Contains(s, "content policy"),
			strings.Contains(s, "safety"):
			return errs.ContentBlocked
		case strings.Contains(s, "model"), strings.Contains(s, "not found"),
			strings.Contains(s, "does not exist"), strings.Contains(s, "unsupported"):
			return errs.ModelUnavailable
		}
		return errs.Parse
	case status == 404:
		// 多数是模型名写错或 base_url 少/多了路径段。
		return errs.ModelUnavailable
	case status >= 500:
		return errs.UpstreamFault
	}
	return errs.Parse
}

// tokenOf 取本次调用用的 key（凭证里带优先）。
func tokenOf(c *channel.Credential, fallback string) string {
	if c != nil && strings.TrimSpace(c.AccessToken) != "" {
		return strings.TrimSpace(c.AccessToken)
	}
	return strings.TrimSpace(fallback)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
