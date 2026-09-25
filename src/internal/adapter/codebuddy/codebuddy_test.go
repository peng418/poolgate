package codebuddy

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

// Spec 能力位必须说实话：原生工具调用（Tools=true、ToolsShim=false），上游只接受流式。
func TestSpecIsHonest(t *testing.T) {
	sp := Spec()
	if sp.Kind != channel.CodeBuddy {
		t.Fatalf("Kind 应为 codebuddy，实际 %q", sp.Kind)
	}
	if !sp.Tools || sp.ToolsShim {
		t.Fatalf("上游原生支持 tools → Tools=true 且 ToolsShim=false，实际 Tools=%v ToolsShim=%v", sp.Tools, sp.ToolsShim)
	}
	if !sp.SSEOnly {
		t.Fatal("后端只接受流式 → SSEOnly 必须为 true")
	}
	if !sp.Downstream() {
		t.Fatal("CodeBuddy 应为 Active（下发模型）")
	}
	if sp.Category != channel.CategoryCoding {
		t.Fatalf("编程助手应归 coding 类，实际 %q", sp.Category)
	}
}

// 对话头的默认档是 CodeBuddy 极简身份：X-Domain=www.codebuddy.cn，且**不带**机器码/IDE 头；
// 请求体强制 stream=true。
func TestChatHeadersCodeBuddyIdentity(t *testing.T) {
	var hdr http.Header
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	s, err := a.Chat(context.Background(), &channel.Credential{
		UID: "u1", AccessToken: "tok", Extra: map[string]string{"enterprise_id": "ent-9"},
	}, channel.ChatRequest{Model: "glm-5.2", Messages: []channel.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}
	defer s.Close()

	checks := map[string]string{
		"X-Domain":        DomainCB,
		"Authorization":   "Bearer tok",
		"X-User-Id":       "u1",
		"X-Enterprise-Id": "ent-9",
		"X-Tenant-Id":     "ent-9",
		"User-Agent":      clientUA,
	}
	for k, want := range checks {
		if got := hdr.Get(k); got != want {
			t.Errorf("头 %s 应为 %q，实际 %q", k, want, got)
		}
	}
	// 极简档的「极简」：不得出现 WorkBuddy 那套设备指纹/IDE 头。
	for _, k := range []string{"X-IDE-Type", "X-IDE-Name", "X-Machine-ID", "X-Session-ID", "X-Product"} {
		if got := hdr.Get(k); got != "" {
			t.Errorf("CodeBuddy 极简档不应带 %s，实际 %q", k, got)
		}
	}
	if body["stream"] != true {
		t.Fatal("后端只接受流式，请求体必须 stream=true")
	}
	if _, ok := body["tools"]; ok {
		t.Fatal("客户端没要工具时不应带 tools 字段")
	}
}

// 备选档（WorkBuddy 身份）：X-Domain 换成 copilot.tencent.com，并补齐设备指纹与 IDE 头。
func TestChatHeadersWorkBuddyTier(t *testing.T) {
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := NewWithBaseIdentity(srv.URL, srv.Client(), IdentityWorkBuddy)
	s, err := a.Chat(context.Background(), &channel.Credential{UID: "u1", AccessToken: "tok"},
		channel.ChatRequest{Model: "glm-5.2", Messages: []channel.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}
	defer s.Close()

	if got := hdr.Get("X-Domain"); got != DomainWB {
		t.Fatalf("WorkBuddy 档 X-Domain 应为 %q，实际 %q", DomainWB, got)
	}
	if got := hdr.Get("X-IDE-Type"); got != "WorkBuddy" {
		t.Fatalf("WorkBuddy 档应带 X-IDE-Type=WorkBuddy，实际 %q", got)
	}
	if hdr.Get("X-Machine-ID") == "" || hdr.Get("X-Session-ID") == "" {
		t.Fatal("WorkBuddy 档应带按 uid 派生的设备指纹")
	}
	if hdr.Get("X-Request-ID") == "" {
		t.Fatal("WorkBuddy 档应带请求 ID")
	}
}

// 同一 uid 派生的设备指纹必须稳定（幂等），否则上游会看到随机机器码。
func TestDeriveIDStable(t *testing.T) {
	if deriveID("u1", "machine") != deriveID("u1", "machine") {
		t.Fatal("同 uid 的 machineId 必须稳定")
	}
	if deriveID("u1", "machine") == deriveID("u2", "machine") {
		t.Fatal("不同 uid 的 machineId 不应相同（多账号隔离）")
	}
}

// OpenAI SSE 透传：内容/推理/停止/usage 与**工具调用分片**都要解析出来。
func TestStreamParsesOpenAISSEWithToolCalls(t *testing.T) {
	body := io.NopCloser(strings.NewReader(strings.Join([]string{
		"event: ping\n",
		"\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n",
		"\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n",
		"\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"思考\"}}]}\n",
		"\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n",
		"\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\"}}]}}]}\n",
		"\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n",
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
	if len(chunks) != 7 {
		t.Fatalf("期望 7 个 chunk（角色/内容/推理/工具首片/工具参数片/停止/usage），实际 %d 个: %+v", len(chunks), chunks)
	}
	if chunks[1].Choices[0].Delta.Content != "你好" {
		t.Fatalf("内容未透传: %+v", chunks[1])
	}
	if chunks[2].Choices[0].Delta.ReasoningContent != "思考" {
		t.Fatalf("推理内容未透传: %+v", chunks[2])
	}
	tc0 := chunks[3].Choices[0].Delta.ToolCalls
	if len(tc0) != 1 || tc0[0].ID != "call_1" || tc0[0].Function.Name != "get_weather" || tc0[0].Index != 0 {
		t.Fatalf("工具调用首片解析不对: %+v", tc0)
	}
	tc1 := chunks[4].Choices[0].Delta.ToolCalls
	if len(tc1) != 1 || tc1[0].Function.Arguments != `{"city":"北京"}` || tc1[0].Index != 0 {
		t.Fatalf("工具调用参数分片解析不对: %+v", tc1)
	}
	if chunks[5].Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason 未透传: %+v", chunks[5])
	}
	if chunks[6].Usage == nil || chunks[6].Usage["completion_tokens"] == nil {
		t.Fatalf("usage 帧必须交给客户端: %+v", chunks[6])
	}
	for i, c := range chunks {
		if c.Model != "glm-5.2" {
			t.Fatalf("第 %d 个 chunk 应回填请求模型，实际 %q", i, c.Model)
		}
	}
}

// usage 帧（无 choices）不能丢，空 delta 帧要丢。
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
		// 与 WorkBuddy 同源：额度耗尽回 HTTP 429 + 信封 code 14018，必须判成 HardCredit。
		{429, `{"error":{"data":{"code":14018,"msg":"额度已用尽，请购买加量包"}}}`, errs.HardCredit},
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
			if !ok || k != c.want {
				t.Fatalf("期望 %s，实际 %s（%v）", c.want, k, err)
			}
			ee, _ := err.(*errs.Error)
			if ee.Upstream == "" || ee.Account != "u1" {
				t.Fatalf("错误应带上游原话与账号：%+v", ee)
			}
		})
	}
}

// 刷新：身份档决定 X-Auth-Refresh-Source 与 X-Domain；成功后轮换 token。
func TestRefreshUsesIdentitySource(t *testing.T) {
	for _, tc := range []struct {
		ident      Identity
		wantDomain string
		wantSource string
	}{
		{IdentityCodeBuddy, DomainCB, "plugin"},
		{IdentityWorkBuddy, DomainWB, "workbuddy"},
	} {
		t.Run(string(tc.ident), func(t *testing.T) {
			var hdr http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hdr = r.Header.Clone()
				_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`)
			}))
			defer srv.Close()

			a := NewWithBaseIdentity(srv.URL, srv.Client(), tc.ident)
			nc, err := a.Refresh(context.Background(), &channel.Credential{
				UID: "u1", AccessToken: "old-at", RefreshToken: "rt-good", Extra: map[string]string{"enterprise_id": "ent-1"},
			})
			if err != nil {
				t.Fatalf("刷新失败: %v", err)
			}
			if got := hdr.Get("X-Refresh-Token"); got != "rt-good" {
				t.Fatalf("refresh token 应放在 X-Refresh-Token，实际 %q", got)
			}
			if got := hdr.Get("X-Auth-Refresh-Source"); got != tc.wantSource {
				t.Fatalf("刷新来源应为 %q，实际 %q", tc.wantSource, got)
			}
			if got := hdr.Get("X-Domain"); got != tc.wantDomain {
				t.Fatalf("X-Domain 应为 %q，实际 %q", tc.wantDomain, got)
			}
			if got := hdr.Get("X-Enterprise-Id"); got != "ent-1" {
				t.Fatalf("刷新也应带企业身份，实际 %q", got)
			}
			if nc.AccessToken != "new-at" || nc.RefreshToken != "new-rt" {
				t.Fatalf("刷新后凭证未轮换: %+v", nc)
			}
			if d := time.Until(nc.ExpiresAt); d < 50*time.Minute || d > 70*time.Minute {
				t.Fatalf("expiresIn=3600 应换算成约 1 小时后，实际 %v", nc.ExpiresAt)
			}
		})
	}
}

// 刷新响应的两种排布（扁平 / 再套一层 data）都要认；缺 token 判需重授权。
func TestRefreshTokenShapesAndFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"data":{"accessToken":"nested-at","expiresAt":4102444800000}}}`)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.Client())
	nc, err := a.Refresh(context.Background(), &channel.Credential{UID: "u1", RefreshToken: "rt"})
	if err != nil {
		t.Fatalf("嵌套 data 的令牌响应应能解析: %v", err)
	}
	if nc.AccessToken != "nested-at" || nc.ExpiresAt.IsZero() {
		t.Fatalf("嵌套结构解析不对: %+v", nc)
	}

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":""}}`)
	}))
	defer badSrv.Close()
	a2 := NewWithBase(badSrv.URL, badSrv.Client())
	if _, err := a2.Refresh(context.Background(), &channel.Credential{UID: "u1", RefreshToken: "rt"}); err == nil {
		t.Fatal("无 accessToken 必须报错")
	} else if k, _ := errs.KindOf(err); k != errs.SessionDead {
		t.Fatalf("期望 SessionDead，实际 %s", k)
	}
	if _, err := a2.Refresh(context.Background(), &channel.Credential{UID: "u1"}); err == nil {
		t.Fatal("缺 refresh token 必须报错")
	}
}

