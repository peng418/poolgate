package kiro

// kiro_test.go 覆盖五件最容易出错的事：
//  1. AWS Event Stream 帧的切分（**半包**是常态；切错的表现是「客户端拿到乱码/一直转圈」）；
//  2. 工具调用的重组（{"name":…} + {"input":…} + {"stop":…} → 一条 tool_calls）；
//  3. 两种鉴权模式的换令牌（桌面版 / 企业版 AWS SSO OIDC）；
//  4. 请求头（x-amz-target 等）与请求体形态；
//  5. 错误归一（402/429/403/5xx）。

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// ---------------------------------------------------------------------------
// 测试用：AWS Event Stream 帧构造器
// ---------------------------------------------------------------------------

// headers 构造一个最小头区（":message-type" + ":event-type"）。
func frameHeaders(eventType string) []byte {
	h := map[string]string{
		":content-type": "application/json",
		":event-type":   eventType,
		":message-type": "event",
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []byte
	for _, k := range keys {
		v := h[k]
		out = append(out, byte(len(k)))
		out = append(out, k...)
		out = append(out, 7) // 值类型 7 = string
		var n [2]byte
		binary.BigEndian.PutUint16(n[:], uint16(len(v)))
		out = append(out, n[:]...)
		out = append(out, v...)
	}
	return out
}

// awsFrame 造一个合法的 AWS Event Stream 帧（前导长度 + 头 + 载荷 + 双 CRC）。
// 必须真的算 CRC：我们的切片器就是靠它判「这是不是帧」。
func awsFrame(payload string) []byte {
	body := []byte(payload)
	hdr := frameHeaders("assistantResponseEvent")
	total := framePreludeLen + len(hdr) + len(body) + frameCRCLen

	buf := make([]byte, 0, total)
	var pre [framePreludeLen]byte
	binary.BigEndian.PutUint32(pre[0:4], uint32(total))
	binary.BigEndian.PutUint32(pre[4:8], uint32(len(hdr)))
	binary.BigEndian.PutUint32(pre[8:12], crc32.ChecksumIEEE(pre[0:8]))
	buf = append(buf, pre[:]...)
	buf = append(buf, hdr...)
	buf = append(buf, body...)
	var mcrc [4]byte
	binary.BigEndian.PutUint32(mcrc[:], crc32.ChecksumIEEE(buf))
	buf = append(buf, mcrc[:]...)
	return buf
}

// frameJSON 把对象序列化成帧载荷（注意 Go 的 map 是按 key 排序序列化的，
// 所以这里天然会造出「键序与上游不同」的载荷 —— 正好检验按键分派的健壮性）。
func frameJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return awsFrame(string(raw))
}

