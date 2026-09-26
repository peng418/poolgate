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
	"time"

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

// 上游「有帧、没内容」也要判失败：适配器认不出新帧形态时，流里照样有帧、照样正常结束。
// chatgpt 网页渠道 2026-09-26 就是这样整轮空的（客户端拿到空的 200 + stop，以为模型没话说）。
func TestChatFramesWithoutContentAreError(t *testing.T) {
	// 只带 role 与 finish_reason 的帧：形状合法、内容为零。
	empty := channel.ChatCompletionChunk{ID: "chatcmpl-1", Model: "qwen3.8-max"}
	var ch0 channel.ChunkChoice
	ch0.Delta.Role = "assistant"
	ch0.FinishReason = "stop"
	empty.Choices = append(empty.Choices, ch0)

	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			return &sliceStream{chunks: []channel.ChatCompletionChunk{empty}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)

	// 流式：必须写错误帧，而不是发个空 delta 再 [DONE]。
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, chatReqBody(t, "qodercn/qwen3.8-max", true))
	body := w.Body.String()
	if !strings.Contains(body, "空帧") || !strings.Contains(body, "Parse") {
		t.Fatalf("只有空帧时必须回错误帧，实际 %s", body)
	}

	// 非流式：同样不能是 content 为空的 200。
	srv2, _ := newTestGateway(t, ch, true)
	w2 := httptest.NewRecorder()
	srv2.Routes().ServeHTTP(w2, chatReqBody(t, "qodercn/qwen3.8-max", false))
	if w2.Code < 400 || !strings.Contains(w2.Body.String(), "空内容") {
		t.Fatalf("非流式空内容应返回错误，实际 %d body=%s", w2.Code, w2.Body.String())
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

// ---------------------------------------------------------------------------
// 工具调用（tools / tool_calls）—— Studio 这类 coding agent 的命门
//
// 背景：工具定义只要在网关这一层被丢掉，上游就会把「我要调用工具」写成普通文本，
// 客户端拿不到 tool_calls，表现成「模型不回复 / 回一堆乱码」。所以这里逐条钉死。
// ---------------------------------------------------------------------------

func toolCallChunk(index int, id, name, args string) channel.ChatCompletionChunk {
	c := channel.ChatCompletionChunk{ID: "chatcmpl-t", Model: "qwen3.8-max"}
	ch := channel.ChunkChoice{}
	ch.Delta.ToolCalls = []channel.ToolCall{{
		Index: index, ID: id, Type: "function",
		Function: channel.FunctionCall{Name: name, Arguments: args},
	}}
	c.Choices = append(c.Choices, ch)
	return c
}

func postJSON(t *testing.T, srv *Server, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer good-key")
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	return w
}

// tools 与工具消息必须原样到达渠道层。
func TestToolsForwardedToChannel(t *testing.T) {
	var got channel.ChatRequest
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active, Tools: true},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			got = req
			return &sliceStream{chunks: []channel.ChatCompletionChunk{chunk("ok")}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model": "qodercn/qwen3.8-max",
		"messages": []map[string]any{
			{"role": "user", "content": "北京天气"},
			{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city":"北京"}`},
			}}},
			{"role": "tool", "tool_call_id": "call_1", "name": "get_weather", "content": "晴 26℃"},
		},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{"name": "get_weather", "description": "查天气",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		}},
		"stream": false,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools 应原样到达渠道层，got %+v", got.Tools)
	}
	if fn, _ := got.Tools[0]["function"].(map[string]any); fn["name"] != "get_weather" {
		t.Fatalf("tools 内容不对：%+v", got.Tools[0])
	}
	if len(got.Messages) != 3 {
		t.Fatalf("应 3 条消息，got %d：%+v", len(got.Messages), got.Messages)
	}
	if asst := got.Messages[1]; len(asst.ToolCalls) != 1 || asst.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("assistant 的 tool_calls 丢了：%+v", asst)
	}
	if toolMsg := got.Messages[2]; toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" || toolMsg.Name != "get_weather" {
		t.Fatalf("tool 消息的 tool_call_id/name 丢了：%+v", toolMsg)
	}
}

