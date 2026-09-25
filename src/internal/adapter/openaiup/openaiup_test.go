package openaiup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

func testAdapter(baseURL string) *Adapter {
	return New(Config{
		Name: "acme", DisplayName: "Acme AI", BaseURL: baseURL,
		APIKey: "sk-test-key", SupportsTools: true,
	})
}

func tools() []map[string]any {
	return []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name": "get_weather", "description": "查天气",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}}
}

// 工具定义与工具消息必须进上游请求体 —— 这是这个适配器存在的意义。
func TestBuildBodyCarriesTools(t *testing.T) {
	req := channel.ChatRequest{
		Model: "gemini-3.8-flash",
		Messages: []channel.Message{
			{Role: "user", Content: "北京天气"},
			{Role: "assistant", ToolCalls: []channel.ToolCall{{ID: "call_1", Type: "function",
				Function: channel.FunctionCall{Name: "get_weather", Arguments: `{"city":"北京"}`}}}},
			{Role: "tool", Content: "晴 26℃", ToolCallID: "call_1", Name: "get_weather"},
		},
		Tools:      tools(),
		ToolChoice: "auto",
	}
	var body map[string]any
	if err := json.Unmarshal(buildBody(req), &body); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v", err)
	}
	if body["stream"] != true {
		t.Fatal("上游统一走流式（Spec.SSEOnly）")
	}
	ts, _ := body["tools"].([]any)
	if len(ts) != 1 {
		t.Fatalf("tools 丢了：%v", body["tools"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("应 3 条消息，got %d", len(msgs))
	}
	if m, _ := msgs[1].(map[string]any); m["tool_calls"] == nil {
		t.Fatalf("assistant 的 tool_calls 丢了：%v", msgs[1])
	}
	if m, _ := msgs[2].(map[string]any); m["tool_call_id"] != "call_1" || m["name"] != "get_weather" {
		t.Fatalf("tool 消息缺 id/name：%v", msgs[2])
	}
}

// tool_choice:"none" 是客户端明确要求「本轮不要工具」—— 这时不转发 tools 是有意为之，不是丢弃。
func TestBuildBodyHonoursToolChoiceNone(t *testing.T) {
	req := channel.ChatRequest{
		Model: "m", Messages: []channel.Message{{Role: "user", Content: "hi"}},
		Tools: tools(), ToolChoice: "none",
	}
	var body map[string]any
	json.Unmarshal(buildBody(req), &body)
	if _, has := body["tools"]; has {
		t.Fatal("tool_choice=none 时不应带 tools")
	}
}

// 端到端：工具定义发出去、流式 tool_calls 分片能拼回完整调用。
func TestChatStreamsToolCalls(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}`,
			`{"id":"c1","choices":[{"index":0,"finish_reason":"tool_calls"}]}`,
		}
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := testAdapter(srv.URL)
	cred := a.Credential()
	st, err := a.Chat(context.Background(), &cred, channel.ChatRequest{
		Model:    "gemini-3.8-flash",
		Messages: []channel.Message{{Role: "user", Content: "北京天气"}},
		Tools:    tools(),
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	if gotAuth != "Bearer sk-test-key" {
		t.Fatalf("鉴权头不对：%q", gotAuth)
	}
	if !strings.Contains(gotBody, "get_weather") {
		t.Fatalf("tools 没进上游请求体：%s", gotBody)
	}

	var merged []channel.ToolCall
	finish := ""
	n := 0
	for {
		c, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("流读取失败：%v", err)
		}
		n++
		for _, ch := range c.Choices {
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
			merged = mergeCalls(merged, ch.Delta.ToolCalls)
		}
	}
	if n == 0 {
		t.Fatal("一个 chunk 都没收到")
	}
	if len(merged) != 1 {
		t.Fatalf("应拼出 1 个工具调用，got %d：%+v", len(merged), merged)
	}
	if merged[0].ID != "call_1" || merged[0].Function.Name != "get_weather" ||
		merged[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("工具调用拼装不对：%+v", merged[0])
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason 应为 tool_calls，got %q", finish)
	}
}

// 上游无视 stream:true 回整包 JSON（并按 message.tool_calls 给结果）时要能归一。
func TestChatSingleJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c2","choices":[{"index":0,"message":{"role":"assistant","content":"",
			"tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"上海\"}"}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	}))
	defer srv.Close()

	a := testAdapter(srv.URL)
	cred := a.Credential()
	st, err := a.Chat(context.Background(), &cred, channel.ChatRequest{
		Model: "m", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	var calls []channel.ToolCall
	usageSeen := false
	for {
		c, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("流读取失败：%v", err)
		}
		if c.Usage != nil {
			usageSeen = true
		}
		for _, ch := range c.Choices {
			calls = append(calls, ch.Delta.ToolCalls...)
		}
	}
	if len(calls) != 1 || calls[0].Function.Arguments != `{"city":"上海"}` {
		t.Fatalf("整包 JSON 未归一：%+v", calls)
	}
	if !usageSeen {
		t.Fatal("usage 应透传（客户端可能要看 token 数）")
	}
}

// 上游把错误包在 200 的信封里（红线二）：不能当成功，要当失败抛出去。
func TestChatErrorEnvelopeIn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"message":"model overloaded","code":503}}`)
	}))
	defer srv.Close()
	a := testAdapter(srv.URL)
	cred := a.Credential()
	st, err := a.Chat(context.Background(), &cred, channel.ChatRequest{
		Model: "m", Messages: []channel.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat 本身不该失败：%v", err)
	}
	defer st.Close()
	_, err = st.Next()
	if err == nil {
		t.Fatal("200 里的 error 信封必须报错，不能静默当成功")
	}
	if k, ok := errs.KindOf(err); !ok || k != errs.UpstreamFault {
		t.Fatalf("应归一成 UpstreamFault，got %v", err)
	}
}