// writeFrames 按**很小的切片**把帧流写出去，故意把帧头/载荷切开，
// 逼出「半包」处理路径 —— 真实网络也不会按帧边界送达。
func writeFrames(w http.ResponseWriter, frames [][]byte) {
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
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

func TestSplitEventFramesHandlesHalfFrames(t *testing.T) {
	f1 := frameJSON(map[string]any{"content": "甲"})
	f2 := frameJSON(map[string]any{"content": "乙"})
	f3 := frameJSON(map[string]any{"stop": true})
	all := append(append(append([]byte{}, f1...), f2...), f3...)

	var got []string
	buf := make([]byte, 0, len(all))
	for i := range all {
		buf = append(buf, all[i])
		frames, rest, framed := splitEventFrames(buf)
		if !framed {
			t.Fatalf("第 %d 字节时被判成「不是帧」，缓冲只有 %d 字节", i, len(buf))
		}
		for _, f := range frames {
			got = append(got, strings.TrimSpace(string(f.payload)))
		}
		buf = buf[:copy(buf, rest)]
	}
	if len(got) != 3 {
		t.Fatalf("逐字节喂完应恰好切出 3 个载荷，得到 %d 个：%v", len(got), got)
	}
	if !strings.Contains(got[0], "甲") || !strings.Contains(got[1], "乙") || !strings.Contains(got[2], "stop") {
		t.Fatalf("载荷内容不对：%v", got)
	}
}

func TestSplitEventFramesRejectsNonFrame(t *testing.T) {
	// 队首不是合法帧（模拟中间层直接回了一段 JSON 文本）：必须明确说「不是帧」，
	// 让 stream 切到文本扫描兜底，而不是一直等一个永远不会来的长度。
	raw := []byte(`{"content":"这不是帧，是裸 JSON 文本"}`)
	if _, _, framed := splitEventFrames(raw); framed {
		t.Fatal("裸 JSON 文本不该被判成合法帧")
	}
}

func TestSplitEventFramesReadsHeaders(t *testing.T) {
	raw := awsFrame(`{"content":"x"}`)
	frames, rest, framed := splitEventFrames(raw)
	if !framed || len(rest) != 0 || len(frames) != 1 {
		t.Fatalf("应切出 1 帧且无残留：frames=%d rest=%d framed=%v", len(frames), len(rest), framed)
	}
	if got := frames[0].headers[":event-type"]; got != "assistantResponseEvent" {
		t.Fatalf("头区没解出来：%v", frames[0].headers)
	}
	if got := frames[0].headers[":message-type"]; got != "event" {
		t.Fatalf(":message-type 解错了：%q", got)
	}
}

// 半包：长度头到了、载荷没到齐时，绝不能把它当内容吐出去。
func TestSplitEventFramesWaitsForTruncatedPayload(t *testing.T) {
	raw := awsFrame(`{"content":"完整的"}`)
	cut := raw[:len(raw)-6] // 掐掉一点（消息 CRC 与部分载荷）
	frames, rest, framed := splitEventFrames(cut)
	if !framed {
		t.Fatal("前半截仍是合法帧头，不该被判成「不是帧」")
	}
	if len(frames) != 0 || len(rest) != len(cut) {
		t.Fatalf("整帧没到齐时必须一帧都不吐：frames=%d rest=%d", len(frames), len(rest))
	}
}

// 文本扫描兜底：没有帧也能把事件 JSON 抠出来（参考实现走的就是这条路）。
func TestScanTextEvents(t *testing.T) {
	raw := []byte(`{"content":"甲"}{"content":"乙"}{"content":"半截`)
	got, rest := scanTextEvents(raw)
	if len(got) != 2 || !strings.Contains(got[0], "甲") || !strings.Contains(got[1], "乙") {
		t.Fatalf("应抠出 2 个完整事件：%v", got)
	}
	if !strings.Contains(string(rest), "半截") {
		t.Fatalf("半截 JSON 应留到下一批：%q", rest)
	}
}

// 内容里带花括号/转义时，括弧配平不能数错（工具参数与代码片段里很常见）。
func TestFindMatchingBraceSkipsStrings(t *testing.T) {
	s := `{"content":"里面有 } 和 { 还有 \" 转义"}{"next":1}`
	end := findMatchingBrace(s, 0)
	if end < 0 {
		t.Fatal("没配平出来")
	}
	if got := s[:end+1]; !strings.HasSuffix(got, `转义"}`) {
		t.Fatalf("配平位置不对：%q", got)
	}
}

// ---------------------------------------------------------------------------
// 2. 工具调用重组
// ---------------------------------------------------------------------------

func TestParserReassemblesToolCall(t *testing.T) {
	p := newParser()
	// 第一帧：空对象 input + name（Go 序列化会把 input 排到 name 前面，
	// 正好检验「按键分派、不依赖键序」）。空对象必须被当成「还没有内容」，
	// 否则会拼出 {}{…} 这种非法 JSON。
	for _, payload := range []map[string]any{
		{"name": "get_weather", "toolUseId": "tu_1", "input": map[string]any{}},
		{"input": `{"city":"`},
		{"input": `北京"}`},
		{"stop": true},
	} {
		if evs := p.feed(framePayload(t, payload)); len(evs) != 0 {
			t.Fatalf("工具帧不该产出即时事件：%+v", evs)
		}
	}
	calls := p.finish()
	if len(calls) != 1 {
		t.Fatalf("应重组出恰好 1 条 tool_call，得到 %d 条：%+v", len(calls), calls)
	}
	c := calls[0]
	if c.Index != 0 || c.ID != "tu_1" || c.Type != "function" {
		t.Fatalf("工具调用的元信息不对：%+v", c)
	}
	if c.Function.Name != "get_weather" {
		t.Fatalf("函数名不对：%q", c.Function.Name)
	}
	if c.Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("参数没有正确拼回去：%q", c.Function.Arguments)
	}
}

// 同 id 的重复调用（上游会先发一条空参数占位）必须去重成一条。
func TestParserDedupesToolCalls(t *testing.T) {
	p := newParser()
	for _, payload := range []map[string]any{
		{"name": "bash", "toolUseId": "tu_9", "input": map[string]any{}, "stop": true},
		{"name": "bash", "toolUseId": "tu_9", "input": `{"cmd":"ls"}`, "stop": true},
	} {
		p.feed(framePayload(t, payload))
	}
	calls := p.finish()
	if len(calls) != 1 {
		t.Fatalf("同 id 应去重成 1 条，得到 %d 条：%+v", len(calls), calls)
	}
	if calls[0].Function.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("去重应保留参数更全的那条：%q", calls[0].Function.Arguments)
	}
}

// 参数被上游截断（拼不成合法 JSON）时兜 "{}"，不能把坏 JSON 透给客户端。
func TestParserBadArgumentsFallback(t *testing.T) {
	p := newParser()
	p.feed(framePayload(t, map[string]any{"name": "x", "toolUseId": "tu_2", "input": `{"a":`}))
	calls := p.finish()
	if len(calls) != 1 || calls[0].Function.Arguments != "{}" {
		t.Fatalf("坏参数应兜成 {}：%+v", calls)
	}
}

// 正文增量去重 + followupPrompt 丢弃。
func TestParserContentDedupe(t *testing.T) {
	p := newParser()
	if evs := p.feed(framePayload(t, map[string]any{"content": "甲"})); len(evs) != 1 || evs[0].text != "甲" {
		t.Fatalf("第一段正文应产出：%+v", evs)
	}
	if evs := p.feed(framePayload(t, map[string]any{"content": "甲"})); len(evs) != 0 {
		t.Fatalf("与上一段完全相同的增量应丢掉：%+v", evs)
	}
	if evs := p.feed(framePayload(t, map[string]any{"followupPrompt": "要不要继续", "content": "建议"})); len(evs) != 0 {
		t.Fatalf("followupPrompt 是建议，不是正文：%+v", evs)
	}
}

