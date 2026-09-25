package yuanbao

// yuanbao_test.go 覆盖四件最容易出错的事：
//  1. 凭证解析（用户会粘整段请求头、JSON 或裸 uskey —— 都要认）；
//  2. 请求体形态（两份参考实现的共识，字段名一个都不能错）；
//  3. 帧分流（think 的字段是 content、text 的字段是 msg —— 取错字段不报错但结果全错）；
//  4. 非 message 事件不能当内容（上游会在同一路流里混入其它事件）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

func TestParseCredForms(t *testing.T) {
	// 1) 整段请求头
	headers := "accept: text/event-stream\nx-uskey: uskey-abcdef1234567890\ncookie: hy_user=abc; hy_token=	def\nuser-agent: Mozilla/5.0 TestUA"
	c, err := parseCred(headers)
	if err != nil {
		t.Fatalf("整段请求头应能解析：%v", err)
	}
	if c.uskey != "uskey-abcdef1234567890" {
		t.Fatalf("x-uskey 解析不对：%q", c.uskey)
	}
	if !strings.HasPrefix(c.cookie, "hy_user=abc") {
		t.Fatalf("cookie 解析不对：%q", c.cookie)
	}
	if c.ua != "Mozilla/5.0 TestUA" {
		t.Fatalf("UA 解析不对：%q", c.ua)
	}

	// 2) JSON
	c2, err := parseCred(`{"uskey":"u-json-123","cookie":"a=b"}`)
	if err != nil || c2.uskey != "u-json-123" {
		t.Fatalf("JSON 形态解析失败：%v %+v", err, c2)
	}

	// 3) 裸 uskey
	c3, err := parseCred("just-a-bare-uskey-value")
	if err != nil || c3.uskey != "just-a-bare-uskey-value" {
		t.Fatalf("裸串形态解析失败：%v %+v", err, c3)
	}

	// 4) 空 / 无 uskey 必须报错
	if _, err := parseCred("   "); err == nil {
		t.Fatal("空内容必须报错")
	}
	if _, err := parseCred("accept: text/event-stream\ncookie: a=b"); err == nil {
		t.Fatal("没有 x-uskey 时必须报错，而不是带着空凭证继续")
	}
}

func TestBuildBodyMatchesConsensus(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal(buildBody("你好", "deep_seek_v3", false), &body); err != nil {
		t.Fatalf("请求体不是 JSON：%v", err)
	}
	for k, want := range map[string]any{
		"model": "gpt_175B_0404", "plugin": "Adaptive", "displayPromptType": float64(1),
		"agentId": agentID, "supportHint": float64(1), "version": "v2", "chatModelId": "deep_seek_v3",
		"prompt": "你好", "displayPrompt": "你好",
	} {
		if got := body[k]; got != want {
			t.Errorf("请求体 %s 应为 %v，得到 %v", k, want, got)
		}
	}
	if _, ok := body["multimedia"]; !ok {
		t.Error("multimedia 字段必须存在（上游 SDK 如此）")
	}
	if _, ok := body["supportFunctions"]; ok {
		t.Error("没开联网时不应带 supportFunctions")
	}
	if err := json.Unmarshal(buildBody("hi", "deep_seek_v3", true), &body); err != nil {
		t.Fatalf("开启联网的请求体不是 JSON：%v", err)
	}
	if _, ok := body["supportFunctions"]; !ok {
		t.Error("开启联网时必须带 supportFunctions")
	}
}

func TestChatEndToEndFrameRouting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == epCreate:
			if got := r.Header.Get("x-uskey"); got != "u-1" {
				t.Errorf("建会话也要带 x-uskey，得到 %q", got)
			}
			_, _ = w.Write([]byte(`{"id":"conv-1"}`))

		case strings.HasPrefix(r.URL.Path, epChatPrefix):
			if !strings.HasSuffix(r.URL.Path, "conv-1") {
				t.Errorf("对话路径应带上会话 id：%s", r.URL.Path)
			}
			if got := r.Header.Get("x-uskey"); got != "u-1" {
				t.Errorf("对话缺 x-uskey：%q", got)
			}
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Prompt      string `json:"prompt"`
				ChatModelID string `json:"chatModelId"`
			}
			_ = json.Unmarshal(raw, &body)
			if body.ChatModelID != "deep_seek_v3" {
				t.Errorf("chatModelId 传错了：%q", body.ChatModelID)
			}
			if !strings.Contains(body.Prompt, "user:你好") {
				t.Errorf("prompt 拼接不对：%q", body.Prompt)
			}

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			for _, frame := range []string{
				"event: heartbeat\ndata: {\"type\":\"text\",\"msg\":\"不该出现\"}\n\n",
				"event: message\ndata: {\"type\":\"think\",\"content\":\"想一下\"}\n\n",
				"event: message\ndata: {\"type\":\"text\",\"msg\":\"你好\"}\n\n",
				"event: message\ndata: {\"type\":\"text\",\"msg\":\"世界\"}\n\n",
				"event: message\ndata: {\"stopReason\":\"stop\"}\n\n",
			} {
				_, _ = w.Write([]byte(frame))
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
	cred := &channel.Credential{UID: "yuanbao-test", AccessToken: "x-uskey: u-1"}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "deepseek-v3", Messages: []channel.Message{{Role: "user", Content: "你好"}},
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
		t.Fatalf("正文不对：%q（把非 message 事件当内容、或取错字段都会在这里暴露）", content.String())
	}
	if reasoning.String() != "想一下" {
		t.Fatalf("思考不对：%q（think 的字段是 content）", reasoning.String())
	}
	if finish != "stop" {
		t.Fatalf("结束帧不对：%q", finish)
	}
}

func TestCreateConversationWithoutIDIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`)) // 没有 id
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "yuanbao-test", AccessToken: "x-uskey: u-1"}
	_, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "deepseek-v3", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("拿不到会话 id 时必须报错，而不是带着空 id 继续请求")
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
		{429, `{"message":"too many"}`, errs.SoftRate},
		{429, `{"message":"quota exhausted"}`, errs.HardCredit},
		{400, `{"message":"内容违规"}`, errs.ContentBlocked},
		{404, `{}`, errs.ModelUnavailable},
		{500, `{}`, errs.UpstreamFault},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("status=%d body=%s：期望 %v 得到 %v", c.status, c.body, c.want, got)
		}
	}
}
