// password.go 面板改密码（需求：控制台不能只有命令行一条改密路径）。
//
// 设计取舍：只做「知道旧密码才能改新密码」这一条最普通的路径。
// 找回密码仍然走命令行（poolgate admin reset-password）—— 单管理员、无邮件、无手机号，
// 面板里能「忘记密码也能改」等于把唯一的门钥匙挂在门上。
//
// 控制台登录（本地管理员密码）与渠道授权（上游账号）是两套东西：这里改的是前者。
package console

import (
	"log"
	"net/http"

	"poolgate/internal/errs"
)

type changePasswordReq struct {
	Current string `json:"current"`
	Next    string `json:"next"`
	Confirm string `json:"confirm"`
}

type changePasswordResp struct {
	OK bool `json:"ok"`
	// SessionsRevoked 是改密后被注销的其它会话数（面板要如实告诉用户「别的设备退了」）。
	SessionsRevoked int `json:"sessions_revoked"`
}

// handleChangePassword 修改管理员密码。
//
// 校验顺序刻意如此：先确认旧密码，再比对新密码两次输入 —— 反过来的话，
// 攻击者可以用「两次不一致」和「旧密码错」两种响应的差别去试探旧密码。
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req changePasswordReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	// 与登录共用同一把锁与失败计数：否则这里就是一条不限速的密码爆破入口。
	ip := clientIP(r)
	if remain, locked := s.sess.CheckLock(ip); locked {
		writeErr(w, http.StatusTooManyRequests, errs.New(errs.AuthFailed,
			"连续失败次数过多，已锁定；剩余 "+fmtDuration(remain)))
		return
	}
	ok, err := s.admin.Verify(req.Current)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "校验失败").WithCause(err))
		return
	}
	if !ok {
		_, locked := s.sess.RecordFailure(ip)
		msg := "当前密码不正确"
		if locked {
			msg = "当前密码不正确，已连续失败 5 次，锁定 15 分钟"
		}
		log.Printf("console: 改密码失败（旧密码不符） ip=%s", ip)
		writeErr(w, http.StatusBadRequest, errs.New(errs.AuthFailed, msg))
		return
	}
	if req.Next == "" {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "新密码不能为空"))
		return
	}
	if req.Next != req.Confirm {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "两次输入的新密码不一致"))
		return
	}
	if req.Next == req.Current {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "新密码与当前密码相同，没有意义"))
		return
	}
	// 强度只提示不拦截（与首次设置一致）：这是自用单管理员系统，锁的是自己。
	if err := s.admin.Reset(req.Next); err != nil {
		writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "保存新密码失败").WithCause(err))
		return
	}
	s.sess.ClearFailures(ip)
	revoked := s.sess.DestroyOthers(sessionToken(r))
	log.Printf("console: 管理员密码已修改，注销其它会话 %d 个", revoked)
	writeJSON(w, http.StatusOK, changePasswordResp{OK: true, SessionsRevoked: revoked})
}
