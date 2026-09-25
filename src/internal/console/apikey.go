// apikey.go 面板上的「网关 API Key」：看掩码 / 显示明文 / 重新生成。
//
// 为什么要开这个口子：Key 原先只在首次启动时生成，用户想拿到它只能 ssh 上去
// cat config.json —— 而钥匙本来就该能从门上取，不该从窗户爬。面板上加这一屏
// 之后，「拿到地址 + 拿到 Key」都能在一处完成。
//
// 安全边界（三条，改动时别松）：
//  1. 全部接口走 withAuth（管理员会话），与其它管理接口同级；
//  2. 明文只在响应里出现，绝不写日志（这里只用 log.Printf 记「轮换了」这类事实）；
//  3. 掩码是默认展示态，明文必须由用户显式点「显示明文」才回来。
package console

import (
	"log"
	"net/http"

	"poolgate/internal/errs"
)

// apiKeyMasked 把 key 打成 pg-abcd…wxyz 形式。
//
// 短 key（异常数据）整条打码，绝不做「短到能还原」的掩码 —— 前端会把掩码
// 直接贴在屏幕上。
func apiKeyMasked(key string) string {
	const fixed = 4 // 前缀 pg- 之外保留的可见字符数
	if key == "" {
		return ""
	}
	// 只看 rune 数，ASCII key 下等价于字节数；非 ASCII 走保守分支。
	r := []rune(key)
	if len(r) <= fixed*2+1 {
		return "••••••"
	}
	return string(r[:fixed]) + "…" + string(r[len(r)-fixed:])
}

// handleAPIKeyGet 返回当前 Key 的掩码与创建时间（不回显明文）。
func (s *Server) handleAPIKeyGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 GET"))
		return
	}
	if s.opts.Keys == nil {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.Parse, "API Key 存储未装配"))
		return
	}
	key, createdAt, ok := s.opts.Keys.Current()
	writeJSON(w, http.StatusOK, map[string]any{
		"exists":     ok,
		"masked":     apiKeyMasked(key),
		"created_at": createdAt,
		"count":      s.opts.Keys.Count(),
		"path":       s.opts.Keys.Path(),
	})
}

// handleAPIKeyReveal 回显当前 Key 明文（点「显示明文」才走这里）。
//
// 单独做成 POST 而不是塞进 GET：明文是显式请求的动作，不该在页面加载时
// 顺带出现在任何响应里（也避免被代理/取证日志顺手记下）。
func (s *Server) handleAPIKeyReveal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	if s.opts.Keys == nil {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.Parse, "API Key 存储未装配"))
		return
	}
	key, createdAt, ok := s.opts.Keys.Current()
	if !ok {
		writeErr(w, http.StatusNotFound, errs.New(errs.Parse, "还没有 API Key（重启服务会自动生成一条）"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "created_at": createdAt})
}

// handleAPIKeyRotate 生成新 Key 并废弃旧 Key，明文只在这一次响应里返回。
func (s *Server) handleAPIKeyRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	if s.opts.Keys == nil {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.Parse, "API Key 存储未装配"))
		return
	}
	key, createdAt, err := s.opts.Keys.Rotate()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "生成新 Key 失败").WithCause(err))
		return
	}
	// 只记事实，不记明文（明文进日志等于把钥匙贴在门上）。
	log.Printf("console: 网关 API Key 已重新生成，旧 Key 立即失效")
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "created_at": createdAt, "rotated": true})
}
