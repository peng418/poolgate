package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/health"
	"poolgate/internal/pool"
	"poolgate/internal/registry"
)

// fakeKey 是 KeyVerifier 的测试实现。
type fakeKey struct{ keys map[string]bool }

func (f *fakeKey) Verify(k string) bool { return f.keys[k] }

// fakeChannel 是 channel.Channel 的测试实现。
type fakeChannel struct {
	kind channel.Kind
	spec channel.Spec
	chat func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error)
	snap []channel.ModelInfo
}

func (f *fakeChannel) Kind() channel.Kind                                 { return f.kind }
func (f *fakeChannel) Spec() channel.Spec                                 { return f.spec }
func (f *fakeChannel) Login(context.Context) (*channel.Credential, error) { return nil, nil }
func (f *fakeChannel) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}
func (f *fakeChannel) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	return f.snap, nil
}
func (f *fakeChannel) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{}, nil
}
func (f *fakeChannel) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{}, nil
}
func (f *fakeChannel) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	return f.chat(ctx, c, req)
}
func (f *fakeChannel) Classify(status int, body []byte) errs.Kind { return errs.Parse }
func (f *fakeChannel) ModelsSnapshot() []channel.ModelInfo        { return f.snap }

// sliceStream 是 channel.Stream 的内存实现。
type sliceStream struct {
	chunks []channel.ChatCompletionChunk
	i      int
}

func (s *sliceStream) Next() (channel.ChatCompletionChunk, error) {
	if s.i >= len(s.chunks) {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}
func (s *sliceStream) Close() error { return nil }

func chunk(content string) channel.ChatCompletionChunk {
	c := channel.ChatCompletionChunk{ID: "chatcmpl-1", Model: "qwen3.8-max"}
	ch := channel.ChunkChoice{}
	ch.Delta.Content = content
	c.Choices = append(c.Choices, ch)
	return c
}

func newTestGateway(t *testing.T, ch *fakeChannel, account bool) (*Server, *pool.Pool) {
	t.Helper()
	registry.Reset()
	registry.Register(ch, ch.spec)
	p := pool.New()
	if account {
		p.AddFor(ch.kind, channel.Credential{UID: "u1", Nickname: "u1", AccessToken: "dt"})
		p.SetCredits(ch.kind, "u1", 100)
	}
	keys := &fakeKey{keys: map[string]bool{"good-key": true}}
	return New(p, keys, Options{}), p
}

func chatReqBody(t *testing.T, model string, stream bool) *http.Request {
	t.Helper()
	body := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"stream":   stream,
	}
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer good-key")
	return r
}

func TestChatRequiresAPIKey(t *testing.T) {
	ch := &fakeChannel{kind: channel.QoderCN, spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active}}
	srv, _ := newTestGateway(t, ch, true)
	// 不带 Authorization 头
	r := chatReqBody(t, "qodercn/qwen3.8-max", false)
	r.Header.Del("Authorization")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无 API Key 应 401，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "AuthFailed") {
		t.Fatalf("401 应带 kind，实际 %s", w.Body.String())
	}
}

func TestChatNonStreaming(t *testing.T) {
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			return &sliceStream{chunks: []channel.ChatCompletionChunk{chunk("你"), chunk("好")}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, chatReqBody(t, "qodercn/qwen3.8-max", false))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "你好" {
		t.Fatalf("聚合内容应为「你好」，got %+v", out.Choices)
	}
}

func TestChatStreaming(t *testing.T) {
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			return &sliceStream{chunks: []channel.ChatCompletionChunk{chunk("你"), chunk("好")}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, chatReqBody(t, "qodercn/qwen3.8-max", true))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "data: ") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("流式应含 data 帧与 [DONE]，got %s", body)
	}
	if !strings.Contains(body, "你") || !strings.Contains(body, "好") {
		t.Fatalf("流式应透传内容，got %s", body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("流式 Content-Type 应为 text/event-stream，got %q", ct)
	}
}