// framePayload 把对象序列化成帧载荷字节。
func framePayload(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ---------------------------------------------------------------------------
// 3+4. 端到端：换令牌 → 对话（请求头/请求体/正文增量/半包）
// ---------------------------------------------------------------------------

type drainResult struct {
	content  string
	calls    []channel.ToolCall
	finish   string
	usage    map[string]any
	terminal error
}

// drain 把一条流读完。
func drain(t *testing.T, st channel.Stream) drainResult {
	t.Helper()
	var out drainResult
	var content strings.Builder
	for {
		ch, err := st.Next()
		if err != nil {
			if err != io.EOF {
				out.terminal = err
			}
			out.content = content.String()
			return out
		}
		for _, choice := range ch.Choices {
			content.WriteString(choice.Delta.Content)
			out.calls = mergeCalls(out.calls, choice.Delta.ToolCalls)
			if choice.FinishReason != "" {
				out.finish = choice.FinishReason
			}
		}
		if ch.Usage != nil {
			out.usage = ch.Usage
		}
	}
}

// mergeCalls 按 index 合并流式工具调用分片（与客户端要做的事一致）。
func mergeCalls(dst, add []channel.ToolCall) []channel.ToolCall {
	for _, tc := range add {
		found := false
		for i := range dst {
			if dst[i].Index != tc.Index {
				continue
			}
			if tc.Function.Name != "" {
				dst[i].Function.Name = tc.Function.Name
			}
			if tc.ID != "" {
				dst[i].ID = tc.ID
			}
			dst[i].Function.Arguments += tc.Function.Arguments
			found = true
			break
		}
		if !found {
			dst = append(dst, tc)
		}
	}
	return dst
}

func TestChatEndToEnd(t *testing.T) {
	const accessToken = "access-token-1"
	const profileArn = "arn:aws:codewhisperer:us-east-1:123:profile/abc"

	var refreshCalls, generateCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathDesktopRefresh:
			refreshCalls++
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("换令牌应是 JSON 请求，Content-Type=%q", got)
			}
			if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "KiroIDE-"+kiroVersion+"-") {
				t.Errorf("换令牌的 UA 应带 KiroIDE 指纹：%q", ua)
			}
			body, _ := io.ReadAll(r.Body)
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			if in["refreshToken"] != "refresh-tok" {
				t.Errorf("桌面版换令牌的请求体不对：%s", body)
			}
			_, _ = w.Write([]byte(`{"accessToken":"` + accessToken + `","refreshToken":"refresh-tok","expiresIn":3600,"profileArn":"` + profileArn + `"}`))

		case pathGenerate:
			generateCalls++
			// 请求头：Amz-Json 那一整套，少一个上游就按别的形态路由
			for k, want := range map[string]string{
				"Authorization":               "Bearer " + accessToken,
				"Content-Type":                amzContentType,
				"x-amz-target":                xAmzTarget,
				"x-amzn-codewhisperer-optout": "true",
				"x-amzn-kiro-agent-mode":      "vibe",
				"amz-sdk-request":             "attempt=1; max=3",
			} {
				if got := r.Header.Get(k); got != want {
					t.Errorf("请求头 %s 不对：%q（期望 %q）", k, got, want)
				}
			}
			if got := r.Header.Get("amz-sdk-invocation-id"); len(got) != 36 {
				t.Errorf("amz-sdk-invocation-id 应是 UUID 形态：%q", got)
			}
			if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "KiroIDE-"+kiroVersion+"-") ||
				!strings.Contains(ua, "api/codewhispererstreaming") {
				t.Errorf("对话 UA 形态不对：%q", ua)
			}

			// 请求体：形态 + 换令牌时下发的 profileArn 必须带上
			body, _ := io.ReadAll(r.Body)
			var in struct {
				ProfileArn string `json:"profileArn"`
				State      struct {
					ChatTriggerType string `json:"chatTriggerType"`
					ConversationID  string `json:"conversationId"`
					CurrentMessage  struct {
						UserInputMessage struct {
							Content string `json:"content"`
							ModelID string `json:"modelId"`
							Origin  string `json:"origin"`
							Context struct {
								Tools []map[string]any `json:"tools"`
							} `json:"userInputMessageContext"`
						} `json:"userInputMessage"`
					} `json:"currentMessage"`
				} `json:"conversationState"`
			}
			if err := json.Unmarshal(body, &in); err != nil {
				t.Fatalf("请求体不是 JSON：%v", err)
			}
			if in.ProfileArn != profileArn {
				t.Errorf("换令牌拿到的 profileArn 必须放进请求体：%q", in.ProfileArn)
			}
			if in.State.ChatTriggerType != chatTriggerManual || in.State.ConversationID == "" {
				t.Errorf("conversationState 形态不对：%+v", in.State)
			}
			cur := in.State.CurrentMessage.UserInputMessage
			if !strings.Contains(cur.Content, "你好") || cur.ModelID != "claude-sonnet-4.5" || cur.Origin != originAIEditor {
				t.Errorf("currentMessage 不对：%+v", cur)
			}
			if len(cur.Context.Tools) != 1 {
				t.Errorf("工具定义没有透传：%+v", cur.Context.Tools)
			}

			// 响应：故意逐 7 字节写，逼出半包路径
			writeFrames(w, [][]byte{
				frameJSON(map[string]any{"content": "你"}),
				frameJSON(map[string]any{"content": "好"}),
				frameJSON(map[string]any{"usage": 0.75}),
			})

		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.authBase, a.oidcBase, a.apiBase = srv.URL, srv.URL, srv.URL

	cred := &channel.Credential{UID: "kiro-test", RefreshToken: "refresh-tok"}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []channel.Message{{Role: "user", Content: "你好"}},
		Tools: []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "查天气",
				"parameters": map[string]any{
					"type":                 "object",
					"additionalProperties": false, // 上游不认这个字段，必须被洗掉
					"required":             []any{},
					"properties":           map[string]any{"city": map[string]any{"type": "string"}},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	got := drain(t, st)
	if got.content != "你好" {
		t.Fatalf("正文不对：%q（期望 你好；半包切分或去重出了问题）", got.content)
	}
	if got.finish != "stop" {
		t.Fatalf("结束帧不对：%q", got.finish)
	}
	if got.usage["credits_used"] != 0.75 {
		t.Fatalf("计费额度没有如实转出：%+v", got.usage)
	}
	if refreshCalls != 1 || generateCalls != 1 {
		t.Fatalf("应各打一次：refresh=%d generate=%d", refreshCalls, generateCalls)
	}
	// 换令牌时学到的 profileArn 应回填到凭证上（进程内后续请求就不用再学了）
	if profileArnOf(cred) != profileArn {
		t.Fatalf("profileArn 没回填到凭证：%q", profileArnOf(cred))
	}
}

