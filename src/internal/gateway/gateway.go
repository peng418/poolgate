// Package gateway 是 OpenAI 兼容网关：/v1/chat/completions 与 /v1/models。
//
// 网关不直接知道渠道协议：它只读 registry（哪个渠道 Active、能力位）与 pool
// （选号），把请求归一成 channel.ChatRequest 交给适配器，再把 channel.Stream
// 透传成标准 OpenAI SSE。错误统一走 errs.Error —— 响应头未发出返回结构化 JSON，
// 已发出则写 SSE 错误帧 + [DONE]（F3.6，红线一）。
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/health"
	"poolgate/internal/pool"
	"poolgate/internal/registry"
	"poolgate/internal/router"
	"poolgate/internal/store"
	"poolgate/internal/toolshim"
)

// sseHeartbeat 是等待上游下一个 chunk 期间发 SSE 保活注释帧的间隔（见 streamChunks）。
const sseHeartbeat = 15 * time.Second

// heartbeatInterval 返回实际使用的保活间隔（测试里可以把它调小）。
func (s *Server) heartbeatInterval() time.Duration {
	if s.heartbeat > 0 {
		return s.heartbeat
	}
	return sseHeartbeat
}

// Server 是网关服务。
type Server struct {
	pool     *pool.Pool
	router   *router.Router
	keys     KeyVerifier
	log      *store.RequestLog
	basePath string
	excluded Excluder
	health   func() health.Snapshot
	// heartbeat 覆盖 SSE 保活间隔（0 = 用默认的 sseHeartbeat）。
	// 存在的原因是它必须可测：15 秒的间隔没法写进单测。
	heartbeat time.Duration
}

// KeyVerifier 校验 API Key。store.APIKeyStore 实现之；测试可注入。
type KeyVerifier interface {
	Verify(key string) bool
}

// Options 网关配置。
type Options struct {
	BasePath string
	// Log 请求流水（可观测）。可为 nil。
	Log *store.RequestLog
	// Excluded 是「因健康度被剔除下发」的模型集合（F4.4）。
	// 被剔除的模型不出现在 /v1/models，也不参与路由。可为 nil。
	Excluded Excluder
	// Gate 每账号串行 + 最小间隔（防封号，见 docs/06）。可为 nil。
	Gate router.Gate
	// Health 返回最近一次体检的结论索引，用于「只下发可用模型」（F4.5）。
	//
	// 与 Excluded 分开：Excluded 是人工裁定（关掉某个模型），这里跟着体检结论走，
	// 下次探测通过就自动回到列表。nil = 不做健康过滤；
	// 开关关着时装配层返回空索引，网关不必知道设置长什么样。
	Health func() health.Snapshot
}

// Excluder 报告某模型（<渠道>/<模型> 形式）是否被剔除下发。
type Excluder interface {
	Has(id string) bool
}

// New 建立网关服务。
func New(p *pool.Pool, keys KeyVerifier, o Options) *Server {
	ro := router.DefaultOptions()
	ro.Gate = o.Gate
	return &Server{
		pool: p, router: router.New(p, ro),
		keys: keys, log: o.Log, basePath: o.BasePath, excluded: o.Excluded,
		health: o.Health,
	}
}

// Routes 返回网关路由。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.withKey(s.handleChatCompletions))
	mux.HandleFunc("/v1/models", s.withKey(s.handleModels))
	mux.HandleFunc("/v1/messages", s.withKey(s.handleAnthropicMessages))
	mux.HandleFunc("/v1/messages/count_tokens", s.withKey(s.handleAnthropicCountTokens))
	mux.HandleFunc("/v1/responses", s.withKey(s.handleResponses))
	return mux
}

