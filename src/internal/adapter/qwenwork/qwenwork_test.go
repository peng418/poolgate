package qwenwork

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 网页端鉴权必须用 Cookie: token —— 实测网页域用 Bearer 反而 401。
// 这条用例钉住「用哪个头」，改错会让整个渠道静默失效。
func TestRequestsCarryCookieToken(t *testing.T) {
	var gotCookie, gotBearer, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotBearer = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"qwork":[]}`))
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, nil)
	cred := channel.Credential{UID: "u1", AccessToken: "tok-abc"}
	if _, err := a.Models(context.Background(), &cred); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotCookie, "token=tok-abc") {
		t.Fatalf("应以 Cookie 携带 token，实际 %q", gotCookie)
	}
	if gotBearer != "" {
		t.Fatalf("不应使用 Bearer 鉴权，实际 %q", gotBearer)
	}
	if gotUA == "" {
		t.Fatal("应带浏览器 UA（上游对非浏览器 UA 另眼相待）")
	}
}

// 模型目录要如实标注来源：上游实时返回的标 upstream，拿不到窗口就标 unknown 不猜数字。
func TestModelsSourceIsHonest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"qwork":[
			{"key":"pro","enable":true,"is_reasoning":true,"is_vl":false,"max_input_tokens":200000},
			{"key":"flash","enable":true,"is_reasoning":true,"is_vl":true,"max_input_tokens":0},
			{"key":"off","enable":false}
		]}`))
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, nil)
	cred := channel.Credential{UID: "u1", AccessToken: "t"}
	models, err := a.Models(context.Background(), &cred)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("enable=false 的档位应被过滤，实际 %d 个", len(models))
	}
	byID := map[string]channel.ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if byID["pro"].Source != channel.SourceUpstream {
		t.Fatalf("有窗口的应标 upstream，实际 %v", byID["pro"].Source)
	}
	if byID["pro"].ContextWindow != 200000 {
		t.Fatalf("窗口应取上游值，实际 %d", byID["pro"].ContextWindow)
	}
	if byID["flash"].Source != channel.SourceUnknown {
		t.Fatalf("上游没给窗口时应标 unknown（不猜数字），实际 %v", byID["flash"].Source)
	}
	if byID["flash"].Images != channel.CapYes {
		t.Fatal("is_vl=true 应映射为支持图片")
	}
}

// 展示名要取上游的**扁平** display_name（实测报文形态，wild-work modelEntry 同款字段）：
// 只解析 i18n.display_name 会拿不到值，面板会把 key（pro/flash）当名字显示。
func TestModelsUsesUpstreamDisplayName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"qwork":[
			{"key":"flash","display_name":"标准","enable":true,"max_input_tokens":200000},
			{"key":"pro","i18n":{"display_name":{"zh":"高级"}},"enable":true,"max_input_tokens":200000}
		]}`))
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, nil)
	cred := channel.Credential{UID: "u1", AccessToken: "t"}
	models, err := a.Models(context.Background(), &cred)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, m := range models {
		byID[m.ID] = m.DisplayName
	}
	if byID["flash"] != "标准" {
		t.Fatalf("应取扁平 display_name，实际 %q", byID["flash"])
	}
	if byID["pro"] != "高级" {
		t.Fatalf("i18n.display_name.zh 存在时优先，实际 %q", byID["pro"])
	}
}

// 目录接口不可用时要回退静态表，而不是返回空（空目录会让面板看起来「没有模型」）。
func TestModelsFallsBackToStatic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, nil)
	cred := channel.Credential{UID: "u1", AccessToken: "t"}
	models, err := a.Models(context.Background(), &cred)
	if err != nil {
		t.Fatalf("目录失败不应报错，应回退静态表：%v", err)
	}
	if len(models) == 0 {
		t.Fatal("应回退静态兜底表")
	}
	for _, m := range models {
		if m.Source != channel.SourceLocal {
			t.Fatalf("静态表来源应为 local（不冒充上游值），实际 %v", m.Source)
		}
	}
}

// 余额：网页域返回 {code:"ok",data:{balance}}。
func TestBalanceParsesWebShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ok","data":{"balance":316.0943,"freeze_credit":0}}`))
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, nil)
	cred := channel.Credential{UID: "u1", AccessToken: "t"}
	b, err := a.Balance(context.Background(), &cred)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Known || b.Credits != 316 {
		t.Fatalf("余额解析不对：%+v", b)
	}
}

