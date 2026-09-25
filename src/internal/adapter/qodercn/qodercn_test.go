package qodercn

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

func TestQoderEncodingRoundTrip(t *testing.T) {
	cases := []string{
		"hello",
		"",
		`{"model":"qwen3.8-max","messages":[{"role":"user","content":"你好"}]}`,
	}
	for _, c := range cases {
		enc := qoderEncode([]byte(c))
		dec, err := qoderDecode(enc)
		if err != nil {
			t.Fatalf("decode %q: %v", c, err)
		}
		if string(dec) != c {
			t.Fatalf("round trip mismatch: got %q want %q", dec, c)
		}
	}
}

// 与 wild-work 一致：编码产物是自定义字母表 + '$' 填充，不含标准 base64 的 '=' 或 '+'。
func TestQoderEncodingAlphabet(t *testing.T) {
	enc := qoderEncode([]byte("some payload that is long enough to produce padding and rearrangement"))
	if strings.ContainsAny(enc, "=+/") {
		t.Fatalf("encoded should not contain standard base64 chars = + /, got %q", enc)
	}
	if !strings.ContainsRune(enc, '$') && len(enc) > 0 {
		// '$' 只在有填充时出现；此处不强制。
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{http.StatusPaymentRequired, "", errs.HardCredit},
		{http.StatusUnauthorized, "TOKEN_EXPIRE", errs.SessionDead},
		{http.StatusUnauthorized, "whatever", errs.SessionDead},
		{http.StatusTooManyRequests, "quota exceeded", errs.SoftRate},
		{200, `{"error":"insufficient credit"}`, errs.HardCredit},
		{503, "Model catalog unavailable", errs.UpstreamFault},
		{502, "", errs.UpstreamFault},
		{404, "", errs.ModelUnavailable},
		{400, "prompt is too long", errs.PromptTooLong},
		{403, "blocked by security policy", errs.ContentBlocked},
		{200, "rate limit exceeded", errs.SoftRate},
	}
	for _, c := range cases {
		got := Classify(c.status, c.body)
		if got != c.want {
			t.Errorf("Classify(%d, %q) = %s, want %s", c.status, c.body, got, c.want)
		}
	}
}

// 红线一：未知错误绝不静默当成功 —— Classify 必须返回一个可定位的分类。
func TestClassifyNeverEmpty(t *testing.T) {
	if got := Classify(200, "garbage that means nothing"); got == "" {
		t.Fatal("Classify 不得返回空分类（未知应归 Parse/UpstreamFault 而非静默成功）")
	}
}

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"Qwen3.8-Max":         "qwen3.8-max",
		"GLM-5.3":             "glm-5.3",
		" Auto ":              "auto",
		"DeepSeek V4.1 Flash": "deepseek-v4.1-flash",
	}
	for in, want := range cases {
		if got := NormalizeModelName(in); got != want {
			t.Errorf("NormalizeModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

// toChunk 归一化：空帧跳过，有效帧带 model 覆盖（上游恒为 "auto"）。
func TestToChunk(t *testing.T) {
	raw := map[string]any{
		"id":    "chatcmpl-1",
		"model": "auto",
		"choices": []any{
			map[string]any{"index": float64(0), "delta": map[string]any{"content": "你好"}},
		},
	}
	c, ok := toChunk(raw, "qwen3.8-max")
	if !ok {
		t.Fatal("有效帧应归一化")
	}
	if c.Model != "qwen3.8-max" {
		t.Fatalf("model 应被覆盖为客户端模型名，got %q", c.Model)
	}
	if len(c.Choices) != 1 || c.Choices[0].Delta.Content != "你好" {
		t.Fatalf("choice 归一化错误: %+v", c.Choices)
	}

	// 空帧（无 id/usage/content/finish）应跳过
	empty := map[string]any{"choices": []any{}}
	if _, ok := toChunk(empty, "x"); ok {
		t.Fatal("空帧应返回 ok=false")
	}
}

func TestSpecActiveAndCapable(t *testing.T) {
	s := Spec()
	if s.Kind != channel.QoderCN {
		t.Fatalf("kind 应为 qodercn")
	}
	if !s.Downstream() {
		t.Fatal("QoderCN 应为 Active，下发模型")
	}
	if !s.Tools || !s.Reasoning || !s.SSEOnly {
		t.Fatalf("能力位声明不符: %+v", s)
	}
}

// 工具调用：工具定义与工具消息必须进上游请求体。
// 丢了 tools，上游只能把「要调用工具」写成文本，客户端拿不到 tool_calls。
func TestBuildAgentBodyCarriesTools(t *testing.T) {
	tools := []map[string]any{{
		"type":     "function",
		"function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}},
	}}
	msgs := []channel.Message{
		{Role: "user", Content: "北京天气"},
		{Role: "assistant", ToolCalls: []channel.ToolCall{{ID: "call_1", Type: "function",
			Function: channel.FunctionCall{Name: "get_weather", Arguments: `{"city":"北京"}`}}}},
		{Role: "tool", Content: "晴 26℃", ToolCallID: "call_1", Name: "get_weather"},
	}
	raw, err := buildAgentBody(msgs, nil, tools, false, 0, "")
	if err != nil {
		t.Fatalf("构造请求体失败：%v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v", err)
	}
	ts, _ := body["tools"].([]any)
	if len(ts) != 1 {
		t.Fatalf("tools 未进上游请求体：%v", body["tools"])
	}
	fn, _ := ts[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["parameters"] == nil {
		t.Fatalf("tools 内容不对：%v", ts[0])
	}
	um, _ := body["messages"].([]any)
	if len(um) != 3 {
		t.Fatalf("应 3 条消息，got %d：%v", len(um), um)
	}
	if a, _ := um[1].(map[string]any); a["tool_calls"] == nil {
		t.Fatalf("assistant 的 tool_calls 丢了：%v", a)
	}
	tl, _ := um[2].(map[string]any)
	if tl["role"] != "tool" || tl["tool_call_id"] != "call_1" || tl["name"] != "get_weather" {
		t.Fatalf("tool 消息缺 tool_call_id/name：%v", tl)
	}
}
