package lingma

// lingma_test.go 覆盖这条链路里最容易出错、且错了最难查的四件事：
//  1. **COSY 签名**：md5 原文的拼接顺序（尤其 path 要去掉 /algo）—— 错了上游只回 403，
//     本地看不出任何异常。测试里用**独立复算**的 md5 逐字节比对。
//  2. **cache/user 解密**：AES-128-CBC + key=IV=machineID[:16]，密钥错时不会报解密错，
//     只会让填充校验失败 —— 必须能区分这两种失败。
//  3. **双层嵌套 SSE**：少剥一层壳，客户端一个字都收不到；tool_calls 分片要逐片透传。
//  4. **错误归一**：429 的「额度不足」与「限流」必须分对，否则用户要么白等、要么白查。

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/store"
)

// testCred 造一份固定的签名素材（cosy_key 用假值，但形状与真的一致）。
func testCred() lingmaCred {
	return lingmaCred{
		CosyKey:   "Q09TWUtFWUZBS0VLZXlGb3JUZXN0MDAwMDAwMDA=",
		Info:      "aW5mb19jaXBoZXJfdGV4dF9mYWtlX2Jhc2U2NF9ibG9iX3N0cmluZw==",
		MachineID: "0123456789abcdef0123456789abcdef",
		UserID:    "user-12345678",
	}
}

// ---------------------------------------------------------------------------
// 1. COSY 签名
// ---------------------------------------------------------------------------