// 请求体：developer 改写成 system、缺 system 时补一条、强制 stream；tools/tool_choice 透传。
func TestBuildBodyShape(t *testing.T) {
	temp := 0.3
	raw := buildBody(channel.ChatRequest{
		Model:       "glm-5.2",
		MaxTokens:   128,
		Temperature: &temp,
		Messages:    []channel.Message{{Role: "developer", Content: "d"}, {Role: "user", Content: "u"}},
		Tools:       []map[string]any{{"type": "function", "function": map[string]any{"name": "f"}}},
		ToolChoice:  "auto",
	})
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	if obj["stream"] != true {
		t.Fatal("必须强制 stream=true")
	}
	if obj["temperature"] != 0.3 {
		t.Fatalf("temperature 应透传，实际 %v", obj["temperature"])
	}
	if obj["tool_choice"] != "auto" {
		t.Fatalf("tool_choice 应透传，实际 %v", obj["tool_choice"])
	}
	if _, ok := obj["tools"]; !ok {
		t.Fatal("客户端要工具时应带 tools")
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("developer 应改写成 system 且不额外插系统提示: %#v", msgs)
	}

	// 没有 system 时补一条默认系统提示；tool_choice:none 时 tools 与 tool_choice 都不带。
	raw2 := buildBody(channel.ChatRequest{
		Model:      "glm-5.2",
		Messages:   []channel.Message{{Role: "user", Content: "u"}},
		Tools:      []map[string]any{{"type": "function", "function": map[string]any{"name": "f"}}},
		ToolChoice: "none",
	})
	var obj2 map[string]any
	_ = json.Unmarshal(raw2, &obj2)
	msgs2 := obj2["messages"].([]any)
	if len(msgs2) != 2 || msgs2[0].(map[string]any)["role"] != "system" {
		t.Fatalf("缺 system 时应补一条: %#v", msgs2)
	}
	if _, ok := obj2["tools"]; ok {
		t.Fatal("tool_choice:none 时不应带 tools")
	}
}

// 余额：信封剥壳 + 周期额度优先、否则用总量；上游报错不能返回 0 余额冒充「用尽」。
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
	if !b.Known || b.Credits != 65 {
		t.Fatalf("期望 40+25=65 且 Known=true，实际 %+v", b)
	}

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

// 静态模型表是唯一目录来源：非空、无重复。
func TestStaticModelsSane(t *testing.T) {
	ms := New().ModelsSnapshot()
	if len(ms) == 0 {
		t.Fatal("静态模型表不能为空")
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
}
