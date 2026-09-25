package antigravity

// login.go 登录：Google OAuth 授权码 + PKCE（粘贴回码形态）。
//
// 三步，缺一不可：
//  1. 生成授权地址（PKCE + state），用户在浏览器里授权；
//  2. 用户把回调地址/授权码粘回面板，我们换取令牌（access + refresh）；
//  3. **开通**（loadCodeAssist / onboardUser，见 project.go）—— 只换令牌不开通，
//     调对话会 412/403。这一步放在 Poll 里做完，绝不把「没开通的凭证」塞进池子。
//
// 为什么用「粘回码」而不是起本机回调服务器：Antigravity 的回调锁死在
// http://localhost:51121/oauth-callback，NAS 上既没有桌面浏览器也没有那个端口 ——
// 让用户把地址栏那一整条 URL 粘回来，是不改回调、不开放端口、不需要公网地址的唯一完成方式。
// 这正是 gemini/oauth.go 已经在用的模式，控制台也已有对应的粘贴通道（CallbackAcceptor）。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// hintText 是给面板的粘贴引导语。
//
// 必须提前说明「跳到 localhost 打不开是正常的」，否则用户看到「无法访问此网站」就会以为
// 授权失败了，然后关掉页面 —— 那串 code 只在地址栏里，关掉就得从头再来一遍。
const hintText = "在浏览器里打开授权页并同意授权。授权后浏览器会跳到 http://localhost:51121/oauth-callback?code=…；" +
	"这个地址**打不开是正常的**（那是本机端口，不是错误）。请把地址栏里那一整条 URL 复制下来粘到下面" +
	"（只粘 code= 后面那串也行）。"

type session struct {
	a         *Adapter
	verifier  string
	state     string
	authURL   string
	createdAt time.Time

	mu   sync.Mutex
	code string // 用户粘回来的授权码
	done bool
}

// StartLogin 生成授权地址（带 PKCE + state）。
func (a *Adapter) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	verifier, challenge := makePKCE()
	state := randHex(32)

	u := url.Values{}
	u.Set("client_id", clientID)
	u.Set("redirect_uri", redirectURI)
	u.Set("response_type", "code")
	u.Set("scope", scope)
	u.Set("access_type", "offline") // 必须：要 refresh_token，否则令牌一小时一过期
	u.Set("prompt", "consent")      // 强制重新同意，确保每次都给 refresh_token
	u.Set("state", state)
	u.Set("code_challenge", challenge)
	u.Set("code_challenge_method", "S256")

	return &session{
		a:         a,
		verifier:  verifier,
		state:     state,
		authURL:   epAuth + "?" + u.Encode(),
		createdAt: time.Now(),
	}, nil
}

func (s *session) AuthURL() string { return s.authURL }

// Hint 实现控制台的可选接口：面板据此切换成「粘贴」形态（不画二维码、不提示扫码）。
func (s *session) Hint() string { return hintText }

// AcceptCallback 收用户粘回来的授权码（裸 code / 整条回调 URL 都认）。
func (s *session) AcceptCallback(raw string) error {
	code := extractCode(raw)
	if code == "" {
		return errs.New(errs.Parse,
			"没识别出授权码：请把浏览器地址栏里那一整条 URL（含 code=…）粘过来").
			WithChannel(string(channel.Antigravity))
	}
	s.mu.Lock()
	s.code = code
	s.mu.Unlock()
	return nil
}

// Poll 在用户粘了码之后完成「换令牌 → 认身份 → 开通」。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	code, done := s.code, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次授权已经完成过了").WithChannel(string(channel.Antigravity))
	}
	if code == "" {
		return nil, channel.ErrPending // 还没粘码：正常的等待态，不是错误
	}

	tok, err := s.a.exchangeCode(ctx, code, s.verifier)
	if err != nil {
		return nil, err
	}
	cred := &channel.Credential{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.expiry(),
		Extra:        map[string]string{},
	}
	// 身份：用 userinfo 的邮箱当账号标识（拿不到就退回 user-<随机>，至少不冲突）。
	if email, name := s.a.fetchIdentity(ctx, cred); email != "" {
		cred.UID, cred.Nickname = email, name
	}
	if cred.UID == "" {
		cred.UID = "google-" + randHex(6)
		cred.Nickname = cred.UID
	}
	// 开通：拿到 project。失败就是失败 —— 不把「没开通的凭证」塞进池子，
	// 否则用户装完第一次调用收到 412 却不知道为什么（红线一）。
	project, err := s.a.ensureProject(ctx, cred, cred.AccessToken)
	if err != nil {
		return nil, err
	}
	cred.Extra["project"] = project

	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	return cred, nil
}

func (s *session) Cancel() {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
}

