package deepseek

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