func TestNoCandidateReturnsError(t *testing.T) {
	ch := &fakeChannel{kind: channel.QoderCN, spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active}}
	srv, _ := newTestGateway(t, ch, false) // 无账号
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, chatReqBody(t, "qodercn/qwen3.8-max", false))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("无可用账号应 503，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "NoCandidate") {
		t.Fatalf("应带 NoCandidate kind，实际 %s", w.Body.String())
	}
}

func TestPausedChannelRejected(t *testing.T) {
	ch := &fakeChannel{kind: channel.QoderCN, spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Paused}}
	srv, _ := newTestGateway(t, ch, true)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, chatReqBody(t, "qodercn/qwen3.8-max", false))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("暂停渠道应拒绝，实际 %d", w.Code)
	}
}

// 红线一：上游错误必须客户端可见 —— 非流式下返回结构化 kind + 上游原话。
func TestUpstreamErrorVisibleNonStreaming(t *testing.T) {
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			return nil, errs.New(errs.HardCredit, "余额不足").WithUpstream("insufficient credit")
		},
	}
	srv, p := newTestGateway(t, ch, true)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, chatReqBody(t, "qodercn/qwen3.8-max", false))
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("HardCredit 应 402，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "insufficient credit") {
		t.Fatalf("上游原话应透传，实际 %s", w.Body.String())
	}
	// 账号应被冷却（HardCredit 计入账号错误）
	if _, ok := p.Pick(context.Background(), channel.QoderCN, nil); ok {
		t.Fatal("HardCredit 应冷却账号")
	}
}

func TestModelsEndpoint(t *testing.T) {
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active},
		snap: []channel.ModelInfo{{ID: "qwen3.8-max", DisplayName: "Qwen3.8-Max"}},
	}
	srv, _ := newTestGateway(t, ch, true)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer good-key")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "qodercn/qwen3.8-max") {
		t.Fatalf("模型 ID 应带渠道前缀，实际 %s", w.Body.String())
	}
}

func TestParseModel(t *testing.T) {
	if k, m := parseModel("qodercn/qwen3.8-max"); k != channel.QoderCN || m != "qwen3.8-max" {
		t.Fatalf("应拆分 qodercn + qwen3.8-max，got %s %s", k, m)
	}
	if k, m := parseModel("auto"); k != channel.QoderCN || m != "auto" {
		t.Fatalf("无前缀默认 QoderCN，got %s %s", k, m)
	}
}

// 红线二：上游 200 + 空流（一个 chunk 都没有）必须判失败。
// 这正是 wild-work 的老毛病形态：客户端拿到「HTTP 200 + 没有任何内容」，
// 既不回复也不报错，排查方向被带偏。
func TestChatEmptyUpstreamStreamIsError(t *testing.T) {
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			return &sliceStream{}, nil // 空流
		},
	}
	srv, p := newTestGateway(t, ch, true)

	// 流式：必须出现错误帧（不能只发一个 [DONE] 就当成功）。
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, chatReqBody(t, "qodercn/qwen3.8-max", true))
	body := w.Body.String()
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "Parse") {
		t.Fatalf("空流必须回错误帧，实际 %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("错误帧后仍应发 [DONE] 收尾，实际 %s", body)
	}
	// 空流计入账号错误（冷却），否则会一直拿这个空号去试。
	if _, ok := p.Pick(context.Background(), channel.QoderCN, nil); ok {
		t.Fatal("空流应冷却该账号")
	}

	// 非流式：必须是错误响应，而不是 content 为空的 200。
	srv2, _ := newTestGateway(t, ch, true)
	w2 := httptest.NewRecorder()
	srv2.Routes().ServeHTTP(w2, chatReqBody(t, "qodercn/qwen3.8-max", false))
	if w2.Code < 400 {
		t.Fatalf("非流式空流应返回错误码，实际 %d body=%s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "Parse") {
		t.Fatalf("非流式空流应带结构化 kind，实际 %s", w2.Body.String())
	}
}

// 上游流中途出错：错误必须回传给客户端（F3.6），并且带上游原话。
func TestChatStreamMidErrorSurfaces(t *testing.T) {
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			return &errStream{chunks: []channel.ChatCompletionChunk{chunk("半句")}, err: errs.New(errs.UpstreamFault, "上游断了").WithUpstream("EOF from upstream")}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, chatReqBody(t, "qodercn/qwen3.8-max", true))
	body := w.Body.String()
	if !strings.Contains(body, "半句") {
		t.Fatalf("出错前的内容应已下发，实际 %s", body)
	}
	if !strings.Contains(body, "UpstreamFault") || !strings.Contains(body, "EOF from upstream") {
		t.Fatalf("流中错误必须带 kind 与上游原话，实际 %s", body)
	}
}