// tokenResp 是 OAuth 令牌端点的响应。
type tokenResp struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (t tokenResp) expiry() time.Time {
	if t.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	return time.Now().Add(time.Hour) // 上游没给：保守 1 小时
}

// exchangeCode 用授权码换令牌。
func (a *Adapter) exchangeCode(ctx context.Context, code, verifier string) (tokenResp, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)

	var out tokenResp
	status, raw, err := a.postForm(ctx, nil, a.tokenURL, form, &out)
	if err != nil {
		return out, errs.New(errs.Transport, "换取令牌失败").WithChannel(string(channel.Antigravity)).WithCause(err)
	}
	if out.AccessToken == "" {
		msg := out.ErrorDescription
		if msg == "" {
			msg = out.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", status)
		}
		return out, errs.New(errs.AuthFailed, "授权码兑换失败："+msg).
			WithChannel(string(channel.Antigravity)).WithUpstream(truncate(string(raw), 200))
	}
	// 没有 refresh_token 就等于「只能用一小时」，那不是可用的凭证 —— 当场说清，
	// 别让用户装上之后一小时再回来问为什么坏了。
	if strings.TrimSpace(out.RefreshToken) == "" {
		return out, errs.New(errs.AuthFailed,
			"上游没有返回 refresh_token：授权页里请确认点了「同意」（access_type=offline 已请求）").
			WithChannel(string(channel.Antigravity)).WithUpstream(truncate(string(raw), 200))
	}
	return out, nil
}

// Refresh 用 refresh_token 换新的 access token（Google 可能轮换 refresh_token，新的要落盘）。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if c == nil || strings.TrimSpace(c.RefreshToken) == "" {
		return nil, nil // 刷不了：交给上层按 401 处理，不假装成功
	}
	out, err := a.refreshWith(ctx, c, strings.TrimSpace(c.RefreshToken))
	if err != nil {
		return nil, err
	}
	nc := *c
	nc.AccessToken = out.AccessToken
	if strings.TrimSpace(out.RefreshToken) != "" {
		nc.RefreshToken = out.RefreshToken
	}
	nc.ExpiresAt = out.expiry()
	a.cacheToken(strings.TrimSpace(c.RefreshToken), nc.AccessToken, nc.ExpiresAt)
	return &nc, nil
}

// refreshWith 真正向上游换令牌。
func (a *Adapter) refreshWith(ctx context.Context, c *channel.Credential, refresh string) (tokenResp, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)

	var out tokenResp
	status, raw, err := a.postForm(ctx, c, a.tokenURL, form, &out)
	if err != nil {
		return out, errs.New(errs.Transport, "刷新令牌失败").WithChannel(string(channel.Antigravity)).
			WithAccount(uidOf(c)).WithCause(err)
	}
	if out.AccessToken == "" {
		kind := errs.SessionDead
		if status == 429 {
			kind = errs.SoftRate
		}
		msg := out.ErrorDescription
		if msg == "" {
			msg = out.Error
		}
		return out, errs.New(kind, "刷新失败，需要重新授权："+msg).
			WithChannel(string(channel.Antigravity)).WithAccount(uidOf(c)).WithUpstream(truncate(string(raw), 200))
	}
	return out, nil
}

// fetchIdentity 取账号邮箱/昵称（失败不影响授权，只影响展示）。
func (a *Adapter) fetchIdentity(ctx context.Context, c *channel.Credential) (email, name string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.userInfoURL, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", ""
	}
	var out struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return "", ""
	}
	if out.Name == "" {
		out.Name = out.Email
	}
	return out.Email, out.Name
}

// postForm 发一个表单请求（token 端点用），返回状态码与原始报文。
func (a *Adapter) postForm(ctx context.Context, c *channel.Credential, urlStr string, form url.Values, out any) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode, raw, nil
}

// makePKCE 生成 code_verifier / code_challenge（S256）。
func makePKCE() (verifier, challenge string) {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

func randHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// extractCode 从用户粘回来的内容里抽出授权码：整条回调 URL 也行，裸 code 也行。
func extractCode(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "code="); i >= 0 {
		s = s[i+len("code="):]
		if j := strings.IndexAny(s, "&# \n\r\t"); j >= 0 {
			s = s[:j]
		}
	}
	// Google 的 code 里有 `/`，粘回来时可能被百分号编码成 %2F，这里解一次。
	if u, err := url.QueryUnescape(s); err == nil {
		s = u
	}
	s = strings.TrimSpace(s)
	// code 形如 4/0Axxxxxxxx…；不做严格校验，只挡明显不是的（太短/含空白）。
	if len(s) < 16 || strings.ContainsAny(s, " \t\n") {
		return ""
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
