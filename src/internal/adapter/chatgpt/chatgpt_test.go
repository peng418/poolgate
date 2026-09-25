package chatgpt

// chatgpt_test.go 端到端（mock 上游）+ 错误归一 + 登录 + 指纹。
//
// mock 上游按真实路径与形状返回，测试重点压四件最容易静默出错的事：
//  1. 三道门有没有真的走完（sentinel 的 p / PoW 头 / turnstile 头都要在对话请求上）；
//  2. PoW 头是不是**真的解出了难度**（测试用独立实现复算，不复用 pow.go）；
//  3. 对话请求体形态（action / model / force_use_sse / 消息结构）；
//  4. SSE 的正文与思考分流、用户消息快照不被当正文、错误归一。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const homeHTML = `<html data-build="c/abc123/_"><head>` +
	`<script src="/backend-api/sentinel/sdk.js"></script>` +
	`<script src="/_next/static/chunks/main-abc.js"></script>` +
	`</head><body></body></html>`

// fakeAccessToken 造一个能被解析的 accessToken（不校验签名，只读 payload）。
func fakeAccessToken(t *testing.T, accountID, email string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"exp":   float64(time.Now().Add(72 * time.Hour).Unix()),
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func writeSSE(w http.ResponseWriter, payloads ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, p := range payloads {
		_, _ = io.WriteString(w, "data: "+p+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// drain 读完一条流，汇总正文/思考/结束原因。
func drain(t *testing.T, st channel.Stream) (content, reasoning, finish string, err error) {
	t.Helper()
	for {
		c, e := st.Next()
		if e != nil {
			if e != io.EOF { // io.EOF 是流的正常结束
				err = e
			}
			break
		}
		for _, ch := range c.Choices {
			content += ch.Delta.Content
			reasoning += ch.Delta.ReasoningContent
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	return
}

// ---------------------------------------------------------------------------
// 端到端
// ---------------------------------------------------------------------------

func TestChatEndToEnd(t *testing.T) {
	const seed = "0.987654321"
	const difficulty = "0fffff"
	// 参考实现里的样例挑战：认证态用空密钥异或。
	tsDx := base64.StdEncoding.EncodeToString([]byte(`[[3,"ok"]]`))

	var deviceIDs, proofs, targetPaths []string
	var convHeaders http.Header
	var convBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, homeHTML)

		case epSentinel:
			deviceIDs = append(deviceIDs, r.Header.Get("OAI-Device-Id"))
			body, _ := io.ReadAll(r.Body)
			var req struct {
				P string `json:"p"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("sentinel 请求体不是 JSON：%v", err)
			}
			if !strings.HasPrefix(req.P, requirementsPrefix) {
				t.Errorf("sentinel 的 p 前缀不对：%q", truncate(req.P, 20))
			}
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(req.P, requirementsPrefix))
			if err != nil {
				t.Errorf("p 去掉前缀后不是 base64：%v", err)
			} else {
				var arr []any
				if json.Unmarshal(raw, &arr) != nil || len(arr) != 18 {
					t.Errorf("p 的载荷应是 18 元素 JSON 数组，得到 %s", truncate(string(raw), 120))
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w,
				`{"token":"req-token","proofofwork":{"required":true,"seed":%q,"difficulty":%q},"turnstile":{"required":true,"dx":%q}}`,
				seed, difficulty, tsDx)

		case epConversation:
			convHeaders = r.Header.Clone()
			targetPaths = append(targetPaths, r.Header.Get("X-OpenAI-Target-Path"))
			proofs = append(proofs, r.Header.Get("openai-sentinel-proof-token"))
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &convBody); err != nil {
				t.Errorf("对话请求体不是 JSON：%v", err)
			}
			writeSSE(w,
				// 用户消息快照：既不能被当正文发出去，也**不能**因为它的
				// status/end_turn 就判定「这一轮答完了」（用户那句话本来就"发完"了）
				`{"v":{"conversation_id":"c-1","message":{"author":{"role":"user"},"content":{"content_type":"text","parts":["你好"]},"status":"finished_successfully","end_turn":true}}}`,
				// 助手的一条 thoughts 消息结束：同样不能收工（后面还有正文）
				`{"v":{"conversation_id":"c-1","message":{"author":{"role":"assistant"},"content":{"content_type":"thoughts","parts":["先想想"]},"status":"finished_successfully"}}}`,
				// message_stream_complete 也是「答完了」信号，但不是流的结束
				`{"type":"message_stream_complete"}`,
				// patch 增量：正文
				`{"v":"北京","p":"/message/content/parts/0","o":"append"}`,
				// patch 增量：思考
				`{"v":"今天","p":"/message/content/thoughts/content","o":"append"}`,
				`{"v":"是晴天","p":"/message/content/parts/0","o":"append"}`,
				// 整条消息快照：与已有正文重复，不该重复发
				`{"v":{"conversation_id":"c-1","message":{"author":{"role":"assistant"},"content":{"content_type":"text","parts":["北京是晴天"]},"status":"finished_successfully"}}}`,
				"[DONE]",
			)

		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	tok := fakeAccessToken(t, "acc-1", "someone@example.com")
	cred := &channel.Credential{UID: "chatgpt-acc-1", AccessToken: tok}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model:    "gpt-5-3",
		Messages: []channel.Message{{Role: "user", Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	content, reasoning, finish, err := drain(t, st)
	if err != nil {
		t.Fatalf("读流失败：%v", err)
	}
	if content != "北京是晴天" {
		t.Fatalf("正文不对：%q（期望 北京是晴天 —— 用户消息快照被当成正文了？）", content)
	}
	// 思考 = thoughts 消息快照里的「先想想」+ thoughts/content 的增量「今天」。
	if reasoning != "先想想今天" {
		t.Fatalf("思考分流不对：%q（期望 先想想今天）", reasoning)
	}
	if finish != "stop" {
		t.Fatalf("结束原因不对：%q", finish)
	}

	// ---- 对话请求的头
	if got := convHeaders.Get("Authorization"); got != "Bearer "+tok {
		t.Errorf("Authorization 不对：%q", got)
	}
	if got := convHeaders.Get("openai-sentinel-chat-requirements-token"); got != "req-token" {
		t.Errorf("缺 sentinel token 头：%q", got)
	}
	if got := convHeaders.Get("openai-sentinel-turnstile-token"); got != "b2s=" {
		t.Errorf("turnstile 头不对：%q", got)
	}
	if got := convHeaders.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept 不对：%q", got)
	}
	proof := convHeaders.Get("openai-sentinel-proof-token")
	if !strings.HasPrefix(proof, powPrefix) {
		t.Fatalf("PoW 头前缀不对：%q", truncate(proof, 20))
	}
	// 用独立实现复算：这个 proof 必须真的满足上游给的难度。
	verifyAgainstDifficulty(t, seed, difficulty, strings.TrimPrefix(proof, powPrefix))
	if len(targetPaths) != 1 || targetPaths[0] != epConversation {
		t.Errorf("X-OpenAI-Target-Path 不对：%v", targetPaths)
	}

	// ---- 对话请求体
	if convBody["action"] != "next" {
		t.Errorf("action 不对：%v", convBody["action"])
	}
	if convBody["model"] != "gpt-5" { // gpt-5-3 → gpt-5 + 高思考
		t.Errorf("模型 slug 不对：%v", convBody["model"])
	}
	if convBody["thinking_effort"] != "high" {
		t.Errorf("思考强度不对：%v", convBody["thinking_effort"])
	}
	if convBody["force_use_sse"] != true || convBody["supports_buffering"] != true {
		t.Errorf("流式标志不对：force_use_sse=%v supports_buffering=%v",
			convBody["force_use_sse"], convBody["supports_buffering"])
	}
	mode, _ := convBody["conversation_mode"].(map[string]any)
	if mode["kind"] != "primary_assistant" {
		t.Errorf("conversation_mode 不对：%v", convBody["conversation_mode"])
	}
	if pid, _ := convBody["parent_message_id"].(string); pid == "" {
		t.Error("缺 parent_message_id")
	}
	if wid, _ := convBody["websocket_request_id"].(string); wid == "" {
		t.Error("缺 websocket_request_id")
	}
	info, _ := convBody["client_contextual_info"].(map[string]any)
	if info["app_name"] != "chatgpt.com" {
		t.Errorf("client_contextual_info 不对：%v", info)
	}
	msgs, _ := convBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 应恰好 1 条，得到 %d", len(msgs))
	}
	m0, _ := msgs[0].(map[string]any)
	author, _ := m0["author"].(map[string]any)
	if author["role"] != "user" {
		t.Errorf("消息角色不对：%v", m0["author"])
	}
	content0, _ := m0["content"].(map[string]any)
	if content0["content_type"] != "text" {
		t.Errorf("content_type 不对：%v", content0["content_type"])
	}
	if parts, _ := content0["parts"].([]any); len(parts) != 1 || parts[0] != "你好" {
		t.Errorf("消息内容不对：%v", content0["parts"])
	}

	// ---- 指纹持久性：第二次请求必须用同一个设备号（不能每请求随机）
	st2, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "auto", Messages: []channel.Message{{Role: "user", Content: "再来"}},
	})
	if err != nil {
		t.Fatalf("第二次 Chat 失败：%v", err)
	}
	_, _, _, _ = drain(t, st2)
	_ = st2.Close()
	if len(deviceIDs) != 2 {
		t.Fatalf("应记录到 2 次 sentinel，得到 %d", len(deviceIDs))
	}
	if deviceIDs[0] == "" || deviceIDs[0] != deviceIDs[1] {
		t.Fatalf("设备指纹必须稳定：%v", deviceIDs)
	}
}

// 凭证里存了指纹时，必须用存的那个（而不是派生值）。
func TestChatUsesPersistedFingerprint(t *testing.T) {
	var gotDeviceID, gotClientVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, homeHTML)
		case epSentinel:
			gotDeviceID = r.Header.Get("OAI-Device-Id")
			gotClientVersion = r.Header.Get("OAI-Client-Version")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"req-token"}`)
		case epConversation:
			writeSSE(w, `{"v":{"message":{"author":{"role":"assistant"},"content":{"content_type":"text","parts":["好"]},"end_turn":true}}}`, "[DONE]")
		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{
		UID: "chatgpt-acc-2", AccessToken: fakeAccessToken(t, "acc-2", "a@b.c"),
		Extra: map[string]string{fpDeviceID: "fixed-device-id", fpClientVersion: "prod-fixed"},
	}
	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "auto", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	_, _, _, _ = drain(t, st)
	if gotDeviceID != "fixed-device-id" {
		t.Fatalf("应使用存档的设备号，得到 %q", gotDeviceID)
	}
	if gotClientVersion != "prod-fixed" {
		t.Fatalf("应使用存档的版本号，得到 %q", gotClientVersion)
	}
}

// 对话 401：必须归一成 SessionDead，并说清楚「重新粘贴」。
func TestChatUnauthorizedIsSessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, homeHTML)
		case epSentinel:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"req-token"}`)
		case epConversation:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"detail":"unauthorized"}`)
		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "chatgpt-acc-3", AccessToken: fakeAccessToken(t, "acc-3", "a@b.c")}
	_, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "auto", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("401 应当报错")
	}
	k, _ := errs.KindOf(err)
	if k != errs.SessionDead {
		t.Fatalf("401 应归一成 SessionDead，得到 %v（%v）", k, err)
	}
	if !strings.Contains(err.Error(), "重新粘贴") {
		t.Fatalf("错误里应告诉用户下一步做什么：%v", err)
	}
}