// withKey 校验 Authorization: Bearer <key>。失败返回结构化 401（带 kind）。
func (s *Server) withKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.keys.Verify(bearerToken(r)) {
			writeErr(w, http.StatusUnauthorized, errs.New(errs.AuthFailed, "无效的 API Key"))
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// /v1/models
// ---------------------------------------------------------------------------

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 GET"))
		return
	}
	type modelOut struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := []modelOut{}
	// 健康结论索引按请求取一次（内部带缓存），避免逐个模型扫历史。
	// 设置里「只下发可用模型」没开时它是一个空索引，下面两句判断自然都不生效。
	var snap health.Snapshot
	if s.health != nil {
		snap = s.health()
	}
	// 模型 ID = <渠道前缀>/<客户端模型名>；只下发 Active 渠道（暂停不下发 —— F2.3）。
	// 每个渠道取一个可用账号凭证，主动向上游拉取模型目录；拉取失败回退缓存，
	// 缓存也空则跳过该渠道（记日志，不静默给空 —— 面板/客户端能看出哪个渠道缺目录）。
	for _, e := range registry.Active() {
		models := s.modelsFor(r, e)
		for _, m := range models {
			id := string(e.Spec.Kind) + "/" + m.ID
			// 被健康探测剔除的模型不下发（F4.4）：面板上关了就必须在客户端也消失，
			// 否则「剔除」只是面板上的一个说法。
			if s.excluded != nil && s.excluded.Has(id) {
				continue
			}
			// 体检明确失败的模型不下发（F4.5，设置里的开关）。
			// 未体检的不在这里消失 —— 「不知道」不等于「不可用」。
			if snap.Hide(id) {
				continue
			}
			out = append(out, modelOut{
				ID:      id,
				Object:  "model",
				OwnedBy: string(e.Spec.Kind),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// modelsFor 返回某渠道的模型目录：优先用真实账号凭证向上游拉取，失败回退缓存快照。
//
// 硬性前提：该渠道**必须至少有一个账号**。模型是「用某个账号向上游问出来的」，
// 没有账号就不存在可用模型 —— 否则全新安装（0 个账号）也会给客户端返回一堆模型，
// 客户端会以为可用，实际每次调用都失败（红线一：不做会撒谎的目录）。
func (s *Server) modelsFor(r *http.Request, e registry.Entry) []channel.ModelInfo {
	cred, ok := s.pool.Pick(r.Context(), e.Spec.Kind, nil)
	if !ok {
		return nil // 没有可用账号：不下发任何模型（含静态兜底表）
	}
	// 优先用真实凭证拉取。
	if models, err := e.Channel.Models(r.Context(), &cred); err == nil && len(models) > 0 {
		return models
	}
	// 回退缓存快照（上次成功拉取）。
	if snap, ok := e.Channel.(interface{ ModelsSnapshot() []channel.ModelInfo }); ok {
		return snap.ModelsSnapshot()
	}
	return nil
}

// ---------------------------------------------------------------------------
// /v1/chat/completions
// ---------------------------------------------------------------------------

// chatRequest 是 OpenAI chat.completions 请求的解析子集。
type chatRequest struct {
	Model       string           `json:"model"`
	Messages    []chatMessage    `json:"messages"`
	MaxTokens   int              `json:"max_tokens"`
	Temperature *float64         `json:"temperature"`
	Stream      bool             `json:"stream"`
	Stop        []string         `json:"stop"`
	TopP        *float64         `json:"top_p"`
	Tools       []map[string]any `json:"tools"`
	ToolChoice  any              `json:"tool_choice"`
}

// chatMessage 是一条入站消息。content 允许三种形态（OpenAI 规范）：
// 字符串、parts 数组、null（assistant 只发 tool_calls 时可以是 null）。
// 这里用 RawMessage 收下来再归一成文本 —— 工具结果本来就是文本。
type chatMessage struct {
	Role       string             `json:"role"`
	Content    json.RawMessage    `json:"content"`
	ToolCalls  []channel.ToolCall `json:"tool_calls"`
	ToolCallID string             `json:"tool_call_id"`
	Name       string             `json:"name"`
}

// text 把 content 归一成纯文本：字符串直用；parts 数组取 text 片段；null/缺失为空串。
func (m chatMessage) text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if t, _ := p["text"].(string); t != "" {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

// toChannel 归一成内部消息，**保留工具调用的往返结构**：
// assistant 的 tool_calls 与 role=tool 消息的 tool_call_id 必须原样带给上游，
// 否则模型会以为自己上一轮什么都没调用，从而重复调用同一个工具。
func (m chatMessage) toChannel() channel.Message {
	return channel.Message{
		Role:       m.Role,
		Content:    m.text(),
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req chatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	if req.Model == "" {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "缺少 model 字段"))
		return
	}
	if len(req.Messages) == 0 {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "缺少 messages"))
		return
	}

	// 模型名解析：<渠道前缀>/<模型名>；无前缀时默认 QoderCN。
	kind, modelName := parseModel(req.Model)
	ch, ok := registry.Get(kind)
	if !ok {
		writeErr(w, http.StatusBadRequest, errs.New(errs.ModelUnavailable,
			"未注册的渠道 "+string(kind)).WithChannel(string(kind)))
		return
	}
	spec := ch.Spec()
	if !spec.Downstream() {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.UpstreamFault,
			"渠道已暂停").WithChannel(string(kind)))
		return
	}
	// 被剔除下发的模型直接拒绝，不让客户端绕过 /v1/models 直接点名调用（F4.4）。
	if s.excluded != nil && s.excluded.Has(string(kind)+"/"+modelName) {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.ModelUnavailable,
			"模型 "+string(kind)+"/"+modelName+" 因健康度被剔除下发，请在面板「模型」页恢复").
			WithChannel(string(kind)))
		return
	}
	// 工具调用能力分三档（F3.7）：
	//   ① 原生支持（spec.Tools）→ tools 原样透传给上游；
	//   ② 只支持「网关代做模拟」（spec.ToolsShim）→ 把工具定义翻成提示词，
	//      再把模型输出的标记解析回结构化 tool_calls（见 internal/toolshim）；
	//   ③ 都不支持 → **明确拒绝**。
	// 静默丢掉 tools 是最坏的一种处理：上游收不到工具定义，会把「我要调用工具」
	// 写成普通文本（各家的标记还不一样），客户端拿不到 tool_calls，
	// 表现成「模型不回复 / 回一堆看不懂的乱码」—— coding agent 直接不可用。
	useShim := false
	if len(req.Tools) > 0 {
		switch {
		case spec.Tools:
		case spec.ToolsShim:
			useShim = true
		default:
			writeErr(w, http.StatusBadRequest, errs.New(errs.ModelUnavailable,
				"渠道 "+string(kind)+" 暂不支持工具调用（tools）：已明确拒绝而不是静默忽略，"+
					"请去掉 tools 或改用支持工具调用的渠道").WithChannel(string(kind)))
			return
		}
	}

	msgs := make([]channel.Message, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = m.toChannel()
	}
	creq := channel.ChatRequest{
		Model:       modelName,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      req.Stream,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
	}

	// 模拟工具调用：把请求改写成「只会聊天的上游能吃的形态」（工具说明进系统提示词，
	// 工具结果改写成普通消息）。响应侧再由 Wrap 把标记解析回 tool_calls。
	if useShim {
		creq = toolshim.BuildRequest(creq)
	}

	// 网关侧最后一道防线（见 pairing.go）：剔除无法配对的 tool_call / tool 结果，
	// 并把插在配对组中间的消息挪到整组之后。放在这里 = 发往上游前的最后一步
	// （shim 改写也已完成），与 wild-work 在 wire 改写末尾做这一步的定位一致。
	// 缺一侧的配对会被上游 400 顶死整条会话，因此宁可丢一轮工具上下文也要先清掉。
	creq.Messages = sanitizeToolPairing(creq.Messages)

	// 路由：选号 + 换号重试 + 分档冷却。
	session := r.Header.Get("X-Poolgate-Session")
	start := time.Now()
	res, err := s.router.Route(r.Context(), ch, kind, session, creq)
	if err != nil {
		if k, ok := errs.KindOf(err); ok {
			s.logRequest(kind, "", modelName, "error", string(k), upstreamNote(err), 0, start)
		}
		writeErrFromErr(w, err)
		return
	}
	st := res.Stream
	if useShim {
		st = toolshim.Wrap(st)
	}
	defer st.Close()
	cred := res.Cred

	// 流式：逐 chunk 透传 SSE。
	if req.Stream {
		ttft, ok := s.streamChunks(w, kind, cred, st)
		status, errKind, note := "ok", "", ""
		if !ok {
			// 空流/流中出错：这里已经写过错误帧，流水必须如实记为失败（红线一）。
			status, errKind, note = "error", string(errs.Parse), "上游未返回任何内容"
		}
		s.logRequest(kind, cred.UID, modelName, status, errKind, note, ttft, start)
		return
	}

	// 非流式：聚合（SSEOnly 渠道由本地聚合成单个 completion）。
	ttft, ok := s.aggregateChunks(w, kind, cred, st, modelName)
	status, errKind, note := "ok", "", ""
	if !ok {
		status, errKind, note = "error", string(errs.Parse), "上游未返回任何内容"
	}
	s.logRequest(kind, cred.UID, modelName, status, errKind, note, ttft, start)
}

