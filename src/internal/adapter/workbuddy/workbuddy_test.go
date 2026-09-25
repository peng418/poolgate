package workbuddy

import (
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
)

func drain(t *testing.T, s channel.Stream) ([]channel.ChatCompletionChunk, error) {
	t.Helper()
	var out []channel.ChatCompletionChunk
	for {
		chunk, err := s.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, chunk)
	}
}

// WorkBuddy 是 OpenAI 兼容 SSE：正常帧透传，[DONE] 结束，usage 帧（无 choices）不能丢。
func TestStreamPassesThroughOpenAISSE(t *testing.T) {
	body := io.NopCloser(strings.NewReader(strings.Join([]string{
		"event: ping\n",
		"\n",
		": 这是注释行\n",
		"\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n",
		"\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n",
		"\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"推理\"}}]}\n",
		"\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n",
		"\n",
		"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n",
		"\n",
		"data: [DONE]\n",
		"\n",
	}, "")))
	s := newStream(body, "glm-5.2")
	defer s.Close()

	chunks, err := drain(t, s)
	if err != nil {
		t.Fatalf("正常 SSE 不应报错: %v", err)
	}
	if len(chunks) != 5 {
		t.Fatalf("期望 5 个 chunk（角色/内容/推理/停止/usage），实际 %d 个: %+v", len(chunks), chunks)
	}
	if chunks[1].Choices[0].Delta.Content != "你好" {
		t.Fatalf("内容未透传: %+v", chunks[1])
	}
	if chunks[2].Choices[0].Delta.ReasoningContent != "推理" {
		t.Fatalf("推理内容未透传: %+v", chunks[2])
	}
	if chunks[3].Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason 未透传: %+v", chunks[3])
	}
	if chunks[4].Usage == nil || chunks[4].Usage["completion_tokens"] == nil {
		t.Fatalf("usage 帧必须交给客户端: %+v", chunks[4])
	}
	for i, c := range chunks {
		if c.Model != "glm-5.2" {
			t.Fatalf("第 %d 个 chunk 应回填请求模型，实际 %q", i, c.Model)
		}
	}
}

// 拿不到内容、也没有 id/usage 的帧直接丢，避免产出空 chunk 占满客户端。
func TestToChunkDropsEmptyFrames(t *testing.T) {
	if _, ok := toChunk(map[string]any{"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{}}}}, "m"); ok {
		t.Fatal("空 delta 且无 id/usage 的帧应被丢弃")
	}
	if _, ok := toChunk(map[string]any{"id": "c1", "choices": []any{}}, "m"); !ok {
		t.Fatal("带 id 的帧应保留")
	}
	if _, ok := toChunk(map[string]any{"usage": map[string]any{"total_tokens": float64(3)}}, "m"); !ok {
		t.Fatal("usage 帧应保留")
	}
}

// 红线二的关键形态：上游 200 + 信封错误（非 SSE），适配器给不出任何 chunk。
// 这种情况必须被网关判成失败 —— 这里钉住「适配器确实零 chunk」，别让它假装有内容。
func TestChat200ErrorEnvelopeYieldsNoChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"code":11217,"msg":"login ing...","data":null}`)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, err := a.Chat(context.Background(), &channel.Credential{UID: "u1", AccessToken: "tok"}, channel.ChatRequest{Model: "glm-5.2"})
	if err != nil {
		t.Fatalf("HTTP 200 时 Chat 不报错（判失败在网关层），实际 %v", err)
	}
	defer s.Close()
	chunks, err := drain(t, s)
	if err != nil {
		t.Fatalf("信封体不是合法 SSE，应安静地零 chunk 结束: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("信封错误不应产出任何 chunk，实际 %d 个", len(chunks))
	}
}

// 上游 HTTP 错误归一成有限枚举（D4）。
func TestChatClassifiesUpstreamErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{"msg":"unauthorized"}`, errs.SessionDead},
		{429, `{"msg":"too many requests"}`, errs.SoftRate},
		{429, `{"msg":"Credits exhausted"}`, errs.HardCredit},
		// 实测：WorkBuddyCN 账号额度耗尽时上游回 HTTP 429 +
		// {"error":{"data":{"code":14018,"msg":"额度已用尽，请…购买加量包…"}}}
		// 这必须判成 HardCredit（冷却 12h、不换号重试），不是 SoftRate（60s 后继续试）。
		{429, `{"error":{"data":{"code":14018,"msg":"额度已用尽，请访问以下链接，购买加量包以获取更多额度："}}}`, errs.HardCredit},
		{402, `{"msg":"余额不足"}`, errs.HardCredit},
		{500, `{"msg":"boom"}`, errs.UpstreamFault},
		{404, `{"msg":"not found"}`, errs.UpstreamFault},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()

			a := NewWithBase(srv.URL, srv.Client())
			_, err := a.Chat(context.Background(), &channel.Credential{UID: "u1", AccessToken: "tok"}, channel.ChatRequest{Model: "glm-5.2"})
			if err == nil {
				t.Fatal("上游错误必须返回错误，不能静默")
			}
			k, ok := errs.KindOf(err)
			if !ok {
				t.Fatalf("必须是结构化错误，实际 %T", err)
			}
			if k != c.want {
				t.Fatalf("期望 %s，实际 %s（%v）", c.want, k, err)
			}
			ee, _ := err.(*errs.Error)
			if ee.Upstream == "" || ee.Account != "u1" {
				t.Fatalf("错误应带上游原话与账号：%+v", ee)
			}
		})
	}
}