// 403 要看报文区分：风控/人机校验 ≠ 凭证失效（后者会**禁用**账号，误判代价很大）。
func TestClassify403SplitsAuthAndChallenge(t *testing.T) {
	a := New()
	if k := a.Classify(403, []byte(`{"detail":"Chat verification required"}`)); k != errs.UpstreamFault {
		t.Fatalf("人机校验类的 403 应判上游故障，得到 %v", k)
	}
	if k := a.Classify(403, []byte(`{"detail":"invalid access token"}`)); k != errs.SessionDead {
		t.Fatalf("凭证类的 403 应判 SessionDead，得到 %v", k)
	}
}

// arkose 要求时明确失败（参考实现也没实现）。
func TestArkoseRequiredFailsExplicitly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, homeHTML)
		case epSentinel:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"req-token","arkose":{"required":true}}`)
		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "chatgpt-acc-4", AccessToken: fakeAccessToken(t, "acc-4", "a@b.c")}
	_, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "auto", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("arkose 要求时应当明确报错，而不是静默降级")
	}
	if !strings.Contains(err.Error(), "arkose") {
		t.Fatalf("错误里应点名 arkose：%v", err)
	}
	// 这一条不是账号问题（不该把好号冷却/禁用）。
	if k, _ := errs.KindOf(err); k.AccountBlamed() {
		t.Fatalf("arkose 未实现不该算账号的错，得到 %v", k)
	}
}

// turnstile 下发的程序解不出来时：明确报错，不发空 token 硬闯。
func TestTurnstileUnsolvableFailsExplicitly(t *testing.T) {
	badDx := base64.StdEncoding.EncodeToString([]byte(`this is not an opcode program`))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, homeHTML)
		case epSentinel:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w,
				`{"token":"req-token","turnstile":{"required":true,"dx":%q}}`, badDx)
		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "chatgpt-acc-5", AccessToken: fakeAccessToken(t, "acc-5", "a@b.c")}
	_, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "auto", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("turnstile 解不出来时应当明确报错")
	}
	if !strings.Contains(err.Error(), "turnstile") {
		t.Fatalf("错误里应点名 turnstile：%v", err)
	}
}

// 挑战接口没给 token：也必须是明确失败。
func TestSentinelMissingTokenFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, homeHTML)
		case epSentinel:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "chatgpt-acc-6", AccessToken: fakeAccessToken(t, "acc-6", "a@b.c")}
	_, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model: "auto", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "sentinel token") {
		t.Fatalf("缺 token 时应明确报错：%v", err)
	}
}

// ---------------------------------------------------------------------------
// SSE 解析（直接用构造的流，不经 HTTP）
// ---------------------------------------------------------------------------

func TestStreamRoutesThoughtsAndBlocksModeration(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"v":"","p":"/message/content/parts/0","o":"append"}`,
		`data: {"v":"答","p":"/message/content/parts/0","o":"append"}`,
		`data: {"v":"思考","p":"/message/content/thoughts/summary","o":"append"}`,
		`data: {"v":"中","p":"/message/content/thoughts/content","o":"append"}`,
		`data: {"v":"案","p":"/message/content/parts/0","o":"append"}`,
		`data: {"v":{"message":{"author":{"role":"assistant"},"content":{"content_type":"text","parts":["答案"]},"end_turn":true}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	st := newStream(io.NopCloser(strings.NewReader(sse)), "auto")
	content, reasoning, finish, err := drain(t, st)
	if err != nil {
		t.Fatalf("读流失败：%v", err)
	}
	// 摘要与思考正文各按自己的累积器走，都落到 reasoning_content。
	if content != "答案" {
		t.Fatalf("正文不对：%q", content)
	}
	if reasoning != "思考中" {
		t.Fatalf("思考不对：%q", reasoning)
	}
	if finish != "stop" {
		t.Fatalf("结束原因不对：%q", finish)
	}

	// 审核拦截：归一成 ContentBlocked（会原样透传给客户端）。
	mod := `data: {"type":"moderation","moderation_response":{"blocked":true}}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	st2 := newStream(io.NopCloser(strings.NewReader(mod)), "auto")
	_, _, _, err = drain(t, st2)
	if k, ok := errs.KindOf(err); !ok || k != errs.ContentBlocked {
		t.Fatalf("审核拦截应归一成 ContentBlocked，得到 %v（%v）", k, err)
	}
}

