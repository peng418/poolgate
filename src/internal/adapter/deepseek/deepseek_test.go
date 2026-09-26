package deepseek

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// stubSolver 给测试用：不跑 WASM，直接给一个固定头。
type stubSolver struct{ got powChallenge }

func (s *stubSolver) powHeader(_ context.Context, ch powChallenge) (string, error) {
	s.got = ch
	return "c3R1Yi1wb3c=", nil
}

// guestPowHeader 是游客 PoW 的桩：返回一个可识别的固定值，测试据此断言
// 「登录类请求确实带上了 X-DS-Guest-PoW-Response」。
func (s *stubSolver) guestPowHeader(_ context.Context, ch powChallenge) (string, error) {
	s.got = ch
	return "c3R1Yi1ndWVzdA==", nil
}

// patch 协议：初始快照 + APPEND 文本 + THINK/RESPONSE 分流 + status 收尾。
func TestPatchStateMachine(t *testing.T) {
	state := &patchState{}
	frame := func(raw string) (string, string, bool) {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("测试帧不是 JSON：%v", err)
		}
		return state.feed(m)
	}

	// 1) 初始快照：fragments 里带全文
	think, content, done := frame(`{"v":{"response":{"status":"WIP","fragments":[
		{"type":"THINK","content":"想一下"},
		{"type":"RESPONSE","content":"你好"}]}}}`)
	if think != "想一下" || content != "你好" || done {
		t.Fatalf("初始快照解析不对：%q %q %v", think, content, done)
	}

	// 2) p/o 跨帧持久化：只给 p 一次，后面只给 v
	if _, c, _ := frame(`{"p":"response/fragments/-1/content","o":"APPEND","v":"，世界"}`); c != "，世界" {
		t.Fatalf("APPEND 增量不对：%q", c)
	}
	if _, c, _ := frame(`{"v":"！"}`); c != "！" {
		t.Fatalf("省略 p/o 之后应沿用上一帧的路径与操作：%q", c)
	}

	// 3) BATCH：父路径 + 子项各自 p/v
	think2, content2, _ := frame(`{"p":"response","o":"BATCH","v":[
		{"p":"fragments/-1/content","o":"APPEND","v":"（补）"},
		{"p":"status","v":"FINISHED"}]}`)
	if content2 != "（补）" {
		t.Fatalf("BATCH 里的 APPEND 没生效：%q", content2)
	}
	if think2 != "" {
		t.Fatalf("BATCH 不该产出思考：%q", think2)
	}
	if !state.done() {
		t.Fatalf("BATCH 里的 status=FINISHED 应判结束，status=%q", state.status)
	}

	// 4) 新的 RESPONSE 片段（fragments APPEND）也要认
	if _, c, _ := frame(`{"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":"新段"}]}`); c != "新段" {
		t.Fatalf("fragments APPEND 不对：%q", c)
	}
}

// 工具定义降级成提示词后，提示词里只有 prompt 一个字段 —— 多轮要打成 ChatML。
func TestPackMessagesChatML(t *testing.T) {
	got := packMessages([]channel.Message{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "在的"},
	})
	for _, want := range []string{"<｜System｜>你是助手", "<｜User｜>你好", "<｜Assistant｜>在的", "<｜end▁of▁sentence｜>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ChatML 缺 %q：%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "<｜Assistant｜>") {
		t.Fatalf("末尾必须补 Assistant 锚点：%s", got)
	}
}