// content 是 parts 数组（多模态形态）时也要解析出文本，不能整条消息变空。
func TestChatMessageContentParts(t *testing.T) {
	var got channel.ChatRequest
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active, Tools: true},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			got = req
			return &sliceStream{chunks: []channel.ChatCompletionChunk{chunk("ok")}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model": "qodercn/qwen3.8-max",
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "前半"},
				{"type": "text", "text": "后半"},
			}},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "前半后半" {
		t.Fatalf("parts 数组应拼成文本，got %+v", got.Messages)
	}
}

// 渠道声明不支持 tools 时必须明确拒绝（可读原因），不能静默丢掉。
func TestToolsRejectedWhenUnsupported(t *testing.T) {
	called := false
	ch := &fakeChannel{
		kind: channel.QwenWork,
		spec: channel.Spec{Kind: channel.QwenWork, Status: channel.Active, Tools: false},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			called = true
			return nil, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model":    "qwenwork/pro",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools": []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": map[string]any{"type": "object"}},
		}},
	})
	if called {
		t.Fatal("不支持的渠道不应打到上游")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("应明确拒绝（400），实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "工具调用") {
		t.Fatalf("拒绝原因要能读懂，实际 %s", w.Body.String())
	}
}

// 非流式：上游分片的 tool_calls 要按 index 拼回完整调用，finish_reason 变 tool_calls。
func TestToolCallsAggregatedNonStream(t *testing.T) {
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active, Tools: true},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			return &sliceStream{chunks: []channel.ChatCompletionChunk{
				toolCallChunk(0, "call_1", "get_weather", `{"city"`),
				toolCallChunk(0, "", "", `:"北京"}`),
			}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model":    "qodercn/qwen3.8-max",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(out.Choices) != 1 || len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("应聚合成 1 个工具调用，got %s", w.Body.String())
	}
	tc := out.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("工具调用拼装不对：%+v", tc)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("有工具调用时 finish_reason 应为 tool_calls，got %q", out.Choices[0].FinishReason)
	}
}

// mergeToolCalls：OpenAI 式分片按 index 归并，agent 式整包按 id 区分。
func TestMergeToolCalls(t *testing.T) {
	acc := mergeToolCalls(nil, []channel.ToolCall{
		{Index: 0, ID: "call_1", Function: channel.FunctionCall{Name: "f", Arguments: "{"}},
	})
	acc = mergeToolCalls(acc, []channel.ToolCall{
		{Index: 0, Function: channel.FunctionCall{Arguments: "}"}},
	})
	if len(acc) != 1 || acc[0].Function.Arguments != "{}" || acc[0].Function.Name != "f" {
		t.Fatalf("按 index 归并失败：%+v", acc)
	}

	// 没有 index、但每帧都是完整调用（各带 id）时不能被粘成一个。
	acc = mergeToolCalls(nil, []channel.ToolCall{
		{ID: "a", Function: channel.FunctionCall{Name: "f1", Arguments: "{}"}},
		{ID: "b", Function: channel.FunctionCall{Name: "f2", Arguments: "{}"}},
	})
	if len(acc) != 2 {
		t.Fatalf("两个不同 id 的调用应各自保留，got %+v", acc)
	}
}

