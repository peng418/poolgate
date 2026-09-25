package doubao

// doubao_test.go 覆盖四件最容易出错的事：
//  1. Cookie 串的解析（要粘的是整行 Cookie，不是单个值）；
//  2. 安全参数与请求体形态（少一个参数上游就判非官方客户端）；
//  3. 思考开关（10040 一次进、二次出 —— 写错的表现是思考混进正文）；
//  4. 风控码要报成「该去做什么」，不能当成普通上游故障。

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

func TestParseCredCookieString(t *testing.T) {
	raw := "ttwid=1%7Cabc; sessionid=deadbeef0123456789; passport_csrf_token=tok123; " +
		"s_v_web_id=verify_mlcfw5f7_TPq0YmFD; other=1"
	c, err := parseCred(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if c.sessionid != "deadbeef0123456789" {
		t.Fatalf("sessionid 解析不对：%q", c.sessionid)
	}
	if c.csrf != "tok123" {
		t.Fatalf("csrf 解析不对：%q", c.csrf)
	}
	if c.fp != "verify_mlcfw5f7_TPq0YmFD" {
		t.Fatalf("fp 应取 cookie s_v_web_id：%q", c.fp)
	}
	if len(c.deviceID) != 15 || len(c.webID) != 19 {
		t.Fatalf("派生的设备号位数不对：device=%q web=%q", c.deviceID, c.webID)
	}
	// 派生必须稳定：同一个账号每次解析出来必须是同一个设备号（否则风控看起来像换了设备）。
	c2, _ := parseCred(raw)
	if c2.deviceID != c.deviceID || c2.webID != c.webID {
		t.Fatal("派生设备号不稳定：同一凭证两次解析结果不同")
	}
	// 不同账号必须不同
	c3, _ := parseCred("sessionid=anothersessionvalue123")
	if c3.deviceID == c.deviceID {
		t.Fatal("不同账号派生出了相同设备号（会被看成同一个人）")
	}
}

func TestParseCredJSONOverride(t *testing.T) {
	c, err := parseCred(`{"cookie":"sessionid=abc123456789012345","device_id":"714003710229497","web_id":"7604137868021548590","fp":"verify_x"}`)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if c.deviceID != "714003710229497" || c.webID != "7604137868021548590" {
		t.Fatalf("用户显式给的设备参数应优先：%+v", c)
	}
}

func TestParseCredRequiresSessionid(t *testing.T) {
	if _, err := parseCred("ttwid=1; other=2"); err == nil {
		t.Fatal("没有 sessionid 时必须报错，而不是带着空凭证继续")
	}
}

func TestQueryParamsAndBody(t *testing.T) {
	c, _ := parseCred("sessionid=abc123456789012345")
	c.msToken = "" // 空：不该出现在 query 里
	q := queryParams(c, "tab-1")
	if strings.Contains(q, "msToken") {
		t.Fatalf("msToken 为空时不能出现在 query 里（假的会触发 710022002）：%s", q)
	}
	for _, want := range []string{"aid=582478", "device_platform=web", "samantha_web=1", "web_tab_id=tab-1"} {
		if !strings.Contains(q, want) {
			t.Fatalf("query 缺参数 %q：%s", want, q)
		}
	}
	c.msToken = "real-token"
	if q2 := queryParams(c, "tab-1"); !strings.Contains(q2, "msToken=real-token") {
		t.Fatalf("有真 msToken 时应带上：%s", q2)
	}

	var body struct {
		ClientMeta struct {
			BotID string `json:"bot_id"`
		} `json:"client_meta"`
		Messages []struct {
			ContentBlock []struct {
				BlockType int `json:"block_type"`
				Content   struct {
					TextBlock struct {
						Text string `json:"text"`
					} `json:"text_block"`
				} `json:"content"`
			} `json:"content_block"`
		} `json:"messages"`
		Option struct {
			NeedDeepThink    int  `json:"need_deep_think"`
			SupportChunkDone bool `json:"-"`
		} `json:"option"`
		Ext struct {
			UseDeepThink string `json:"use_deep_think"`
			FP           string `json:"fp"`
		} `json:"ext"`
	}
	if err := json.Unmarshal(buildBody("你好", 1, c), &body); err != nil {
		t.Fatalf("请求体不是 JSON：%v", err)
	}
	if body.ClientMeta.BotID != botID {
		t.Errorf("bot_id 不对：%q", body.ClientMeta.BotID)
	}
	if body.Option.NeedDeepThink != 1 || body.Ext.UseDeepThink != "1" {
		t.Errorf("深度思考档位没传对：option=%d ext=%q", body.Option.NeedDeepThink, body.Ext.UseDeepThink)
	}
	if len(body.Messages) != 1 || body.Messages[0].ContentBlock[0].BlockType != 10000 {
		t.Fatal("消息形态不对：必须是单条消息 + block_type 10000 的文本块")
	}
	if got := body.Messages[0].ContentBlock[0].Content.TextBlock.Text; got != "你好" {
		t.Fatalf("正文没传对：%q", got)
	}
	if body.Ext.FP != c.fp {
		t.Errorf("ext.fp 必须与 query 的 fp 一致：%q vs %q", body.Ext.FP, c.fp)
	}
}

func TestChatEndToEndThinkingToggle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 校验请求形态：路径、方法、关键头
		if r.URL.Path != epCompletion {
			t.Errorf("路径不对：%s", r.URL.Path)
		}
		if !strings.Contains(r.URL.RawQuery, "device_id=") || !strings.Contains(r.URL.RawQuery, "fp=") {
			t.Errorf("query 缺安全参数：%s", r.URL.RawQuery)
		}
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "sessionid=abc") {
			t.Errorf("Cookie 没带上：%q", got)
		}
		if got := r.Header.Get("x-tt-passport-csrf-token"); got != "tok123" {
			t.Errorf("csrf 头不对：%q", got)
		}
		_, _ = io.Copy(io.Discard, r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Join([]string{
			"event: SSE_ACK\ndata: {\"ack_client_meta\":{\"conversation_id\":\"c1\"}}\n\n",
			"event: CHUNK_DELTA\ndata: {\"text\":\"正文1\"}\n\n",
			"event: CHUNK_DELTA\ndata: {\"content_block\":[{\"block_type\":10040,\"content\":{}}]}\n\n",
			"event: CHUNK_DELTA\ndata: {\"text\":\"思考1\"}\n\n",
			"event: CHUNK_DELTA\ndata: {\"content_block\":[{\"block_type\":10040,\"content\":{}}]}\n\n",
			"event: CHUNK_DELTA\ndata: {\"text\":\"正文2\"}\n\n",
			"event: done\ndata: {}\n\n",
		}, "")))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "doubao-test", AccessToken: "sessionid=abc123456789012345; passport_csrf_token=tok123"}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "doubao-think", Messages: []channel.Message{{Role: "user", Content: "你好"}},
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
	if content.String() != "正文1正文2" {
		t.Fatalf("正文不对：%q（思考混进正文就是开关块判错了）", content.String())
	}
	if reasoning.String() != "思考1" {
		t.Fatalf("思考不对：%q", reasoning.String())
	}
	if finish != "stop" {
		t.Fatalf("结束帧不对：%q", finish)
	}
}

func TestRiskCodeMapsToSessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: STREAM_ERROR\ndata: {\"error_code\":710022004,\"error_msg\":\"verify\"}\n\n"))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "doubao-test", AccessToken: "sessionid=abc123456789012345"}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "doubao", Messages: []channel.Message{{Role: "user", Content: "hi"}},
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
		t.Fatalf("风控码应归一成 SessionDead（提示用户去过验证码），得到 %v（%v）", k, got)
	}
	if !strings.Contains(got.Error(), "验证码") {
		t.Fatalf("错误里要写清楚该做什么：%v", got)
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
		{502, `{}`, errs.UpstreamFault},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("status=%d body=%s：期望 %v 得到 %v", c.status, c.body, c.want, got)
		}
	}
}

func TestPackMessagesMarksTools(t *testing.T) {
	got := packMessages([]channel.Message{
		{Role: "system", Content: "你是助手"},
		{Role: "assistant", ToolCalls: []channel.ToolCall{{
			ID: "call_1", Type: "function",
			Function: channel.FunctionCall{Name: "get_weather", Arguments: `{"city":"北京"}`},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: "晴"},
	})
	for _, want := range []string{"system:你是助手", "[call:get_weather]", `{"city":"北京"}`, "[TOOL_RESULT for call_1] 晴"} {
		if !strings.Contains(got, want) {
			t.Fatalf("拼装缺 %q：%s", want, got)
		}
	}
}
