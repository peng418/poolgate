package iflow

// iflow_test.go 用 httptest 造一个 mock 上游，覆盖四件最容易出错的事：
//  1. **签名**：测试里用同样的算法独立算一遍，与请求头比对（签名错上游只回 401，最难查）；
//  2. 请求形态：默认参数、stream、tools 原样透传、tool 消息的字段；
//  3. SSE 增量解析（含半行、思考分流、tool_calls 分片、[DONE]）；
//  4. 错误归一（Classify）与登录粘贴 + 探活。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const testKey = "iflow-secret-key-123456"

// refSignature 在测试里**独立**按参考实现的算法算一遍签名。
//
// 这里刻意写死字面量 "iFlow-Cli" 与冒号拼法，而不是调用被测包的常量 ——
// 万一有人改了 cliUserAgent 或拼法，这个测试才会失败（用 cliUserAgent 就成了自证）。
func refSignature(key, session, tsMillis string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("iFlow-Cli:" + session + ":" + tsMillis))
	return hex.EncodeToString(mac.Sum(nil))
}

// writeSSE 把 SSE 文本按很小的切片写出去（故意把 data 行切碎），
// 逼出跨读的「半行」组装路径 —— 真实网络也不会按行边界送达。
func writeSSE(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for i := 0; i < len(body); i += 5 {
		end := i + 5
		if end > len(body) {
			end = len(body)
		}
		_, _ = w.Write([]byte(body[i:end]))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// 1. 签名
// ---------------------------------------------------------------------------

func TestSignatureMatchesReferenceAlgorithm(t *testing.T) {
	const session = "session-11111111-2222-4333-8444-555555555555"
	const ts = "1735689600123" // 13 位毫秒
	got := signature(testKey, session, 1735689600123)
	want := refSignature(testKey, session, ts)
	if got != want {
		t.Fatalf("签名与参考算法不一致：\n got=%s\nwant=%s", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("HMAC-SHA256 十六进制应为 64 位，得到 %d 位", len(got))
	}
}

// 端到端同时验证：请求头齐全 + 头里的签名与头里的 session/timestamp 自洽 + 请求体形态。
func TestChatStreamsAndSignsHeaders(t *testing.T) {
	var sawChat bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != epChat {
			t.Errorf("未预期的路径：%s", r.URL.Path)
			return
		}
		sawChat = true

		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("应带 Bearer apiKey，得到 %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "iFlow-Cli" {
			t.Errorf("UA 必须是 iFlow-Cli（解锁高级模型的关键），得到 %q", got)
		}
		session := r.Header.Get("Session-Id")
		if !strings.HasPrefix(session, "session-") {
			t.Errorf("session-id 应以 session- 开头，得到 %q", session)
		}
		if r.Header.Get("Conversation-Id") == "" {
			t.Error("缺 conversation-id")
		}
		ts := r.Header.Get("X-Iflow-Timestamp")
		if n, err := strconv.ParseInt(ts, 10, 64); err != nil || n < 1_000_000_000_000 {
			t.Errorf("x-iflow-timestamp 应是毫秒时间戳，得到 %q", ts)
		}
		sig := r.Header.Get("X-Iflow-Signature")
		if want := refSignature(testKey, session, ts); sig != want {
			t.Errorf("签名不对：\n got=%s\nwant=%s（session=%s ts=%s）", sig, want, session, ts)
		}

		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("请求体不是 JSON：%v", err)
		}
		if body["model"] != "glm-4.6" {
			t.Errorf("model 不对：%v", body["model"])
		}
		if body["stream"] != true {
			t.Errorf("流式请求必须带 stream=true：%v", body["stream"])
		}
		if body["temperature"] != defaultTemperature || body["top_p"] != defaultTopP {
			t.Errorf("默认采样参数没补齐：temp=%v top_p=%v", body["temperature"], body["top_p"])
		}
		if body["max_new_tokens"] != float64(defaultMaxNewTokens) {
			t.Errorf("默认 max_new_tokens 应为 %d，得到 %v", defaultMaxNewTokens, body["max_new_tokens"])
		}
		// glm-4.6 应带上思考参数（参考实现的模型专属配置）。
		ctk, _ := body["chat_template_kwargs"].(map[string]any)
		if ctk == nil || ctk["enable_thinking"] != true {
			t.Errorf("glm-4.6 应带 chat_template_kwargs.enable_thinking=true：%v", body["chat_template_kwargs"])
		}
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("messages 数量不对：%v", body["messages"])
		}
		m0, _ := msgs[0].(map[string]any)
		if m0["role"] != "user" || m0["content"] != "你好" {
			t.Errorf("消息形态不对：%v", m0)
		}

		writeSSE(w, strings.Join([]string{
			`data: {"id":"c1","model":"glm-4.6","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			``,
			`data: {"id":"c1","model":"glm-4.6","choices":[{"index":0,"delta":{"content":"你"}}]}`,
			``,
			`data: {"id":"c1","model":"glm-4.6","choices":[{"index":0,"delta":{"reasoning_content":"想"}}]}`,
			``,
			`data: {"id":"c1","model":"glm-4.6","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\""}}]}}]}`,
			``,
			`data: {"id":"c1","model":"glm-4.6","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"北京\"}"}}]}}]}`,
			``,
			`data: {"id":"c1","model":"glm-4.6","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n"))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "iflow-test", AccessToken: testKey}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model:    "glm-4.6",
		Messages: []channel.Message{{Role: "user", Content: "你好"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	var content, reasoning strings.Builder
	finish := ""
	argsByIdx := map[int]string{}
	nameByIdx := map[int]string{}
	for {
		c, err := st.Next()
		if err != nil {
			break
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
			reasoning.WriteString(ch.Delta.ReasoningContent)
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Function.Name != "" {
					nameByIdx[tc.Index] = tc.Function.Name
				}
				argsByIdx[tc.Index] += tc.Function.Arguments
			}
		}
	}
	if !sawChat {
		t.Fatal("对话端点没被调用")
	}
	if content.String() != "你" {
		t.Fatalf("正文不对：%q", content.String())
	}
	if reasoning.String() != "想" {
		t.Fatalf("思考分流不对：%q", reasoning.String())
	}
	if finish != "tool_calls" {
		t.Fatalf("结束帧不对：%q", finish)
	}
	if nameByIdx[0] != "get_weather" || argsByIdx[0] != `{"city":"北京"}` {
		t.Fatalf("tool_calls 分片没拼对：name=%q args=%q", nameByIdx[0], argsByIdx[0])
	}
}