// errStream 是先吐几个 chunk 再报错的流。
type errStream struct {
	chunks []channel.ChatCompletionChunk
	err    error
	i      int
}

func (s *errStream) Next() (channel.ChatCompletionChunk, error) {
	if s.i < len(s.chunks) {
		c := s.chunks[s.i]
		s.i++
		return c, nil
	}
	if s.err != nil {
		err := s.err
		s.err = nil
		return channel.ChatCompletionChunk{}, err
	}
	return channel.ChatCompletionChunk{}, io.EOF
}
func (s *errStream) Close() error { return nil }

// modelIDs 取一次 /v1/models，返回模型 ID 列表。
func modelIDs(t *testing.T, srv *Server) []string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer good-key")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/models 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		out = append(out, m.ID)
	}
	return out
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func newFilterGateway(t *testing.T) *Server {
	t.Helper()
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active},
		snap: []channel.ModelInfo{{ID: "good"}, {ID: "bad"}, {ID: "never-probed"}},
	}
	srv, _ := newTestGateway(t, ch, true)
	return srv
}

// 「只下发可用模型」：体检明确失败的模型不出现在 /v1/models。
func TestModelsHidesFailedWhenFilterOn(t *testing.T) {
	srv := newFilterGateway(t)

	// 开关关着时装配层返回空索引 → 一个都不隐藏。
	srv.health = func() health.Snapshot { return health.Snapshot{} }
	ids := modelIDs(t, srv)
	for _, want := range []string{"qodercn/good", "qodercn/bad", "qodercn/never-probed"} {
		if !hasID(ids, want) {
			t.Fatalf("开关关着时 %s 应照常下发，实际 %v", want, ids)
		}
	}

	// 打开：只隐藏体检明确失败的；未体检的保留。
	srv.health = func() health.Snapshot {
		return health.Snapshot{
			Passed: map[string]bool{"qodercn/good": true},
			Failed: map[string]bool{"qodercn/bad": true},
		}
	}
	ids = modelIDs(t, srv)
	if hasID(ids, "qodercn/bad") {
		t.Fatalf("体检失败的模型不该下发，实际 %v", ids)
	}
	if !hasID(ids, "qodercn/good") {
		t.Fatalf("体检通过的模型必须下发，实际 %v", ids)
	}
	if !hasID(ids, "qodercn/never-probed") {
		t.Fatalf("未体检的模型必须照常下发（否则刚装完的列表会是空的），实际 %v", ids)
	}
}

// 开关只影响下发列表：直连调用不拦（探测失败可能是一次性的，
// 拦下来会让「偶发失败」变成一个必须去面板操作才能恢复的故障）。
func TestHealthFilterDoesNotBlockDirectCall(t *testing.T) {
	called := false
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active},
		snap: []channel.ModelInfo{{ID: "bad"}},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			called = true
			return &sliceStream{chunks: []channel.ChatCompletionChunk{chunk("hi")}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	srv.health = func() health.Snapshot {
		return health.Snapshot{Failed: map[string]bool{"qodercn/bad": true}}
	}
	if hasID(modelIDs(t, srv), "qodercn/bad") {
		t.Fatal("列表里应已隐藏")
	}

	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, chatReqBody(t, "qodercn/bad", false))
	if !called {
		t.Fatalf("直连调用仍应打到上游，实际响应 %d %s", w.Code, w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("直连调用应正常返回，实际 %d %s", w.Code, w.Body.String())
	}
}
