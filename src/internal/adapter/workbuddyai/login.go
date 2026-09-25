// login.go WorkBuddyAI（国际版）的面板授权：state 轮询式。
//
// 与国内版路径完全相同、仅 host 不同（www.workbuddy.ai）：
//
//	POST /v2/plugin/auth/state?platform=CLI   → {state, authUrl}
//	GET  /v2/plugin/auth/token?state=<state>  → 权威登录状态；未完成时 code=11217
//	GET  /v2/plugin/login/account?state=...   → uid / nickname（带 Bearer）
//
// 国际版未完成时固定返回 code=11217（"login ing..."），语义比国内版更明确；
// 这里仍沿用同一套 pending 判据，两个渠道行为一致。
package workbuddyai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const (
	EpAuthState = "/v2/plugin/auth/state?platform=CLI"
	EpAuthToken = "/v2/plugin/auth/token?state="
	EpLoginAcct = "/v2/plugin/login/account?state="
)

// authStateResp 是 auth/state 的响应体。
type authStateResp struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

// wbSession 是一次进行中的 WorkBuddy 授权。
type wbSession struct {
	a         *Adapter
	state     string
	authURL   string
	cancelled bool
}

// StartLogin 实现 channel.Authorizer：拿 state 与授权页地址。
func (a *Adapter) StartLogin(ctx context.Context, opts channel.LoginOptions) (channel.LoginSession, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+EpAuthState, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造授权请求失败").WithChannel(string(channel.WorkBuddyAI)).WithCause(err)
	}
	commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	data, err := a.doJSON(req)
	if err != nil {
		return nil, err
	}
	var st authStateResp
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return nil, errs.New(errs.Parse, "上游未返回 state/authUrl，无法发起授权").
			WithChannel(string(channel.WorkBuddyAI)).WithUpstream(truncate(string(data), 200))
	}
	// 跟随跳转链拿到最终地址；解析失败就用原始地址（不阻塞授权）。
	if resolved, rerr := a.resolveAuthURL(ctx, st.AuthURL); rerr == nil && resolved != "" {
		st.AuthURL = resolved
	}
	return &wbSession{a: a, state: st.State, authURL: st.AuthURL}, nil
}

func (s *wbSession) AuthURL() string { return s.authURL }
func (s *wbSession) Cancel()         { s.cancelled = true }

// Poll 实现 channel.LoginSession：问一次「浏览器那边登录好了没」。
func (s *wbSession) Poll(ctx context.Context) (*channel.Credential, error) {
	if s.cancelled {
		return nil, errs.New(errs.AuthFailed, "授权已取消")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.a.base+EpAuthToken+url.QueryEscape(s.state), nil)
	if err != nil {
		return nil, errs.New(errs.Transport, "构造轮询请求失败").WithCause(err)
	}
	commonHeaders(req)

	data, err := s.a.doJSON(req)
	if err != nil {
		if isLoginPending(err) {
			return nil, channel.ErrPending
		}
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		// 200 + 空 token：上游还在处理中，继续等（不是失败，也不是成功）。
		return nil, channel.ErrPending
	}

	cred := channel.Credential{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Extra:        map[string]string{},
	}
	if tok.Domain != "" {
		cred.Extra["domain"] = tok.Domain
	}
	if tok.ExpiresIn > 0 {
		cred.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	// uid/nickname 要另外问一次（带 Bearer）。取不到就是取不到 —— 不编造 UID。
	if uid, ent, nick, aerr := s.a.fetchAccount(ctx, tok.AccessToken, s.state); aerr == nil {
		cred.UID = uid
		cred.Nickname = nick
		if ent != "" {
			cred.Extra["enterprise_id"] = ent
		}
	}
	if cred.UID == "" {
		return nil, errs.New(errs.Parse, "授权成功但拿不到账号 uid（login/account 未返回）").
			WithChannel(string(channel.WorkBuddyAI)).WithUpstream(truncate(string(data), 200))
	}
	return &cred, nil
}

// fetchAccount 取 uid / enterpriseId / nickname。
func (a *Adapter) fetchAccount(ctx context.Context, token, state string) (uid, enterpriseID, nickname string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+EpLoginAcct+url.QueryEscape(state), nil)
	if err != nil {
		return "", "", "", err
	}
	commonHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)

	data, derr := a.doJSON(req)
	if derr != nil {
		return "", "", "", derr
	}
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if jerr := json.Unmarshal(data, &acct); jerr != nil {
		return "", "", "", jerr
	}
	return acct.UID, acct.EnterpriseID, acct.Nickname, nil
}

// resolveAuthURL 手动跟随重定向链，拿到浏览器可直接打开的最终地址。
// 上游链形如 copilot.tencent.com/login → 301 加斜杠 → www.codebuddy.cn/login/…。
func (a *Adapter) resolveAuthURL(ctx context.Context, rawURL string) (string, error) {
	current := rawURL
	for hop := 0; hop < 5; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return "", err
		}
		commonHeaders(req)
		resp, err := a.noFollow.Do(req)
		if err != nil {
			return "", err
		}
		loc := resp.Header.Get("Location")
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if loc == "" {
			return current, nil
		}
		ref, err := url.Parse(loc)
		if err != nil {
			return "", err
		}
		base, err := url.Parse(current)
		if err != nil {
			return "", err
		}
		current = base.ResolveReference(ref).String()
	}
	return current, nil
}

// isLoginPending 判断 doJSON 的错误是不是「还没登录完」。
//
// 上游用业务 code 非 0（如 login ing）表达未完成，与真正的 HTTP 失败混在一起。
// 这里按 wild-work 的实测判据区分：带 code= / login 字样且不是 http/parse 错误。
func isLoginPending(err error) bool {
	s := err.Error()
	return (strings.Contains(s, "code=") || strings.Contains(s, "login")) &&
		!strings.Contains(s, "http ") && !strings.Contains(s, "parse")
}