// 余额接口异常时标 Known=false，让 UI 显示「未知」而不是 0（0 会被误读成「没钱了」）。
func TestBalanceUnknownOnBadCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"error","data":{}}`))
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, nil)
	cred := channel.Credential{UID: "u1", AccessToken: "t"}
	b, err := a.Balance(context.Background(), &cred)
	if err != nil {
		t.Fatal(err)
	}
	if b.Known {
		t.Fatal("上游没给可信余额时应标 Known=false")
	}
}

// 千问办公没有签到活动 —— 必须如实标注 NoActivity，而不是假装签到成功。
func TestCheckinReportsNoActivity(t *testing.T) {
	a := New()
	cred := channel.Credential{UID: "u1", AccessToken: "t"}
	r, err := a.Checkin(context.Background(), &cred)
	if err != nil {
		t.Fatal(err)
	}
	if !r.NoActivity {
		t.Fatal("该渠道应标 NoActivity=true")
	}
}

// 错误分类：额度/限流/会话失效必须分开，否则冷却策略会误杀。
func TestClassify(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `unauthorized`, errs.SessionDead},
		{403, `forbidden`, errs.SessionDead},
		{429, `too many requests`, errs.SoftRate},
		{500, `internal`, errs.UpstreamFault},
		{200, `{"msg":"credits exhausted"}`, errs.HardCredit},
		{200, `{"msg":"insufficient balance"}`, errs.HardCredit},
		// 中文额度文案（wild-work hardMarkers 原表）不能落成 Parse，否则会被当上游故障。
		{200, `{"msg":"积分不足"}`, errs.HardCredit},
		{400, `{"msg":"not enough credit"}`, errs.HardCredit},
		// 402 Payment Required 是额度不足（wild-work Classify 首判）。
		{402, `payment required`, errs.HardCredit},
		// 403 带内容拦截标记时是内容拦截，不是账号失效（不然会误冷却好号）。
		{403, `{"msg":"检测到敏感内容"}`, errs.ContentBlocked},
		{403, `{"msg":"blocked by security policy"}`, errs.ContentBlocked},
		{200, `{"msg":"Model is not available for this user"}`, errs.ModelUnavailable},
		{404, `not found`, errs.ModelUnavailable},
		// 同账号并发建会话：409 CONCURRENT_OPERATION 是**瞬态闸门**，不是限流。
		// 真机实测（2026-09-27）：+0.2s → 409，+2.4s → 201。判成 SoftRate 会冷却 60 秒，
		// 单账号渠道等于整条渠道冻结一分钟 —— 用户投诉的「一直冻结」就是这个。
		{409, `{"code":"CONCURRENT_OPERATION","message":"Already handling session start request for this user"}`, errs.SessionBusy},
		{200, `{"code":"CONCURRENT_OPERATION"}`, errs.SessionBusy},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Errorf("Classify(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// 模型别名要映射到上游档位 key（客户端可能传旧名字）。
func TestResolveModelAliases(t *testing.T) {
	a := New()
	cases := map[string]string{
		"":               "pro",
		"auto":           "pro",
		"qwork-advanced": "pro",
		"qwork-lite":     "flash",
		"qwen3.8-max":    "qwen3.8-max-preview",
		"flash":          "flash",
		"unknown-model":  "unknown-model",
	}
	for in, want := range cases {
		if got := a.resolveModel(in); got != want {
			t.Errorf("resolveModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// 多轮对话必须把上下文拼进单条 prompt：上游服务端无状态。
func TestPromptTextCarriesFullHistory(t *testing.T) {
	a := New()
	req := channel.ChatRequest{Messages: []channel.Message{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "第一问"},
		{Role: "assistant", Content: "第一答"},
		{Role: "user", Content: "第二问"},
	}}
	got := a.promptText(req)
	for _, want := range []string{"你是助手", "第一问", "第一答", "第二问"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt 应含 %q，实际 %q", want, got)
		}
	}
}

// 单条消息不应加角色前缀（保持与网页端一致的裸文本）。
func TestPromptTextSingleMessageIsBare(t *testing.T) {
	a := New()
	got := a.promptText(channel.ChatRequest{Messages: []channel.Message{{Role: "user", Content: "你好"}}})
	if got != "你好" {
		t.Fatalf("单条消息应原样发送，实际 %q", got)
	}
}

// JSON-RPC 错误要能归一：额度类不能落成上游故障（否则会把好号冷却）。
func TestClassifyRPC(t *testing.T) {
	s := &wsStream{}
	if k, _ := errs.KindOf(s.classifyRPC(0, "Credits exhausted")); k != errs.HardCredit {
		t.Fatalf("额度错误应归 HardCredit，实际 %v", k)
	}
	if k, _ := errs.KindOf(s.classifyRPC(0, "too many requests")); k != errs.SoftRate {
		t.Fatalf("限流应归 SoftRate，实际 %v", k)
	}
	if k, _ := errs.KindOf(s.classifyRPC(4001, "token expired")); k != errs.SessionDead {
		t.Fatalf("会话失效应归 SessionDead，实际 %v", k)
	}
	if k, _ := errs.KindOf(s.classifyRPC(0, "内容被 blocked by security policy")); k != errs.ContentBlocked {
		t.Fatalf("流内内容拦截应归 ContentBlocked，实际 %v", k)
	}
	if k, _ := errs.KindOf(s.classifyRPC(0, "weird upstream thing")); k != errs.UpstreamFault {
		t.Fatalf("其余应归 UpstreamFault，实际 %v", k)
	}
}

// 正文与思考要分别映射到 content / reasoning_content。
func TestEmitUpdateMapsContentAndReasoning(t *testing.T) {
	s := &wsStream{chunks: make(chan channel.ChatCompletionChunk, 4), ctx: context.Background(), model: "pro"}

	msg, _ := json.Marshal(map[string]any{
		"sessionId": "s1",
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"text": "你好", "type": "text"},
			"messageId":     "m1",
		},
	})
	s.emitUpdate(msg)
	if !s.wroteContent {
		t.Fatal("收到正文后 wroteContent 应为 true")
	}
	got := <-s.chunks
	if got.Choices[0].Delta.Content != "你好" {
		t.Fatalf("正文应进 content，实际 %+v", got.Choices[0].Delta)
	}

	think, _ := json.Marshal(map[string]any{
		"update": map[string]any{
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"text": "想一想", "type": "text"},
		},
	})
	s.emitUpdate(think)
	got = <-s.chunks
	if got.Choices[0].Delta.ReasoningContent != "想一想" {
		t.Fatalf("思考应进 reasoning_content，实际 %+v", got.Choices[0].Delta)
	}
	if got.Choices[0].Delta.Content != "" {
		t.Fatal("思考不应混进 content")
	}
}

// 空文本帧不应产生 chunk（上游会发空 delta）。
func TestEmitUpdateSkipsEmptyText(t *testing.T) {
	s := &wsStream{chunks: make(chan channel.ChatCompletionChunk, 4), ctx: context.Background()}
	raw, _ := json.Marshal(map[string]any{
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"text": "", "type": "text"},
		},
	})
	s.emitUpdate(raw)
	if len(s.chunks) != 0 {
		t.Fatal("空文本不应产生 chunk")
	}
}

// 能力声明：千问办公支持图片/推理但没有签到，**不支持工具调用**
// （chat-ws 的 new_prompt 只有纯文本，放不下工具定义 → 如实声明 Tools=false）。
// 但它是纯文本入口：客户端（coding agent）习惯性带 tools 时不许把整轮请求顶死
// → ToolsIgnore=true（网关丢掉 tools 按纯文本转发 + 落日志）。
func TestSpec(t *testing.T) {
	sp := New().Spec()
	if sp.Kind != "qwenwork" {
		t.Fatalf("Kind 不对：%v", sp.Kind)
	}
	if sp.Tools {
		t.Fatal("本渠道不实现工具调用，Tools 应为 false")
	}
	if sp.ToolsShim {
		t.Fatal("本渠道不做网关代做模拟，ToolsShim 应为 false")
	}
	if !sp.ToolsIgnore {
		t.Fatal("本渠道是纯文本入口，应声明 ToolsIgnore（带 tools 丢转发，而不是 400 顶死）")
	}
	if !sp.Images || !sp.Reasoning {
		t.Fatal("应声明图片/推理能力")
	}
	if sp.CheckinCap {
		t.Fatal("无签到活动，CheckinCap 应为 false")
	}
	if !sp.SSEOnly {
		t.Fatal("上游只有流式，SSEOnly 应为 true")
	}
}

// deviceJWT 造一个能被 parseJWTIdentity 解出昵称的 device_token（测试用，非真实签名）。
func deviceJWTWithExp(nick string, exp time.Time) string {
	h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	p, _ := json.Marshal(map[string]any{"user_id": "u1", "username": nick, "exp": exp.Unix()})
	enc := base64.RawURLEncoding.EncodeToString
	return enc(h) + "." + enc(p) + ".sig"
}

func deviceJWT(nick string) string {
	h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	p, _ := json.Marshal(map[string]any{"user_id": "u1", "username": nick})
	enc := base64.RawURLEncoding.EncodeToString
	return enc(h) + "." + enc(p) + ".sig"
}

// 到期时间必须取 device_token 的 **JWT exp**，不是上游的 expires_in。
// 2026-09-27 事故回归：expires_in(≈10 分钟) 被当到期时间 → 「临期才续期」永远成立 →
// 每个请求都续一次（一次性 rt 被轮换到断链）。
func TestRefreshUsesJWTExpiryNotExpiresIn(t *testing.T) {
	want := time.Now().Add(7 * 24 * time.Hour).Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_token":  deviceJWTWithExp("demo-qwen", want),
			"refresh_token": "rt-rotated",
			"expires_in":    600 * 1000, // 上游的「多久换一次」提示：10 分钟（毫秒口径）
		})
	}))
	defer srv.Close()

	a := New()
	a.deviceURL = srv.URL + epDeviceToken
	c := channel.Credential{UID: "u1", AccessToken: "oauth", RefreshToken: "rt-old", ExpiresAt: time.Now()}
	nc, err := a.Refresh(context.Background(), &c)
	if err != nil {
		t.Fatalf("续期失败：%v", err)
	}
	if d := time.Until(nc.ExpiresAt); d < 6*24*time.Hour {
		t.Fatalf("到期时间应取 JWT exp（≈7 天），实际还剩 %v（是不是又用了 expires_in？）", d)
	}
}

// 一次性 refresh token 的渠道必须声明续期节奏（闲置会失效），且节奏不能大到让链放坏。
func TestSpecDeclaresRefreshCadence(t *testing.T) {
	sp := New().Spec()
	if sp.RefreshCadence <= 0 {
		t.Fatal("千问办公的 refresh token 是一次性轮换的，必须声明 RefreshCadence（闲置会失效）")
	}
	if sp.RefreshCadence > 30*time.Minute {
		t.Fatalf("续期节奏过大（实测闲置 ~31 小时 rt 就失效了）：%v", sp.RefreshCadence)
	}
}

// 续期必须走 **deviceToken 换发端点**，而不是 OAuth 的 /oauth2/token。
//
// 这条是用户实测逼出来的：OAuth 授权码换出来的 access token 拿去做网页域请求会
// 401 {"code":"invalid-credential","msg":"Invalid JWT token"}；只有
// /api/v1/deviceToken/refresh 换回的 device_token 才是网页域认的凭证。
func TestRefreshExchangesForDeviceToken(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("deviceToken 端点要 JSON body，实际 Content-Type=%q", ct)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":         "at-should-be-ignored",
			"device_token":  deviceJWT("demo-qwen"),
			"refresh_token": "rt-rotated",
			"expires_in":    3600 * 1000, // 毫秒
		})
	}))
	defer srv.Close()

	a := New()
	// 生产默认必须是 deviceToken 端点（别又退回 OAuth 的 /oauth2/token —— 那条路换出来的
	// token 网页域不认，正是用户踩的坑）。
	if !strings.HasSuffix(a.deviceURL, epDeviceToken) {
		t.Fatalf("默认续期端点应是 %s，实际 %s", epDeviceToken, a.deviceURL)
	}
	a.deviceURL = srv.URL + epDeviceToken
	c := channel.Credential{UID: "u1", AccessToken: "oauth-access-token", RefreshToken: "rt-old",
		ExpiresAt: time.Now().Add(-time.Minute)}
	nc, err := a.Refresh(context.Background(), &c)
	if err != nil {
		t.Fatalf("续期失败：%v", err)
	}
	if gotPath != epDeviceToken {
		t.Fatalf("应打 deviceToken 端点 %s，实际 %s", epDeviceToken, gotPath)
	}
	if !strings.Contains(gotBody, `"refresh_token":"rt-old"`) || !strings.Contains(gotBody, `"target":"c"`) {
		t.Fatalf("body 必须带 refresh_token 与 target=c，实际 %s", gotBody)
	}
	if nc.AccessToken == "at-should-be-ignored" || nc.AccessToken == c.AccessToken {
		t.Fatalf("必须用 device_token 覆盖 access token，实际 %q", nc.AccessToken)
	}
	if nc.RefreshToken != "rt-rotated" {
		t.Fatalf("轮换后的 refresh token 必须带回去，实际 %q", nc.RefreshToken)
	}
	if time.Until(nc.ExpiresAt) < 30*time.Minute {
		t.Fatalf("到期时间应按 expires_in(ms) 换算，实际 %v", nc.ExpiresAt)
	}
	// device_token 里才有 username：昵称空着要补上，免得面板显示 hex uid。
	if nc.Nickname != "demo-qwen" {
		t.Fatalf("应从 device_token 补昵称，实际 %q", nc.Nickname)
	}
}

// refresh_token 被上游拒（INVALID_REFRESH_TOKEN）→ SessionDead + 说清要重新授权。
func TestRefreshInvalidTokenNeedsReauth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errorCode":    "INVALID_REFRESH_TOKEN",
			"errorMessage": "refresh token is invalid",
		})
	}))
	defer srv.Close()

	a := New()
	a.deviceURL = srv.URL
	c := channel.Credential{UID: "u1", AccessToken: "oauth", RefreshToken: "rt-dead"}
	nc, err := a.Refresh(context.Background(), &c)
	if err == nil || nc != nil {
		t.Fatalf("续期失败应返回错误而不是凭证，got %v %v", nc, err)
	}
	k, _ := errs.KindOf(err)
	if k != errs.SessionDead {
		t.Fatalf("refresh_token 失效应归为 SessionDead（面板据此提示重新登录），实际 %s", k)
	}
	if !strings.Contains(err.Error(), "重新授权") {
		t.Fatalf("要说清怎么办，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "INVALID_REFRESH_TOKEN") {
		t.Fatalf("要带上游原话，实际 %q", err.Error())
	}
}

// 换回来的 token 不完整（缺 device_token 或 refresh_token）：宁可不换，也不留半份。
func TestRefreshRejectsIncompleteTokenPair(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"device_token": deviceJWT("x")}) // 没给 refresh_token
	}))
	defer srv.Close()

	a := New()
	a.deviceURL = srv.URL
	c := channel.Credential{UID: "u1", AccessToken: "oauth", RefreshToken: "rt"}
	nc, err := a.Refresh(context.Background(), &c)
	if err == nil || nc != nil {
		t.Fatalf("不完整的一对 token 应报错，got %v %v", nc, err)
	}
	if !strings.Contains(err.Error(), "不完整") {
		t.Fatalf("应说明原因，实际 %q", err.Error())
	}
}

// 没有 refresh token（老凭证）：如实返回「刷不了」，且**不发任何请求**。
func TestRefreshWithoutTokenIsNoop(t *testing.T) {
	a := New()
	a.deviceURL = "http://127.0.0.1:1/never" // 真去请求必然失败
	c := channel.Credential{UID: "u1", AccessToken: "dt-old"}
	nc, err := a.Refresh(context.Background(), &c)
	if err != nil || nc != nil {
		t.Fatalf("没有 refresh token 时应返回 (nil, nil)，实际 %v %v", nc, err)
	}
}

// ---------------------------------------------------------------------------
// 同账号并发建会话（409 CONCURRENT_OPERATION）的处置纪律
//
// 真机实测（2026-09-27，对真上游连发 4 次 POST /api/chat-sessions）：
//
//	+0.0s → 201   +0.2s → 409   +2.4s → 201   +4.0s → 201
//
// 结论：这是 2 秒级的瞬态闸门，不是限流、更不是账号故障。
// 处置：适配器内联退避重试；耗尽后抛 SessionBusy（不冷却账号）。
// ---------------------------------------------------------------------------

// shrinkSessionBackoff 把退避压到毫秒级，测试里不真睡 1.6 秒。
func shrinkSessionBackoff(t *testing.T) {
	t.Helper()
	orig := sessionStartBackoff
	sessionStartBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { sessionStartBackoff = orig })
}

// 409 之后退避重试同号即成功：调用方（路由/体检）根本看不到这次冲突。
func TestCreateSessionRetriesOnConcurrentOperation(t *testing.T) {
	shrinkSessionBackoff(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"CONCURRENT_OPERATION","message":"Already handling session start request for this user"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"sess-1"}`))
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, nil)
	sid, err := a.createSession(context.Background(), &channel.Credential{UID: "u1", AccessToken: "tok"}, "pro")
	if err != nil {
		t.Fatalf("并发冲突应被内联重试消化，实际报错：%v", err)
	}
	if sid != "sess-1" {
		t.Fatalf("应拿到会话 id，实际 %q", sid)
	}
	if hits != 2 {
		t.Fatalf("应只重试 1 次（共 2 次请求），实际 %d 次", hits)
	}
}

