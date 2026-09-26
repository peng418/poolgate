package chatglm

// chatglm_test.go 覆盖四件最容易出错的事：
//  1. 时间戳变换与签名（错一位 = 所有请求都失败，且报文看不出原因）；
//  2. 消息拼装的角色标记（含上游那个 sytstem 拼写）；
//  3. parts 快照 → 增量的求差（写错会重复吐字）；
//  4. 业务码错误（HTTP 200 + code != 0）不能被当成功。

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

func TestTransformTimestamp(t *testing.T) {
	// 手算：1730000000000（13 位）→ 各位和 11，倒数第二位是 0，(11-0)%10 = 1 →
	// 把**倒数第二位**换成 1，位数保持 13 不变。
	if got := transformTimestamp(1730000000000); got != "1730000000010" {
		t.Fatalf("时间戳变换不对：%s（期望 1730000000010）", got)
	}
	if n := len(transformTimestamp(1730000000000)); n != 13 {
		t.Fatalf("变换后必须还是 13 位，得到 %d 位", n)
	}
}

func TestMakeSignMatchesAlgorithm(t *testing.T) {
	sign := makeSign("secret-under-test")
	if len(sign.timestamp) != 13 {
		t.Fatalf("时间戳应保持 13 位：%s", sign.timestamp)
	}
	if len(sign.nonce) != 32 {
		t.Fatalf("nonce 应是 32 位无横线 uuid：%s", sign.nonce)
	}
	// 版本位：参考实现用的是 uuid v1（util.ts:10 `import { v1 as uuid }`），第 13 位 hex 必须是 '1'。
	if sign.nonce[12] != '1' {
		t.Fatalf("nonce 应是 v1 uuid（第 13 位为 '1'），得到：%s", sign.nonce)
	}
	sum := md5.Sum([]byte(sign.timestamp + "-" + sign.nonce + "-secret-under-test"))
	if want := hex.EncodeToString(sum[:]); sign.sign != want {
		t.Fatalf("签名原文拼法不对：got %s want %s", sign.sign, want)
	}
}

func TestPackMessagesRoleMarks(t *testing.T) {
	got := packMessages([]channel.Message{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "在的"},
		{Role: "user", Content: "看图 ![图](https://x/y.png) 再 ![图2](https://x/z.png) 和 /mnt/data/tmp.txt"},
	})
	for _, want := range []string{"<|sytstem|>\n你是助手", "<|user|>\n你好", "<|assistant|>\n在的"} {
		if !strings.Contains(got, want) {
			t.Fatalf("角色标记缺失 %q：\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "<|assistant|>\n") {
		t.Fatalf("末尾必须补 assistant 锚点：\n%s", got)
	}
	if strings.Contains(got, "![") || strings.Contains(got, "/mnt/data/") {
		t.Fatalf("Markdown 图片与临时路径必须剥掉（否则模型会幻觉出并不存在的文件）：\n%s", got)
	}
}

func TestPackMessagesSingleMessagePassesThrough(t *testing.T) {
	// 只有一条消息时不加角色标记（参考实现的分支），但段末的换行照参考实现要带（chat.ts:827-835）。
	if got := packMessages([]channel.Message{{Role: "user", Content: "就一句"}}); got != "就一句\n" {
		t.Fatalf("单条消息应原样透传（段末带换行），得到：%q", got)
	}
}

func TestStreamStateDiff(t *testing.T) {
	st := newStreamState()
	frame := func(raw string) (string, string) {
		var ev map[string]any
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("测试帧不是 JSON：%v", err)
		}
		return st.feed(ev)
	}

	// 第一帧：正文 + 思考（快照）
	txt, rsn := frame(`{"status":"streaming","parts":[{"logic_id":"p1","status":"wip","content":[
		{"type":"text","text":"你好"},{"type":"think","think":"想一下"}]}]}`)
	if txt != "你好" || rsn != "想一下" {
		t.Fatalf("首帧渲染不对：text=%q reasoning=%q", txt, rsn)
	}

	// 第二帧：同一个 part 内容变长 → 只应给出**新增**的那一段（不能重发全文）
	txt, rsn = frame(`{"status":"streaming","parts":[{"logic_id":"p1","status":"wip","content":[
		{"type":"text","text":"你好世界"},{"type":"think","think":"想一下再想"}]}]}`)
	if txt != "世界" {
		t.Fatalf("正文增量应是「世界」，得到 %q（重发全文就是重复吐字的 bug）", txt)
	}
	if rsn != "再想" {
		t.Fatalf("思考增量应是「再想」，得到 %q", rsn)
	}
}