// 帧内身份错误 → SessionDead；帧内其它错误 → 上游故障。
func TestStreamFrameErrors(t *testing.T) {
	cases := []struct {
		payload string
		want    errs.Kind
	}{
		{`{"error":{"message":"invalid access token"}}`, errs.SessionDead},
		{`{"error":{"message":"upstream exploded"}}`, errs.UpstreamFault},
	}
	for _, c := range cases {
		st := newStream(io.NopCloser(strings.NewReader("data: "+c.payload+"\n\n")), "auto")
		_, _, _, err := drain(t, st)
		if k, ok := errs.KindOf(err); !ok || k != c.want {
			t.Fatalf("%s 应归一成 %v，得到 %v（%v）", c.payload, c.want, k, err)
		}
	}
}

// 引用/实体标记必须剥掉：留着客户端会看到一串看不见的码点。
func TestNormalizeMarkup(t *testing.T) {
	in := "答案来自 entity[\"song\",\"Night Trouble\",\"Klangkarussell\"] 以及 citeturn0search4。"
	got := normalizeMarkup(in)
	if got != "答案来自 Night Trouble - Klangkarussell 以及 。" {
		t.Fatalf("标记剥离不对：%q", got)
	}
	// 半截标记（流被切开）必须先丢掉尾段，等下一片。
	if got := normalizeMarkup("前 ent"); got != "前 " {
		t.Fatalf("半截标记处理不对：%q", got)
	}
}

