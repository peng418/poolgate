// login.go CodeBuddy 的面板授权：**主路**与 WorkBuddy 相同的 state 轮询，
// **兜底**为「粘贴凭据 JSON / accessToken」。
//
// 主路（与 WorkBuddy 同源，只是出站身份不同）：
//
//	POST /v2/plugin/auth/state?platform=CLI   → {state, authUrl}
//	GET  /v2/plugin/auth/token?state=<state>  → 权威登录状态；未完成时业务 code != 0
//	GET  /v2/plugin/login/account?state=...   → uid / nickname / enterpriseId（带 Bearer）
//
// 兜底（为什么要有）：CodeBuddy 桌面端/插件会把登录态落在本机文件
//
//	.../CodeBuddyExtension/Data/Public/auth/*.info
//
// 里面是 `auth.accessToken / auth.refreshToken / auth.expiresAt` + `account.uid /
// account.enterpriseId`。当面板所在机器打不通上游授权接口、或用户不想再跳一次浏览器时，
// 把这整段 JSON（或只是一个 accessToken）粘回来即可完成授权 —— 兜底不放松任何校验：
// 有 refreshToken 就先刷新一次当验证，只有 accessToken 就做本地结构/过期校验。
//
// 与 WorkBuddy 的差别：state 存内存里（授权是 5 分钟的一次性交互），
// 且起始返回的 authUrl 会先跟随 301 解析成最终地址再给用户。
package codebuddy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const (
	EpAuthState = "/v2/plugin/auth/state?platform=CLI"
	EpAuthToken = "/v2/plugin/auth/token?state="
	EpLoginAcct = "/v2/plugin/login/account?state="

	// loginPage 是兜底会话里给用户打开的页面（主路可用时会被真实授权地址覆盖）。
	loginPage = "https://www.codebuddy.cn"
)

// hintText 是给面板的粘贴引导语。两条路都写清楚：主路点链接登录，兜底粘凭据。
const hintText = "方式一（推荐）：点上面的授权链接，在浏览器里登录 CodeBuddy / 腾讯云代码助手，" +
	"登录完成后本页面会自动拿到令牌。" +
	"方式二（兜底）：若链接打不开或上游授权接口不可用，在你已登录 CodeBuddy 的电脑上找到凭据文件 " +
	"`CodeBuddyExtension/Data/Public/auth/*.info`（macOS: ~/Library/Application Support/…，" +
	"Windows: %LOCALAPPDATA%\\…，Linux: ~/.local/share/…），用记事本打开后**把整段 JSON 粘到下面**" +
	"（它含 auth.accessToken / auth.refreshToken / account.uid，刷新令牌能自动续期，最稳）；" +
	"也可以只粘一个 accessToken。"

// authStateResp 是 auth/state 的响应体。
type authStateResp struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

// session 是一次进行中的 CodeBuddy 授权。主路与兜底共用同一个会话：
// 用户先粘贴了就当场用粘贴的凭据，否则按 state 轮询上游。
type session struct {
	a  *Adapter
	mu sync.Mutex
	// state / authURL 来自主路；state 为空表示主路不可用（只能靠粘贴兜底）。
	state   string
	authURL string
	// startErr 记录主路发起失败的原因，如实写进 Hint（不假装主路可用）。
	startErr string
	// pasted 是用户粘回来的凭据（兜底）。
	pasted    *pastedCredential
	cancelled bool
	done      bool
}

// 编译期断言：会话必须能发起授权，并能接收手工粘贴的凭据。
var (
	_ channel.LoginSession       = (*session)(nil)
	_ channel.CallbackAcceptor   = (*session)(nil)
	_ interface{ Hint() string } = (*session)(nil)
)

// StartLogin 实现 channel.Authorizer：主路拿 state 与授权页地址；主路不通时
// **不报错**，退回「等粘贴」会话（授权兜底必须仍可用）。
func (a *Adapter) StartLogin(ctx context.Context, opts channel.LoginOptions) (channel.LoginSession, error) {
	s := &session{a: a, authURL: loginPage}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+EpAuthState, bytes.NewReader([]byte("{}")))
	if err != nil {
		s.startErr = err.Error()
		return s, nil
	}
	a.commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	data, derr := a.doJSON(req)
	if derr != nil {
		s.startErr = derr.Error()
		return s, nil
	}
	var st authStateResp
	if json.Unmarshal(data, &st) != nil || st.State == "" || st.AuthURL == "" {
		s.startErr = "上游未返回 state/authUrl"
		return s, nil
	}
	s.state = st.State
	// 跟随跳转链拿到最终地址；解析失败就用原始地址（不阻塞授权）。
	if resolved, rerr := a.resolveAuthURL(ctx, st.AuthURL); rerr == nil && resolved != "" {
		s.authURL = resolved
	} else {
		s.authURL = st.AuthURL
	}
	return s, nil
}