// logRequest 记录一条请求流水（可观测，F6.1）。Log 为 nil 时跳过。
//
// ttft 是首字延迟；未收到内容时为 0。note 是人话补充（换号、上游原话摘要）。
func (s *Server) logRequest(kind channel.Kind, uid, model, status, errKind, note string, ttft time.Duration, start time.Time) {
	if s.log == nil {
		return
	}
	s.log.Add(store.RequestRecord{
		Time:     time.Now(),
		Channel:  string(kind),
		Account:  uid,
		Model:    model,
		Status:   status,
		ErrKind:  errKind,
		Duration: time.Since(start).Milliseconds(),
		TTFT:     ttft.Milliseconds(),
		Note:     note,
	})
}

// streamChunks 把 channel.Stream 透传成 OpenAI SSE，末尾补 [DONE]。
// 首帧前若出错，仍可退回结构化错误；首帧后出错则写 SSE 错误帧 + [DONE]。
//
// 返回首字延迟与「是否真的收到了内容」—— 后者是红线二的落点：
// 一个 chunk 都没收到就不算成功，流水里也必须如实记为失败。
func (s *Server) streamChunks(w http.ResponseWriter, kind channel.Kind, cred channel.Credential, stream channel.Stream) (time.Duration, bool) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	w.WriteHeader(http.StatusOK)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}

	start := time.Now()
	var ttft time.Duration
	wroteFirst := false
	// sawContent：整条流里有没有出现过真正的内容（正文/工具调用/思考）。
	// 上游把内容帧换成我们不认识的形态时，流里照样有帧、照样正常结束 —— 只有这个标记能发现。
	sawContent := false

	// 上游 Next() 是阻塞的：放到独立 goroutine 里读，主循环用 select 兼顾
	// 「等下一个 chunk」与「发保活心跳」两件事。
	//
	// 为什么需要心跳：思考型档位（豆包深度思考、TraeWork 长推理等）在出字前可能几十秒
	// 没有任何输出，这条连接在这段时间里是**完全静默**的 —— 夹在中间的 nginx / 飞牛网关 /
	// CDN 会按空闲超时把它掐掉，客户端表现成「莫名其妙断开」。参考实现（doubao2api 新版）
	// 为此每 5 秒发一次 SSE 注释帧；我们取 15 秒（够躲开常见的 30/60 秒空闲超时，也不刷屏）。
	// 注释帧以 `:` 开头，按 SSE 规范客户端必须忽略，不会污染正文。
	type chunkOrErr struct {
		chunk channel.ChatCompletionChunk
		err   error
	}
	chunks := make(chan chunkOrErr)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(chunks)
		for {
			c, err := stream.Next()
			select {
			case chunks <- chunkOrErr{chunk: c, err: err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(s.heartbeatInterval())
	defer ticker.Stop()

	for {
		var chunk channel.ChatCompletionChunk
		var err error
		select {
		case r, ok := <-chunks:
			if !ok {
				err = errEOF
			} else {
				chunk, err = r.chunk, r.err
			}
		case <-ticker.C:
			if _, werr := w.Write([]byte(": keep-alive\n\n")); werr != nil {
				return ttft, wroteFirst
			}
			flush()
			continue
		}
		if err != nil {
			if err == errEOF {
				break
			}
			// 上游流中出错：写错误帧 + [DONE]，不允许静默关闭（F3.6）。
			s.noteFailure(kind, cred.UID, err)
			writeSSEErr(w, err, flush)
			writeSSEDone(w, flush)
			return ttft, false
		}
		raw, _ := json.Marshal(chunk)
		if _, err := w.Write([]byte("data: " + string(raw) + "\n\n")); err != nil {
			return ttft, wroteFirst
		}
		if hasContent(chunk) {
			// 首个「带内容」的 chunk 才算首字：上游常先发一个空 role 帧。
			// 之前写成「只有第一个 chunk 带内容才记 TTFT」，于是先发 role 帧的渠道
			// TTFT 永远记成 0（面板那列一直显示 0 就是这个原因）—— 已于 2026-09-26 修正。
			if !sawContent {
				ttft = time.Since(start)
				sawContent = true
			}
		}
		wroteFirst = true
		flush()
	}
	if !wroteFirst {
		// 红线二：一个 chunk 都没收到不算成功。上游把错误包在 200 信封里、
		// 或者干脆返回空流时，客户端必须看到错误帧，而不是「HTTP 200 + 只有 [DONE]」。
		err := errs.New(errs.Parse, "上游返回空流：未收到任何内容").WithChannel(string(kind)).WithAccount(cred.UID)
		s.noteFailure(kind, cred.UID, err)
		writeSSEErr(w, err, flush)
		writeSSEDone(w, flush)
		return 0, false
	}
	if !sawContent {
		// 「有帧、没内容」：上游改了帧形态而适配器没认出来（chatgpt 网页渠道 2026-09-26
		// 就是这样整轮空的）。必须写错误帧，不能让客户端对着一个空回复猜。
		err := errs.New(errs.Parse, "上游只发了空帧：没有正文、工具调用或思考").
			WithChannel(string(kind)).WithAccount(cred.UID)
		s.noteFailure(kind, cred.UID, err)
		writeSSEErr(w, err, flush)
		writeSSEDone(w, flush)
		return ttft, false
	}
	s.pool.NoteSuccess(kind, cred.UID)
	writeSSEDone(w, flush)
	return ttft, true
}

// hasContent 判断一个 chunk 是否携带真实内容（首字判据用）。
func hasContent(c channel.ChatCompletionChunk) bool {
	for _, ch := range c.Choices {
		if ch.Delta.Content != "" || ch.Delta.ReasoningContent != "" || len(ch.Delta.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// mergeToolCalls 把流式 tool_calls 分片拼回完整调用。
//
// 两种上游形态都要吃下：
//   - OpenAI 式增量：首片带 id/type/name，后续片只有 arguments 片段，靠 index 归并；
//   - agent 式整包：每帧就是一个完整调用（带 id），此时按 id 归并、遇到新 id 就新增。
//
// 只按 index 归并会把「没有 index 的多个完整调用」错误地粘成一个，所以优先认 id。
func mergeToolCalls(acc []channel.ToolCall, delta []channel.ToolCall) []channel.ToolCall {
	for _, d := range delta {
		idx := -1
		for i := range acc {
			if d.ID != "" {
				if acc[i].ID == d.ID {
					idx = i
					break
				}
				continue
			}
			if acc[i].Index == d.Index {
				idx = i
				break
			}
		}
		if idx < 0 {
			acc = append(acc, d)
			continue
		}
		if d.ID != "" {
			acc[idx].ID = d.ID
		}
		if d.Type != "" {
			acc[idx].Type = d.Type
		}
		if d.Function.Name != "" {
			acc[idx].Function.Name = d.Function.Name
		}
		acc[idx].Function.Arguments += d.Function.Arguments
	}
	return acc
}

// aggregateChunks 聚合为单个 chat.completion 响应（非流式）。
// 返回首字延迟与「是否真的收到了内容」，与 streamChunks 同一套判据。
func (s *Server) aggregateChunks(w http.ResponseWriter, kind channel.Kind, cred channel.Credential, stream channel.Stream, model string) (time.Duration, bool) {
	var (
		id        string
		created   int64
		content   strings.Builder
		role      = "assistant"
		finish    = "stop"
		usage     map[string]any
		toolCalls []channel.ToolCall // 流式 tool_calls 分片按 index 拼回完整调用
		got       int                // 收到的 chunk 数：一个都没收到 = 上游空流，不能当成功
	)
	start := time.Now()
	var ttft time.Duration
	for {
		chunk, err := stream.Next()
		if err != nil {
			if err == errEOF {
				break
			}
			s.noteFailure(kind, cred.UID, err)
			writeErrFromErr(w, err)
			return ttft, false
		}
		got++
		if ttft == 0 && hasContent(chunk) {
			ttft = time.Since(start)
		}
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			if c.Delta.Role != "" {
				role = c.Delta.Role
			}
			content.WriteString(c.Delta.Content)
			toolCalls = mergeToolCalls(toolCalls, c.Delta.ToolCalls)
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
	}
	if got == 0 {
		// 红线二：非流式下「一个 chunk 都没有」同样是失败，不能回一个空 content 的 200。
		err := errs.New(errs.Parse, "上游返回空流：未收到任何内容").WithChannel(string(kind)).WithAccount(cred.UID)
		s.noteFailure(kind, cred.UID, err)
		writeErrFromErr(w, err)
		return 0, false
	}
	// 「分片收到了、但一个字都没有」也是失败 —— 不能当成功。
	//
	// 这条是 2026-09-26 补的：chatgpt 网页渠道上游换了帧形态，适配器认不出内容帧，
	// 于是整轮「HTTP 200 + finish_reason=stop + content 空」——客户端以为模型没话说，
	// 其实是解析器瞎了。判断口径与流式那条一致（正文/工具调用/思考三者都没有才算空）。
	if content.Len() == 0 && len(toolCalls) == 0 {
		err := errs.New(errs.Parse,
			fmt.Sprintf("上游返回了空内容：收到 %d 个分片，但没有正文、工具调用或思考", got)).
			WithChannel(string(kind)).WithAccount(cred.UID)
		s.noteFailure(kind, cred.UID, err)
		writeErrFromErr(w, err)
		return 0, false
	}
	if id == "" {
		id = "chatcmpl-" + time.Now().Format("20060102150405.000000000")
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	s.pool.NoteSuccess(kind, cred.UID)
	message := map[string]any{"role": role, "content": content.String()}
	// 流被截断（finish_reason==length）时，tool_call 的 arguments 可能只剩半截 JSON：
	// 不把脏参数交给客户端（见 truncation.go）。完整参数与无参工具照常保留。
	if truncatedFinish(finish) {
		toolCalls = dropTruncatedToolCalls(toolCalls)
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		// 有工具调用时 finish_reason 必须是 tool_calls（客户端据此判断「先执行工具再继续」）。
		// 上游常常仍报 stop/空，这里按事实纠正；length 等真实截断原因不动。
		if finish == "" || finish == "stop" {
			finish = "tool_calls"
		}
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	writeJSON(w, http.StatusOK, resp)
	return ttft, true
}

// noteFailure 按 errs.Kind 分档冷却；UpstreamFault 不计账号错误（pool 内部处理）。
func (s *Server) noteFailure(kind channel.Kind, uid string, err error) {
	if k, ok := errs.KindOf(err); ok {
		until, _ := errs.RetryAtOf(err)
		s.pool.NoteErrorAt(kind, uid, k, until)
	}
}

// upstreamNote 取上游原话摘要，让流水里的失败行带上真实原因（红线一）。
func upstreamNote(err error) string {
	var e *errs.Error
	if errors.As(err, &e) && e.Upstream != "" {
		return truncate(e.Upstream, 300)
	}
	return truncate(err.Error(), 300)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// parseModel 把 "<渠道>/<模型>" 拆开；无前缀默认 QoderCN。
func parseModel(m string) (channel.Kind, string) {
	if i := strings.IndexByte(m, '/'); i >= 0 {
		k := channel.Kind(m[:i])
		return k, m[i+1:]
	}
	return channel.QoderCN, m
}