func TestStreamStateRewritesSearchRefs(t *testing.T) {
	st := newStreamState()
	var ev map[string]any
	_ = json.Unmarshal([]byte(`{"parts":[{"logic_id":"p1","status":"wip","content":[
		{"type":"tool_result","meta_data":{"tool_result_extra":{"search_results":[
			{"match_key":"turn1search1","title":"标题","url":"https://example.com/a"}]}}},
		{"type":"text","text":"参考【turn1search1】"}]}]}`), &ev)
	txt, rsn := st.feed(ev)
	if !strings.Contains(txt, "[1](https://example.com/a)") {
		t.Fatalf("检索引用应换成编号链接：%q", txt)
	}
	if !strings.Contains(rsn, "> 检索 标题(https://example.com/a)") {
		t.Fatalf("检索结果应进思考流：%q", rsn)
	}
}

func TestChatEndToEnd(t *testing.T) {
	var chatCalls, refreshCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case epRefresh:
			refreshCalls++
			if got := r.Header.Get("Authorization"); got != "Bearer refresh-tok" {
				t.Errorf("换令牌应带 Bearer refresh token，得到 %q", got)
			}
			assertSigned(t, r)
			_, _ = w.Write([]byte(`{"code":0,"result":{"access_token":"access-tok","refresh_token":"refresh-tok"}}`))

		case epStream:
			chatCalls++
			if got := r.Header.Get("Authorization"); got != "Bearer access-tok" {
				t.Errorf("对话应带 Bearer access token，得到 %q（refresh 链路没接上）", got)
			}
			assertSigned(t, r)
			if got := r.Header.Get("X-Device-Id"); len(got) != 32 {
				t.Errorf("每请求都要新的设备 id（32 位）：%q", got)
			}
			// 对话请求要带网页端形态的 Referer（chat.ts:424-428），默认智能体走 alltoolsdetail。
			if got := r.Header.Get("Referer"); got != "https://chatglm.cn/main/alltoolsdetail" {
				t.Errorf("对话请求的 Referer 不对：%q", got)
			}
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				AssistantID string `json:"assistant_id"`
				ChatType    string `json:"chat_type"`
				Messages    []struct {
					Role    string `json:"role"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"messages"`
				Meta struct {
					ChatMode     string `json:"chat_mode"`
					IsNetworking bool   `json:"is_networking"`
				} `json:"meta_data"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("请求体不是 JSON：%v", err)
			}
			if body.AssistantID != defaultAssistantID {
				t.Errorf("assistant_id 应为默认智能体：%q", body.AssistantID)
			}
			if body.ChatType != "user_chat" || len(body.Messages) != 1 {
				t.Errorf("请求体形态不对：chat_type=%q messages=%d", body.ChatType, len(body.Messages))
			}
			// -think 后缀 → chat_mode=zero（推理模式）
			if body.Meta.ChatMode != "zero" {
				t.Errorf("-think 档应把 chat_mode 设成 zero，得到 %q", body.Meta.ChatMode)
			}
			// 单条消息不加角色标记（只有多轮才需要区分谁说的），段末带换行（chat.ts:827-835）
			if body.Messages[0].Content[0].Text != "你好\n" {
				t.Errorf("历史拼接不对：%q", body.Messages[0].Content[0].Text)
			}

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			for _, frame := range []string{
				`{"status":"streaming","parts":[{"logic_id":"p1","status":"wip","content":[{"type":"text","text":"你好"}]}]}`,
				`{"status":"streaming","parts":[{"logic_id":"p2","status":"wip","content":[{"type":"think","think":"想一下"}]}]}`,
				`{"status":"finish","conversation_id":"c1"}`,
			} {
				_, _ = w.Write([]byte("data: " + frame + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}

		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "chatglm-test", RefreshToken: "refresh-tok"}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model:    "glm-4.7-think",
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
	if content.String() != "你好" {
		t.Fatalf("正文不对：%q", content.String())
	}
	if reasoning.String() != "想一下" {
		t.Fatalf("思考分流不对：%q", reasoning.String())
	}
	if finish != "stop" {
		t.Fatalf("结束帧不对：%q", finish)
	}
	if chatCalls != 1 || refreshCalls != 1 {
		t.Fatalf("调用次数不对：chat=%d refresh=%d", chatCalls, refreshCalls)
	}
}

// 默认档位（非 think / 非 deepresearch）的请求体里**不能出现 chat_mode 键**（空串也不行）：
// 参考实现是 `chat_mode: chatMode || undefined`，空串会被 JSON.stringify 整个删掉（chat.ts:285）。
func TestChatOmitsChatModeOnDefaultSpec(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == epRefresh {
			_, _ = w.Write([]byte(`{"code":0,"result":{"access_token":"access-tok"}}`))
			return
		}
		rawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"status":"finish","conversation_id":"c1"}` + "\n\n"))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "chatglm-test", RefreshToken: "refresh-tok"}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "glm-4.6", Messages: []channel.Message{{Role: "user", Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	if strings.Contains(string(rawBody), "chat_mode") {
		t.Fatalf("默认档位不应带 chat_mode 键：%s", rawBody)
	}
}

// 上游轮换 refresh token 时，新值必须写回凭证。
// 网页端是靠 Set-Cookie 覆盖 chatglm_refresh_token 持久化新值的；参考实现 chat.ts:143-148
// 解构了 refresh_token 却没用它 —— 我们不留这个缺陷（旧值继续用迟早失效）。
func TestRefreshPersistsRotatedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == epRefresh {
			_, _ = w.Write([]byte(`{"code":0,"result":{"access_token":"access-new","refresh_token":"refresh-new"}}`))
			return
		}
		t.Errorf("未预期的路径：%s", r.URL.Path)
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "chatglm-test", RefreshToken: "refresh-old"}
	nc, err := a.Refresh(context.Background(), cred)
	if err != nil {
		t.Fatalf("Refresh 失败：%v", err)
	}
	if nc == nil || nc.RefreshToken != "refresh-new" || nc.AccessToken != "access-new" {
		t.Fatalf("轮换后的 refresh token 没写回：%+v", nc)
	}
}