// 帧内错误（:message-type=error）必须冒出来，不能被静默吞掉。
func TestChatSurfacesErrorFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathDesktopRefresh {
			_, _ = w.Write([]byte(`{"accessToken":"a","expiresIn":3600}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 错误帧的头是 :message-type=error
		errHdr := frameHeaders("error")
		body := []byte(`{"message":"Invalid token"}`)
		total := framePreludeLen + len(errHdr) + len(body) + frameCRCLen
		var pre [framePreludeLen]byte
		binary.BigEndian.PutUint32(pre[0:4], uint32(total))
		binary.BigEndian.PutUint32(pre[4:8], uint32(len(errHdr)))
		binary.BigEndian.PutUint32(pre[8:12], crc32.ChecksumIEEE(pre[0:8]))
		raw := append([]byte{}, pre[:]...)
		raw = append(raw, errHdr...)
		raw = append(raw, body...)
		var mcrc [4]byte
		binary.BigEndian.PutUint32(mcrc[:], crc32.ChecksumIEEE(raw))
		raw = append(raw, mcrc[:]...)
		_, _ = w.Write(raw)
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	a := New()
	a.authBase, a.oidcBase, a.apiBase = srv.URL, srv.URL, srv.URL
	cred := &channel.Credential{UID: "kiro-test", RefreshToken: "rt"}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "claude-sonnet-4.5", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat 应成功建流（错误在流里）：%v", err)
	}
	defer st.Close()
	got := drain(t, st)
	if got.terminal == nil {
		t.Fatal("错误帧必须冒出来，不能静默结束")
	}
	k, ok := errs.KindOf(got.terminal)
	if !ok || k != errs.SessionDead {
		t.Fatalf("令牌类错误应归一成 SessionDead，得到 %v（%v）", k, got.terminal)
	}
	if !strings.Contains(got.terminal.Error(), "Invalid token") {
		t.Fatalf("错误必须带上上游原话：%v", got.terminal)
	}
}

// 工具调用走完整链路：半包帧 → 重组 → 一条 tool_calls + finish_reason=tool_calls。
func TestChatToolCallEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathDesktopRefresh {
			_, _ = w.Write([]byte(`{"accessToken":"a","expiresIn":3600}`))
			return
		}
		writeFrames(w, [][]byte{
			frameJSON(map[string]any{"name": "get_weather", "toolUseId": "tu_1", "input": map[string]any{}}),
			frameJSON(map[string]any{"input": `{"city":"`}),
			frameJSON(map[string]any{"input": `北京"}`}),
			frameJSON(map[string]any{"stop": true}),
		})
	}))
	defer srv.Close()

	a := New()
	a.authBase, a.oidcBase, a.apiBase = srv.URL, srv.URL, srv.URL
	cred := &channel.Credential{UID: "kiro-test", RefreshToken: "rt"}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "claude-sonnet-4.5", Messages: []channel.Message{{Role: "user", Content: "北京天气"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	got := drain(t, st)
	if got.terminal != nil {
		t.Fatalf("不该有错误：%v", got.terminal)
	}
	if len(got.calls) != 1 {
		t.Fatalf("应拿到 1 条 tool_call，得到 %d 条：%+v", len(got.calls), got.calls)
	}
	if got.calls[0].Function.Name != "get_weather" || got.calls[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("工具调用重组不对：%+v", got.calls[0])
	}
	if got.finish != "tool_calls" {
		t.Fatalf("有工具调用时 finish_reason 必须是 tool_calls，得到 %q", got.finish)
	}
}

// 上游回的不是帧（中间层插了裸 JSON）时，文本扫描兜底要能救回来。
func TestChatFallsBackToTextScan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathDesktopRefresh {
			_, _ = w.Write([]byte(`{"accessToken":"a","expiresIn":3600}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{`{"content":"甲"}`, `{"content":"乙"}`} {
			_, _ = w.Write([]byte(chunk))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	a := New()
	a.authBase, a.oidcBase, a.apiBase = srv.URL, srv.URL, srv.URL
	cred := &channel.Credential{UID: "kiro-test", RefreshToken: "rt"}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "claude-sonnet-4.5", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	if got := drain(t, st); got.content != "甲乙" {
		t.Fatalf("文本兜底没救回来：%q", got.content)
	}
}

// 上游把过期令牌判 403：必须换一次令牌重试，且只重试一次。
func TestChatRefreshesOnceOn403(t *testing.T) {
	var generates, refreshes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathDesktopRefresh:
			refreshes++
			_, _ = w.Write([]byte(`{"accessToken":"a","expiresIn":3600}`))
		case pathGenerate:
			generates++
			if generates == 1 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"token expired"}`))
				return
			}
			writeFrames(w, [][]byte{frameJSON(map[string]any{"content": "ok"})})
		}
	}))
	defer srv.Close()

	a := New()
	a.authBase, a.oidcBase, a.apiBase = srv.URL, srv.URL, srv.URL
	cred := &channel.Credential{UID: "kiro-test", RefreshToken: "rt"}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "claude-sonnet-4.5", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("403 之后应换令牌重试并成功，却失败了：%v", err)
	}
	defer st.Close()
	if got := drain(t, st); got.content != "ok" {
		t.Fatalf("重试后正文不对：%q", got.content)
	}
	if generates != 2 || refreshes != 2 {
		t.Fatalf("应重试恰好一次：generate=%d refresh=%d（期望 2/2）", generates, refreshes)
	}
}

