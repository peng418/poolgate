package console

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"poolgate/internal/errs"
	"poolgate/internal/store"
)

const sessionCookie = "pg_session"

// withAuth 保护管理接口：无会话一律 401，不落到业务逻辑里。
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeErr(w, http.StatusUnauthorized, errs.New(errs.AuthFailed, "未登录或会话已过期"))
			return
		}
		next(w, r)
	}
}

// authed 依次检查 Authorization 头与 Cookie，便于 API 客户端与浏览器两种用法。
func (s *Server) authed(r *http.Request) bool {
	return s.sess.Valid(sessionToken(r))
}

func sessionToken(r *http.Request) string {
	if t := strings.TrimSpace(r.Header.Get("Authorization")); t != "" {
		t = strings.TrimPrefix(t, "Bearer ")
		if strings.TrimSpace(t) != "" {
			return strings.TrimSpace(t)
		}
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

func setSessionCookie(w http.ResponseWriter, sess store.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sess.Token,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		MaxAge:   int(time.Until(sess.ExpiresAt).Seconds()),
		HttpOnly: true, // 前端脚本取不到，降低 XSS 影响
		SameSite: http.SameSiteLaxMode,
		Secure:   false, // 局域网 HTTP 场景；反代 HTTPS 时应由反代设置为 Secure
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if ra := r.Header.Get("X-Real-IP"); ra != "" {
		return strings.TrimSpace(ra)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB 上限
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
		// 编码失败说明结构本身有问题；此时已写过状态码，只能记日志。
		// 注意：这里不静默丢弃 —— dev 阶段直接 panic 更容易暴露问题。
		panic("poolgate: JSON 编码失败: " + err.Error())
	}
}

// writeErr 是唯一的错误出口（D1）：客户端永远拿到结构化 kind + message。
func writeErr(w http.ResponseWriter, code int, e *errs.Error) {
	type payload struct {
		Error struct {
			Kind     string `json:"kind"`
			Message  string `json:"message"`
			Upstream string `json:"upstream,omitempty"`
		} `json:"error"`
	}
	var p payload
	p.Error.Kind = string(e.Kind)
	p.Error.Message = e.Message
	p.Error.Upstream = e.Upstream
	writeJSON(w, code, p)
}

func fmtDuration(d time.Duration) string {
	if d <= 0 {
		return "0 秒"
	}
	d = d.Round(time.Second)
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	if m > 0 {
		return itoa(m) + " 分 " + itoa(s) + " 秒"
	}
	return itoa(s) + " 秒"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// writeErrFromErr 从任意 error 取 Kind 并写出结构化错误（含 HTTP 码）。
// 面板与网关用同一套 kind 枚举，用户在两处看到的分类一致。
func writeErrFromErr(w http.ResponseWriter, err error) {
	k, _ := errs.KindOf(err)
	e := errs.New(k, "请求失败")
	if ee, ok := err.(*errs.Error); ok {
		e = ee
	}
	// 上游凭证失效时补一句「怎么办」：面板上出现的多半是刷新余额 / 签到这类按钮
	// 触发的，用户看到「上游返回 HTTP 401」只会以为面板坏了。
	if k == errs.SessionDead {
		e = e.WithMessage(e.Error() + "（该渠道账号的凭证已失效：到「账号」页点它的「重新登录」重新授权）")
	}
	// 禁言不是凭证问题 —— 这里必须说清楚「重新登录解不开」，否则用户会去点「重新登录」。
	if k == errs.Muted {
		e = e.WithMessage(e.Error() + "（该渠道账号被上游风控禁言：不是凭证问题，重新登录解不开，到解禁时间自动恢复）")
	}
	writeErr(w, statusForKind(k), e)
}

// statusForKind 把 errs.Kind 映射到 HTTP 状态码（与网关保持一致）。
func statusForKind(k errs.Kind) int {
	switch k {
	case errs.HardCredit:
		return http.StatusPaymentRequired
	case errs.SoftRate:
		return http.StatusTooManyRequests
	case errs.Muted:
		// 账号被上游禁言：网关自己这边没问题，是上游暂时不给这个号用 → 503。
		// 用 401 会把用户带沟里（以为自己的 Key/会话坏了），用 429 又像「限流，立刻重试」。
		return http.StatusServiceUnavailable
	case errs.SessionDead, errs.AuthFailed:
		// 上游凭证失效**不是**「管理员会话过期」。
		//
		// 这里返回 401 会被前端当成「我被登出了」：用户点一下「刷新余额」，
		// 面板立刻清空前端状态跳回登录页（实测踩到）—— 而管理员会话其实好好的。
		// 401 只属于 withAuth（管理员会话）；上游拒绝我们的凭证用 502 表示
		// 「我是网关，我这边上游不通」。kind 仍在响应里，前端照样能分辨。
		return http.StatusBadGateway
	case errs.ContentBlocked, errs.PromptTooLong, errs.ModelUnavailable, errs.Parse:
		return http.StatusBadRequest
	case errs.UpstreamFault, errs.Transport:
		return http.StatusBadGateway
	case errs.NoCandidate:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