// ---------------------------------------------------------------------------
// 2. tools 原样透传 + 消息字段
// ---------------------------------------------------------------------------

func TestToolsPassthrough(t *testing.T) {
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "查天气",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		},
	}}
	choice := map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}

	var checkErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			checkErr = err
			return
		}
		// tools 必须与客户端给的**逐字节一致**（上游原生支持，网关不做翻译）。
		wantJSON, _ := json.Marshal(tools)
		gotJSON, _ := json.Marshal(body["tools"])
		if !strings.EqualFold(string(wantJSON), string(gotJSON)) {
			checkErr = errs.New(errs.Parse, "tools 没有被原样透传")
			t.Errorf("tools 透传不对：\n got=%s\nwant=%s", gotJSON, wantJSON)
		}
		if !reflect.DeepEqual(body["tool_choice"], any(choice)) {
			t.Errorf("tool_choice 没透传：%v", body["tool_choice"])
		}
		// assistant 的历史工具调用与 tool 结果必须带齐字段，否则模型会重复调用。
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 3 {
			t.Errorf("messages 数量不对：%d", len(msgs))
		}
		asst, _ := msgs[1].(map[string]any)
		calls, _ := asst["tool_calls"].([]any)
		if len(calls) != 1 {
			t.Fatalf("assistant 的 tool_calls 丢了：%v", asst)
		}
		fn, _ := calls[0].(map[string]any)["function"].(map[string]any)
		if fn["name"] != "get_weather" {
			t.Errorf("tool_call function.name 丢了：%v", calls[0])
		}
		toolMsg, _ := msgs[2].(map[string]any)
		if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" {
			t.Errorf("tool 消息字段不对：%v", toolMsg)
		}

		writeSSE(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "iflow-test", AccessToken: testKey}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "glm-5",
		Messages: []channel.Message{
			{Role: "user", Content: "北京天气"},
			{Role: "assistant", ToolCalls: []channel.ToolCall{{
				ID: "call_1", Type: "function",
				Function: channel.FunctionCall{Name: "get_weather", Arguments: `{"city":"北京"}`},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "晴"},
		},
		Tools:      tools,
		ToolChoice: choice,
		Stream:     true,
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	for {
		if _, err := st.Next(); err != nil {
			break
		}
	}
	if checkErr != nil {
		t.Fatalf("透传校验失败：%v", checkErr)
	}
}

// tool_choice="none" 时不应把 tools 发给上游（ForwardTools 的语义），但仍保留空数组。
func TestToolChoiceNoneDropsTools(t *testing.T) {
	var checkErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if tools, _ := body["tools"].([]any); len(tools) != 0 {
			checkErr = errs.New(errs.Parse, "tool_choice=none 时不该带 tools")
		}
		if _, ok := body["tool_choice"]; ok && len(body["tools"].([]any)) == 0 {
			// tool_choice 在 tools 为空时不应出现（OpenAI 会拒）。
			if body["tool_choice"] != nil {
				checkErr = errs.New(errs.Parse, "tools 为空时不该带 tool_choice")
			}
		}
		writeSSE(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "iflow-test", AccessToken: testKey}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model:      "glm-4.6",
		Messages:   []channel.Message{{Role: "user", Content: "hi"}},
		Tools:      []map[string]any{{"type": "function", "function": map[string]any{"name": "x"}}},
		ToolChoice: "none",
		Stream:     true,
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	for {
		if _, err := st.Next(); err != nil {
			break
		}
	}
	if checkErr != nil {
		t.Fatal(checkErr)
	}
}

// ---------------------------------------------------------------------------
// 3. 非流式回包解析
// ---------------------------------------------------------------------------

func TestNonStreamJSONResponse(t *testing.T) {
	var hasStream bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		_, hasStream = body["stream"] // 非流式请求不应带 stream 字段
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c9","model":"glm-4.6","choices":[{"index":0,` +
			`"message":{"role":"assistant","content":"整包回复"},"finish_reason":"stop"}],` +
			`"usage":{"total_tokens":3}}`))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "iflow-test", AccessToken: testKey}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "glm-4.6", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	var content, finish string
	for {
		c, err := st.Next()
		if err != nil {
			break
		}
		for _, ch := range c.Choices {
			content += ch.Delta.Content
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	if hasStream {
		t.Error("非流式请求不应带 stream 字段")
	}
	if content != "整包回复" || finish != "stop" {
		t.Fatalf("整包 JSON 解析不对：content=%q finish=%q", content, finish)
	}
}

// SSE 里的错误帧若是身份/签名问题 → SessionDead（提示重新粘贴），不是上游故障。
func TestStreamErrorFrameClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, "data: {\"error\":{\"message\":\"invalid signature\"}}\n\n")
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "iflow-test", AccessToken: testKey}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "glm-4.6", Messages: []channel.Message{{Role: "user", Content: "hi"}}, Stream: true,
	})
	if err != nil {
		t.Fatalf("错误应在流里出现，而不是建立时：%v", err)
	}
	defer st.Close()
	var got error
	for {
		_, err := st.Next()
		if err != nil {
			got = err
			break
		}
	}
	if k, ok := errs.KindOf(got); !ok || k != errs.SessionDead {
		t.Fatalf("签名错误应归一成 SessionDead，得到 %v（%v）", k, got)
	}
}