func (s *session) AuthURL() string { return s.authURL }

// Hint 实现控制台的可选接口：面板据此显示粘贴引导语（主路失败时附上原因）。
func (s *session) Hint() string {
	s.mu.Lock()
	errStr := s.startErr
	s.mu.Unlock()
	if errStr != "" {
		return "（上游授权接口暂时不可用：" + errStr + "，请用「方式二」粘贴凭据完成授权）\n" + hintText
	}
	return hintText
}

func (s *session) Cancel() {
	s.mu.Lock()
	s.cancelled = true
	s.mu.Unlock()
}

// AcceptCallback 收用户粘回来的凭据（整段 .info JSON / 扁平 JSON / 裸 accessToken 都认）。
func (s *session) AcceptCallback(raw string) error {
	pc := extractCredential(raw)
	if pc == nil {
		return errs.New(errs.Parse,
			"没识别出 CodeBuddy 凭据：请把 auth/*.info 文件里的整段 JSON 粘过来（含 accessToken/refreshToken），"+
				"或只粘一个 accessToken").WithChannel(string(channel.CodeBuddy))
	}
	s.mu.Lock()
	s.pasted = pc
	s.mu.Unlock()
	return nil
}

// Poll 实现 channel.LoginSession：用户粘了就先用粘贴的凭据；否则按 state 轮询上游。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	cancelled, done, pasted, state := s.cancelled, s.done, s.pasted, s.state
	s.mu.Unlock()

	if cancelled {
		return nil, errs.New(errs.AuthFailed, "授权已取消")
	}
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.CodeBuddy))
	}
	if pasted != nil {
		return s.completePaste(ctx, pasted)
	}
	if state == "" {
		// 主路不可用：等用户粘贴。这是等待态，不是失败。
		return nil, channel.ErrPending
	}
	return s.pollOAuth(ctx, state)
}

// pollOAuth 问一次「浏览器那边登录好了没」。
func (s *session) pollOAuth(ctx context.Context, state string) (*channel.Credential, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.a.base+EpAuthToken+url.QueryEscape(state), nil)
	if err != nil {
		return nil, errs.New(errs.Transport, "构造轮询请求失败").WithCause(err)
	}
	s.a.commonHeaders(req)

	data, err := s.a.doJSON(req)
	if err != nil {
		if isLoginPending(err) {
			return nil, channel.ErrPending
		}
		return nil, err
	}
	tok := parseToken(data)
	if tok.AccessToken == "" {
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
	if exp := tokenExpiry(tok.ExpiresIn, tok.ExpiresAt); !exp.IsZero() {
		cred.ExpiresAt = exp
	}
	// uid/nickname/企业 要另外问一次（带 Bearer）。login/account 拿不到 uid 时**回落到令牌自带的 sub**：
	// 参考实现就以 JWT 的 sub 为权威 uid（workbuddy2api-hub/wb_accounts.py:1004
	// `uid = jwt_uid(token)`，login/account 只用来补昵称，失败也 try/except 放过），
	// 本包的粘贴兜底路径同样用 jwtUID 取 sub（见 completePaste）。两边都拿不到才报错，绝不编造 UID。
	if uid, ent, nick, aerr := s.a.fetchAccount(ctx, tok.AccessToken, state); aerr == nil {
		cred.UID = uid
		cred.Nickname = nick
		if ent != "" {
			cred.Extra["enterprise_id"] = ent
		}
	}
	if cred.UID == "" {
		cred.UID = jwtUID(tok.AccessToken)
	}
	if cred.UID == "" {
		return nil, errs.New(errs.Parse, "授权成功但拿不到账号 uid（login/account 未返回，令牌里也解不出 sub）").
			WithChannel(string(channel.CodeBuddy)).WithUpstream(truncate(string(data), 200))
	}
	s.markDone()
	return &cred, nil
}

