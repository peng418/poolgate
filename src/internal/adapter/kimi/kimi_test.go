package kimi

// kimi_test.go 覆盖三件最容易出错的事：
//  1. Connect 帧的切分（**半帧**是常态，切错的表现是「客户端拿到乱码/一直转圈」）；
//  2. 思考与正文的分流（分错了用户会看到正文被吞进 reasoning）；
//  3. 凭证链路（refresh token → access token、401 换一次再试）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// connectFrame 在测试里造一个 Connect 帧（0x00 + 4 字节大端长度 + JSON）。
func connectFrame(v any) []byte {
	raw, _ := encodeConnect(v)
	return raw
}

// fakeJWT 造一个能被 isAccessToken 认出来的 access token（不校验签名，只读 payload）。
func fakeJWT(t *testing.T) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"app_id": "kimi",
		"typ":    "access",
		"exp":    4102444800, // 2100-01-01，测试期间不会过期
	})
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// writeFrames 按**很小的切片**把帧流写出去，故意把帧头/载荷切开，
// 逼出「半帧」处理路径 —— 真实网络也不会按帧边界送达。
func writeFrames(w http.ResponseWriter, frames [][]byte) {
	w.Header().Set("Content-Type", "application/connect+json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, f := range frames {
		for i := 0; i < len(f); i += 7 {
			end := i + 7
			if end > len(f) {
				end = len(f)
			}
			_, _ = w.Write(f[i:end])
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 1. 帧切分
// ---------------------------------------------------------------------------

func TestSplitFramesHandlesHalfFrames(t *testing.T) {
	f1 := connectFrame(map[string]any{"block": map[string]any{"text": map[string]any{"content": "甲"}}})
	f2 := connectFrame(map[string]any{"done": map[string]any{}})
	all := append(append([]byte{}, f1...), f2...)

	var got []string
	var buf []byte
	for i := range all {
		buf = append(buf, all[i])
		frames, rest := splitFrames(buf)
		for _, f := range frames {
			if f.trailer {
				continue
			}
			got = append(got, strings.TrimSpace(string(f.payload)))
		}
		buf = buf[:copy(buf, rest)]
	}
	if len(got) != 2 {
		t.Fatalf("逐字节喂完应恰好切出 2 个载荷，得到 %d 个：%v", len(got), got)
	}
	if !strings.Contains(got[0], "甲") || !strings.Contains(got[1], "done") {
		t.Fatalf("载荷内容不对：%v", got)
	}
}

func TestSplitFramesSkipsTrailer(t *testing.T) {
	body, _ := encodeConnect(map[string]any{"done": map[string]any{}})
	trailer := append([]byte{0x80}, body[1:]...) // flag 最高位置 1 = trailer
	frames, rest := splitFrames(append(body, trailer...))
	if len(rest) != 0 || len(frames) != 2 {
		t.Fatalf("应切出 2 帧且无残留：frames=%d rest=%d", len(frames), len(rest))
	}
	if frames[0].trailer || !frames[1].trailer {
		t.Fatal("trailer 标记认错了")
	}
}

// ---------------------------------------------------------------------------
// 2. 消息拼装与模型档位
// ---------------------------------------------------------------------------

func TestPackMessagesPutsSystemFirstAndMarksTools(t *testing.T) {
	got := packMessages([]channel.Message{
		{Role: "user", Content: "北京天气"},
		{Role: "system", Content: "你是助手"},
		{Role: "assistant", ToolCalls: []channel.ToolCall{{
			ID: "call_1", Type: "function",
			Function: channel.FunctionCall{Name: "get_weather", Arguments: `{"city":"北京"}`},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: "晴"},
	})
	if !strings.HasPrefix(got, "system:你是助手\n") {
		t.Fatalf("system 行必须提到最前面：%s", got)
	}
	for _, want := range []string{"[call:get_weather]", `{"city":"北京"}`, "[/function_calls]", "[TOOL_RESULT for call_1] 晴"} {
		if !strings.Contains(got, want) {
			t.Fatalf("拼装缺 %q：%s", want, got)
		}
	}
}

func TestSpecOfSuffixes(t *testing.T) {
	cases := []struct {
		id       string
		thinking bool
		search   bool
		known    bool
	}{
		{"kimi-k2.6", false, false, true},
		{"kimi-k2.6-thinking", true, false, true},
		{"kimi-k2.6-search", false, true, true},
		{"kimi-k2.6-thinking-search", true, true, true},
		{"kimi-k2.6-search-thinking", true, true, true}, // 后缀顺序不定
		{"kimi-k2", false, false, true},
		{"gpt-5", false, false, false},            // 不是 Kimi 的档位
		{"kimi-k2.6-vision", false, false, false}, // 带别名：形态不明，不认
	}
	for _, c := range cases {
		got := specOf(c.id)
		if got.known != c.known || (c.known && (got.thinking != c.thinking || got.search != c.search)) {
			t.Fatalf("%s: got=%+v want thinking=%v search=%v known=%v", c.id, got, c.thinking, c.search, c.known)
		}
		if got.scenario != scenario {
			t.Fatalf("%s: scenario 应为 %s，得到 %s", c.id, scenario, got.scenario)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. 端到端：换令牌 → 对话（含半帧、思考分流、结束）
// ---------------------------------------------------------------------------

func TestChatEndToEnd(t *testing.T) {
	jwt := fakeJWT(t)
	var chatCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epRefresh:
			if got := r.Header.Get("Authorization"); got != "Bearer refresh-tok" {
				t.Errorf("换令牌应带 Bearer refresh token，得到 %q", got)
			}
			if n := len(r.Header.Get("X-Msh-Device-Id")); n != 19 {
				t.Errorf("设备号必须是 19 位十进制，得到 %d 位：%q", n, r.Header.Get("X-Msh-Device-Id"))
			}
			_, _ = w.Write([]byte(`{"access_token":"` + jwt + `"}`))

		case epChat:
			chatCalls++
			if got := r.Header.Get("Authorization"); got != "Bearer "+jwt {
				t.Errorf("对话应带 Bearer access token，得到 %q（refresh 链路没接上）", got)
			}
			if got := r.Header.Get("Content-Type"); got != "application/connect+json" {
				t.Errorf("对话必须是 Connect 信封形态，Content-Type=%q", got)
			}
			if got := r.Header.Get("Connect-Protocol-Version"); got != "1" {
				t.Errorf("缺 Connect-Protocol-Version：%q", got)
			}
			raw, _ := io.ReadAll(r.Body)
			frames, rest := splitFrames(raw)
			if len(frames) != 1 || len(rest) != 0 {
				t.Fatalf("请求体应恰好 1 帧，得到 %d 帧（残留 %d 字节）", len(frames), len(rest))
			}
			var payload struct {
				Scenario string `json:"scenario"`
				Tools    []any  `json:"tools"`
				Message  struct {
					Role   string `json:"role"`
					Blocks []struct {
						Text struct {
							Content string `json:"content"`
						} `json:"text"`
					} `json:"blocks"`
				} `json:"message"`
				Options struct {
					Thinking bool `json:"thinking"`
				} `json:"options"`
			}
			if err := json.Unmarshal(frames[0].payload, &payload); err != nil {
				t.Fatalf("请求体不是 JSON：%v", err)
			}
			if payload.Scenario != scenario {
				t.Errorf("scenario 不对：%q", payload.Scenario)
			}
			if got := payload.Message.Blocks[0].Text.Content; got != "user:你好" {
				t.Errorf("消息拼装不对：%q", got)
			}
			if !payload.Options.Thinking {
				t.Error("-thinking 档应把 thinking 打开")
			}
			if len(payload.Tools) != 0 {
				t.Errorf("不带 -search 的档位不该带联网工具：%v", payload.Tools)
			}

			writeFrames(w, [][]byte{
				// 正文（flags 显式标 answer）
				connectFrame(map[string]any{
					"block": map[string]any{"text": map[string]any{"content": "你好", "flags": "answer"}}}),
				// 思考阶段中的文本：multiStage 说处于 THINKING，text 要归到 reasoning
				connectFrame(map[string]any{
					"block": map[string]any{
						"multiStage": map[string]any{"stages": []any{map[string]any{"name": thinkingStage}}},
						"text":       map[string]any{"content": "想一下"},
					}}),
				// 独立的 think 块
				connectFrame(map[string]any{
					"block": map[string]any{"think": map[string]any{"content": "再想"}},
					"mask":  "block.think"}),
				// 思考阶段结束（status=completed → answer），正文继续
				connectFrame(map[string]any{
					"block": map[string]any{
						"multiStage": map[string]any{"stages": []any{map[string]any{
							"name": thinkingStage, "status": "completed"}}},
						"text": map[string]any{"content": "世界"},
					}}),
				// 心跳帧：不能当成内容
				connectFrame(map[string]any{"heartbeat": map[string]any{}}),
				connectFrame(map[string]any{"done": map[string]any{}}),
			})

		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "kimi-test", RefreshToken: "refresh-tok"}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model:    "kimi-k2.6-thinking",
		Messages: []channel.Message{{Role: "user", Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	var content, reasoning strings.Builder
	finish := ""
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
		}
	}
	if content.String() != "你好世界" {
		t.Fatalf("正文不对：%q（期望 你好世界）", content.String())
	}
	if reasoning.String() != "想一下再想" {
		t.Fatalf("思考分流不对：%q（期望 想一下再想）", reasoning.String())
	}
	if finish != "stop" {
		t.Fatalf("结束帧不对：%q", finish)
	}
	if chatCalls != 1 {
		t.Fatalf("应只打一次对话接口，实际 %d", chatCalls)
	}
}

// access token 过期（401）时应换一次令牌再试，且只重试一次。
func TestChatRefreshesOnceOn401(t *testing.T) {
	jwt := fakeJWT(t)
	var chats, refreshes int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epRefresh:
			refreshes++
			_, _ = w.Write([]byte(`{"access_token":"` + jwt + `"}`))
		case epChat:
			chats++
			if chats == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
				return
			}
			writeFrames(w, [][]byte{connectFrame(map[string]any{"done": map[string]any{}})})
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "kimi-test", RefreshToken: "refresh-tok"}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "kimi-k2.6", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("401 之后应当换令牌重试成功，却失败了：%v", err)
	}
	defer st.Close()
	for {
		if _, err := st.Next(); err != nil {
			break
		}
	}
	if chats != 2 {
		t.Fatalf("应重试恰好一次（共 2 次调用），实际 %d", chats)
	}
	if refreshes != 2 {
		t.Fatalf("401 后必须丢掉缓存再换一次令牌（共 2 次），实际 %d", refreshes)
	}
}

// 帧内错误若是身份问题，应归一成 SessionDead（提示重新粘贴），而不是当成上游故障。
func TestFrameErrorIsSessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == epRefresh {
			_, _ = w.Write([]byte(`{"access_token":"` + fakeJWT(t) + `"}`))
			return
		}
		writeFrames(w, [][]byte{connectFrame(map[string]any{
			"error": map[string]any{"message": "invalid token"}})})
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "kimi-test", RefreshToken: "refresh-tok"}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "kimi-k2.6", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat 应成功建立流（错误在流里）：%v", err)
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
	k, ok := errs.KindOf(got)
	if !ok || k != errs.SessionDead {
		t.Fatalf("帧内 token 错误应归一成 SessionDead，得到 %v（%v）", k, got)
	}
}

func TestClassify(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{}`, errs.SessionDead},
		{403, `{"error":{"message":"forbidden"}}`, errs.SessionDead},
		{429, `{"error":{"message":"rate limited"}}`, errs.SoftRate},
		{429, `{"error":{"message":"quota exhausted"}}`, errs.HardCredit},
		{404, `{}`, errs.ModelUnavailable},
		{400, `{"error":{"message":"context too long"}}`, errs.PromptTooLong},
		{503, `{}`, errs.UpstreamFault},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("status=%d body=%s: 期望 %v 得到 %v", c.status, c.body, c.want, got)
		}
	}
}
