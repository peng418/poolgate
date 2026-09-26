package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/registry"
)

// errEOF 是内部流结束哨兵（避免处处 import io）。
var errEOF = io.EOF

// registryGet 从注册表取渠道实现。
func registryGet(k channel.Kind) (channel.Channel, bool) {
	return registry.Get(k)
}

// parseJSONBody 解析 JSON 请求体为 map；失败时写 400 并返回 false。
func parseJSONBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var m map[string]any
	if err := decodeJSON(r, &m); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return nil, false
	}
	return m, true
}

func bearerToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	auth = strings.TrimPrefix(auth, "Bearer ")
	auth = strings.TrimSpace(auth)
	if auth != "" {
		return auth
	}
	// Anthropic 客户端用 x-api-key。
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("空请求体")
	}
	return json.Unmarshal(body, dst)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic("poolgate: JSON 编码失败: " + err.Error())
	}
}

// writeErr 是网关唯一的错误出口（D1）：客户端永远拿到结构化 kind + message。
func writeErr(w http.ResponseWriter, code int, e *errs.Error) {
	writeJSON(w, code, errPayload(e))
}

// writeErrFromErr 从任意 error 提取 Kind 并写出结构化错误。
func writeErrFromErr(w http.ResponseWriter, err error) {
	k, _ := errs.KindOf(err)
	e := errs.New(k, "上游请求失败")
	if ee, ok := err.(*errs.Error); ok {
		e = ee
	} else if err != nil {
		// 非结构化错误（适配器/底层库漏归一的那些）也要把**原话**带出去。
		// 只回一句「上游请求失败」等于把原因吞掉：实测 TraeWork 额度耗尽时，
		// 上游原话「Your requests have exceeded the quota.」就是这样被换成一句
		// 无从定位的 Parse 的（红线一：失败必带原因）。
		text := truncate(err.Error(), 300)
		e = errs.New(k, "上游请求失败："+text).WithUpstream(text)
	}
	code := httpStatusFor(k)
	writeJSON(w, code, errPayload(e))
}

func errPayload(e *errs.Error) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"kind":     string(e.Kind),
			"message":  e.Message,
			"upstream": e.Upstream,
		},
	}
}

// writeSSEErr 写一个 SSE 错误帧（含 kind + 上游原话），客户端能读到明确失败。
func writeSSEErr(w http.ResponseWriter, err error, flush func()) {
	k, _ := errs.KindOf(err)
	payload := map[string]any{"error": map[string]any{"kind": string(k), "message": "上游流式错误"}}
	if ee, ok := err.(*errs.Error); ok {
		payload["error"].(map[string]any)["message"] = ee.Message
		if ee.Upstream != "" {
			payload["error"].(map[string]any)["upstream"] = ee.Upstream
		}
	} else if err != nil {
		// 同上：非结构化错误也要带原话，否则客户端只看到「上游流式错误」。
		text := truncate(err.Error(), 300)
		payload["error"].(map[string]any)["message"] = "上游流式错误：" + text
		payload["error"].(map[string]any)["upstream"] = text
	}
	raw, _ := json.Marshal(payload)
	_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
	flush()
}

func writeSSEDone(w http.ResponseWriter, flush func()) {
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flush()
}

// sseHeader 设置 SSE 响应头并返回 flush 函数。
func sseHeader(w http.ResponseWriter) func() {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	return func() {
		if fl != nil {
			fl.Flush()
		}
	}
}

// writeSSEEvent 写一个命名 SSE 事件（Anthropic 风格）。
func writeSSEEvent(w http.ResponseWriter, flush func(), event string, payload map[string]any) error {
	raw, _ := json.Marshal(payload)
	if _, err := w.Write([]byte("event: " + event + "\ndata: " + string(raw) + "\n\n")); err != nil {
		return err
	}
	flush()
	return nil
}

// httpStatusFor 把 errs.Kind 映射到 HTTP 状态码。
func httpStatusFor(k errs.Kind) int {
	switch k {
	case errs.HardCredit:
		return http.StatusPaymentRequired
	case errs.SoftRate:
		return http.StatusTooManyRequests
	case errs.Muted:
		// 池内账号被上游禁言：**不是**调用方的 Key 错了，也不是限流（重试立刻也是白试）。
		// 与 NoCandidate 同一个语义档位：我是网关，我这边上游现在不给用。
		return http.StatusServiceUnavailable
	case errs.SessionDead, errs.AuthFailed:
		// 池内账号的上游凭证失效 —— **不是**调用方的 API Key 错了。
		//
		// 这里回 401 会把客户端带沟里：Studio / Claude Code 看到 401 会判定
		// 「我的 Key 无效」，提示用户重新填 Key（填了也没用，坏的是池里的账号）。
		// 401 只属于网关自己的 API Key 校验（handleModels/withKey）。
		return http.StatusBadGateway
	case errs.ContentBlocked, errs.PromptTooLong, errs.ModelUnavailable:
		return http.StatusBadRequest
	case errs.UpstreamFault:
		return http.StatusBadGateway
	case errs.Transport:
		return http.StatusBadGateway
	case errs.SessionBusy:
		// 上游同一账号的「会话启动」还没落地（2 秒级瞬态闸门）。这不是调用方的问题，
		// 也不是限流：网关侧已经退避重试过，仍然冲突就如实回 503 + Retry。
		// **绝不用 429**：客户端看到 429 会立刻重试，正好再撞一次闸门。
		return http.StatusServiceUnavailable
	case errs.NoCandidate:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