// 用与实现不同的代码路径（encoding/json 会对 map 键排序）独立复算 md5，与实现产物比对。
func TestCosySignatureRecomputed(t *testing.T) {
	lc := testCred()
	const body = `{"request_id":"abc","stream":true}`
	rawURL := "https://lingma.alibabacloud.com" + epChat + chatQuery

	req, err := http.NewRequest(http.MethodPost, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lc.signer().applyHeaders(req, body, rawURL, "text/event-stream"); err != nil {
		t.Fatalf("applyHeaders: %v", err)
	}

	auth := req.Header.Get("Authorization")
	parts := strings.Split(auth, ".")
	if len(parts) != 3 || parts[0] != "Bearer COSY" {
		t.Fatalf("Authorization 形态不对：%q", auth)
	}
	payloadB64, sig := parts[1], parts[2]

	// 签名 payload 必须是排序紧凑、且字段齐全。
	rawPayload, err := base64.StdEncoding.DecodeString(payloadB64)
	if err != nil {
		t.Fatalf("payload 不是合法 base64：%v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		t.Fatalf("payload 不是 JSON：%v", err)
	}
	if payload["cosyVersion"] != cosyVersion || payload["version"] != "v1" || payload["ideVersion"] != "" {
		t.Fatalf("payload 字段不对：%v", payload)
	}
	if payload["info"] != lc.Info {
		t.Fatalf("payload.info 必须等于 encrypt_user_info，得到 %q", payload["info"])
	}
	if payload["requestId"] == "" {
		t.Fatal("payload.requestId 不能为空")
	}

	// 独立复算：json.Marshal 对 map 键排序，与实现的 jsonSortedCompact 等价。
	canonical, _ := json.Marshal(payload)
	if string(canonical) != string(rawPayload) {
		t.Fatalf("payload 不是排序紧凑形态：\n got %s\nwant %s", rawPayload, canonical)
	}

	date := req.Header.Get("Cosy-Date")
	if date == "" {
		t.Fatal("缺 Cosy-Date")
	}
	// path 必须去掉 /algo、且不含查询串。
	signedPath, err := signPath(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if signedPath != "/api/v2/service/pro/sse/agent_chat_generation" {
		t.Fatalf("参与签名的 path 不对：%q", signedPath)
	}
	preimage := strings.Join([]string{payloadB64, lc.CosyKey, date, body, signedPath}, "\n")
	sum := md5.Sum([]byte(preimage))
	if got := hex.EncodeToString(sum[:]); got != sig {
		t.Fatalf("签名复算不一致：\n got %s\nwant %s", sig, got)
	}

	// 反证：如果 path 忘了去 /algo，签名就对不上（这正是最常见的 bug）。
	wrong := strings.Join([]string{payloadB64, lc.CosyKey, date, body, "/algo/api/v2/service/pro/sse/agent_chat_generation"}, "\n")
	wsum := md5.Sum([]byte(wrong))
	if hex.EncodeToString(wsum[:]) == sig {
		t.Fatal("带 /algo 的 path 竟算出同一个签名：path 归一没生效")
	}

	// 配套头必须齐全。
	for k, want := range map[string]string{
		"Cosy-Key":        lc.CosyKey,
		"Cosy-User":       lc.UserID,
		"Cosy-Machineid":  lc.MachineID,
		"Cosy-Version":    cosyVersion,
		"Cosy-Clienttype": cosyClientType,
		"Cosy-Clientip":   cosyClientIP,
		"Appcode":         appcode,
	} {
		if got := req.Header.Get(k); got != want {
			t.Fatalf("头 %s = %q，期望 %q", k, got, want)
		}
	}
	if req.Header.Get("Cosy-Machineos") == "" {
		t.Fatal("缺 Cosy-Machineos")
	}
}

func TestSignPathStripsAlgoAndQuery(t *testing.T) {
	cases := map[string]string{
		"https://lingma.alibabacloud.com" + epChat + chatQuery: "/api/v2/service/pro/sse/agent_chat_generation",
		"https://lingma.alibabacloud.com" + epModels:           "/api/v2/model/list",
	}
	for in, want := range cases {
		got, err := signPath(in)
		if err != nil {
			t.Fatalf("signPath(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("signPath(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. cache/user 解密
// ---------------------------------------------------------------------------

// encryptCacheUser 是测试侧的独立加密实现（与实现的解密方向相反，逐字节自洽即为通过）。
func encryptCacheUser(t *testing.T, machineID string, plain []byte) string {
	t.Helper()
	if len(machineID) < aesBlockSize {
		t.Fatal("machineID 太短")
	}
	key := []byte(machineID[:aesBlockSize])
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out)
}

func TestDecryptCacheUserRoundTrip(t *testing.T) {
	machineID := "1234567890abcdef1234567890abcdef"
	plain := []byte(`{"key":"cosy-key","encrypt_user_info":"info-blob","uid":"u-1","expire_time":"9999999999999"}`)
	enc := encryptCacheUser(t, machineID, plain)

	got, err := decryptCacheUser(machineID, mustB64(t, enc))
	if err != nil {
		t.Fatalf("解密失败：%v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("明文不一致：\n got %s\nwant %s", got, plain)
	}

	// 密钥错（machineID 与密文不匹配）必须在填充校验处报错。
	if _, err := decryptCacheUser("ffffffffffffffffffffffffffffffff", mustB64(t, enc)); err == nil {
		t.Fatal("machineID 不匹配时应报解密错误")
	}
	// 密文长度不是 16 的倍数：直接报错，不去做 CBC。
	if _, err := decryptCacheUser(machineID, []byte("short")); err == nil {
		t.Fatal("非法密文长度应报错")
	}
	// machineID 太短：不能当密钥。
	if _, err := decryptCacheUser("short", mustB64(t, enc)); err == nil {
		t.Fatal("machineID 过短应报错")
	}
}

func TestDecodeCacheUser(t *testing.T) {
	machineID := "1234567890abcdef1234567890abcdef"
	plain := []byte(`{"key":"cosy-key","encrypt_user_info":"info-blob","uid":"u-1"}`)
	enc := encryptCacheUser(t, machineID, plain)

	lc, err := decodeCacheUser("\n  "+enc+"  \n", machineID)
	if err != nil {
		t.Fatalf("decodeCacheUser: %v", err)
	}
	if lc.CosyKey != "cosy-key" || lc.Info != "info-blob" || lc.UserID != "u-1" || lc.MachineID != machineID {
		t.Fatalf("字段抽取错误：%+v", lc)
	}

	// 解密成功但字段缺失：必须报「字段不全」，而不是放一个半残凭证过去。
	missing := encryptCacheUser(t, machineID, []byte(`{"key":"only-key"}`))
	if _, err := decodeCacheUser(missing, machineID); err == nil {
		t.Fatal("字段不全时应报错")
	}
	// 根本不是 base64。
	if _, err := decodeCacheUser("!!!not-base64!!!", machineID); err == nil {
		t.Fatal("非 base64 输入应报错")
	}
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// 3. 粘贴解析
// ---------------------------------------------------------------------------

func TestAcceptCallbackForms(t *testing.T) {
	machineID := "1234567890abcdef1234567890abcdef"
	plain := []byte(`{"key":"cosy-key","encrypt_user_info":"info-blob","uid":"u-1"}`)
	enc := encryptCacheUser(t, machineID, plain)

	cases := []struct {
		name string
		raw  string
		want func(*session) bool
	}{
		{
			name: "导出的凭证 JSON（嵌套形）",
			raw:  `{"source":"x","auth":{"cosy_key":"k","encrypt_user_info":"i","user_id":"u","machine_id":"m"}}`,
			want: func(s *session) bool { return s.ready.CosyKey == "k" && s.ready.UserID == "u" },
		},
		{
			name: "两段式 JSON",
			raw:  `{"user":"` + enc + `","id":"` + machineID + `"}`,
			want: func(s *session) bool { return s.userContent == enc && s.machineID == machineID },
		},
		{
			name: "分隔行两段",
			raw:  enc + "\n" + pasteSeparator + "\n" + machineID,
			want: func(s *session) bool { return s.userContent == enc && s.machineID == machineID },
		},
		{
			name: "单段密文（先粘 cache/user）",
			raw:  enc,
			want: func(s *session) bool { return s.userContent == enc && s.machineID == "" },
		},
		{
			name: "单段 machineID（再粘 cache/id）",
			raw:  machineID,
			want: func(s *session) bool { return s.machineID == machineID && s.userContent == "" },
		},
	}
	for _, c := range cases {
		s := &session{a: New()}
		if err := s.AcceptCallback(c.raw); err != nil {
			t.Fatalf("%s: AcceptCallback 失败：%v", c.name, err)
		}
		if !c.want(s) {
			t.Fatalf("%s: 状态不对 (user=%q id=%q ready=%+v)", c.name, s.userContent, s.machineID, s.ready)
		}
	}

	// 认不出的内容必须当场报错（红线一）。
	s := &session{a: New()}
	if err := s.AcceptCallback("这是一段随手粘的中文，既不是 base64 也不是 machineID 的形态，并且很长很长很长很长很长很长很长很长很长很长很长很长"); err == nil {
		t.Fatal("无法识别的粘贴内容应报错")
	}
	if err := s.AcceptCallback("   "); err == nil {
		t.Fatal("空内容应报错")
	}
}

// ---------------------------------------------------------------------------
// 4. 模型目录
// ---------------------------------------------------------------------------

func TestParseModelList(t *testing.T) {
	// 参考实现实测形态：chat + inline，只保留 enable 的条目。
	raw := []byte(`{
		"chat":[{"key":"kmodel","display_name":"Kimi-K2.6","enable":true},
		         {"key":"disabled","display_name":"X","enable":false}],
		"inline":[{"key":"Qwen3-Thinking","display_name":"Qwen3 Thinking","enable":true}]
	}`)
	entries, err := parseModelList(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("应保留 2 个 enable 条目，得到 %d：%+v", len(entries), entries)
	}
	infos := toModelInfos(entries)
	if infos[0].ID != "kimi-k2.6" {
		t.Fatalf("display_name 归一化不对：%q", infos[0].ID)
	}
	if infos[0].Source != channel.SourceUpstream {
		t.Fatal("模型来源应标 upstream")
	}
	if infos[0].Tools != channel.CapUnknown || infos[0].Reasoning != channel.CapUnknown {
		t.Fatal("上游没给能力位时必须标 CapUnknown，不许猜")
	}
	if infos[0].ContextWindow != 0 {
		t.Fatal("上游没给上下文长度，不许编数字")
	}

	// 顶层数组形态。
	if _, err := parseModelList([]byte(`[{"key":"a","display_name":"A","enable":true}]`)); err != nil {
		t.Fatalf("顶层数组形态应可解析：%v", err)
	}
	// 完全没有可用条目 → 报错，而不是回一张空表当成成功。
	if _, err := parseModelList([]byte(`{"chat":[{"key":"x","enable":false}]}`)); err == nil {
		t.Fatal("无可用条目应报错")
	}
}

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"Kimi-K2.6":      "kimi-k2.6",
		"Qwen3 Thinking": "qwen3-thinking",
		"  Auto  ":       "auto",
		"Deep_Qwen 3":    "deep-qwen-3",
	}
	for in, want := range cases {
		if got := normalizeModelName(in); got != want {
			t.Fatalf("normalizeModelName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. 双层嵌套 SSE
// ---------------------------------------------------------------------------

// sseEnvelope 造一行 data: 事件（外层信封 + 内层 OpenAI 分片）。
func sseEnvelope(t *testing.T, status int, inner any) string {
	t.Helper()
	body := ""
	if inner != nil {
		raw, err := json.Marshal(inner)
		if err != nil {
			t.Fatal(err)
		}
		body = string(raw)
	}
	raw, err := json.Marshal(map[string]any{"body": body, "statusCodeValue": status})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(raw) + "\n\n"
}

func collect(t *testing.T, s channel.Stream) (content string, calls []channel.ToolCall, finish string, err error) {
	t.Helper()
	var cb strings.Builder
	for {
		c, e := s.Next()
		if e != nil {
			if e == io.EOF {
				return cb.String(), calls, finish, nil
			}
			return cb.String(), calls, finish, e
		}
		for _, ch := range c.Choices {
			cb.WriteString(ch.Delta.Content)
			calls = append(calls, ch.Delta.ToolCalls...)
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
}

func TestStreamDoubleNestedSSE(t *testing.T) {
	var buf bytes.Buffer
	// 第一片带 role，后面只有内容。
	buf.WriteString(sseEnvelope(t, 200, map[string]any{"id": "chatcmpl-1", "choices": []any{
		map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "你"}}}}))
	buf.WriteString(sseEnvelope(t, 200, map[string]any{"choices": []any{
		map[string]any{"index": 0, "delta": map[string]any{"content": "好"}}}}))
	// 一层心跳（body 为空）不能当内容。
	buf.WriteString(`data: {"body":"","statusCodeValue":0}` + "\n\n")
	// 一行杂音不能掐断整条流。
	buf.WriteString("data: not-json-at-all\n\n")
	buf.WriteString("data: [DONE]\n\n")

	s := newStream(io.NopCloser(&buf), "kmodel")
	content, calls, finish, err := collect(t, s)
	if err != nil {
		t.Fatalf("流解析失败：%v", err)
	}
	if content != "你好" {
		t.Fatalf("正文不对：%q（期望 你好）", content)
	}
	if len(calls) != 0 {
		t.Fatalf("不该有工具调用：%+v", calls)
	}
	if finish != "stop" {
		t.Fatalf("结束原因不对：%q", finish)
	}
}

func TestStreamToolCallsFragmentAcrossChunks(t *testing.T) {
	var buf bytes.Buffer
	// 工具调用分片：先 name，再分两次 arguments（OpenAI 流式规范）。
	buf.WriteString(sseEnvelope(t, 200, map[string]any{"choices": []any{map[string]any{
		"index": 0,
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_weather"}}}}}}}))
	buf.WriteString(sseEnvelope(t, 200, map[string]any{"choices": []any{map[string]any{
		"index": 0,
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "function": map[string]any{"arguments": `{"city":`}}}}}}}))
	buf.WriteString(sseEnvelope(t, 200, map[string]any{"choices": []any{map[string]any{
		"index": 0, "finish_reason": "tool_calls",
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "function": map[string]any{"arguments": `"北京"}`}}}}}}}))
	buf.WriteString(sseEnvelope(t, 200, nil)) // 空 body 的心跳行，不能当内容
	buf.WriteString("data: " + mustJSON(t, map[string]any{"body": "[DONE]", "statusCodeValue": 200}) + "\n\n")

	s := newStream(io.NopCloser(&buf), "kmodel")
	content, frags, finish, err := collect(t, s)
	if err != nil {
		t.Fatalf("流解析失败：%v", err)
	}
	if content != "" {
		t.Fatalf("不该有正文：%q", content)
	}
	if len(frags) != 3 {
		t.Fatalf("工具调用应逐片透传 3 片，得到 %d：%+v", len(frags), frags)
	}
	// 客户端按 index 拼回去。
	var name, args strings.Builder
	for _, f := range frags {
		name.WriteString(f.Function.Name)
		args.WriteString(f.Function.Arguments)
	}
	if name.String() != "get_weather" || args.String() != `{"city":"北京"}` {
		t.Fatalf("工具调用拼装不对：name=%q args=%q", name.String(), args.String())
	}
	if frags[0].ID != "call_1" || frags[0].Index != 0 {
		t.Fatalf("首片应带 id 与 index：%+v", frags[0])
	}
	if finish != "tool_calls" {
		t.Fatalf("有工具调用时结束原因应为 tool_calls，得到 %q", finish)
	}
}

func TestStreamEnvelopeError(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(sseEnvelope(t, 403, "Signature invalid"))
	s := newStream(io.NopCloser(&buf), "kmodel")
	_, _, _, err := collect(t, s)
	k, ok := errs.KindOf(err)
	if !ok || k != errs.SessionDead {
		t.Fatalf("信封 403 应归一成 SessionDead，得到 %v（%v）", k, err)
	}
}

func TestStreamInnerErrorAuth(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(sseEnvelope(t, 200, map[string]any{"error": map[string]any{"message": "invalid token"}}))
	s := newStream(io.NopCloser(&buf), "kmodel")
	_, _, _, err := collect(t, s)
	k, ok := errs.KindOf(err)
	if !ok || k != errs.SessionDead {
		t.Fatalf("分片内 token 错误应归一成 SessionDead，得到 %v（%v）", k, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// 6. 错误归一
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{http.StatusUnauthorized, `{}`, errs.SessionDead},
		{http.StatusForbidden, `{"message":"Signature invalid"}`, errs.SessionDead},
		{http.StatusPaymentRequired, `{}`, errs.HardCredit},
		{http.StatusTooManyRequests, `{"message":"rate limit exceeded"}`, errs.SoftRate},
		{http.StatusTooManyRequests, `{"message":"quota exceeded"}`, errs.HardCredit},
		{http.StatusNotFound, `{}`, errs.ModelUnavailable},
		{http.StatusBadRequest, `{"message":"context too long"}`, errs.PromptTooLong},
		{http.StatusBadRequest, `{"message":"sensitive content"}`, errs.ContentBlocked},
		{http.StatusBadRequest, `{"message":"model not found"}`, errs.ModelUnavailable},
		{http.StatusBadGateway, `{}`, errs.UpstreamFault},
		{http.StatusInternalServerError, `{}`, errs.UpstreamFault},
	}
	for _, c := range cases {
		if got := Classify(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("Classify(%d,%q) = %s，期望 %s", c.status, c.body, got, c.want)
		}
	}
	// 红线一：未知错误绝不静默当成功。
	if got := Classify(200, []byte("garbage")); got == "" {
		t.Fatal("Classify 不得返回空分类")
	}
}

// ---------------------------------------------------------------------------
// 7. 端到端：模型目录 → 对话（mock 上游，逐处复算签名）
// ---------------------------------------------------------------------------

// verifySignature 在 mock 上游侧复算签名，把「客户端签名算错」这类问题钉死在测试里。
func verifySignature(t *testing.T, r *http.Request, body string, lc lingmaCred) {
	t.Helper()
	auth := r.Header.Get("Authorization")
	parts := strings.Split(auth, ".")
	if len(parts) != 3 {
		t.Fatalf("Authorization 形态不对：%q", auth)
	}
	path := strings.TrimPrefix(r.URL.Path, algoPrefix)
	preimage := strings.Join([]string{parts[1], lc.CosyKey, r.Header.Get("Cosy-Date"), body, path}, "\n")
	sum := md5.Sum([]byte(preimage))
	if got := hex.EncodeToString(sum[:]); got != parts[2] {
		t.Fatalf("上游侧签名复算不一致（path=%s）：\n got %s\nwant %s", path, parts[2], got)
	}
	if r.Header.Get("Cosy-Key") != lc.CosyKey || r.Header.Get("Cosy-Machineid") != lc.MachineID {
		t.Fatal("COSY 标识头不齐")
	}
}

func TestEndToEndModelsAndChat(t *testing.T) {
	lc := testCred()
	var modelsHit, chatHit int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case epModels:
			modelsHit++
			if r.Method != http.MethodGet {
				t.Errorf("模型目录必须是 GET，得到 %s", r.Method)
			}
			// GET 无 body：签名原文的 body 段必须是空串。
			verifySignature(t, r, "", lc)
			_, _ = w.Write([]byte(`{"chat":[{"key":"kmodel","display_name":"Kimi-K2.6","enable":true}]}`))

		case epChat:
			chatHit++
			verifySignature(t, r, string(raw), lc)
			if !strings.Contains(string(raw), `"tools"`) {
				t.Error("请求体应原样透传 tools（本渠道支持原生工具调用）")
			}
			if !strings.Contains(string(raw), `"tool_call_id":"call_1"`) {
				t.Error("tool 消息的 tool_call_id 必须回传")
			}
			if !strings.Contains(string(raw), `"request_id"`) {
				t.Error("请求体应带 request_id")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(sseEnvelope(t, 200, map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": "北京"}}}})))
			_, _ = w.Write([]byte(sseEnvelope(t, 200, map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": "晴"}}}})))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))

		default:
			t.Errorf("未预期的路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := toChannelCredential(lc, "测试账号")

	// 模型目录（同时验证签名在 GET 上也算得对）。
	models, err := a.Models(context.Background(), cred)
	if err != nil {
		t.Fatalf("Models 失败：%v", err)
	}
	if len(models) != 1 || models[0].ID != "kimi-k2.6" {
		t.Fatalf("模型列表不对：%+v", models)
	}
	if got := a.modelKey("kimi-k2.6"); got != "kmodel" {
		t.Fatalf("客户端名应映射到上游 key，得到 %q", got)
	}

	// 对话：带原生 tools 与一轮工具结果。
	req := channel.ChatRequest{
		Model: "kimi-k2.6",
		Messages: []channel.Message{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "北京天气"},
			{Role: "assistant", ToolCalls: []channel.ToolCall{{Index: 0, ID: "call_1", Type: "function",
				Function: channel.FunctionCall{Name: "get_weather", Arguments: `{"city":"北京"}`}}}},
			{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: "晴"},
		},
		Tools: []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}},
		}},
	}
	st, err := a.Chat(context.Background(), cred, req)
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	content, _, finish, err := collect(t, st)
	if err != nil {
		t.Fatalf("读流失败：%v", err)
	}
	if content != "北京晴" {
		t.Fatalf("正文不对：%q", content)
	}
	if finish != "stop" {
		t.Fatalf("结束原因不对：%q", finish)
	}
	if modelsHit != 1 || chatHit != 1 {
		t.Fatalf("接口调用次数不对：models=%d chat=%d", modelsHit, chatHit)
	}
}

// 登录全链路：粘两段 → Poll 解密 + 用模型目录当场校验 → 拿到可用凭证。
func TestLoginPollDecryptsAndValidates(t *testing.T) {
	machineID := "1234567890abcdef1234567890abcdef"
	plain := []byte(`{"key":"cosy-key-xyz","encrypt_user_info":"info-xyz","uid":"u-42"}`)
	enc := encryptCacheUser(t, machineID, plain)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != epModels {
			t.Errorf("校验应打模型目录，得到 %s", r.URL.Path)
			return
		}
		verifySignature(t, r, "", lingmaCred{CosyKey: "cosy-key-xyz", MachineID: machineID, UserID: "u-42", Info: "info-xyz"})
		_, _ = w.Write([]byte(`{"chat":[{"key":"kmodel","display_name":"Kimi-K2.6","enable":true}]}`))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 还没粘东西：必须是 ErrPending（正常的等待态）。
	if _, err := sess.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("未粘贴时应返回 ErrPending，得到 %v", err)
	}
	if _, ok := sess.(interface{ Hint() string }); !ok {
		t.Fatal("必须实现 Hint()（面板据此切换成粘贴形态）")
	}
	// 控制台是用这个接口收粘贴内容的，必须实现。
	acc, ok := sess.(channel.CallbackAcceptor)
	if !ok {
		t.Fatal("必须实现 channel.CallbackAcceptor（控制台的粘贴通道）")
	}
	// 分两次粘贴。
	if err := acc.AcceptCallback(enc); err != nil {
		t.Fatalf("粘 cache/user 失败：%v", err)
	}
	if _, err := sess.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("只粘一半时仍应 ErrPending，得到 %v", err)
	}
	if err := acc.AcceptCallback(machineID); err != nil {
		t.Fatalf("粘 cache/id 失败：%v", err)
	}
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if cred.UID != "u-42" || cred.AccessToken != "cosy-key-xyz" {
		t.Fatalf("凭证组装不对：%+v", cred)
	}
	if cred.Extra["machine_id"] != machineID || cred.Extra["machine_token"] != "info-xyz" {
		t.Fatalf("Extra 落盘字段不对：%+v", cred.Extra)
	}
	// 组装出来的凭证要能立即用于签名（credOf 往返一致）。
	back := credOf(cred)
	if back.CosyKey != "cosy-key-xyz" || back.Info != "info-xyz" || back.UserID != "u-42" || back.MachineID != machineID {
		t.Fatalf("credOf 往返不一致：%+v", back)
	}
}

// 校验失败时 Poll 必须报错，而不是放一个坏凭证入池。
func TestLoginPollRejectsBadCredential(t *testing.T) {
	machineID := "1234567890abcdef1234567890abcdef"
	plain := []byte(`{"key":"bad","encrypt_user_info":"bad","uid":"u-1"}`)
	enc := encryptCacheUser(t, machineID, plain)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Signature invalid"}`))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	sess, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	_ = sess.(channel.CallbackAcceptor).AcceptCallback(enc + "\n" + pasteSeparator + "\n" + machineID)
	_, err := sess.Poll(context.Background())
	if err == nil {
		t.Fatal("上游拒绝时 Poll 必须报错")
	}
	if k, _ := errs.KindOf(err); k != errs.SessionDead {
		t.Fatalf("403 校验失败应归一成 SessionDead，得到 %v（%v）", k, err)
	}
}

// ---------------------------------------------------------------------------
// 8. Spec
// ---------------------------------------------------------------------------

// 凭证必须能经 store 落盘再读回（这是本包最容易悄悄坏掉的一环）：
// store 的 parseCredential 只认固定的 5 个 Extra 键，我们正是靠这句话选的槽位
// （AccessToken 存 cosy_key、UID 存 user_id、Extra 的 machine_id/machine_token 存
// machineID 与 encode_user_info）。这个测试把那个决策钉死 —— 一旦 store 改了键集合，
// 这里会红，而不是等到线上重启后账号全变成「凭证不完整」。
func TestCredentialSurvivesStoreRoundTrip(t *testing.T) {
	lc := testCred()
	cred := toChannelCredential(lc, nicknameOf(lc.UserID))

	dir := t.TempDir()
	cs := store.NewCredsStore(dir)
	if _, err := cs.Save(channel.Lingma, *cred); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	loaded, err := cs.Load(channel.Lingma)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("应读回 1 个账号，得到 %d", len(loaded))
	}
	back := credOf(&loaded[0])
	if back != lc {
		t.Fatalf("落盘往返后凭证变了：\n got %+v\nwant %+v", back, lc)
	}
}

func TestSpecCapabilities(t *testing.T) {
	a := New()
	s := a.Spec()
	if s.Kind != channel.Lingma || !s.Downstream() {
		t.Fatalf("渠道标识/状态不对：%+v", s)
	}
	if !s.Tools || s.ToolsShim {
		t.Fatal("灵码是原生工具调用：Tools=true 且 ToolsShim=false")
	}
	if s.Images || s.Reasoning {
		t.Fatal("未实现图片与思考分流，能力位必须为 false（不撒谎）")
	}
	if !s.SSEOnly {
		t.Fatal("端点只有流式，SSEOnly 应为 true")
	}
	if s.Category != channel.CategoryCoding {
		t.Fatalf("灵码属编程助手类，得到 %q", s.Category)
	}
	if !strings.Contains(s.Docs, "封号") {
		t.Fatal("Docs 必须写明第三方客户端复用的封号风险")
	}
	if a.Kind() != channel.Lingma {
		t.Fatal("Kind() 不对")
	}
}

// 无续期机制：Refresh 必须明确说「刷不了」，而不是假装成功。
func TestRefreshIsNoop(t *testing.T) {
	a := New()
	nc, err := a.Refresh(context.Background(), &channel.Credential{UID: "u"})
	if err != nil || nc != nil {
		t.Fatalf("Refresh 应返回 (nil,nil)，得到 (%v,%v)", nc, err)
	}
	if got, _ := a.Login(context.Background()); got != nil {
		t.Fatal("Login 应返回明确错误而不是凭证")
	}
}

// 凭证不完整时 Chat/Models 必须在本地就报清楚，不去打上游。
func TestIncompleteCredentialFailsFast(t *testing.T) {
	a := New()
	a.base = "http://127.0.0.1:1" // 打不通的地址：走到网络就说明没做本地校验
	if _, err := a.Chat(context.Background(), &channel.Credential{UID: "u"}, channel.ChatRequest{}); err == nil {
		t.Fatal("空凭证应报错")
	}
	if _, err := a.Models(context.Background(), &channel.Credential{UID: "u"}); err == nil {
		t.Fatal("空凭证应报错")
	}
}