// 退避耗尽仍冲突：如实抛出 SessionBusy（**不是**账号错误），可换号、可再试。
func TestCreateSessionConcurrentOperationGivesUpAsSessionBusy(t *testing.T) {
	shrinkSessionBackoff(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"CONCURRENT_OPERATION","message":"Already handling session start request for this user"}`))
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, nil)
	_, err := a.createSession(context.Background(), &channel.Credential{UID: "u1", AccessToken: "tok"}, "pro")
	if err == nil {
		t.Fatal("一直并发冲突时应把真实原因抛出去（红线一：零静默失败）")
	}
	k, _ := errs.KindOf(err)
	if k != errs.SessionBusy {
		t.Fatalf("应归 SessionBusy（不冷却账号），实际 %v", k)
	}
	if k.AccountBlamed() {
		t.Fatal("并发冲突不得计入账号错误：否则单账号渠道会整条冻结 60 秒")
	}
	want := 1 + len(sessionStartBackoff)
	if hits != want {
		t.Fatalf("应请求 %d 次（首次 + %d 次退避重试），实际 %d 次", want, len(sessionStartBackoff), hits)
	}
}

// 同账号会话令牌：等不到要能被 ctx 取消（sync.Mutex 那种「冻死在锁上」的写法不许回来）。
func TestAcquireSessionWaitIsCancellable(t *testing.T) {
	a := NewWithBase("http://127.0.0.1:1", nil)
	release, err := a.acquireSession(context.Background(), "u1")
	if err != nil {
		t.Fatalf("首次取令牌不该失败：%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = a.acquireSession(ctx, "u1")
	if err == nil {
		t.Fatal("同账号第二次取令牌应等不到并报错，而不是一直等")
	}
	if k, _ := errs.KindOf(err); k != errs.SessionBusy {
		t.Fatalf("等锁超时应归 SessionBusy（可换号、不冷号），实际 %v", k)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("应随 ctx 立刻返回，实际等了 %s", d)
	}
	release()
	// 释放后应能再取到（令牌是复用的，不能泄漏）。
	release2, err := a.acquireSession(context.Background(), "u1")
	if err != nil {
		t.Fatalf("释放后应能再次取到令牌：%v", err)
	}
	release2()
}

// 端到端（真 HTTP + 真 WebSocket 握手，上游是桩）：上游第一次建会话回 409，
// Chat 必须**照样返回可用流** —— 这一次冲突在适配器内部就被退避重试消化掉了，
// 调用方（路由 / 体检）既看不到它，也不该因此冷却账号。
func TestChatSurvivesConcurrentSessionStart(t *testing.T) {
	shrinkSessionBackoff(t)
	var creates int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat-sessions", func(w http.ResponseWriter, r *http.Request) {
		creates++
		if creates == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"CONCURRENT_OPERATION","message":"Already handling session start request for this user"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"sess-ok"}`))
	})
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux.HandleFunc("/api/chat-ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for { // 空转：等客户端关闭
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := NewWithBase(srv.URL, nil)
	cred := channel.Credential{UID: "u1", AccessToken: "tok"}
	stream, err := a.Chat(context.Background(), &cred, channel.ChatRequest{
		Model:    "pro",
		Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("一次并发冲突不该让整轮请求失败（应在适配器内退避重试掉），实际：%v", err)
	}
	_ = stream.Close()
	if creates != 2 {
		t.Fatalf("应重试一次建会话（共 2 次），实际 %d 次", creates)
	}
}