// completePaste 用粘贴的凭据组装凭证，并**当场验一次**（不把失效凭据塞进池子）。
//
//   - 有 refreshToken：调刷新接口换一次（成功即证明凭据有效，并顺手拿到新 access token）；
//   - 只有 accessToken：本地判断是否可解出 uid、是否已过期（JWT 的 sub/exp），
//     拿不到 uid 或已过期就明确报错，绝不编造 UID、也不放行过期令牌。
func (s *session) completePaste(ctx context.Context, pc *pastedCredential) (*channel.Credential, error) {
	cred := &channel.Credential{
		UID:          pc.uid,
		Nickname:     pc.nickname,
		AccessToken:  pc.access,
		RefreshToken: pc.refresh,
		ExpiresAt:    pc.expiry(),
		Extra:        map[string]string{},
	}
	if pc.enterprise != "" {
		cred.Extra["enterprise_id"] = pc.enterprise
	}

	if pc.refresh != "" {
		// 走刷新即验证；Refresh 会把 uid/Extra 原样带过去。
		nc, err := s.a.Refresh(ctx, cred)
		if err != nil {
			return nil, err
		}
		if nc.UID == "" {
			nc.UID = jwtUID(nc.AccessToken)
		}
		if nc.UID == "" {
			return nil, errs.New(errs.Parse, "凭据缺少账号 uid，且令牌里解不出 sub，无法落盘").
				WithChannel(string(channel.CodeBuddy))
		}
		if nc.Extra == nil {
			nc.Extra = map[string]string{}
		}
		if pc.nickname != "" {
			nc.Nickname = pc.nickname
		}
		// 新令牌自己是权威的过期时间：换出来的 access token 通常是 JWT，
		// 用它覆盖从 .info 文件带过来的旧 expiresAt，避免拿着旧时间反复刷新。
		if exp := jwtExpiry(nc.AccessToken); !exp.IsZero() {
			nc.ExpiresAt = exp
		}
		s.markDone()
		return nc, nil
	}

	if cred.AccessToken == "" {
		return nil, errs.New(errs.Parse, "凭据里没有 accessToken 也没有 refreshToken").
			WithChannel(string(channel.CodeBuddy))
	}
	if cred.UID == "" {
		return nil, errs.New(errs.Parse,
			"这段 accessToken 解不出账号 uid（它不是 JWT 或已损坏）：请改粘完整的 auth/*.info JSON（含 account.uid）").
			WithChannel(string(channel.CodeBuddy))
	}
	if exp := cred.ExpiresAt; !exp.IsZero() && !time.Now().Before(exp) {
		return nil, errs.New(errs.SessionDead,
			"accessToken 已过期，且没有 refreshToken 可续期：请重新粘贴含 refreshToken 的凭据").
			WithChannel(string(channel.CodeBuddy))
	}
	// 标记只有 access token：过期后无法自动续，需要重粘。
	cred.Extra["access_token_only"] = "1"
	s.markDone()
	return cred, nil
}

func (s *session) markDone() {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
}

// fetchAccount 取 uid / enterpriseId / nickname。
func (a *Adapter) fetchAccount(ctx context.Context, token, state string) (uid, enterpriseID, nickname string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+EpLoginAcct+url.QueryEscape(state), nil)
	if err != nil {
		return "", "", "", err
	}
	a.commonHeaders(req)
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
func (a *Adapter) resolveAuthURL(ctx context.Context, rawURL string) (string, error) {
	current := rawURL
	for hop := 0; hop < 5; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return "", err
		}
		a.commonHeaders(req)
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
// 上游用业务 code 非 0（如 11217 "login ing..."）表达未完成，与真正的 HTTP 失败混在一起。
// 这里按实测判据区分：带 code= / login 字样，且不是 http/parse 错误。
func isLoginPending(err error) bool {
	s := err.Error()
	return (strings.Contains(s, "code=") || strings.Contains(s, "login")) &&
		!strings.Contains(s, "http ") && !strings.Contains(s, "parse")
}

// ---------------------------------------------------------------------------
// 粘贴凭据的解析：auth/*.info（嵌套）+ 扁平 JSON + 裸令牌
// ---------------------------------------------------------------------------

// pastedCredential 是粘贴内容解析后的中间形态。字段都可能缺席 —— 缺席就如实留空。
type pastedCredential struct {
	access     string
	refresh    string
	uid        string
	enterprise string
	nickname   string
	expiresAt  time.Time
}

// expiry 返回凭据的过期时间（优先文件里的 expiresAt，其次 JWT 的 exp）。
func (p *pastedCredential) expiry() time.Time {
	if !p.expiresAt.IsZero() {
		return p.expiresAt
	}
	return jwtExpiry(p.access)
}

// extractCredential 从粘贴内容里抽凭据。认这些形态（用户会怎么粘没法规定，全认下来最省事）：
//
//	auth/*.info 整段：{"auth":{"accessToken":"…","refreshToken":"…","expiresAt":1712…}, "account":{"uid":"…","enterpriseId":"…"}}
//	扁平 JSON：{"accessToken":"…","refreshToken":"…","uid":"…"}
//	带 data 包裹：{"data":{"auth":{…},"account":{…}}}
//	裸串 / "带引号" / Bearer xxx / accessToken=xxx / key = value
func extractCredential(raw string) *pastedCredential {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(s), &obj) == nil {
			if pc := credFromObject(obj); pc != nil {
				return pc
			}
		}
	}
	// 非 JSON（或 JSON 里没抽到令牌）：当作裸令牌处理。
	if tok := cleanToken(s); tok != "" {
		return &pastedCredential{access: tok, uid: jwtUID(tok), expiresAt: jwtExpiry(tok)}
	}
	return nil
}