// 非 2xx 时错误必须带 kind 与上游原话。
func TestChatHTTPErrorIsNormalized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathDesktopRefresh {
			_, _ = w.Write([]byte(`{"accessToken":"a","expiresIn":3600}`))
			return
		}
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"message":"payment required"}`))
	}))
	defer srv.Close()

	a := New()
	a.authBase, a.oidcBase, a.apiBase = srv.URL, srv.URL, srv.URL
	cred := &channel.Credential{UID: "kiro-test", RefreshToken: "rt"}
	_, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "claude-sonnet-4.5", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("402 必须报错")
	}
	k, ok := errs.KindOf(err)
	if !ok || k != errs.HardCredit {
		t.Fatalf("402 应归一成 HardCredit，得到 %v（%v）", k, err)
	}
}

// ---------------------------------------------------------------------------
// 5. 错误归一
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{}`, errs.SessionDead},
		{403, `{"message":"token expired"}`, errs.SessionDead},
		{402, `{"message":"payment required"}`, errs.HardCredit},
		{429, `{"message":"Too many requests"}`, errs.SoftRate},
		{429, `{"message":"quota exceeded"}`, errs.HardCredit},
		{400, `{"message":"invalid_grant"}`, errs.SessionDead},
		{400, `{"message":"context too long"}`, errs.PromptTooLong},
		{400, `{"message":"Improperly formed request"}`, errs.Parse},
		{404, `{}`, errs.ModelUnavailable},
		{413, `{}`, errs.PromptTooLong},
		{500, `{}`, errs.UpstreamFault},
		{503, `{}`, errs.UpstreamFault},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("status=%d body=%s: 期望 %v 得到 %v", c.status, c.body, c.want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 能力声明与模型清单
// ---------------------------------------------------------------------------

func TestSpecAndModels(t *testing.T) {
	a := New()
	sp := a.Spec()
	if sp.Kind != channel.Kiro || !sp.Downstream() {
		t.Fatalf("Kind/Status 不对：%+v", sp)
	}
	if !sp.Tools || sp.ToolsShim {
		t.Fatalf("上游原生支持工具调用：应 Tools=true 且 ToolsShim=false，得到 %+v", sp)
	}
	if sp.Reasoning {
		t.Fatal("上游没有思考字段，Reasoning 必须为 false（不申明不存在的字段）")
	}
	if sp.Images {
		t.Fatal("内部消息契约里没有图片部件，Images 必须为 false")
	}
	if !sp.SSEOnly || sp.DefaultMinIntervalSec <= 0 {
		t.Fatalf("SSEOnly/最小间隔不对：%+v", sp)
	}
	if sp.Category != channel.CategoryCoding {
		t.Fatalf("Kiro 是订阅额度池的 IDE 类，Category 应为 coding：%q", sp.Category)
	}

	models, err := a.Models(context.Background(), nil)
	if err != nil || len(models) == 0 {
		t.Fatalf("模型清单为空：%v", err)
	}
	for _, m := range models {
		if m.Source != channel.SourceLocal {
			t.Fatalf("%s 的来源必须如实标 local（上游不提供目录）：%q", m.ID, m.Source)
		}
		if m.Tools != channel.CapYes || m.Reasoning != channel.CapNo {
			t.Fatalf("%s 的能力位不对：tools=%v reasoning=%v", m.ID, m.Tools, m.Reasoning)
		}
	}
	// 认不出的档位原样透传（上游才是最终裁判），点/横线两种写法都要认
	if got := resolveModel("claude-haiku-4-5"); got != "claude-haiku-4.5" {
		t.Fatalf("横线写法没归一：%q", got)
	}
	if got := resolveModel("auto-kiro"); got != "auto" {
		t.Fatalf("别名没生效：%q", got)
	}
	if got := resolveModel("some-future-model"); got != "some-future-model" {
		t.Fatalf("未知档位应原样透传：%q", got)
	}
	// 上下文窗口标记：参考实现剥的是「[数字+m/k]」任意位数、大小写不敏感
	//（model_resolver.py:132），不是只有字面量 [1m]/[200k]。
	for in, want := range map[string]string{
		"claude-sonnet-4.5[1m]":   "claude-sonnet-4.5",
		"claude-sonnet-4.5[200k]": "claude-sonnet-4.5",
		"claude-sonnet-4.5[1M]":   "claude-sonnet-4.5",
		"claude-sonnet-4.5[256k]": "claude-sonnet-4.5",
		"claude-sonnet-4.5[abc]":  "claude-sonnet-4.5[abc]", // 不是窗口标记，原样透传
		"claude-sonnet-4.5[1g]":   "claude-sonnet-4.5[1g]",  // 单位不是 m/k，原样透传
	} {
		if got := resolveModel(in); got != want {
			t.Fatalf("窗口标记剥离不对 %q：期望 %q 得到 %q", in, want, got)
		}
	}
}

// conversationId 的形态要照参考实现：不带 messages 调用 generate_conversation_id()
// 得到的是 str(uuid.uuid4())（utils.py:102-127 + routes 的 4 处调用），
// 即带连字符的 8-4-4-4-12。
func TestBuildPayloadConversationIDIsUUID(t *testing.T) {
	payload, err := buildPayload(channel.ChatRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []channel.Message{{Role: "user", Content: "hi"}},
	}, "claude-sonnet-4.5", "")
	if err != nil {
		t.Fatalf("buildPayload 失败：%v", err)
	}
	id, _ := payload["conversationState"].(map[string]any)["conversationId"].(string)
	if len(id) != 36 {
		t.Fatalf("conversationId 应是带连字符的 UUID（36 字符），得到 %q", id)
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("conversationId 的连字符位置不对：%q", id)
	}
}