// Anthropic 入口：tools / tool_use / tool_result 的往返 + 响应里的 tool_use 块。
func TestAnthropicToolsRoundTrip(t *testing.T) {
	var got channel.ChatRequest
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active, Tools: true},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			got = req
			return &sliceStream{chunks: []channel.ChatCompletionChunk{
				toolCallChunk(0, "call_9", "get_weather", `{"city":"北京"}`),
			}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := postJSON(t, srv, "/v1/messages", map[string]any{
		"model":      "qodercn/qwen3.8-max",
		"max_tokens": 256,
		"tools": []map[string]any{{
			"name": "get_weather", "description": "查天气",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		"messages": []map[string]any{
			{"role": "user", "content": "北京天气"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "我查一下"},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather",
					"input": map[string]any{"city": "北京"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "晴 26℃"},
			}},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	// 请求侧：Anthropic tools → OpenAI tools
	if len(got.Tools) != 1 {
		t.Fatalf("tools 未转换：%+v", got.Tools)
	}
	if fn, _ := got.Tools[0]["function"].(map[string]any); fn["name"] != "get_weather" || fn["parameters"] == nil {
		t.Fatalf("tools 转换不对（input_schema→parameters）：%+v", got.Tools[0])
	}
	// 请求侧：tool_use → assistant.tool_calls；tool_result → role=tool + tool_call_id + name
	if len(got.Messages) != 3 {
		t.Fatalf("应 3 条消息，got %d：%+v", len(got.Messages), got.Messages)
	}
	if a := got.Messages[1]; len(a.ToolCalls) != 1 || a.ToolCalls[0].Function.Name != "get_weather" ||
		a.ToolCalls[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("tool_use 未转成 tool_calls：%+v", a)
	}
	if tr := got.Messages[2]; tr.Role != "tool" || tr.ToolCallID != "toolu_1" || tr.Name != "get_weather" || tr.Content != "晴 26℃" {
		t.Fatalf("tool_result 未转成 tool 消息：%+v", tr)
	}
	// 响应侧：tool_use 块 + stop_reason=tool_use
	var out struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if out.StopReason != "tool_use" {
		t.Fatalf("stop_reason 应为 tool_use，got %q（body=%s）", out.StopReason, w.Body.String())
	}
	found := false
	for _, b := range out.Content {
		if b.Type == "tool_use" && b.Name == "get_weather" && b.Input["city"] == "北京" {
			found = true
		}
	}
	if !found {
		t.Fatalf("响应里应含 tool_use 块（含 id/name/input），实际 %s", w.Body.String())
	}
}

// 只支持「网关代做模拟」的渠道：客户端发 tools，网关把工具定义翻成提示词给上游，
// 再把上游文本里的标记解析回结构化 tool_calls —— 对客户端来说合同没变。
func TestToolsShimChannel(t *testing.T) {
	var got channel.ChatRequest
	ch := &fakeChannel{
		kind: channel.QwenWork,
		spec: channel.Spec{Kind: channel.QwenWork, Status: channel.Active, ToolsShim: true},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			got = req
			return &sliceStream{chunks: []channel.ChatCompletionChunk{
				chunk("好的，"),
				chunk(`<tool_call>{"name":"get_weather","arguments":{"city":"北京"}}</tool_call>`),
			}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model":    "qwenwork/pro",
		"messages": []map[string]any{{"role": "user", "content": "北京天气"}},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{"name": "get_weather", "description": "查天气",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	// 请求侧：tools 被清掉，工具说明进了系统提示词
	if len(got.Tools) != 0 {
		t.Fatalf("模拟模式下不该把 tools 原样传给上游：%+v", got.Tools)
	}
	if len(got.Messages) == 0 || got.Messages[0].Role != "system" ||
		!strings.Contains(got.Messages[0].Content, "get_weather") {
		t.Fatalf("工具说明没进系统提示词：%+v", got.Messages)
	}
	// 响应侧：文本里的标记变成了结构化 tool_calls，且正文没有被吞
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	msg := out.Choices[0].Message
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "get_weather" ||
		msg.ToolCalls[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("标记没解析成工具调用：%+v", msg)
	}
	if msg.Content != "好的，" {
		t.Fatalf("正文应保留标记以外的内容，got %q", msg.Content)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason 应为 tool_calls，got %q", out.Choices[0].FinishReason)
	}
}

// 声明「忽略 tools」的渠道（千问办公这条路的合同）：客户端带 tools 也不报错，
// 网关把 tools 丢掉、按纯文本转发并落一条日志（不静默）；响应侧不做任何工具解析。
func TestToolsIgnoreChannel(t *testing.T) {
	var got channel.ChatRequest
	ch := &fakeChannel{
		kind: channel.QwenWork,
		spec: channel.Spec{Kind: channel.QwenWork, Status: channel.Active, ToolsIgnore: true},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			got = req
			return &sliceStream{chunks: []channel.ChatCompletionChunk{chunk("你好")}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model":       "qwenwork/pro",
		"messages":    []map[string]any{{"role": "user", "content": "你好"}},
		"tool_choice": "auto",
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{"name": "get_weather", "description": "查天气",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("声明忽略 tools 的渠道不该 400，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(got.Tools) != 0 || got.ToolChoice != nil {
		t.Fatalf("tools / tool_choice 都该被丢掉：%+v / %v", got.Tools, got.ToolChoice)
	}
	// 忽略档**不代做**：工具说明不许进提示词（那是 shim 档的事）
	for _, m := range got.Messages {
		if strings.Contains(m.Content, "get_weather") {
			t.Fatalf("忽略档不该代做工具（不该出现工具说明）：%+v", m)
		}
	}
	if !strings.Contains(w.Body.String(), "你好") {
		t.Fatalf("正常回答要逐字透传：%s", w.Body.String())
	}
}

// 默认档：没声明任何工具能力的渠道，带 tools 必须**明确拒绝**（不是静默丢掉、也不是忽略）。
// 这条锁红线一：新渠道忘了声明能力时默认是拒绝，而不是「看起来能用」。
func TestToolsRejectedByDefault(t *testing.T) {
	reached := false
	ch := &fakeChannel{
		kind: channel.QwenWork,
		spec: channel.Spec{Kind: channel.QwenWork, Status: channel.Active},
		chat: func(context.Context, *channel.Credential, channel.ChatRequest) (channel.Stream, error) {
			reached = true
			return &sliceStream{chunks: []channel.ChatCompletionChunk{chunk("不该走到上游")}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model":    "qwenwork/pro",
		"messages": []map[string]any{{"role": "user", "content": "你好"}},
		"tools":    []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather"}}},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("默认档应 400 明确拒绝，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "暂不支持工具调用") {
		t.Fatalf("拒绝原因要可读：%s", w.Body.String())
	}
	if reached {
		t.Fatal("拒绝必须发生在打上游之前")
	}
}

// Anthropic 入口（Claude Code 那类客户端）同样认「忽略 tools」档。
func TestAnthropicToolsIgnoreChannel(t *testing.T) {
	var got channel.ChatRequest
	ch := &fakeChannel{
		kind: channel.QwenWork,
		spec: channel.Spec{Kind: channel.QwenWork, Status: channel.Active, ToolsIgnore: true},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			got = req
			return &sliceStream{chunks: []channel.ChatCompletionChunk{chunk("你好")}}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	w := postJSON(t, srv, "/v1/messages", map[string]any{
		"model":      "qwenwork/pro",
		"max_tokens": 64,
		"messages":   []map[string]any{{"role": "user", "content": "北京天气"}},
		"tools": []map[string]any{{
			"name": "get_weather", "description": "查天气",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("Anthropic 入口也该放行，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(got.Tools) != 0 {
		t.Fatalf("tools 应被丢掉：%+v", got.Tools)
	}
}

// stallStream 在 release 被关闭前一直阻塞（模拟上游长时间不出字）。
type stallStream struct {
	release chan struct{}
	sent    bool
}

func (s *stallStream) Next() (channel.ChatCompletionChunk, error) {
	<-s.release
	if s.sent {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	s.sent = true
	return chunk("好"), nil
}
func (s *stallStream) Close() error { return nil }

// 上游长时间不出字时，网关必须发 SSE 保活注释帧。
//
// 为什么：思考型档位（豆包深度思考、TraeWork 长推理）在出字前可能静默几十秒，
// 夹在中间的 nginx/飞牛网关/CDN 会按空闲超时把连接掐掉，客户端表现成「莫名其妙断开」。
// 参考实现 doubao2api（新版）为此专门发 `: keep-alive` 注释帧。
// 注释帧以 `:` 开头，按 SSE 规范客户端必须忽略，正文不受影响。
func TestStreamSendsHeartbeatWhileWaitingUpstream(t *testing.T) {
	release := make(chan struct{})
	ch := &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			return &stallStream{release: release}, nil
		},
	}
	srv, _ := newTestGateway(t, ch, true)
	srv.heartbeat = 20 * time.Millisecond // 测试里把间隔调小，别真等 15 秒

	go func() {
		time.Sleep(90 * time.Millisecond)
		close(release)
	}()
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, chatReqBody(t, "qodercn/qwen3.8-max", true))

	body := w.Body.String()
	if !strings.Contains(body, ": keep-alive") {
		t.Fatalf("等上游期间应发保活注释帧，got %q", body)
	}
	if !strings.Contains(body, "好") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("保活之后正文仍要正常透传，got %q", body)
	}
	// 保活帧必须排在正文之前（说明它是「等待期间」发出来的）。
	if strings.Index(body, ": keep-alive") > strings.Index(body, "好") {
		t.Fatalf("保活帧应在正文之前，got %q", body)
	}
}