// assertSigned 校验签名三件套齐全且时间戳是 13 位。
func assertSigned(t *testing.T, r *http.Request) {
	t.Helper()
	if len(r.Header.Get("X-Sign")) != 32 {
		t.Errorf("缺 X-Sign（应是 32 位 md5）：%q", r.Header.Get("X-Sign"))
	}
	if len(r.Header.Get("X-Timestamp")) != 13 {
		t.Errorf("X-Timestamp 应是 13 位：%q", r.Header.Get("X-Timestamp"))
	}
	if len(r.Header.Get("X-Nonce")) != 32 {
		t.Errorf("缺 X-Nonce：%q", r.Header.Get("X-Nonce"))
	}
	if r.Header.Get("App-Name") != "chatglm" {
		t.Errorf("缺伪装头 App-Name：%q", r.Header.Get("App-Name"))
	}
}

// 业务码错误（HTTP 200 + code）不能被当成功 —— 尤其 40102（refresh_token 过期）。
func TestBusinessErrorIsSessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == epRefresh {
			_, _ = w.Write([]byte(`{"code":0,"result":{"access_token":"access-tok"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"code":40102,"message":"refresh_token已过期"}` + "\n\n"))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "chatglm-test", RefreshToken: "refresh-tok"}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "glm-4.7", Messages: []channel.Message{{Role: "user", Content: "hi"}},
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
	if k, ok := errs.KindOf(got); !ok || k != errs.SessionDead {
		t.Fatalf("40102 应归一成 SessionDead，得到 %v（%v）", k, got)
	}
}

// 结束帧是 intervene 时，上游的拦截说明要**照实转达**（不假装正常结束）。
func TestInterveneCarriesUpstreamText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == epRefresh {
			_, _ = w.Write([]byte(`{"code":0,"result":{"access_token":"access-tok"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"status":"intervene","last_error":{"intervene_text":"内容不合规"}}` + "\n\n"))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "chatglm-test", RefreshToken: "refresh-tok"}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "glm-4.7", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	var content strings.Builder
	for {
		c, err := st.Next()
		if err != nil {
			break
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
		}
	}
	if !strings.Contains(content.String(), "内容不合规") {
		t.Fatalf("拦截说明必须带给客户端：%q", content.String())
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
		{403, `{}`, errs.SessionDead},
		{429, `{"message":"too many requests"}`, errs.SoftRate},
		{429, `{"message":"quota exhausted"}`, errs.HardCredit},
		{400, `{"message":"40102 refresh_token已过期"}`, errs.SessionDead},
		{400, `{"message":"内容不合规"}`, errs.ContentBlocked},
		{404, `{}`, errs.ModelUnavailable},
		{502, `{}`, errs.UpstreamFault},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("status=%d body=%s：期望 %v 得到 %v", c.status, c.body, c.want, got)
		}
	}
}
