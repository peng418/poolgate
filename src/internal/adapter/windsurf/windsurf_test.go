package windsurf

// windsurf_test.go 覆盖最容易出错、也最不该出错的地方：
//  1. protobuf 编解码往返（手搓 wire format 是我们自己的代码，没有库兜底）；
//  2. Connect 帧切分（**半包/粘包**是常态，切错的表现是「客户端一直转圈/拿到乱码」）；
//  3. 请求信封形态（双写鉴权头、732 hex 指纹、单份 token、未压缩请求帧）；
//  4. 流式帧解析（正文 #3 + 思考 #9 + 用量 + 结束）；
//  5. 错误归一（鉴权/升级墙/内容策略/截断）。
//  6. 登录粘贴 + 额度接口当场验证。

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// ---------------------------------------------------------------------------
// 造帧工具
// ---------------------------------------------------------------------------

func frame(flags byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = flags
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func gzipFrame(payload []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	return frame(0x01, buf.Bytes())
}

func respContentFrame(text string) []byte {
	return frame(0x00, appendStringField(nil, respFieldContent, text))
}

func respReasoningFrame(text string) []byte {
	return frame(0x00, appendStringField(nil, respFieldReasoning, text))
}

func respMetaFrame(prompt, completion, cacheRead uint64) []byte {
	var m []byte
	m = appendVarintField(m, metaUsagePrompt, prompt)
	m = appendVarintField(m, metaUsageCompletion, completion)
	if cacheRead > 0 {
		m = appendVarintField(m, metaUsageCacheRead, cacheRead)
	}
	return frame(0x00, appendBytesField(nil, respFieldMeta, m))
}

func respFinishFrame(v uint64) []byte {
	return frame(0x00, appendVarintField(nil, respFieldFinish, v))
}

// respToolCallFrame 造一帧带一个原生 ChatToolCall（#6）的响应。空串字段不写，
// 便于模拟「第一帧给 id、后续帧只给参数碎片」的分片形态。
func respToolCallFrame(id, name, args string) []byte {
	var sub []byte
	if id != "" {
		sub = appendStringField(sub, callFieldID, id)
	}
	if name != "" {
		sub = appendStringField(sub, callFieldName, name)
	}
	if args != "" {
		sub = appendStringField(sub, callFieldArguments, args)
	}
	return frame(0x00, appendBytesField(nil, respFieldToolCalls, sub))
}

func trailerFrame(jsonStr string) []byte {
	return frame(0x02, []byte(jsonStr))
}

// writeChunks 把字节流按很小的切片写出去并 flush，逼出「半包」处理路径。
func writeChunks(w http.ResponseWriter, data []byte, size int) {
	w.Header().Set("Content-Type", ctConnectProto)
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for i := 0; i < len(data); i += size {
		end := i + size
		if end > len(data) {
			end = len(data)
		}
		_, _ = w.Write(data[i:end])
		if fl != nil {
			fl.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// 1. protobuf 编解码往返
// ---------------------------------------------------------------------------

func TestProtoCodecRoundTrip(t *testing.T) {
	type sample struct {
		Small  uint64
		Big    uint64
		Empty  string
		Text   string
		Blob   []byte
		Double float64
		Nested []byte
	}
	in := sample{
		Small:  1,
		Big:    0x1_0000_0001, // 超过 32 位，专治 varint 用 int32 截断的写法
		Empty:  "",
		Text:   "你好，世界 🌍",
		Blob:   []byte{0x00, 0x01, 0xff, 0xfe},
		Double: 0.95,
		Nested: appendStringField(nil, 2, "inner"),
	}

	var buf []byte
	buf = appendVarintField(buf, 1, in.Small)
	buf = appendVarintField(buf, 2, in.Big)
	buf = appendStringField(buf, 3, in.Empty)
	buf = appendStringField(buf, 4, in.Text)
	buf = appendBytesField(buf, 5, in.Blob)
	buf = appendFixed64Field(buf, 6, in.Double)
	buf = appendBytesField(buf, 7, in.Nested)

	fields, err := parseFields(buf)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(fields) != 7 {
		t.Fatalf("应有 7 个字段，得到 %d", len(fields))
	}
	if v, _ := getVarint(fields, 1); v != in.Small {
		t.Fatalf("字段 1 不相等：%d", v)
	}
	if v, _ := getVarint(fields, 2); v != in.Big {
		t.Fatalf("字段 2（大 varint）不相等：%d", v)
	}
	if got := getString(fields, 3); got != "" {
		t.Fatalf("空字符串字段应解出空串，得到 %q", got)
	}
	if got := getString(fields, 4); got != in.Text {
		t.Fatalf("字段 4 不相等：%q", got)
	}
	if got := getBytes(fields, 5); !bytes.Equal(got, in.Blob) {
		t.Fatalf("字段 5 不相等：%v", got)
	}
	f6 := fields[5]
	if f6.wire != wireFixed64 || len(f6.bytes) != 8 {
		t.Fatalf("字段 6 应是 fixed64，得到 wire=%d len=%d", f6.wire, len(f6.bytes))
	}
	if d := math.Float64frombits(binary.LittleEndian.Uint64(f6.bytes)); d != in.Double {
		t.Fatalf("字段 6 double 不相等：%v", d)
	}
	nested, err := parseFields(getBytes(fields, 7))
	if err != nil || getString(nested, 2) != "inner" {
		t.Fatalf("嵌套消息解析失败：%v", err)
	}

	// 再编码一次，应与原字节逐字节相等（字段号升序、编码确定性）。
	var again []byte
	again = appendVarintField(again, 1, in.Small)
	again = appendVarintField(again, 2, in.Big)
	again = appendStringField(again, 3, in.Empty)
	again = appendStringField(again, 4, in.Text)
	again = appendBytesField(again, 5, in.Blob)
	again = appendFixed64Field(again, 6, in.Double)
	again = appendBytesField(again, 7, in.Nested)
	if !bytes.Equal(buf, again) {
		t.Fatalf("编码不确定：\n%x\n%x", buf, again)
	}
}

func TestParseFieldsRejectsTruncated(t *testing.T) {
	full := appendStringField(nil, 1, "abcdef")
	// 空缓冲是合法的空消息（不是错误），所以从 1 开始截。
	for _, n := range []int{1, 2, 3, len(full) - 1} {
		if _, err := parseFields(full[:n]); err == nil {
			t.Fatalf("截断到 %d 字节时应报错，却成功了", n)
		}
	}
	// group（wire 3）不支持：必须有明确错误，不能静默当成功。
	bad := appendVarint(nil, uint64(1)<<3|3)
	if _, err := parseFields(bad); err == nil {
		t.Fatal("group wire type 应被拒绝")
	}
}

// ---------------------------------------------------------------------------
// 2. 帧切分：半包 / 粘包 / gzip / trailer
// ---------------------------------------------------------------------------

func TestFrameSplitterHalfFrames(t *testing.T) {
	f1 := frame(0x00, []byte("AAA"))    // 未压缩数据帧
	f2 := gzipFrame([]byte("BBBBBBBB")) // gzip 数据帧
	f3 := trailerFrame("{}")            // 结束帧
	all := append(append(append([]byte{}, f1...), f2...), f3...)

	// ① 逐字节喂：每个字节 push 一次，边喂边 drain。
	sp := &frameSplitter{}
	var got []connectFrame
	for i := range all {
		sp.push(all[i : i+1])
		frames, err := sp.drain()
		if err != nil {
			t.Fatalf("第 %d 字节处 drain 出错：%v", i, err)
		}
		got = append(got, frames...)
	}
	assertThreeFrames(t, got)

	// ② 一次性粘包喂：三段一起 push，一次 drain 出全部。
	sp2 := &frameSplitter{}
	sp2.push(all)
	got2, err := sp2.drain()
	if err != nil {
		t.Fatalf("粘包 drain 出错：%v", err)
	}
	assertThreeFrames(t, got2)
}

func assertThreeFrames(t *testing.T, got []connectFrame) {
	t.Helper()
	if len(got) != 3 {
		t.Fatalf("应切出 3 帧，得到 %d", len(got))
	}
	if string(got[0].payload) != "AAA" || got[0].trailer {
		t.Fatalf("帧 1 不对：%q trailer=%v", got[0].payload, got[0].trailer)
	}
	if string(got[1].payload) != "BBBBBBBB" {
		t.Fatalf("gzip 帧应被解压成 BBBBBBBB，得到 %q", got[1].payload)
	}
	if !got[2].trailer || string(got[2].payload) != "{}" {
		t.Fatalf("帧 3 应是 trailer {}，得到 %q trailer=%v", got[2].payload, got[2].trailer)
	}
}

func TestFrameSplitterRejectsOversize(t *testing.T) {
	head := make([]byte, 5)
	head[0] = 0x00
	binary.BigEndian.PutUint32(head[1:5], uint32(maxFrameSize+1))
	sp := &frameSplitter{}
	sp.push(head)
	if _, err := sp.drain(); err == nil {
		t.Fatal("超长帧头应被拒绝（zip 炸弹/坏帧防护）")
	}
}

func TestWrapRequestIsUncompressedSingleFrame(t *testing.T) {
	proto := appendStringField(nil, 1, "hello")
	env := wrapRequest(proto)
	if env[0] != 0x00 {
		t.Fatalf("请求帧 flag 必须是 0（未压缩）—— gzip 请求会被上游判 internal，得到 %#x", env[0])
	}
	if int(binary.BigEndian.Uint32(env[1:5])) != len(proto) {
		t.Fatal("长度头不对")
	}
	if !bytes.Equal(env[5:], proto) {
		t.Fatal("载荷应与 protobuf 原文逐字节一致（不能顺手压缩）")
	}
}

// ---------------------------------------------------------------------------
// 3. 请求体形态
// ---------------------------------------------------------------------------

func TestBuildChatRequestWireShape(t *testing.T) {
	token := "devin-session-token$abc123"
	temp := 0.0 // 故意给 0：应被夹到 0.001
	msgs := []channel.Message{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "甲"},
		{Role: "user", Content: "乙"}, // 连续同角色 → 应合并
		{Role: "assistant", Content: "好"},
		{Role: "tool", ToolCallID: "call_1", Content: "晴"},
	}
	raw := buildChatRequest(token, "swe-1-6-slow", "", msgs, nil, 0, &temp)

	top, err := parseFields(raw)
	if err != nil {
		t.Fatalf("顶层解析失败：%v", err)
	}

	// 鉴权元数据 #1：token 单份、指纹 732 hex。
	metaFields, err := parseFields(getBytes(top, reqFieldMetadata))
	if err != nil {
		t.Fatalf("ClientMetadata 解析失败：%v", err)
	}
	if got := getString(metaFields, metaFieldName); got != clientName {
		t.Fatalf("客户端名应为 %s，得到 %q", clientName, got)
	}
	if got := getString(metaFields, metaFieldVersion); got != clientVersion {
		t.Fatalf("版本应为 %s，得到 %q", clientVersion, got)
	}
	fp := getString(metaFields, metaFieldFingerprint)
	if len(fp) != fingerprintHex {
		t.Fatalf("指纹必须 %d 个 hex 字符（366 字节），得到 %d", fingerprintHex, len(fp))
	}
	if _, err := hex.DecodeString(fp); err != nil {
		t.Fatalf("指纹必须是纯 hex：%v", err)
	}
	// token 在**正文里只出现一次**（双写只发生在 HTTP 头）。
	if n := bytes.Count(raw, []byte(token)); n != 1 {
		t.Fatalf("正文里 session token 应恰好出现 1 次（单份），得到 %d 次", n)
	}

	// 常量与模型。
	if v, _ := getVarint(top, reqFieldConst7); v != 5 {
		t.Fatalf("#7 应为 5，得到 %d", v)
	}
	if v, _ := getVarint(top, reqFieldConst20); v != 1 {
		t.Fatalf("#20 应为 1，得到 %d", v)
	}
	if got := getString(top, reqFieldModel); got != "swe-1-6-slow" {
		t.Fatalf("#21 model 不对：%q", got)
	}
	if got := getString(top, reqFieldSystem); got != "你是助手" {
		t.Fatalf("system 应上提到 #2，得到 %q", got)
	}

	// 对话序列 #3：user 甲+乙 合并且 system 不在其中，另加 assistant、原生 tool_result。
	var chatTexts []string
	var lastMsg []pfield
	for _, f := range top {
		if f.field != reqFieldChatMessage {
			continue
		}
		cf, err := parseFields(f.bytes)
		if err != nil {
			t.Fatalf("ChatMessage 解析失败：%v", err)
		}
		chatTexts = append(chatTexts, getString(cf, cmFieldText))
		lastMsg = cf
	}
	if len(chatTexts) != 3 {
		t.Fatalf("对话序列应有 3 条（合并后的 user / assistant / 原生 tool_result），得到 %d：%v", len(chatTexts), chatTexts)
	}
	if chatTexts[0] != "甲\n\n乙" {
		t.Fatalf("连续同角色轮应合并，得到 %q", chatTexts[0])
	}
	// role=tool 带 id → 原生 source=4 + #7 tool_call_id（不再降级成 user 文本）。
	if v, _ := getVarint(lastMsg, cmFieldSource); v != sourceToolResult {
		t.Fatalf("带 id 的 tool 轮应是 source=4，得到 %d", v)
	}
	if got := getString(lastMsg, cmFieldToolCallID); got != "call_1" {
		t.Fatalf("#7 tool_call_id 应为 call_1，得到 %q", got)
	}

	// CompletionConfig #8：默认输出上限 + 温度夹到 0.001。
	compFields, err := parseFields(getBytes(top, reqFieldCompletion))
	if err != nil {
		t.Fatalf("CompletionConfig 解析失败：%v", err)
	}
	if v, _ := getVarint(compFields, compFieldMaxTokens); v != defaultMaxTokens {
		t.Fatalf("#2 max_tokens 默认应为 %d，得到 %d", defaultMaxTokens, v)
	}
	cf6 := compFields[3] // #5 温度（fixed64）
	if cf6.field != compFieldTemperature || cf6.wire != wireFixed64 {
		t.Fatalf("温度字段应是 #5 fixed64，得到 #%d wire=%d", cf6.field, cf6.wire)
	}
	if d := math.Float64frombits(binary.LittleEndian.Uint64(cf6.bytes)); d != minTemperature {
		t.Fatalf("temperature=0 应夹到 %v，得到 %v", minTemperature, d)
	}
}

// 原生工具定义：ToolDef 内部 tag 照参考实现（name=1 / description=2 / parameters=3），
// 且必须做 MCP 指纹中和（顶层描述=工具名、参数 schema 去掉 description）。
func TestBuildToolDefsWireShape(t *testing.T) {
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "Get the weather for a city (this text trips the MCP gate)",
			"parameters": map[string]any{
				"type":    "object",
				"$schema": "http://json-schema.org/draft-07/schema#",
				"properties": map[string]any{
					"city":        map[string]any{"type": "string", "description": "city name"},
					"description": map[string]any{"type": "string"},
				},
				"required": []any{"city", "not_a_real_prop"},
			},
		},
	}}
	// 故意不给 system：有 tools 但无 system 应触发兜底（上游对 Claude 系会回 internal）。
	raw := buildChatRequest("tok", "swe-1-6-slow", "", []channel.Message{{Role: "user", Content: "hi"}}, tools, 0, nil)
	top, err := parseFields(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got := getString(top, reqFieldSystem); got != toolsGuardSystem {
		t.Fatalf("有 tools 无 system 时应补兜底 system，得到 %q", got)
	}
	// #10 是追加在最后的（参考实现的字节序）。
	if len(top) == 0 || top[len(top)-1].field != reqFieldTools {
		t.Fatalf("#10 ToolDefs 应追加在最后，得到末字段 #%d", top[len(top)-1].field)
	}
	defs := 0
	var def []pfield
	for _, f := range top {
		if f.field == reqFieldTools {
			defs++
			def, _ = parseFields(f.bytes)
		}
	}
	if defs != 1 {
		t.Fatalf("应恰好 1 个 ToolDef，得到 %d", defs)
	}
	if got := getString(def, toolDefFieldName); got != "get_weather" {
		t.Fatalf("ToolDef #1 name 不对：%q", got)
	}
	// ★ 顶层描述必须是工具名（MCP 指纹中和），不是真实描述。
	if got := getString(def, toolDefFieldDescription); got != "get_weather" {
		t.Fatalf("ToolDef #2 description 应被中和成工具名，得到 %q", got)
	}
	var schema map[string]any
	if err := json.Unmarshal(getBytes(def, toolDefFieldParameters), &schema); err != nil {
		t.Fatalf("ToolDef #3 parameters 应是合法 JSON：%v", err)
	}
	if _, has := schema["$schema"]; has {
		t.Fatal("$schema 应被丢掉")
	}
	if schema["type"] != "object" {
		t.Fatalf("schema.type 应为 object，得到 %v", schema["type"])
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil || props["city"] == nil {
		t.Fatalf("schema.properties 丢了 city：%v", schema)
	}
	if _, has := props["city"].(map[string]any)["description"]; has {
		t.Fatal("参数 schema 里的 description 注解应被去掉（MCP 指纹中和）")
	}
	if _, has := props["description"]; !has {
		t.Fatal("名叫 description 的属性必须保留（它是真实参数，不是注解）")
	}
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "city" {
		t.Fatalf("required 应只保留真实属性 city，得到 %v", schema["required"])
	}
}

// 原生工具调用：响应 #6 是**跨帧分片**的 —— 第一帧给 id，后续帧只给参数碎片，必须
// 按 id 合并成一条；name 解不到时用本轮唯一工具反查；结束原因翻成 tool_calls。
func TestChatNativeToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var all []byte
		all = append(all, respToolCallFrame("call_1", "", "{\"ci")...)  // 首帧：id + 参数碎片，无 name
		all = append(all, respToolCallFrame("", "", "ty\":\"SF\"}")...) // 后续帧：只有参数碎片
		all = append(all, respMetaFrame(10, 5, 0)...)
		all = append(all, respFinishFrame(2)...)
		all = append(all, trailerFrame("{}")...)
		writeChunks(w, all, 5)
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	st, err := a.Chat(context.Background(), &channel.Credential{AccessToken: "tok"}, channel.ChatRequest{
		Model:    "swe-1-6-slow",
		Messages: []channel.Message{{Role: "user", Content: "天气"}},
		Tools:    []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather"}}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	var calls []channel.ToolCall
	finish := ""
	for {
		c, err := st.Next()
		if err != nil {
			break
		}
		for _, ch := range c.Choices {
			calls = append(calls, ch.Delta.ToolCalls...)
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	if len(calls) != 1 {
		t.Fatalf("分片应合并成 1 条工具调用，得到 %d：%+v", len(calls), calls)
	}
	if calls[0].ID != "call_1" {
		t.Fatalf("工具调用 id 不对：%q", calls[0].ID)
	}
	if calls[0].Function.Name != "get_weather" {
		t.Fatalf("name 解不到时应用唯一工具反查，得到 %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"city":"SF"}` {
		t.Fatalf("参数碎片应拼回完整 JSON，得到 %q", calls[0].Function.Arguments)
	}
	if calls[0].Type != "function" || calls[0].Index != 0 {
		t.Fatalf("工具调用形状不对：%+v", calls[0])
	}
	if finish != "tool_calls" {
		t.Fatalf("有工具调用时结束原因应为 tool_calls，得到 %q", finish)
	}
}

// utf8Stream：多字节字符被切在帧边界时必须扣下半字符、下一批拼回来。
func TestUTF8StreamHoldsSplitRune(t *testing.T) {
	var u utf8Stream
	if got := u.write([]byte{0xE4}); got != "" {
		t.Fatalf("只收到 3 字节字符的第 1 字节时不应输出，得到 %q", got)
	}
	if got := u.write([]byte{0xBD}); got != "" {
		t.Fatalf("第 2 字节到达时仍不完整，不应输出，得到 %q", got)
	}
	if got := u.write([]byte{0xA0}); got != "你" {
		t.Fatalf("第 3 字节到达后应拼出「你」，得到 %q", got)
	}
	if got := u.write([]byte("abc")); got != "abc" {
		t.Fatalf("ASCII 应立即输出，得到 %q", got)
	}
	// 尾部孤立续字节是非法输入：不能被无限扣下，write 必须原样交出。
	if got := u.write([]byte{0x80, 0x80}); len(got) != 2 {
		t.Fatalf("非法续字节应原样输出、不挂起，得到 %q", got)
	}
	// 上游在字符中途断流：flush 必须交出被扣下的残留字节，不能吞掉。
	var v utf8Stream
	if got := v.write([]byte{0xE4}); got != "" {
		t.Fatalf("半字符不应输出，得到 %q", got)
	}
	if got := v.flush(); len(got) != 1 {
		t.Fatalf("flush 应交出残留字节，得到 %q", got)
	}
}

// 端到端：上游把「你」切成 1+2 字节分两帧发（真实 TCP 切帧常态），
// 我们不能解出 U+FFFD，必须拼回完整的「你好世界」。
func TestChatAssemblesSplitMultibyte(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		full := []byte("你好世界")
		var all []byte
		all = append(all, frame(0x00, appendBytesField(nil, respFieldContent, full[:1]))...)
		all = append(all, frame(0x00, appendBytesField(nil, respFieldContent, full[1:3]))...)
		all = append(all, frame(0x00, appendBytesField(nil, respFieldContent, full[3:]))...)
		all = append(all, trailerFrame("{}")...)
		writeChunks(w, all, 3)
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	st, err := a.Chat(context.Background(), &channel.Credential{AccessToken: "tok"}, channel.ChatRequest{
		Model:    "swe-1-6-slow",
		Messages: []channel.Message{{Role: "user", Content: "hi"}},
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
	if got := content.String(); got != "你好世界" {
		t.Fatalf("跨帧多字节字符应拼回完整文本，得到 %q", got)
	}
	if strings.ContainsRune(content.String(), '�') {
		t.Fatal("不得出现 U+FFFD（说明半字符被逐帧解码了）")
	}
}

func TestResolveModel(t *testing.T) {
	if got := resolveModel(""); got != defaultModel {
		t.Fatalf("空模型名应回落到 %s，得到 %q", defaultModel, got)
	}
	if got := resolveModel("  swe-1-7 "); got != "swe-1-7" {
		t.Fatalf("非空模型名应原样（trim），得到 %q", got)
	}
}

// ---------------------------------------------------------------------------
// 4. 端到端：mock 上游
// ---------------------------------------------------------------------------

func TestChatEndToEnd(t *testing.T) {
	const token = "devin-session-token$test-token"
	var chatCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != epChat {
			t.Errorf("未预期路径：%s", r.URL.Path)
			return
		}
		chatCalls++
		// 鉴权头必须是 Basic <token>-<token>（双写）。
		if got := r.Header.Get("Authorization"); got != "Basic "+token+"-"+token {
			t.Errorf("鉴权头必须是双写形态，得到 %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != ctConnectProto {
			t.Errorf("对话 Content-Type 必须是 %s，得到 %q", ctConnectProto, got)
		}
		if got := r.Header.Get("Connect-Protocol-Version"); got != connectProtoV1 {
			t.Errorf("缺 Connect-Protocol-Version，得到 %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("UA 应为 %q，得到 %q", userAgent, got)
		}
		body, _ := io.ReadAll(r.Body)
		sp := &frameSplitter{}
		sp.push(body)
		frames, err := sp.drain()
		if err != nil || len(frames) != 1 {
			t.Fatalf("请求体应恰好 1 帧：err=%v frames=%d", err, len(frames))
		}
		top, err := parseFields(frames[0].payload)
		if err != nil {
			t.Fatalf("请求 protobuf 解析失败：%v", err)
		}
		meta, _ := parseFields(getBytes(top, reqFieldMetadata))
		if n := len(getString(meta, metaFieldFingerprint)); n != fingerprintHex {
			t.Errorf("请求必须带 732 hex 指纹，实际 %d", n)
		}
		if got := getString(top, reqFieldModel); got != "swe-1-6-slow" {
			t.Errorf("model selector 不对：%q", got)
		}

		var all []byte
		all = append(all, respContentFrame("你好")...)
		all = append(all, respReasoningFrame("想一下")...)
		all = append(all, gzipFrame(appendStringField(nil, respFieldContent, "世界"))...)
		all = append(all, respMetaFrame(100, 5, 30)...)
		all = append(all, respFinishFrame(2)...)
		all = append(all, trailerFrame("{}")...)
		writeChunks(w, all, 7) // 故意切得很碎
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := &channel.Credential{UID: "windsurf-test", AccessToken: token}

	st, err := a.Chat(context.Background(), cred, channel.ChatRequest{
		Model:    "swe-1-6-slow",
		Messages: []channel.Message{{Role: "system", Content: "你是助手"}, {Role: "user", Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()

	var content, reasoning strings.Builder
	finish := ""
	var usage map[string]any
	for {
		c, err := st.Next()
		if err != nil {
			break
		}
		if c.Usage != nil {
			usage = c.Usage
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
		t.Fatalf("正文分流不对：%q（期望 你好世界）—— 注意正文在 #3、思考在 #9", content.String())
	}
	if reasoning.String() != "想一下" {
		t.Fatalf("思考分流不对：%q", reasoning.String())
	}
	if finish != "stop" {
		t.Fatalf("结束帧不对：%q", finish)
	}
	if usage == nil {
		t.Fatal("应带回用量")
	}
	// prompt = 新鲜 100 + cache_read 30；total = prompt + completion(0 写) = 135。
	if usage["prompt_tokens"] != int64(130) || usage["completion_tokens"] != int64(5) || usage["total_tokens"] != int64(135) {
		t.Fatalf("用量口径不对：%v", usage)
	}
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	if details == nil || details["cached_tokens"] != int64(30) {
		t.Fatalf("缓存命中明细缺失：%v", usage)
	}
	if chatCalls != 1 {
		t.Fatalf("应只打一次对话接口，实际 %d", chatCalls)
	}
}

// 结束原因：completion 正好撞上调用方给的输出上限 → length（截断）。
func TestChatMarksLengthWhenHittingCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var all []byte
		all = append(all, respContentFrame("半截")...)
		all = append(all, respMetaFrame(10, 5, 0)...)
		all = append(all, respFinishFrame(2)...)
		all = append(all, trailerFrame("{}")...)
		writeChunks(w, all, 64)
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	st, err := a.Chat(context.Background(), &channel.Credential{AccessToken: "tok"}, channel.ChatRequest{
		Model: "swe-1-6-slow", MaxTokens: 5,
		Messages: []channel.Message{{Role: "user", Content: "写首诗"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	defer st.Close()
	finish := ""
	for {
		c, err := st.Next()
		if err != nil {
			break
		}
		for _, ch := range c.Choices {
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	if finish != "length" {
		t.Fatalf("撞上输出上限应报 length，得到 %q", finish)
	}
}

// 没有 end-of-stream 帧就断流 = 截断，必须报错，不能当正常结束。
func TestChatTruncatedWithoutTrailer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeChunks(w, respContentFrame("半截答案"), 64)
		// 直接结束：没有 trailer 帧。
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	st, err := a.Chat(context.Background(), &channel.Credential{AccessToken: "tok"}, channel.ChatRequest{
		Model: "swe-1-6-slow", Messages: []channel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat 应成功建立流（错误在流里）：%v", err)
	}
	defer st.Close()

	var got error
	sawFinish := false
	for {
		c, err := st.Next()
		if err != nil {
			got = err
			break
		}
		for _, ch := range c.Choices {
			if ch.FinishReason != "" {
				sawFinish = true
			}
		}
	}
	if sawFinish {
		t.Fatal("截断的流不得补一个正常 finish（那会把半截答案写进会话）")
	}
	k, ok := errs.KindOf(got)
	if !ok || k != errs.UpstreamFault {
		t.Fatalf("截断应归一成 UpstreamFault，得到 %v（%v）", k, got)
	}
}

// trailer 里的错误必须按内容归一，而不是一律当上游故障。
func TestTrailerErrorNormalized(t *testing.T) {
	cases := []struct {
		name string
		body string
		want errs.Kind
	}{
		{"鉴权", `{"error":{"code":"permission_denied","message":"permission_denied: invalid token"}}`, errs.SessionDead},
		{"升级墙", `{"error":{"code":"permission_denied","message":"This model requires an upgrade. Visit /upgrade to access claude-opus-4-8"}}`, errs.ModelUnavailable},
		{"上游内部错误", `{"error":{"code":"internal","message":"an internal error occurred (trace ID: 123)"}}`, errs.UpstreamFault},
		{"内容策略", `{"error":{"code":"permission_denied","message":"blocked by our content policy"}}`, errs.ContentBlocked},
	}
	for _, c := range cases {
		e := trailerError([]byte(c.body))
		if e == nil {
			t.Fatalf("%s: 应报错", c.name)
		}
		k, _ := errs.KindOf(e)
		if k != c.want {
			t.Fatalf("%s: 期望 %v 得到 %v（%v）", c.name, c.want, k, e)
		}
	}
	if e := trailerError([]byte("{}")); e != nil {
		t.Fatalf("空 trailer 不是错误：%v", e)
	}
}

// ---------------------------------------------------------------------------
// 5. 归一与能力声明
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	a := New()
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{}`, errs.SessionDead},
		{403, `{"error":{"message":"forbidden"}}`, errs.SessionDead},
		// 瞬时故障被包在 401/403「鉴权壳」里：必须先判成上游故障，绝不能读成 token 死了。
		{401, `{"error":{"message":"an internal error occurred (trace ID: 1)"}}`, errs.UpstreamFault},
		{403, `{"error":{"message":"We're currently facing high demand for this model. Please try again later."}}`, errs.UpstreamFault},
		// 内容策略同样用 403 表达，但它不是凭证问题。
		{403, `{"error":{"message":"blocked by our content policy"}}`, errs.ContentBlocked},
		{429, `{"error":{"message":"rate limited"}}`, errs.SoftRate},
		{429, `{"error":{"message":"you have exceeded your quota"}}`, errs.HardCredit},
		{200, `model requires a paid entitlement`, errs.ModelUnavailable},
		{200, `an internal error occurred (trace ID: abc)`, errs.UpstreamFault},
		{503, `{}`, errs.UpstreamFault},
		{400, `{"error":{"message":"maximum context length exceeded"}}`, errs.PromptTooLong},
		{404, `not found`, errs.ModelUnavailable},
		{418, `teapot`, errs.Parse},
	}
	for _, c := range cases {
		if got := a.Classify(c.status, []byte(c.body)); got != c.want {
			t.Fatalf("status=%d body=%s: 期望 %v 得到 %v", c.status, c.body, c.want, got)
		}
	}
}

func TestSpecDeclaresNativeTools(t *testing.T) {
	sp := New().Spec()
	if sp.Kind != channel.Windsurf {
		t.Fatalf("Kind 不对：%v", sp.Kind)
	}
	if !sp.Tools {
		t.Fatal("ToolDef 内部 tag 已由参考实现付费实弹标定，应声明原生工具调用")
	}
	if sp.ToolsShim {
		t.Fatal("走原生透传时不得再声明 ToolsShim（它与 Tools 互斥）")
	}
	if !sp.Reasoning {
		t.Fatal("上游原生分流思考（#9），应声明 Reasoning")
	}
	if sp.Images || sp.CheckinCap {
		t.Fatal("Images/CheckinCap 都应为 false")
	}
	if sp.Category != channel.CategoryCoding {
		t.Fatalf("这是订阅额度池型 IDE 渠道，应归 Coding，得到 %q", sp.Category)
	}
	if sp.Docs == "" {
		t.Fatal("必须写清风险说明")
	}
}

func TestModelsListFreeSelector(t *testing.T) {
	ms, err := New().Models(context.Background(), nil)
	if err != nil {
		t.Fatalf("Models 失败：%v", err)
	}
	if len(ms) == 0 {
		t.Fatal("模型清单不能为空")
	}
	var found bool
	for _, m := range ms {
		if m.ID == defaultModel {
			found = true
			if m.Tools != channel.CapYes {
				t.Fatal("客户端侧应可用工具（原生透传）")
			}
			if m.Source != channel.SourceLocal {
				t.Fatalf("本地清单来源应标 local，得到 %v", m.Source)
			}
		}
	}
	if !found {
		t.Fatalf("清单必须包含免费可用的 %s", defaultModel)
	}
}

// ---------------------------------------------------------------------------
// 6. 登录：粘贴 + 额度接口当场验证
// ---------------------------------------------------------------------------

func TestLoginPollVerifiesCredential(t *testing.T) {
	const token = "devin-session-token$poll-token"
	var statusCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != epUserStatus {
			t.Errorf("未预期路径：%s", r.URL.Path)
			return
		}
		statusCalls++
		if got := r.Header.Get("Authorization"); got != "Basic "+token+"-"+token {
			t.Errorf("额度接口也要双写鉴权头，得到 %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != ctProto {
			t.Errorf("unary 接口 Content-Type 应为 %s，得到 %q", ctProto, got)
		}
		body, _ := io.ReadAll(r.Body)
		top, err := parseFields(body)
		if err != nil {
			t.Fatalf("GetUserStatus 请求解析失败：%v", err)
		}
		meta, _ := parseFields(getBytes(top, 1))
		if n := len(getString(meta, metaFieldFingerprint)); n != fingerprintHex {
			t.Errorf("额度请求也要带 732 hex 指纹，实际 %d", n)
		}
		// 响应：顶层 #2 = PlanInfo { #2 = "Free" }
		plan := appendStringField(nil, 2, "Free")
		resp := appendBytesField(nil, 2, plan)
		_, _ = w.Write(resp)
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	sess, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	if h := sess.(interface{ Hint() string }).Hint(); !strings.Contains(h, "devin-session-token$") {
		t.Fatalf("Hint 必须写清要粘的是什么：%q", h)
	}

	// 还没粘 → ErrPending。
	if _, err := sess.Poll(context.Background()); err != channel.ErrPending {
		t.Fatalf("未粘凭证时应返回 ErrPending，得到 %v", err)
	}

	acc, ok := sess.(channel.CallbackAcceptor)
	if !ok {
		t.Fatal("必须实现 CallbackAcceptor（粘贴通道）")
	}
	if err := acc.AcceptCallback("  \"" + token + "\"  "); err != nil {
		t.Fatalf("AcceptCallback 失败：%v", err)
	}
	cred, err := sess.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if cred.AccessToken != token {
		t.Fatalf("凭证不对：%q", cred.AccessToken)
	}
	if !strings.HasPrefix(cred.UID, "windsurf-") {
		t.Fatalf("UID 前缀不对：%q", cred.UID)
	}
	if !strings.Contains(cred.Nickname, "Free") {
		t.Fatalf("昵称应带上 plan：%q", cred.Nickname)
	}
	if statusCalls != 1 {
		t.Fatalf("应恰好验一次凭证，实际 %d", statusCalls)
	}
	// 重复 Poll → 已完成。
	if _, err := sess.Poll(context.Background()); err == nil {
		t.Fatal("重复 Poll 应报错")
	}
}

func TestLoginPollRejectsDeadCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid token"}}`))
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	sess, _ := a.StartLogin(context.Background(), channel.LoginOptions{})
	_ = sess.(channel.CallbackAcceptor).AcceptCallback("devin-session-token$dead-token")
	_, err := sess.Poll(context.Background())
	k, ok := errs.KindOf(err)
	if !ok || k != errs.SessionDead {
		t.Fatalf("失效凭证应归一成 SessionDead，得到 %v（%v）", k, err)
	}
}

func TestExtractSessionToken(t *testing.T) {
	want := "devin-session-token$eyJhbGciOi.abc.def"
	cases := []string{
		want,
		`"` + want + `"`,
		`{"token":"` + want + `"}`,
		`{"apiKey":"` + want + `"}`,
		`x = ` + want,
		"  " + want + "  ",
	}
	for _, c := range cases {
		if got := extractSessionToken(c); got != want {
			t.Fatalf("%q 应解出 %q，得到 %q", c, want, got)
		}
	}
	for _, bad := range []string{"", "short", "有 空格 的文本", "{}"} {
		if got := extractSessionToken(bad); got != "" {
			t.Fatalf("%q 不该被认成凭证，得到 %q", bad, got)
		}
	}
}

// Refresh 明确「刷不了」：session token 没有续期路径，不能假装成功。
func TestRefreshReportsNotPossible(t *testing.T) {
	nc, err := New().Refresh(context.Background(), &channel.Credential{AccessToken: "tok"})
	if err != nil || nc != nil {
		t.Fatalf("应返回 (nil,nil) 表示刷不了，得到 %v %v", nc, err)
	}
}