// ---------------------------------------------------------------------------
// 6. 登录：粘贴 → 当场验一次
// ---------------------------------------------------------------------------

// accepter 把会话断言成「能收粘贴」的渠道（控制台也是这么做的）。
func accepter(t *testing.T, s channel.LoginSession) channel.CallbackAcceptor {
	t.Helper()
	acc, ok := s.(channel.CallbackAcceptor)
	if !ok {
		t.Fatal("登录会话没实现 channel.CallbackAcceptor")
	}
	return acc
}

func TestLoginDesktopPasteAndPoll(t *testing.T) {
	var refreshCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathDesktopRefresh {
			t.Errorf("桌面版不该打别的路径：%s", r.URL.Path)
		}
		refreshCalls++
		_, _ = w.Write([]byte(`{"accessToken":"new-access","refreshToken":"rt-desktop","expiresIn":3600,"profileArn":"arn:x"}`))
	}))
	defer srv.Close()

	a := New()
	a.authBase, a.oidcBase, a.apiBase = srv.URL, srv.URL, srv.URL

	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Cancel()
	if sess.AuthURL() == "" {
		t.Fatal("AuthURL 不能为空")
	}
	h, ok := sess.(interface{ Hint() string })
	if !ok || !strings.Contains(h.Hint(), "refreshToken") {
		t.Fatalf("引导语必须说清粘什么：%v", h)
	}
	if _, err := sess.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("还没粘时应返回 ErrPending，得到 %v", err)
	}
	if err := accepter(t, sess).AcceptCallback(`"eyJhbGciOiJIUzI1NiJ9thisIsARefreshTokenLookalike0001"`); err != nil {
		t.Fatalf("AcceptCallback 失败：%v", err)
	}
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if refreshCalls != 1 {
		t.Fatalf("Poll 必须当场用换令牌接口验一次，实际调用 %d 次", refreshCalls)
	}
	if cred.AccessToken != "new-access" || cred.RefreshToken != "rt-desktop" {
		t.Fatalf("凭证组装不对：%+v", cred)
	}
	if cred.ExpiresAt.IsZero() {
		t.Fatal("过期时间必须落下来")
	}
	if !strings.HasPrefix(cred.UID, "kiro-") || strings.HasPrefix(cred.UID, ssoUIDPrefix) {
		t.Fatalf("桌面版 uid 不该带 sso 标记：%q", cred.UID)
	}
}

func TestLoginEnterprisePasteUsesOIDC(t *testing.T) {
	var oidcCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathOIDCToken {
			t.Errorf("企业版应打 OIDC 端点，得到 %s", r.URL.Path)
		}
		oidcCalls++
		var in map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &in)
		// AWS SSO CreateToken 要 JSON + camelCase
		for k, want := range map[string]any{
			"grantType":    "refresh_token",
			"clientId":     "cid-1",
			"clientSecret": "sec-1",
			"refreshToken": "rt-enterprise-token-value-0001",
		} {
			if in[k] != want {
				t.Errorf("OIDC 换令牌字段 %s 不对：%v（期望 %v）", k, in[k], want)
			}
		}
		_, _ = w.Write([]byte(`{"accessToken":"ent-access","refreshToken":"rt-enterprise-token-value-0001","expiresIn":1800}`))
	}))
	defer srv.Close()

	a := New()
	a.authBase, a.oidcBase, a.apiBase = srv.URL, srv.URL, srv.URL

	sess, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer sess.Cancel()
	err := accepter(t, sess).AcceptCallback(`{"refreshToken":"rt-enterprise-token-value-0001","clientId":"cid-1","clientSecret":"sec-1","region":"eu-central-1"}`)
	if err != nil {
		t.Fatalf("AcceptCallback 失败：%v", err)
	}
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if oidcCalls != 1 {
		t.Fatalf("企业版必须走 OIDC 端点，实际 %d 次", oidcCalls)
	}
	if cred.Extra["region"] != "eu-central-1" || cred.Extra["client_id"] != "cid-1" {
		t.Fatalf("附加字段没带上：%+v", cred.Extra)
	}
	if !strings.HasPrefix(cred.UID, ssoUIDPrefix) {
		t.Fatalf("企业版 uid 应带 sso 标记（重启后靠它给出可操作的错误）：%q", cred.UID)
	}
}