// ---------------------------------------------------------------------------
// 登录 / 指纹
// ---------------------------------------------------------------------------

func TestExtractAccessTokenVariants(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature"
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"裸串", jwt, jwt},
		{"带引号", `"` + jwt + `"`, jwt},
		{"赋值形式", "accessToken = " + jwt, jwt},
		{"session JSON", `{"accessToken":"` + jwt + `","user":{"id":"u1"}}`, jwt},
		{"从 DevTools 连上下文复制", `  accessToken: "` + jwt + `"  `, jwt},
		{"误粘多词文本", "请在浏览器里打开 chatgpt.com 然后搜索", ""},
		{"空", "   ", ""},
	}
	for _, c := range cases {
		if got := extractAccessToken(c.raw); got != c.want {
			t.Fatalf("%s：得到 %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestLoginPollBuildsCredentialWithFingerprint(t *testing.T) {
	tok := fakeAccessToken(t, "acc-9", "nine@example.com")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != epMe {
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+tok {
			t.Errorf("校验凭证应带 Bearer：%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"email":"nine@example.com"}`)
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	if sess.AuthURL() != loginPage {
		t.Fatalf("AuthURL 不对：%s", sess.AuthURL())
	}
	// 还没粘贴 → ErrPending（正常的等待态，不是错误）。
	if _, err := sess.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("未粘贴时应返回 ErrPending，得到 %v", err)
	}
	// Hint 必须把「怎么拿 accessToken」说清楚。
	hinter, ok := sess.(interface{ Hint() string })
	if !ok || !strings.Contains(hinter.Hint(), "accessToken") || !strings.Contains(hinter.Hint(), "fetch") {
		t.Fatalf("Hint 引导语不清楚：%v", ok)
	}
	acceptor, ok := sess.(channel.CallbackAcceptor)
	if !ok {
		t.Fatal("应实现 CallbackAcceptor（面板的粘贴入口靠它）")
	}
	if err := acceptor.AcceptCallback("accessToken = " + tok); err != nil {
		t.Fatalf("AcceptCallback 失败：%v", err)
	}
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if cred.AccessToken != tok {
		t.Fatalf("AccessToken 不对：%q", cred.AccessToken)
	}
	if cred.UID != "chatgpt-acc-9" {
		t.Fatalf("UID 应取 JWT 里的 account_id：%q", cred.UID)
	}
	if cred.Nickname != "nine@example.com" {
		t.Fatalf("昵称应取邮箱：%q", cred.Nickname)
	}
	if cred.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt 应从 JWT 的 exp 解析出来（面板要据此提前提示过期）")
	}
	for _, k := range []string{fpDeviceID, fpSessionID, fpClientVersion, fpClientBuild, fpUserAgent, fpSecCHUA} {
		if strings.TrimSpace(cred.Extra[k]) == "" {
			t.Fatalf("指纹键 %q 应写进凭证（必须持久化，不能每请求随机）", k)
		}
	}
	// 再 Poll 一次：已经完成过了。
	if _, err := sess.Poll(context.Background()); err == nil {
		t.Fatal("第二次 Poll 应报「已完成」")
	}
}

func TestEnsureFingerprintIsStableAndIdempotent(t *testing.T) {
	c1 := &channel.Credential{UID: "chatgpt-same"}
	ensureFingerprint(c1)
	before := c1.Extra[fpDeviceID]
	if before == "" {
		t.Fatal("应派生设备号")
	}
	// 幂等：已有值绝不能被覆盖（覆盖等于悄悄换设备）。
	c1.Extra[fpDeviceID] = "manual"
	ensureFingerprint(c1)
	if c1.Extra[fpDeviceID] != "manual" {
		t.Fatal("已有指纹不该被覆盖")
	}
	// 派生值必须稳定且与账号绑定。
	c2 := &channel.Credential{UID: "chatgpt-same"}
	ensureFingerprint(c2)
	if c2.Extra[fpDeviceID] != before {
		t.Fatalf("同账号派生值必须一致：%q vs %q", c2.Extra[fpDeviceID], before)
	}
	c3 := &channel.Credential{UID: "chatgpt-other"}
	ensureFingerprint(c3)
	if c3.Extra[fpDeviceID] == before {
		t.Fatal("不同账号不该共用同一个设备号")
	}
	if !strings.Contains(c3.Extra[fpDeviceID], "-") {
		t.Fatalf("设备号应是 UUID 形状：%q", c3.Extra[fpDeviceID])
	}
}

// ---------------------------------------------------------------------------
// 目录 / 错误归一
// ---------------------------------------------------------------------------

func TestModelsFallsBackToLocalList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, homeHTML)
		case "/backend-api/models":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"detail":"boom"}`)
		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "chatgpt-acc-7", AccessToken: fakeAccessToken(t, "acc-7", "a@b.c")}
	list, err := a.Models(context.Background(), cred)
	if err != nil {
		t.Fatalf("目录不可用时应退回内置清单而不是报错：%v", err)
	}
	if len(list) == 0 || list[0].Source != channel.SourceLocal {
		t.Fatalf("退回的清单应标注为本地来源：%+v", list)
	}
}

func TestModelsFromUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, homeHTML)
		case "/backend-api/models":
			if got := r.Header.Get("X-OpenAI-Target-Path"); got != "/backend-api/models" {
				t.Errorf("目标路径不该带 query：%q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-5-2","title":"GPT-5"},{"slug":"gpt-5-2"},{"slug":"o3"}]}`)
		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "chatgpt-acc-8", AccessToken: fakeAccessToken(t, "acc-8", "a@b.c")}
	list, err := a.Models(context.Background(), cred)
	if err != nil {
		t.Fatalf("Models 失败：%v", err)
	}
	if len(list) != 2 { // 去重后两条
		t.Fatalf("应去重成 2 条，得到 %d：%+v", len(list), list)
	}
	if list[0].Source != channel.SourceUpstream {
		t.Fatalf("上游目录应标注来源：%v", list[0].Source)
	}
	if list[0].Tools != channel.CapYes || list[0].Images != channel.CapNo {
		t.Fatalf("能力位不对：%+v", list[0])
	}
}