// ---------------------------------------------------------------------------
// 4. Classify
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{}`, errs.SessionDead},
		{403, `{"error":{"message":"forbidden"}}`, errs.SessionDead},
		{403, `{"error":{"message":"Signature invalid"}}`, errs.SessionDead},
		{400, `{"error":{"message":"signature mismatch"}}`, errs.SessionDead},
		{429, `{"error":{"message":"rate limited"}}`, errs.SoftRate},
		{429, `{"error":{"message":"quota exhausted"}}`, errs.HardCredit},
		{404, `{}`, errs.ModelUnavailable},
		{400, `{"error":{"message":"model not found"}}`, errs.ModelUnavailable},
		{400, `{"error":{"message":"context too long"}}`, errs.PromptTooLong},
		{400, `{"error":{"message":"content policy violation"}}`, errs.ContentBlocked},
		{503, `{}`, errs.UpstreamFault},
		{200, `{}`, errs.Parse}, // 未知一律 Parse，绝不静默当成功
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("status=%d body=%s: 期望 %v 得到 %v", c.status, c.body, c.want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. 登录：粘贴 + 探活
// ---------------------------------------------------------------------------

func TestLoginPasteAndProbe(t *testing.T) {
	var probeBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("探活应带 Bearer apiKey，得到 %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &probeBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"index":0,` +
			`"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	if sess.AuthURL() == "" {
		t.Error("AuthURL 不能为空")
	}
	hs, ok := sess.(interface{ Hint() string })
	if !ok || !strings.Contains(hs.Hint(), "settings.json") {
		t.Fatalf("Hint 必须引导用户去 ~/.iflow/settings.json 取 apiKey，得到 %v", hs)
	}

	// 还没粘 → ErrPending（正常等待态）。
	if _, err := sess.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("未粘贴时应返回 ErrPending，得到 %v", err)
	}
	// 明显误粘 → 当场拒绝。
	ac := sess.(channel.CallbackAcceptor)
	if err := ac.AcceptCallback("   "); err == nil {
		t.Fatal("空粘贴应被拒绝")
	}
	// 整段 JSON（用户可能把整个 settings.json 粘进来）。
	if err := ac.AcceptCallback(`{"selectedAuthType":"api-key","apiKey":"` + testKey + `","baseUrl":"x"}`); err != nil {
		t.Fatalf("AcceptCallback(JSON) 失败：%v", err)
	}
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if cred.AccessToken != testKey {
		t.Fatalf("apiKey 没落到凭证上：%q", cred.AccessToken)
	}
	if !strings.HasPrefix(cred.UID, "iflow-") {
		t.Fatalf("UID 形态不对：%q", cred.UID)
	}
	if probeBody["model"] != defaultModel {
		t.Errorf("探活应使用默认档，得到 %v", probeBody["model"])
	}
	if _, ok := probeBody["stream"]; ok {
		t.Error("探活不该带 stream（非流式）")
	}
	// 重复 Poll → 明确报错，不重复提交。
	if _, err := sess.Poll(context.Background()); err == nil {
		t.Fatal("重复 Poll 应报错")
	}
}

func TestLoginProbeRejectsBadKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	sess, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err := sess.(channel.CallbackAcceptor).AcceptCallback(testKey); err != nil {
		t.Fatalf("AcceptCallback 失败：%v", err)
	}
	_, err := sess.Poll(context.Background())
	if k, ok := errs.KindOf(err); !ok || k != errs.SessionDead {
		t.Fatalf("坏 key 应归一成 SessionDead，得到 %v（%v）", k, err)
	}
}

// 200 信封里的 error 也要归一：鉴权措辞 → SessionDead，普通错误 → UpstreamFault（不能一律判死）。
func TestProbeClassifiesEnvelopeError(t *testing.T) {
	cases := []struct {
		msg  string
		want errs.Kind
	}{
		{"invalid token", errs.SessionDead},
		{"upstream busy, try later", errs.UpstreamFault},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"error":{"message":"` + c.msg + `"}}`))
		}))
		a := New()
		a.base = srv.URL
		cred := &channel.Credential{UID: "iflow-test", AccessToken: testKey}
		got := errs.Kind("")
		if err := a.probeCredential(context.Background(), cred); err != nil {
			got, _ = errs.KindOf(err)
		}
		srv.Close()
		if got != c.want {
			t.Fatalf("信封错误 %q 应归一成 %v，得到 %v", c.msg, c.want, got)
		}
	}
}

func TestExtractAPIKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{testKey, testKey},
		{`"` + testKey + `"`, testKey},
		{`{"apiKey":"` + testKey + `"}`, testKey},
		{`{"api_key": "` + testKey + `"}`, testKey},
		{`"apiKey" = "` + testKey + `"`, testKey},
		{"   " + testKey + "\n", testKey},
		{"", ""},
		{"too short", ""},
		{"两个 词 的文本", ""},
	}
	for _, c := range cases {
		if got := extractAPIKey(c.in); got != c.want {
			t.Fatalf("extractAPIKey(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 6. 模型档位与能力声明
// ---------------------------------------------------------------------------

func TestThinkingParams(t *testing.T) {
	cases := []struct {
		model string
		check func(map[string]any) bool
	}{
		{"glm-5", func(m map[string]any) bool {
			return m["enable_thinking"] == true && m["thinking"] != nil && m["chat_template_kwargs"] != nil
		}},
		{"glm-4.6", func(m map[string]any) bool {
			_, has := m["chat_template_kwargs"]
			return has && m["enable_thinking"] == nil
		}},
		{"deepseek-v3.2-chat", func(m map[string]any) bool {
			return m["thinking_mode"] == true && m["reasoning"] == true
		}},
		{"kimi-k2-thinking", func(m map[string]any) bool { return m["thinking_mode"] == true }},
		{"kimi-k2.5", func(m map[string]any) bool { return m["thinking"] != nil }},
		{"qwen3-coder-plus", func(m map[string]any) bool { return m == nil }},
	}
	for _, c := range cases {
		if got := thinkingParams(c.model); !c.check(got) {
			t.Fatalf("%s 的思考参数不对：%v", c.model, got)
		}
	}
}

func TestSpecAndModelsHonesty(t *testing.T) {
	a := New()
	sp := a.Spec()
	if sp.Kind != channel.IFlow {
		t.Fatalf("Kind 不对：%s", sp.Kind)
	}
	if !sp.Tools || sp.ToolsShim {
		t.Fatalf("原生 tools 应声明 Tools=true/ToolsShim=false，得到 %+v", sp)
	}
	if sp.Images {
		t.Fatal("本适配器契约里没有图片字段，Images 不能标成支持")
	}
	if sp.SSEOnly {
		t.Fatal("上游流式/非流式都支持，不该标 SSEOnly")
	}
	if sp.DefaultMinIntervalSec <= 0 {
		t.Fatal("缺省最小间隔必须给一个安全值")
	}
	ms, err := a.Models(context.Background(), nil)
	if err != nil {
		t.Fatalf("Models 失败：%v", err)
	}
	if len(ms) != len(localModels) {
		t.Fatalf("模型数不对：%d", len(ms))
	}
	for _, m := range ms {
		if m.Source != channel.SourceLocal {
			t.Fatalf("%s 的来源必须标 local（上游无公开 /models）", m.ID)
		}
		if m.Tools != channel.CapYes {
			t.Fatalf("%s 应支持 tools", m.ID)
		}
	}
}
