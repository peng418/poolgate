package iflow

// stream.go 上游响应 → 标准 OpenAI chunk 流。
//
// 上游是标准 OpenAI 兼容接口，两种形态都要吃下（按 Content-Type 判，不猜）：
//   - SSE（text/event-stream）：`data: {chat.completion.chunk}` 逐帧，末尾 `data: [DONE]`；
//   - 整包 JSON：非流式请求（我们不带 stream 字段）的回包。
//
// 归一化直接用 channel.ParseOpenAIChunk / OpenAIChunkFromMessage —— 上游字段名就是
// OpenAI 那套，不需要任何映射（这与 Kimi 的二进制帧完全不同）。

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

type chunkOrErr struct {
	chunk channel.ChatCompletionChunk
	err   error
}

type stream struct {
	rc    io.ReadCloser
	model string
	sse   bool
	ch    chan chunkOrErr
}

func newStream(rc io.ReadCloser, contentType, model string, wantStream bool) *stream {
	ct := strings.ToLower(contentType)
	// SSE 判定：
	//   - text/event-stream 当然是流；
	//   - **application/octet-stream 也要按流解**：参考实现的流式分支把这两种
	//     content-type 都当流（proxy.py:687 只在「既不是 event-stream 也不是
	//     octet-stream」时才走非流式错误路径），说明上游有时会用 octet-stream 回 SSE。
	//     octet-stream 只在**请求本来就要流式**时才这么判 —— 非流式请求若真回了
	//     octet-stream，按 JSON 解会明确报错，比按空流解出「什么都没收到」更安全。
	//     （第二份独立实现 AIClient2API 完全不看 content-type，只按行解 data:；
	//     故这一条是单源，但保留它不会与「按行解 SSE」冲突。）
	sse := strings.Contains(ct, "text/event-stream") ||
		(wantStream && strings.Contains(ct, "application/octet-stream"))
	s := &stream{
		rc:    rc,
		model: model,
		sse:   sse,
		ch:    make(chan chunkOrErr, 16),
	}
	go s.pump()
	return s
}

func (s *stream) pump() {
	defer close(s.ch)
	if !s.sse {
		s.pumpSingleJSON()
		return
	}
	br := bufio.NewReaderSize(s.rc, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			s.ch <- chunkOrErr{err: err}
			return
		}
		// 按行读是对的：SSE 的边界就是换行。半行由 bufio 帮我们攒齐。
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				return
			}
			if !s.handlePayload([]byte(payload)) {
				return
			}
		}
		// 解不出的行/注释/`event:` 行跳过：上游会在流里插心跳与元数据行，
		// 当成失败会误杀一条正常流（诊断结论：这里绝不能严格）。
		if err == io.EOF {
			return
		}
	}
}

// pumpSingleJSON 处理非流式回包，或「上游无视 stream 直接回了整包 JSON」的情况。
func (s *stream) pumpSingleJSON() {
	raw, err := io.ReadAll(io.LimitReader(s.rc, 32<<20))
	if err != nil {
		s.ch <- chunkOrErr{err: err}
		return
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		s.ch <- chunkOrErr{err: parseFailure(raw)}
		return
	}
	if msg, isErr := upstreamError(obj); isErr {
		// 上游把错误放进了 200 的信封里 —— 不能当成功（红线二）。
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		s.ch <- chunkOrErr{err: errs.New(classifyBody(errs.UpstreamFault, msg), "上游返回错误："+msg).
			WithChannel(string(channel.IFlow)).WithUpstream(truncate(string(raw), 200))}
		return
	}
	chunks := channel.OpenAIChunkFromMessage(obj, s.model)
	if len(chunks) == 0 {
		s.ch <- chunkOrErr{err: parseFailure(raw)}
		return
	}
	for _, c := range chunks {
		s.ch <- chunkOrErr{chunk: c}
	}
}

// handlePayload 处理一条 SSE data 载荷；返回 false 表示流该就此终止（已发过错误）。
func (s *stream) handlePayload(payload []byte) bool {
	var raw map[string]any
	if json.Unmarshal(payload, &raw) != nil {
		return true // 解不出的帧跳过，不误杀
	}
	if msg, isErr := upstreamError(raw); isErr {
		if msg == "" {
			msg = truncate(string(payload), 200)
		}
		// 帧内 token/签名类错误是**凭证问题**（该重新粘贴），不是上游故障 ——
		// 分错了用户会去查网络，而真正该做的是重新登录。
		kind := classifyBody(errs.UpstreamFault, msg)
		s.ch <- chunkOrErr{err: errs.New(kind, "上游返回错误："+msg).
			WithChannel(string(channel.IFlow)).WithUpstream(truncate(string(payload), 200))}
		return false
	}
	if c, ok := channel.ParseOpenAIChunk(raw, s.model); ok {
		s.ch <- chunkOrErr{chunk: c}
	}
	return true
}

// errorMessage 从 OpenAI 形态的 error 字段里取人话（error 可能是对象或字符串）。
func errorMessage(v any) string {
	switch e := v.(type) {
	case map[string]any:
		if m, ok := e["message"].(string); ok {
			return strings.TrimSpace(m)
		}
	case string:
		return strings.TrimSpace(e)
	}
	return ""
}

// upstreamError 从一个上游响应/SSE 帧对象里取错误文案，返回 (文案, 是否错误)。
//
// iFlow 的错误信封是**顶层 `msg` 字段**（参考实现一律读 `error_data.get("msg")`：
// proxy.py:694、app.py:1184/1220/1547），而不是 OpenAI 的 `{"error":{"message":…}}`。
// 只认 `error` 键会把「顶层 msg 的业务错误」当成正常回包（红线二），所以两种都认。
//
// 顶层 msg 只在**没有 choices** 时才判定为错误：正常 chunk/回包一定带 choices，
// 只有错误信封才是「有 msg、没 choices」。这样不会因为某个正常回包恰好带 msg 而误判。
//
// 交叉印证：第二份独立实现 AIClient2API（iflow-core.js）**不**解析信封，只靠 axios 对
// 非 2xx 抛错，也不区分 200+业务错误 —— 故「顶层 msg」这一条仍是单源（iflow2api）。
// 保留它不矛盾：多认一种错误形态只会更安全（能识别出「有错误」而不是当成功）。
func upstreamError(raw map[string]any) (string, bool) {
	if v, ok := raw["error"]; ok && v != nil {
		// 有 error 键就是错误；文案取不到时返回空串，由调用方回退成原始报文摘要。
		return errorMessage(v), true
	}
	if _, hasChoices := raw["choices"]; !hasChoices {
		if m, ok := raw["msg"].(string); ok && strings.TrimSpace(m) != "" {
			return strings.TrimSpace(m), true
		}
	}
	return "", false
}

// errorTextFromBytes 是 upstreamError 的字节版：给看不到 HTTP 语义的场景（非 2xx 回包）取人话。
func errorTextFromBytes(raw []byte) string {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	msg, _ := upstreamError(obj)
	return msg
}

// parseFailure 把「解析不了」变成客户端可见的失败，并带上上游原话摘要（红线一）。
func parseFailure(raw []byte) error {
	return errs.New(errs.Parse, "上游响应不是合法的 OpenAI 兼容格式").
		WithChannel(string(channel.IFlow)).WithUpstream(truncate(string(raw), 200))
}

func (s *stream) Next() (channel.ChatCompletionChunk, error) {
	ce, ok := <-s.ch
	if !ok {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	return ce.chunk, ce.err
}

func (s *stream) Close() error { return s.rc.Close() }
