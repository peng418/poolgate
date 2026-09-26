package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"poolgate/internal/channel"
)

// finishChunk 构造一个只带 finish_reason 的收尾帧。
func finishChunk(reason string) channel.ChatCompletionChunk {
	c := channel.ChatCompletionChunk{ID: "chatcmpl-t", Model: "qwen3.8-max"}
	ch := channel.ChunkChoice{}
	ch.FinishReason = reason
	c.Choices = append(c.Choices, ch)
	return c
}

// TestIsTruncatedArguments 单元：空串（无参工具）合法；非空不可解析=截断；可解析=完整。
func TestIsTruncatedArguments(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},                 // 无参工具空分片
		{"   ", false},              // 纯空白
		{"{}", false},               // 合法空对象
		{`{"a":1}`, false},          // 合法对象
		{"null", false},             // 能解析（类型错误交给 schema，非截断）
		{"[1,2]", false},            // 能解析（数组非对象，非截断）
		{`{"command": "ls -`, true}, // 半截 JSON
		{`{"a":`, true},             // 半截键
		{`\deveco-code-rust\crates\deveco`, true}, // 非 JSON 文本
	}
	for _, c := range cases {
		if got := isTruncatedArguments(c.raw); got != c.want {
			t.Errorf("isTruncatedArguments(%q)=%v want %v", c.raw, got, c.want)
		}
	}
}

// TestDropTruncatedToolCalls 过滤残缺调用，完整调用正例零改动。
func TestDropTruncatedToolCalls(t *testing.T) {
	calls := []channel.ToolCall{
		tc("ok", "read", `{"file_path":"a.go"}`),
		tc("bad", "read", `{"file_path": "`),
		tc("empty", "list_dir", ""),
		{ID: "nofn"}, // 无 function：arguments 视为空串，合法无参 → 保留
	}
	out := dropTruncatedToolCalls(calls)
	if len(out) != 3 {
		t.Fatalf("dropped=%d want 3, out=%#v", len(out), out)
	}
	if out[0].ID != "ok" || out[1].ID != "empty" || out[2].ID != "nofn" {
		t.Fatalf("order/kept wrong: %#v", out)
	}
}

// TestChatAggregateDropsTruncatedToolCalls finish_reason==length + 残缺 arguments →
// 不把脏参数交给客户端（tool_calls 整体剔除）。
func TestChatAggregateDropsTruncatedToolCalls(t *testing.T) {
	var got channel.ChatRequest
	srv, _ := newTestGateway(t, captureChat(&got,
		toolCallChunk(0, "call_bad", "read", `{"file_path":`),
		finishChunk("length"),
	), true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model":    "qodercn/qwen3.8-max",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Choices []struct {
			FinishReason string         `json:"finish_reason"`
			Message      map[string]any `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices 应 1 个，实际 %s", w.Body.String())
	}
	if out.Choices[0].FinishReason != "length" {
		t.Fatalf("finish_reason=%v want length", out.Choices[0].FinishReason)
	}
	if _, ok := out.Choices[0].Message["tool_calls"]; ok {
		t.Fatalf("截断的 tool_calls 不应交给客户端：%#v", out.Choices[0].Message["tool_calls"])
	}
}

// TestChatAggregateKeepsCompleteToolCallsUnderLength 正例零改动：finish_reason==length
// 但参数完整（以及无参数空串）→ 原样保留。
func TestChatAggregateKeepsCompleteToolCallsUnderLength(t *testing.T) {
	var got channel.ChatRequest
	srv, _ := newTestGateway(t, captureChat(&got,
		toolCallChunk(0, "c1", "read", `{"file_path":"a.go"}`),
		finishChunk("length"),
	), true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model":    "qodercn/qwen3.8-max",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	calls := out.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("length 下完整的 tool_calls 必须保留：%s", w.Body.String())
	}
	if calls[0].Function.Arguments != `{"file_path":"a.go"}` {
		t.Fatalf("完整参数被改动：%v", calls[0].Function.Arguments)
	}
}

// TestChatAggregateNormalFinishUntouched finish_reason==tool_calls（非 length）→
// 截断检测不触发，既有行为不变（残缺参数透传由既有路径处理）。
func TestChatAggregateNormalFinishUntouched(t *testing.T) {
	var got channel.ChatRequest
	srv, _ := newTestGateway(t, captureChat(&got,
		toolCallChunk(0, "c1", "read", `{"file_path":`),
		finishChunk("tool_calls"),
	), true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model":    "qodercn/qwen3.8-max",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("非 length 结束必须原样保留 tool_calls（零改动）：%s", w.Body.String())
	}
}

// TestAnthropicAggregateDropsTruncatedToolUse Anthropic 非流式出口同样要挡下残缺调用：
// 否则 buildAnthropicMessage 会把它转成 input 非法 JSON 的 tool_use 块。
func TestAnthropicAggregateDropsTruncatedToolUse(t *testing.T) {
	var got channel.ChatRequest
	srv, _ := newTestGateway(t, captureChat(&got,
		toolCallChunk(0, "call_bad", "read", `{"file_path":`),
		finishChunk("length"),
	), true)
	w := postJSON(t, srv, "/v1/messages", map[string]any{
		"model":      "qodercn/qwen3.8-max",
		"max_tokens": 256,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if out.StopReason != "max_tokens" {
		t.Fatalf("截断时 stop_reason 应为 max_tokens，实际 %q（body=%s）", out.StopReason, w.Body.String())
	}
	for _, b := range out.Content {
		if b.Type == "tool_use" {
			t.Fatalf("截断的 tool_use 块不应交给客户端：%s", w.Body.String())
		}
	}
}