// 验不过就当场报错，别把废凭据塞进池子。
func TestLoginPollRejectsBadCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"invalid_grant: refresh token is not valid"}`))
	}))
	defer srv.Close()

	a := New()
	a.authBase, a.oidcBase, a.apiBase = srv.URL, srv.URL, srv.URL
	sess, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	defer sess.Cancel()
	if err := accepter(t, sess).AcceptCallback("thisIsAnInvalidRefreshToken0001"); err != nil {
		t.Fatal(err)
	}
	_, err := sess.Poll(context.Background())
	if err == nil {
		t.Fatal("换令牌失败时 Poll 必须报错")
	}
	if k, _ := errs.KindOf(err); k != errs.SessionDead {
		t.Fatalf("invalid_grant 应归一成 SessionDead，得到 %v", k)
	}
}

func TestParsePasteForms(t *testing.T) {
	tok := "eyJhbGciOiJIUzI1NiJ9eyJyZWZyZXNoIjp0cnVlfQ.signature0001"
	// 裸串
	if p, err := parsePaste(tok); err != nil || p.refreshToken != tok || p.clientID != "" {
		t.Fatalf("裸串形态解析失败：%+v %v", p, err)
	}
	// 从凭据文件里抄的一行
	line := `"refreshToken": "` + tok + `",`
	if p, err := parsePaste(line); err != nil || p.refreshToken != tok {
		t.Fatalf("抄一行的形态解析失败：%+v %v", p, err)
	}
	// snake_case 整段 JSON（kiro-cli 的 SQLite 形态）
	if p, err := parsePaste(`{"refresh_token":"` + tok + `","region":"us-west-2"}`); err != nil ||
		p.refreshToken != tok || p.region != "us-west-2" {
		t.Fatalf("snake_case 解析失败：%+v %v", p, err)
	}
	// 只给一半三件套：必须当场说清楚
	if _, err := parsePaste(`{"refreshToken":"` + tok + `","clientId":"cid"}`); err == nil {
		t.Fatal("只有 clientId 没有 clientSecret 时应报错")
	}
	// clientIdHash：要说清「去另一份文件里拿」
	_, err := parsePaste(`{"refreshToken":"` + tok + `","clientIdHash":"abc123"}`)
	if err == nil || !strings.Contains(err.Error(), "clientId") {
		t.Fatalf("clientIdHash 形态应给出可操作的提示，得到 %v", err)
	}
	// 明显误粘（路径）
	if _, err := parsePaste(`~/.aws/sso/cache/kiro-auth-token.json`); err == nil {
		t.Fatal("粘了路径时应报错")
	}
}

// 企业版凭据在落盘后丢了 clientId/clientSecret（当前 store 的 schema 不含这两个键）时，
// 必须给出可操作的错误，而不是拿企业版 refreshToken 去撞桌面端点。
func TestEnterpriseCredentialWithoutClientSecret(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	a := New()
	a.authBase, a.oidcBase, a.apiBase = srv.URL, srv.URL, srv.URL
	cred := &channel.Credential{UID: ssoUIDPrefix + "abc12345", RefreshToken: "rt-enterprise"}
	_, err := a.Refresh(context.Background(), cred)
	if err == nil {
		t.Fatal("缺少 clientId/clientSecret 时必须报错")
	}
	if k, _ := errs.KindOf(err); k != errs.SessionDead {
		t.Fatalf("应归一成 SessionDead：%v", err)
	}
	if !strings.Contains(err.Error(), "clientId") {
		t.Fatalf("错误里要说清缺什么、怎么修：%v", err)
	}
	if calls != 0 {
		t.Fatalf("不该拿企业版凭据去撞桌面端点（上游被打了 %d 次）", calls)
	}
}

// ---------------------------------------------------------------------------
// 7. 请求体归一（上游的硬约束）
// ---------------------------------------------------------------------------

func TestBuildPayloadToolRoundTrip(t *testing.T) {
	req := channel.ChatRequest{
		Model: "claude-sonnet-4.5",
		Messages: []channel.Message{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "北京天气"},
			{Role: "assistant", ToolCalls: []channel.ToolCall{{
				ID: "call_1", Type: "function",
				Function: channel.FunctionCall{Name: "get_weather", Arguments: `{"city":"北京"}`},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "晴"},
			{Role: "user", Content: "谢谢"},
		},
		Tools: []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name": "get_weather",
				"parameters": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{},
					"properties":           map[string]any{"city": map[string]any{"type": "string"}},
				},
			},
		}},
	}
	payload, err := buildPayload(req, "claude-sonnet-4.5", "arn:x")
	if err != nil {
		t.Fatalf("buildPayload 失败：%v", err)
	}
	if payload["profileArn"] != "arn:x" {
		t.Fatalf("profileArn 没带上：%+v", payload)
	}

	state := payload["conversationState"].(map[string]any)
	history := state["history"].([]map[string]any)
	// 工具结果那条 user 与最后的「谢谢」是相邻同角色，会被合并成一条 ——
	// 所以工具结果落在 **currentMessage** 上，history 只剩 user + assistant。
	if len(history) != 2 {
		t.Fatalf("history 应有 2 条（user/assistant）：%+v", history)
	}
	first := history[0]["userInputMessage"].(map[string]any)
	if !strings.HasPrefix(first["content"].(string), "你是助手\n\n") {
		t.Fatalf("system 提示词应拼到第一条 user 上：%q", first["content"])
	}
	asst := history[1]["assistantResponseMessage"].(map[string]any)
	uses := asst["toolUses"].([]map[string]any)
	if len(uses) != 1 || uses[0]["name"] != "get_weather" || uses[0]["toolUseId"] != "call_1" {
		t.Fatalf("assistant 的 toolUses 不对：%+v", asst)
	}

	cur := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if cur["content"] != "谢谢" {
		t.Fatalf("currentMessage 应是最后一条 user：%+v", cur)
	}
	ctx := cur["userInputMessageContext"].(map[string]any)
	results := ctx["toolResults"].([]map[string]any)
	if len(results) != 1 || results[0]["toolUseId"] != "call_1" {
		t.Fatalf("工具结果应挂在当前 user 消息的 toolResults 上：%+v", ctx)
	}
	tools := ctx["tools"].([]map[string]any)
	spec := tools[0]["toolSpecification"].(map[string]any)
	schema := spec["inputSchema"].(map[string]any)["json"].(map[string]any)
	if _, bad := schema["additionalProperties"]; bad {
		t.Fatalf("additionalProperties 必须洗掉（上游会 400）：%+v", schema)
	}
	if _, bad := schema["required"]; bad {
		t.Fatalf("空的 required 数组必须洗掉（上游会 400）：%+v", schema)
	}
	if spec["description"] != "Tool: get_weather" {
		t.Fatalf("空描述要补占位：%+v", spec)
	}
}

// 没有 tools 时，工具痕迹必须降级成文本（上游见到「有 toolResults 没有 tools」直接 400）。
func TestBuildPayloadStripsToolsWhenNoToolsDefined(t *testing.T) {
	req := channel.ChatRequest{
		Model: "claude-sonnet-4.5",
		Messages: []channel.Message{
			{Role: "user", Content: "hi"},
			{Role: "tool", ToolCallID: "call_9", Content: "结果"},
		},
	}
	payload, err := buildPayload(req, "claude-sonnet-4.5", "")
	if err != nil {
		t.Fatalf("buildPayload 失败：%v", err)
	}
	state := payload["conversationState"].(map[string]any)
	cur := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if _, has := cur["userInputMessageContext"]; has {
		t.Fatalf("没有 tools 就不该有 userInputMessageContext（会被上游拒）：%+v", cur)
	}
	if !strings.Contains(cur["content"].(string), "[Tool Result (call_9)]") {
		t.Fatalf("工具结果应降级成文本保住上下文：%q", cur["content"])
	}
	if _, has := state["history"]; has {
		t.Fatalf("两条消息合并成一条后不该有 history：%+v", state)
	}
}

// 首条必须是 user；最后一条是 assistant 时要塞进 history 并补一条 user。
func TestBuildPayloadEnsuresUserFirst(t *testing.T) {
	payload, err := buildPayload(channel.ChatRequest{
		Model: "claude-sonnet-4.5",
		Messages: []channel.Message{
			{Role: "assistant", Content: "我先说话"},
		},
	}, "claude-sonnet-4.5", "")
	if err != nil {
		t.Fatalf("buildPayload 失败：%v", err)
	}
	state := payload["conversationState"].(map[string]any)
	history := state["history"].([]map[string]any)
	if len(history) != 2 {
		t.Fatalf("history 应有 2 条（补的 user + 原来的 assistant）：%+v", history)
	}
	if got := history[0]["userInputMessage"].(map[string]any)["content"]; got != placeholder {
		t.Fatalf("首条应是占位 user：%q", got)
	}
	cur := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if cur["content"] != placeholder {
		t.Fatalf("currentMessage 必须补一条 user：%+v", cur)
	}
}

// 工具名超过 64 字符要在本地拦下（上游只会回一句没法定位的 400）。
func TestBuildPayloadRejectsLongToolName(t *testing.T) {
	long := strings.Repeat("x", maxToolNameLen+1)
	_, err := buildPayload(channel.ChatRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []channel.Message{{Role: "user", Content: "hi"}},
		Tools: []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": long, "parameters": map[string]any{"type": "object"}},
		}},
	}, "claude-sonnet-4.5", "")
	if err == nil {
		t.Fatal("超长工具名必须报错")
	}
	if k, _ := errs.KindOf(err); k != errs.Parse {
		t.Fatalf("应归一成 Parse：%v", err)
	}
	if !strings.Contains(err.Error(), "64") {
		t.Fatalf("错误里要说清上限：%v", err)
	}
}