// 模型目录：上游可用就用上游的；不可用回退手填；都没有则明确报错（不静默给空）。
func TestModelsFallback(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("路径不对：%s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"gemini-3.8-flash"},{"id":"gemini-3.7-flash"}]}`)
	}))
	defer ok.Close()
	a := testAdapter(ok.URL)
	cred := a.Credential()
	ms, err := a.Models(context.Background(), &cred)
	if err != nil || len(ms) != 2 || ms[0].ID != "gemini-3.8-flash" {
		t.Fatalf("目录解析失败：%v %+v", err, ms)
	}

	// 上游 404 → 回退手填，来源标注为本地
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer bad.Close()
	b := New(Config{Name: "p", BaseURL: bad.URL, APIKey: "k", Models: []string{"sonar"}, SupportsTools: true})
	bcred := b.Credential()
	ms, err = b.Models(context.Background(), &bcred)
	if err != nil || len(ms) != 1 || ms[0].Source != channel.SourceLocal {
		t.Fatalf("手填兜底失败：%v %+v", err, ms)
	}

	// 都没有 → 报错
	cIdx := New(Config{Name: "p2", BaseURL: bad.URL, APIKey: "k"})
	ccred := cIdx.Credential()
	if _, err := cIdx.Models(context.Background(), &ccred); err == nil {
		t.Fatal("拿不到目录又没手填时必须报错")
	}
}

func TestClassify(t *testing.T) {
	a := testAdapter("http://x")
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{"error":"invalid api key"}`, errs.SessionDead},
		{403, `forbidden`, errs.SessionDead},
		{402, `payment required`, errs.HardCredit},
		{429, `{"error":{"message":"rate limit reached"}}`, errs.SoftRate},
		{429, `{"error":{"message":"You exceeded your current quota"}}`, errs.HardCredit},
		{429, `{"error":{"message":"insufficient credits"}}`, errs.HardCredit},
		{400, `{"error":"maximum context length is 128000 tokens"}`, errs.PromptTooLong},
		{400, `{"error":"content_filter triggered"}`, errs.ContentBlocked},
		{400, `{"error":"model not found"}`, errs.ModelUnavailable},
		{500, `internal`, errs.UpstreamFault},
		{418, `teapot`, errs.Parse},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Errorf("Classify(%d, %q) = %v，want %v", c.status, c.body, got, c.want)
		}
	}
}

// key 式来源没有授权流程，也没有自动续期 —— 这两件事必须**明确**，不能假装成功。
func TestNoAuthFlowAndNoRefresh(t *testing.T) {
	a := testAdapter("http://x")
	if _, err := a.Login(context.Background()); err == nil {
		t.Fatal("key 式来源不该支持面板授权")
	} else if !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("拒绝原因要能读懂：%v", err)
	}
	nc, err := a.Refresh(context.Background(), nil)
	if err != nil || nc != nil {
		t.Fatalf("Refresh 应返回 (nil, nil) 表示刷不了：%v %v", nc, err)
	}
}

// 能力位如实声明：支持才写 true，不支持时网关会明确拒绝带 tools 的请求。
func TestSpecReflectsConfig(t *testing.T) {
	yes := New(Config{Name: "a", BaseURL: "http://x", APIKey: "k", SupportsTools: true})
	no := New(Config{Name: "b", BaseURL: "http://x", APIKey: "k", SupportsTools: false})
	if !yes.Spec().Tools {
		t.Fatal("配置声明支持 tools 时 Spec 应为 true")
	}
	if no.Spec().Tools {
		t.Fatal("配置声明不支持时 Spec 必须为 false")
	}
	if got := string(no.Kind()); got != "b" {
		t.Fatalf("Kind 应等于来源名，got %q", got)
	}
}

// mergeCalls 是测试里用的最小归并（与网关 mergeToolCalls 同语义）。
func mergeCalls(acc []channel.ToolCall, delta []channel.ToolCall) []channel.ToolCall {
	for _, d := range delta {
		idx := -1
		for i := range acc {
			if d.ID != "" && acc[i].ID == d.ID {
				idx = i
				break
			}
			if d.ID == "" && acc[i].Index == d.Index {
				idx = i
				break
			}
		}
		if idx < 0 {
			acc = append(acc, d)
			continue
		}
		if d.ID != "" {
			acc[idx].ID = d.ID
		}
		if d.Function.Name != "" {
			acc[idx].Function.Name = d.Function.Name
		}
		acc[idx].Function.Arguments += d.Function.Arguments
	}
	return acc
}
