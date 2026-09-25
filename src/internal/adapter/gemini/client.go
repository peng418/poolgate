package gemini

// client.go 渠道实现：能力声明、模型表、对话（Code Assist 的 SSE）、错误归一。

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Adapter 实现 channel.Channel（授权走 Authorizer + CallbackAcceptor，见 oauth.go）。
type Adapter struct {
	mu      sync.Mutex
	clients map[string]*http.Client
	// 三个端点字段化，便于测试指向 mock server（生产用 constants.go 里的常量）。
	codeAssistBase string
	tokenURL       string
	userInfoURL    string
}

func New() *Adapter {
	return &Adapter{
		clients:        map[string]*http.Client{},
		codeAssistBase: epCodeAssist,
		tokenURL:       epToken,
		userInfoURL:    epUserInfo,
	}
}

// clientFor 按「有效出口」缓存客户端（全局出口代理可在运行中改）。
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

func (a *Adapter) Kind() channel.Kind { return channel.Gemini }

// Spec 能力声明。
//
// Tools=true 且是**原生**（functionDeclarations / functionCall），所以不需要模拟层。
// Category=chat：这是「登录式」渠道（官方 OAuth，不用 key），与订阅额度池的 IDE 类分开展示。
func (a *Adapter) Spec() channel.Spec {
	return channel.Spec{
		Kind:                  channel.Gemini,
		DisplayName:           "Gemini（Google Code Assist）",
		Status:                channel.Active,
		Category:              channel.CategoryChat,
		Tools:                 true,
		Images:                false,
		Reasoning:             true, // thought parts 会转成 reasoning_content
		SSEOnly:               true,
		CheckinCap:            false,
		DefaultMinIntervalSec: defaultMinIntervalSec,
		Docs:                  "Gemini Code Assist CLI 的官方 OAuth（1000 请求/用户/天）",
	}
}

// Login 是 channel.Channel 要求的「一次性登录」：Gemini 需要用户交互（粘贴授权码），
// 所以走面板授权通道（channel.Authorizer）。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"Gemini 需要交互式授权：请在面板「账号」页点「添加账号 → Gemini」，浏览器授权后把授权码粘回面板").
		WithChannel(string(channel.Gemini))
}

// Models 返回可用模型（本地清单，如实标注 SourceLocal）。
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

// Balance 余额未知：Code Assist 是按请求数计的日额度，没有余额接口，不猜数字。
func (a *Adapter) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{Known: false}, nil
}

// Checkin 无签到活动。
func (a *Adapter) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{OK: false, NoActivity: true, Message: "Code Assist 没有签到活动"}, nil
}

// Chat 发起一次流式对话。
func (a *Adapter) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	project := ""
	if c.Extra != nil {
		project = strings.TrimSpace(c.Extra["project"])
	}
	if project == "" {
		// 凭证里没有项目（老凭证/手工导入）：就地开通一次，失败就说清原因（不静默）。
		p, err := a.ensureProject(ctx, c)
		if err != nil {
			return nil, err
		}
		project = p
	}
	body, err := buildRequest(req, project, "poolgate-"+c.UID)
	if err != nil {
		return nil, errs.New(errs.Parse, "构造请求体失败").WithChannel(string(channel.Gemini)).
			WithAccount(c.UID).WithCause(err)
	}
	url := a.codeAssistBase + "/v1internal:streamGenerateContent?alt=sse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.Gemini)).
			WithAccount(c.UID).WithCause(err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", cliUA)

	resp, err := a.clientFor(c).Do(httpReq)
	if err != nil {
		return nil, errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.Gemini)).
			WithAccount(c.UID).WithCause(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.Gemini)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	return newStream(resp.Body, req.Model), nil
}

// Classify 把上游错误归一成有限枚举。
func (a *Adapter) Classify(status int, body []byte) errs.Kind {
	s := strings.ToLower(string(body))
	switch {
	case status == 401 || status == 403:
		// 403 也可能是「没开通/地区不支持」——报文里有区分的枚举，尽量标出来
		if strings.Contains(s, "unsupported_location") || strings.Contains(s, "ineligible") ||
			strings.Contains(s, "restricted") || strings.Contains(s, "validation") {
			return errs.AuthFailed
		}
		return errs.SessionDead
	case status == 412:
		// Precondition Failed：典型是「账号没开通 Code Assist」
		return errs.AuthFailed
	case status == 429:
		// 关键词要具体：「limit」这种宽词会命中 "rate limited"（那其实是限流，不是额度耗尽），
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

// stream 把 Code Assist 的 SSE 转成 chunk 流。
//
// 两个细节与别家不同：① 每帧外面还包了一层 `response`，要先解包；
// ② SSE 允许 `data:` 跨多行，遇到空行才算一帧（按行直接 parse 会解析失败）。
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
				s.emit(payload)
			}
			data = data[:0]
		}
		if err == io.EOF {
			if payload := strings.Join(data, "\n"); payload != "" {
				s.emit(payload) // 收尾：最后一帧可能没有空行结尾
			}
			return
		}
	}
}

func (s *stream) emit(payload string) {
	var frame map[string]any
	if json.Unmarshal([]byte(payload), &frame) != nil {
		return // 非 JSON 帧（注释/心跳）跳过
	}
	resp, _ := frame["response"].(map[string]any)
	if resp == nil {
		resp = frame // 容错：万一上游没包那层
	}
	if c, ok := chunksFromFrame(resp, s.model, &s.seq); ok {
		s.ch <- chunkOrErr{chunk: c}
	}
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