func TestClassifyTable(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{}`, errs.SessionDead},
		{403, `{"detail":"invalid token"}`, errs.SessionDead},
		{403, `{"detail":"cloudflare challenge"}`, errs.UpstreamFault},
		{429, `{"detail":"rate limited"}`, errs.SoftRate},
		{429, `{"detail":"usage limit reached"}`, errs.HardCredit},
		{400, `{"detail":"context too long"}`, errs.PromptTooLong},
		{400, `{"detail":"moderation flagged"}`, errs.ContentBlocked},
		{404, `{"detail":"model not found"}`, errs.ModelUnavailable},
		{503, `{}`, errs.UpstreamFault},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("status=%d body=%s：期望 %v 得到 %v", c.status, c.body, c.want, got)
		}
	}
}

func TestSpecCapabilityBitsAreHonest(t *testing.T) {
	spec := New().Spec()
	if spec.Tools {
		t.Error("网页协议没有原生工具调用，Tools 必须是 false")
	}
	if !spec.ToolsShim {
		t.Error("无原生工具调用时必须声明 ToolsShim=true，否则网关无法代做模拟")
	}
	if !spec.Reasoning {
		t.Error("思考单独分流，Reasoning 必须是 true")
	}
	if spec.Images {
		t.Error("本适配器没实现图片上传，Images 必须是 false")
	}
	if !spec.SSEOnly {
		t.Error("上游只给 SSE")
	}
	if spec.Kind != channel.ChatGPT {
		t.Errorf("Kind 不对：%v", spec.Kind)
	}
	if spec.DefaultMinIntervalSec <= 0 {
		t.Error("网页渠道必须给一个非零的同号最小间隔默认值")
	}
}

func TestNormalizeModel(t *testing.T) {
	cases := []struct {
		in     string
		slug   string
		effort string
	}{
		{"", "auto", ""},
		{"auto", "auto", ""},
		{"gpt-5-1", "gpt-5", "low"},
		{"gpt-5-2", "gpt-5", "medium"},
		{"gpt-5-3", "gpt-5", "high"},
		{"gpt-5-mini", "gpt-5", "low"},
		{"some-future-slug", "some-future-slug", ""}, // 认不出就原样透传，不偷偷换成 auto
	}
	for _, c := range cases {
		got := normalizeModel(c.in)
		if got.slug != c.slug || got.effort != c.effort {
			t.Fatalf("%q：得到 %+v，期望 slug=%s effort=%s", c.in, got, c.slug, c.effort)
		}
	}
}

func TestTargetPathStripsQueryAndHost(t *testing.T) {
	if got := targetPath("https://chatgpt.com/backend-api/models?x=1"); got != "/backend-api/models" {
		t.Fatalf("得到 %q", got)
	}
	if got := targetPath("https://chatgpt.com/"); got != "/" {
		t.Fatalf("得到 %q", got)
	}
}

func TestBalanceAndCheckinDoNotInventNumbers(t *testing.T) {
	a := New()
	bal, err := a.Balance(context.Background(), nil)
	if err != nil || bal.Known {
		t.Fatalf("余额应标注为「未知」而不是编一个数字：%+v（%v）", bal, err)
	}
	ck, err := a.Checkin(context.Background(), nil)
	if err != nil || !ck.NoActivity {
		t.Fatalf("无签到活动应如实标注：%+v（%v）", ck, err)
	}
}

// Refresh 必须明确表示「刷不了」（返回 nil, nil），而不是假装成功。
func TestRefreshIsHonestlyUnavailable(t *testing.T) {
	a := New()
	nc, err := a.Refresh(context.Background(), &channel.Credential{AccessToken: "x"})
	if err != nil || nc != nil {
		t.Fatalf("网页版没有 refresh token，应返回 (nil, nil)：%+v（%v）", nc, err)
	}
}