// 请求体：developer 改写成 system，缺 system 时补一条，且强制 stream + 带 usage。
func TestBuildBodyShape(t *testing.T) {
	raw := buildBody(channel.ChatRequest{
		Model:     "glm-5.2",
		MaxTokens: 128,
		Messages:  []channel.Message{{Role: "developer", Content: "d"}, {Role: "user", Content: "u"}},
	})
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	if obj["stream"] != true {
		t.Fatal("必须强制 stream=true")
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("developer 应改写成 system 且不额外插系统提示: %#v", msgs)
	}

	// 没有 system 时补一条默认系统提示（上游要求首条必须是 system）。
	raw2 := buildBody(channel.ChatRequest{Model: "glm-5.2", Messages: []channel.Message{{Role: "user", Content: "u"}}})
	var obj2 map[string]any
	_ = json.Unmarshal(raw2, &obj2)
	msgs2 := obj2["messages"].([]any)
	if len(msgs2) != 2 || msgs2[0].(map[string]any)["role"] != "system" {
		t.Fatalf("缺 system 时应补一条: %#v", msgs2)
	}
}

// 刷新：拿 X-Refresh-Token 走专用头，成功后轮换 dt/rt；响应无 token → SessionDead。
func TestRefreshRotationAndFailure(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Refresh-Token")
		if gotHeader == "bad" {
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":""}}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	nc, err := a.Refresh(context.Background(), &channel.Credential{UID: "u1", RefreshToken: "good"})
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if gotHeader != "good" {
		t.Fatalf("refresh token 应放在 X-Refresh-Token 头，实际 %q", gotHeader)
	}
	if nc.AccessToken != "new-at" || nc.RefreshToken != "new-rt" {
		t.Fatalf("刷新后凭证未轮换: %+v", nc)
	}
	if d := time.Until(nc.ExpiresAt); d < 50*time.Minute || d > 70*time.Minute {
		t.Fatalf("expiresIn=3600 应换算成约 1 小时后的过期时间，实际 %v", nc.ExpiresAt)
	}

	// 响应没有 accessToken：判需重新授权，不能拿旧 token 继续用。
	if _, err := a.Refresh(context.Background(), &channel.Credential{UID: "u1", RefreshToken: "bad"}); err == nil {
		t.Fatal("刷新响应无 accessToken 必须报错")
	} else if k, _ := errs.KindOf(err); k != errs.SessionDead {
		t.Fatalf("期望 SessionDead，实际 %s", k)
	}

	// 缺 refresh token：不发请求。
	if _, err := a.Refresh(context.Background(), &channel.Credential{UID: "u1"}); err == nil {
		t.Fatal("缺 refresh token 必须报错")
	}
}

// 余额：信封剥壳 + 周期额度优先、否则用总量；无账号时总额为 0 且 Known=true。
func TestBalanceSumsAccounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"Response":{"Data":{"Accounts":[
			{"CycleCapacitySize":100,"CycleCapacityRemain":40,"CapacitySize":1000,"CapacityRemain":900},
			{"CapacitySize":50,"CapacityRemain":25}
		]}}}}`)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	b, err := a.Balance(context.Background(), &channel.Credential{UID: "u1", AccessToken: "tok"})
	if err != nil {
		t.Fatalf("余额查询失败: %v", err)
	}
	if !b.Known {
		t.Fatal("解析成功时 Known 必须为 true")
	}
	if b.Credits != 65 {
		t.Fatalf("期望 40（周期剩余）+ 25（总量剩余）= 65，实际 %d", b.Credits)
	}

	// 上游报错：必须带出错误，不能返回 0 余额冒充「余额用尽」。
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = io.WriteString(w, `gateway error`)
	}))
	defer errSrv.Close()
	a2 := NewWithBase(errSrv.URL, errSrv.Client())
	if _, err := a2.Balance(context.Background(), &channel.Credential{UID: "u1"}); err == nil {
		t.Fatal("上游报错时必须返回错误，不能返回 0 余额")
	}
}

// 静态模型表是 WorkBuddyCN 的唯一目录来源：非空、无重复、且渠道是 Active（要下发）。
func TestStaticModelsSane(t *testing.T) {
	ms := New().ModelsSnapshot()
	if len(ms) == 0 {
		t.Fatal("静态模型表不能为空（国内版没有动态目录端点）")
	}
	seen := map[string]bool{}
	for _, m := range ms {
		if m.ID == "" {
			t.Fatal("模型 ID 不能为空")
		}
		if seen[m.ID] {
			t.Fatalf("模型 ID 重复: %s", m.ID)
		}
		seen[m.ID] = true
	}
	spec := Spec()
	if !spec.Downstream() {
		t.Fatal("WorkBuddyCN 是首发渠道，必须 Downstream")
	}
	if !spec.SSEOnly {
		t.Fatal("WorkBuddyCN 上游只有流式，SSEOnly 必须为 true")
	}
}