func TestExtractUserToken(t *testing.T) {
	tok := strings.Repeat("a", 40)
	cases := []struct{ in, want string }{
		{tok, tok},
		{`"` + tok + `"`, tok},
		{`{"value":"` + tok + `","__version":"1"}`, tok},
		{"   " + tok + "  \n", tok},
		{"too short", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := extractUserToken(c.in); got != c.want {
			t.Errorf("extractUserToken(%q) = %q，want %q", c.in, got, c.want)
		}
	}
}

// 设备 id 必须「同 token 同值、不同 token 不同值」，且是合法的 v4 UUID。
func TestDeriveDeviceID(t *testing.T) {
	a := deriveDeviceID("token-a")
	if a != deriveDeviceID("token-a") {
		t.Fatal("同一个 token 必须派生出同一个设备 id（否则每次重登都像新设备）")
	}
	if a == deriveDeviceID("token-b") {
		t.Fatal("不同账号必须有不同的设备 id（共用会被判异常）")
	}
	if len(a) != 36 || a[14] != '4' || (a[19] != '8' && a[19] != '9' && a[19] != 'a' && a[19] != 'b') {
		t.Fatalf("不是合法的 v4 UUID：%q", a)
	}
}

// 整条链路：建会话 → 取挑战 → 解 PoW → 打 completion（patch 流）→ 收尾删会话。
func TestChatFlowWithMockServer(t *testing.T) {
	var sawHeaders http.Header
	var sawBody map[string]any
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat_session/create"):
			fmt.Fprint(w, `{"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/create_pow_challenge"):
			fmt.Fprint(w, `{"data":{"biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"c1",
				"salt":"s1","signature":"sig1","difficulty":144000,"expire_at":1775380966945,
				"expire_after":300000,"target_path":"/api/v0/chat/completion"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completion"):
			sawHeaders = r.Header.Clone()
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &sawBody)
			w.Header().Set("Content-Type", "text/event-stream")
			frames := []string{
				`{"v":{"response":{"status":"WIP","fragments":[{"type":"THINK","content":"想"},{"type":"RESPONSE","content":""}]}}}`,
				`{"p":"response/fragments/-1/content","o":"APPEND","v":"好了"}`,
				`{"p":"response/status","v":"FINISHED"}`,
			}
			for _, f := range frames {
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", f)
			}
		case strings.HasSuffix(r.URL.Path, "/chat_session/delete"):
			deleted = true
			fmt.Fprint(w, `{"code":0}`)
		default:
			t.Errorf("意外路径：%s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	stub := &stubSolver{}
	a.SetSolver(stub)
	cred := &channel.Credential{UID: "u1", AccessToken: "tok", Extra: map[string]string{"device_id": "dev-1"}}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model:    "default",
		Messages: []channel.Message{{Role: "user", Content: "在吗"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	var think, content strings.Builder
	done := false
	for {
		c, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("流读取失败：%v", err)
		}
		for _, ch := range c.Choices {
			think.WriteString(ch.Delta.ReasoningContent)
			content.WriteString(ch.Delta.Content)
			if ch.FinishReason != "" {
				done = true
			}
		}
	}
	_ = st.Close()

	if think.String() != "想" || content.String() != "好了" || !done {
		t.Fatalf("流内容不对：think=%q content=%q done=%v", think.String(), content.String(), done)
	}
	// 客户端标识与 PoW 头都要在
	if sawHeaders.Get("X-Ds-Pow-Response") != "c3R1Yi1wb3M=" && sawHeaders.Get("X-Ds-Pow-Response") != "c3R1Yi1wb3c=" {
		t.Fatalf("缺 X-Ds-Pow-Response：%q", sawHeaders.Get("X-Ds-Pow-Response"))
	}
	if sawHeaders.Get("X-Device-Id") != "dev-1" {
		t.Fatalf("缺 X-Device-Id：%q", sawHeaders.Get("X-Device-Id"))
	}
	if !strings.HasPrefix(sawHeaders.Get("User-Agent"), "DeepSeek/") {
		t.Fatalf("UA 必须是 App 身份：%q", sawHeaders.Get("User-Agent"))
	}
	if sawBody["chat_session_id"] != "sess-1" || sawBody["model_type"] != "default" ||
		sawBody["thinking_enabled"] != true {
		t.Fatalf("请求体不对：%v", sawBody)
	}
	if !strings.Contains(fmt.Sprint(sawBody["prompt"]), "<｜User｜>在吗") {
		t.Fatalf("prompt 没有打成 ChatML：%v", sawBody["prompt"])
	}
	if stub.got.Difficulty != 144000 || stub.got.TargetPath != "/api/v0/chat/completion" {
		t.Fatalf("挑战参数没传对：%+v", stub.got)
	}
	if !deleted {
		t.Fatal("流结束后应当删掉会话（否则上游会话表会越堆越多）")
	}
}

// 上游用「HTTP 200 + JSON 业务错误信封」表示失败时，整条流必须报结构化错误，
// 并把上游原话带出来（红线一）——不能当成「成功但没内容」。
func TestStreamSurfacesUpstreamEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat_session/create"):
			fmt.Fprint(w, `{"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/create_pow_challenge"):
			fmt.Fprint(w, `{"data":{"biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"c1",
				"salt":"s1","signature":"sig1","difficulty":1000,"expire_at":1,"target_path":"/api/v0/chat/completion"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completion"):
			// 实测形态：HTTP 200，body 是信封 JSON（不是 SSE）。
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":40003,"msg":"Authorization Failed (invalid token)","data":null}`)
		default:
			fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	a.SetSolver(&stubSolver{})
	cred := &channel.Credential{UID: "u1", AccessToken: "tok", Extra: map[string]string{"device_id": "d"}}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{Model: "default"})
	if err != nil {
		t.Fatalf("Chat 应当成功建流（错误在流里）：%v", err)
	}
	defer st.Close()
	_, err = st.Next()
	if err == nil {
		t.Fatal("HTTP 200 + 业务错误码必须报错，不能静默结束")
	}
	if k, ok := errs.KindOf(err); !ok || k != errs.SessionDead {
		t.Fatalf("invalid token 应归一成 SessionDead：%v", err)
	}
	var ee *errs.Error
	if !errors.As(err, &ee) || !strings.Contains(ee.Upstream, "invalid token") {
		t.Fatalf("必须带上游原话：%+v", err)
	}
}

// 完全空的流（HTTP 200、零字节）也要报错，不能回一个空成功。
func TestStreamEmptyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat_session/create"):
			fmt.Fprint(w, `{"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/create_pow_challenge"):
			fmt.Fprint(w, `{"data":{"biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"c1",
				"salt":"s1","signature":"sig1","difficulty":1000,"expire_at":1,"target_path":"/api/v0/chat/completion"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completion"):
			// 空 body
		default:
			fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	a.SetSolver(&stubSolver{})
	cred := &channel.Credential{UID: "u1", AccessToken: "tok", Extra: map[string]string{"device_id": "d"}}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{Model: "default"})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	if _, err := st.Next(); err == nil {
		t.Fatal("空流必须报错（红线二）")
	}
}

// 模型名解析对齐参考实现：`deepseek-<type>` 与裸 `<type>` 都要认（models.rs:43-64），
// 大小写不敏感；未知名回落 default 但 ok=false。
func TestModelOfAcceptsReferenceIDs(t *testing.T) {
	for _, id := range []string{"default", "DEFAULT", "deepseek-default", "deepseek-DEFAULT"} {
		typ, _, ok := modelOf(id)
		if !ok || typ != "default" {
			t.Errorf("modelOf(%q) = %q ok=%v，want default/true", id, typ, ok)
		}
	}
	for _, id := range []string{"expert", "deepseek-expert"} {
		typ, _, ok := modelOf(id)
		if !ok || typ != "expert" {
			t.Errorf("modelOf(%q) = %q ok=%v，want expert/true", id, typ, ok)
		}
	}
	if _, _, ok := modelOf("gpt-4o"); ok {
		t.Fatal("未知模型名应回落 default 但 ok=false")
	}
}

// 首轮 completion 请求体不得出现 parent_message_id（参考实现 skip_serializing_if 省略）。
func TestBodyOmitsParentMessageID(t *testing.T) {
	var sawBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat_session/create"):
			fmt.Fprint(w, `{"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/create_pow_challenge"):
			fmt.Fprint(w, `{"data":{"biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"c1",
				"salt":"s1","signature":"sig1","difficulty":1000,"expire_at":1,"target_path":"/api/v0/chat/completion"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completion"):
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &sawBody)
			fmt.Fprint(w, "event: message\ndata: {\"p\":\"response/status\",\"v\":\"FINISHED\"}\n\n")
		default:
			fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	a.SetSolver(&stubSolver{})
	cred := &channel.Credential{UID: "u1", AccessToken: "tok", Extra: map[string]string{"device_id": "d"}}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{Model: "default"})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	for {
		if _, err := st.Next(); err == io.EOF {
			break
		} else if err != nil {
			break
		}
	}
	_ = st.Close()
	if _, exists := sawBody["parent_message_id"]; exists {
		t.Fatalf("首轮不应下发 parent_message_id（应为省略而非 null）：%v", sawBody)
	}
}

// 上游的 200 + 业务错误码要能被识别成失败（biz_code=11 设备指纹不对 / 10 封禁）。
func TestBizErrorSurfaces(t *testing.T) {
	err := bizError([]byte(`{"code":0,"data":{"biz_code":11,"biz_msg":"RISK_DEVICE_DETECTED"}}`), "取挑战失败")
	if k, _ := errs.KindOf(err); k != errs.AuthFailed {
		t.Fatalf("设备指纹问题应归一成 AuthFailed：%v", err)
	}
	err = bizError([]byte(`{"code":0,"data":{"biz_code":10,"biz_msg":"USER_IS_BANNED"}}`), "建会话失败")
	if k, _ := errs.KindOf(err); k != errs.SessionDead {
		t.Fatalf("封禁应归一成 SessionDead：%v", err)
	}
	// 信封级 40003 = userToken 失效（实测上游原话）：也要归一成 SessionDead，
	// 否则「重新粘一次 token」就能好的问题会被当成解析错误，把人引向查日志。
	err = bizError([]byte(`{"code":40003,"msg":"Authorization Failed (invalid token)","data":null}`), "建会话失败")
	if k, _ := errs.KindOf(err); k != errs.SessionDead {
		t.Fatalf("invalid token 应归一成 SessionDead：%v", err)
	}
}

// 真实 WASM 的求解自测：验证我们调 ABI 的参数顺序是对的（拿不到 WASM 就跳过，不联网）。
func TestPowSolverAgainstRealWASM(t *testing.T) {
	path := os.Getenv("DEEPSEEK_POW_WASM")
	if path == "" {
		path = "/tmp/pow.wasm"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("没有本地 WASM（%s），跳过 ABI 自测", path)
	}
	s, err := newSolver(context.Background(), raw)
	if err != nil {
		t.Fatalf("实例化求解器失败：%v", err)
	}
	defer s.Close(context.Background())

	ch := powChallenge{
		Algorithm: "DeepSeekHashV1", Challenge: strings.Repeat("ab", 16), Salt: "somesalt",
		Signature: "sig", Difficulty: 1000, ExpireAt: 1775380966945,
		TargetPath: powTargetPath,
	}
	answer, ok, err := s.solve1(context.Background(), ch)
	if err != nil {
		t.Fatalf("求解报错：%v", err)
	}
	if !ok {
		t.Fatal("低难度挑战应当能解出来（解不出来说明 ABI 参数顺序错了）")
	}
	if answer < 0 {
		t.Fatalf("答案应是非负数：%d", answer)
	}
	header, err := s.powHeader(context.Background(), ch)
	if err != nil {
		t.Fatalf("生成 PoW 头失败：%v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("PoW 头不是标准 base64：%v", err)
	}
	var payload map[string]any
	if json.Unmarshal(decoded, &payload) != nil {
		t.Fatalf("PoW 头不是 JSON：%s", decoded)
	}
	if len(payload) != 6 {
		t.Fatalf("PoW 头必须只含 6 个字段（不含 difficulty/expire_at）：%v", payload)
	}
	for _, k := range []string{"algorithm", "challenge", "salt", "answer", "signature", "target_path"} {
		if _, ok := payload[k]; !ok {
			t.Fatalf("PoW 头缺字段 %s：%v", k, payload)
		}
	}
}

func TestSpecUsesShim(t *testing.T) {
	a := New()
	sp := a.Spec()
	if sp.Tools || !sp.ToolsShim {
		t.Fatalf("网页协议没有原生工具调用，必须声明为模拟档：%+v", sp)
	}
	if sp.Category != channel.CategoryChat {
		t.Fatalf("登录式渠道应归 chat 类：%+v", sp)
	}
	if !sp.SSEOnly || sp.DefaultMinIntervalSec <= 0 {
		t.Fatalf("SSEOnly/最小间隔不对：%+v", sp)
	}
}

func TestClassify(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `unauthorized`, errs.SessionDead},
		{403, `forbidden`, errs.SessionDead},
		{429, `too many`, errs.SoftRate},
		{202, `<html>waf challenge</html>`, errs.UpstreamFault},
		{500, `boom`, errs.UpstreamFault},
		{418, `teapot`, errs.UpstreamFault}, // 非标准状态码只可能来自中间盒（WAF），同 202
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Errorf("Classify(%d) = %v，want %v", c.status, got, c.want)
		}
	}
}

// TestParseLoginToken 覆盖直登响应解析：成功取 token、失败把人话原因带出来。
func TestParseLoginToken(t *testing.T) {
	cases := []struct {
		name, raw, wantTok string
		wantMsgHas         string
	}{
		{"成功-user_token", `{"code":0,"data":{"biz_data":{"user_token":"tok-abc-12345678901234567890"}}}`, "tok-abc-12345678901234567890", ""},
		{"成功-token 别名", `{"code":0,"data":{"biz_data":{"token":"tok-xyz-12345678901234567890"}}}`, "tok-xyz-12345678901234567890", ""},
		{"密码错", `{"code":0,"data":{"biz_code":2,"biz_msg":"PASSWORD_OR_USER_NAME_IS_WRONG"}}`, "", "账号或密码不对"},
		{"风控", `{"code":0,"data":{"biz_code":7,"biz_msg":"RISK_DEVICE_DETECTED"}}`, "", "风控"},
		{"无法解析", `not json`, "", "无法解析"},
	}
	for _, c := range cases {
		tok, msg := parseLoginToken([]byte(c.raw))
		if tok != c.wantTok {
			t.Errorf("%s: token = %q，want %q", c.name, tok, c.wantTok)
		}
		if c.wantMsgHas != "" && !strings.Contains(msg, c.wantMsgHas) {
			t.Errorf("%s: 原因 = %q，应含 %q", c.name, msg, c.wantMsgHas)
		}
	}
}

// TestLoginFieldsDeclaresPasswordForm 校验直登表单声明了账号与密码两个必填项，
// 且密码框类型是 password（面板据此渲染，绝不显示明文）。
func TestLoginFieldsDeclaresPasswordForm(t *testing.T) {
	s := &session{}
	fields := s.LoginFields()
	if len(fields) != 3 {
		t.Fatalf("应有 3 个字段（账号/密码/记住密码），得到 %d", len(fields))
	}
	byName := map[string]channel.LoginField{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	acc, ok := byName[fieldAccount]
	if !ok || !acc.Required {
		t.Fatalf("账号字段应存在且必填：%+v", acc)
	}
	pw, ok := byName[fieldPassword]
	if !ok || pw.Type != "password" || !pw.Required {
		t.Fatalf("密码字段应为 password 类型且必填：%+v", pw)
	}
	if _, ok := byName[fieldRemember]; !ok {
		t.Fatal("应有「记住密码」字段（供自动续期）")
	}
}

// TestAcceptPasswordRejectsEmpty 校验空账号/空密码被当场拒绝（红线一：不静默接受）。
func TestAcceptPasswordRejectsEmpty(t *testing.T) {
	s := &session{}
	if err := s.AcceptPassword(map[string]string{fieldAccount: "", fieldPassword: "x"}); err == nil {
		t.Fatal("空账号必须报错")
	}
	if err := s.AcceptPassword(map[string]string{fieldAccount: "a@b.com", fieldPassword: ""}); err == nil {
		t.Fatal("空密码必须报错")
	}
	if err := s.AcceptPassword(map[string]string{fieldAccount: "a@b.com", fieldPassword: "pw"}); err != nil {
		t.Fatalf("填全了不该报错：%v", err)
	}
}

// TestMaskAccount 校验账号打码：邮箱保留前 2 位+域名、手机号保留前 3 后 2。
func TestMaskAccount(t *testing.T) {
	cases := map[string]string{
		"pengjincheng@qq.com": "pe***@qq.com",
		"13800001234":         "138****34",
		"ab@c.com":            "ab@c.com",
	}
	for in, want := range cases {
		if got := maskAccount(in); got != want {
			t.Errorf("maskAccount(%q) = %q，want %q", in, got, want)
		}
	}
}