// credFromObject 从解析后的 JSON 对象里抽凭据，兼容嵌套（auth/account）与扁平两种排布。
func credFromObject(obj map[string]any) *pastedCredential {
	// 常见导出会再套一层 data。
	if inner, ok := obj["data"].(map[string]any); ok {
		if pc := credFromObject(inner); pc != nil {
			return pc
		}
	}

	pc := &pastedCredential{}
	// 嵌套形态：auth.* / account.*
	if auth, ok := obj["auth"].(map[string]any); ok {
		pc.access = firstString(auth, "accessToken", "access_token", "token")
		pc.refresh = firstString(auth, "refreshToken", "refresh_token")
		if n := firstInt(auth, "expiresAt", "expires_at", "expiry"); n > 0 {
			pc.expiresAt = epochMillis(n)
		}
	}
	if acct, ok := obj["account"].(map[string]any); ok {
		pc.uid = firstString(acct, "uid", "userId", "user_id", "accountId")
		pc.enterprise = firstString(acct, "enterpriseId", "enterprise_id", "tenantId", "tenant_id")
		pc.nickname = firstString(acct, "nickname", "name", "userName")
	}
	// 扁平形态（覆盖/补齐上面的空值）。
	if pc.access == "" {
		pc.access = firstString(obj, "accessToken", "access_token", "access")
	}
	if pc.refresh == "" {
		pc.refresh = firstString(obj, "refreshToken", "refresh_token", "refresh")
	}
	if pc.uid == "" {
		pc.uid = firstString(obj, "uid", "userId", "user_id", "accountId", "sub")
	}
	if pc.enterprise == "" {
		pc.enterprise = firstString(obj, "enterpriseId", "enterprise_id", "tenantId", "tenant_id")
	}
	if pc.nickname == "" {
		pc.nickname = firstString(obj, "nickname", "name")
	}
	if pc.expiresAt.IsZero() {
		if n := firstInt(obj, "expiresAt", "expires_at", "expiry"); n > 0 {
			pc.expiresAt = epochMillis(n)
		}
	}

	if pc.access == "" && pc.refresh == "" {
		return nil
	}
	if pc.uid == "" && pc.access != "" {
		pc.uid = jwtUID(pc.access)
	}
	if pc.expiresAt.IsZero() && pc.access != "" {
		pc.expiresAt = jwtExpiry(pc.access)
	}
	return pc
}

// cleanToken 从非 JSON 文本里抽一个令牌：去前缀、去引号、去 `key = value` 包装。
func cleanToken(s string) string {
	s = strings.TrimSpace(s)
	// `key = value`（我们引导语里的输出格式就是 `k+' = '+v`）。
	if i := strings.Index(s, " = "); i >= 0 {
		s = strings.TrimSpace(s[i+3:])
	}
	// `accessToken=xxxx` / `auth.accessToken=xxxx` 这类整行。
	for _, p := range []string{"accessToken=", "access_token=", "refreshToken=", "refresh_token=", "token="} {
		if i := strings.Index(s, p); i >= 0 {
			s = s[i+len(p):]
			if j := strings.IndexAny(s, ";, \t\n\"'"); j >= 0 {
				s = s[:j]
			}
			break
		}
	}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "Bearer ")
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	s = strings.TrimSpace(s)
	// 令牌形态：较长的不透明 ASCII 串；挡掉明显误粘的内容（含空白的多词文本、
	// 中文说明文字 —— 上游令牌都是 ASCII，出现非 ASCII 一定是粘错了别的东西）。
	if len(s) < 20 || strings.ContainsAny(s, " \t\n") || !isASCII(s) {
		return ""
	}
	return s
}

// isASCII 判断是否全为 ASCII 可见字符（令牌一定如此）。
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// epochMillis 把可能是秒或毫秒的 epoch 归一成时间（>1e12 视为毫秒）。
func epochMillis(n int64) time.Time {
	if n > 1e12 {
		return time.UnixMilli(n)
	}
	return time.Unix(n, 0)
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstInt(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n
			}
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// JWT 轻解析（只用来取 uid 与过期时间，不校验签名 —— 校验由上游负责）
// ---------------------------------------------------------------------------

func jwtPayload(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func jwtUID(tok string) string {
	m := jwtPayload(tok)
	if m == nil {
		return ""
	}
	s, _ := m["sub"].(string)
	return strings.TrimSpace(s)
}

func jwtExpiry(tok string) time.Time {
	m := jwtPayload(tok)
	if m == nil {
		return time.Time{}
	}
	switch v := m["exp"].(type) {
	case float64:
		return time.Unix(int64(v), 0)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}
