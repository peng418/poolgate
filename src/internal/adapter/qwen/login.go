package qwen

// login.go 设备码授权（RFC 8628）+ PKCE —— 用户只需在浏览器/手机点一下。
//
// 为什么不用「邮箱密码自动授权」：那条路要把用户的邮箱密码存到本机（公开参考实现就是这么做的），
// 对一个自用的账号池网关来说这是不必要的高风险。设备码流程一样能全程轮询，而**密码始终只在
// 用户自己的浏览器里输入**，我们拿到的只是设备码换来的 token。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// Login 是 channel.Channel 要求的「一次性登录」入口。
//
// 通义的登录是**交互式**的（设备码，用户要在浏览器点一下），所以走 channel.Authorizer
// 那条路（面板「添加账号」）；这里明确报错，别让调用方以为可以非交互登录。
func (a *Adapter) Login(context.Context) (*channel.Credential, error) {
	return nil, errs.New(errs.Parse,
		"通义需要交互式授权：请在面板「账号」页点「添加账号 → 通义」，扫码/点链接完成授权").
		WithChannel(string(channel.Qwen))
}

// deviceSession 是一次进行中的设备码授权。
type deviceSession struct {
	a          *Adapter
	verifier   string
	deviceCode string
	userCode   string
	authURL    string
	expiresAt  time.Time
	done       bool
}

// StartLogin 申请设备码，把「用户要打开的授权页」交出去（面板会画成二维码）。
func (a *Adapter) StartLogin(ctx context.Context, _ channel.LoginOptions) (channel.LoginSession, error) {
	verifier, challenge := makePKCE()
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("scope", scope)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")

	var out struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
	}
	status, raw, err := a.doForm(ctx, nil, a.chatBase+epDeviceCode, form, &out)
	if err != nil {
		return nil, errs.New(errs.Transport, "申请设备码失败").WithChannel(string(channel.Qwen)).WithCause(err)
	}
	if status >= 400 || out.DeviceCode == "" {
		return nil, errs.New(a.Classify(status, raw), "上游拒绝了设备码申请").
			WithChannel(string(channel.Qwen)).WithUpstream(truncate(string(raw), 200))
	}
	// 优先给「带 user_code 的完整地址」：用户点开就只剩登录一步，扫码也直接可用。
	auth := out.VerificationURIComplete
	if auth == "" {
		auth = out.VerificationURI
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &deviceSession{
		a: a, verifier: verifier, deviceCode: out.DeviceCode, userCode: out.UserCode,
		authURL: auth, expiresAt: time.Now().Add(ttl),
	}, nil
}

func (s *deviceSession) AuthURL() string { return s.authURL }

// Poll 问一次「好了没」：按 RFC 8628，未完成时上游返回 400 + authorization_pending / slow_down。
func (s *deviceSession) Poll(ctx context.Context) (*channel.Credential, error) {
	if s.done {
		return nil, errs.New(errs.Parse, "这次授权已经完成过了").WithChannel(string(channel.Qwen))
	}
	if time.Now().After(s.expiresAt) {
		return nil, errs.New(errs.SessionDead, "设备码已过期（通常 5 分钟），请重新发起授权").
			WithChannel(string(channel.Qwen))
	}
	form := url.Values{}
	form.Set("grant_type", deviceGrant)
	form.Set("client_id", clientID)
	form.Set("device_code", s.deviceCode)
	form.Set("code_verifier", s.verifier)

	var out struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	status, raw, err := s.a.doForm(ctx, nil, s.a.chatBase+epToken, form, &out)
	if err != nil {
		return nil, errs.New(errs.Transport, "换取令牌失败").WithChannel(string(channel.Qwen)).WithCause(err)
	}
	if out.AccessToken == "" {
		switch out.Error {
		case "authorization_pending":
			return nil, channel.ErrPending
		case "slow_down":
			// 上游要求放慢轮询；控制台本身是 2 秒问一次，这里照旧返回 pending 即可。
			return nil, channel.ErrPending
		case "expired_token":
			return nil, errs.New(errs.SessionDead, "设备码已过期，请重新发起授权").WithChannel(string(channel.Qwen))
		case "access_denied":
			return nil, errs.New(errs.AuthFailed, "你在授权页拒绝了这次授权").WithChannel(string(channel.Qwen))
		}
		msg := out.ErrorDescription
		if msg == "" && out.Error != "" {
			msg = out.Error
		}
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		return nil, errs.New(s.a.Classify(status, raw), "换取令牌失败："+msg).
			WithChannel(string(channel.Qwen)).WithUpstream(truncate(string(raw), 200))
	}
	exp := time.Now().Add(2 * time.Hour) // 上游没给或给得离谱时的保守默认
	if out.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	s.done = true
	uid, nick := identityOf(out.AccessToken)
	if uid == "" {
		// token 不是 JWT（或不带 sub）时也要有稳定标识：否则控制台会因为「uid 为空」拒绝落盘
		// （它宁可报错也不存幽灵账号）。用 refresh_token 的摘要前缀兜底 —— 同一个授权重复
		// 读取时得到同一个 UID；重新授权拿到新 refresh_token 会成为新账号，这是可接受的代价。
		sum := sha256.Sum256([]byte(out.RefreshToken + out.AccessToken))
		uid = "qwen-" + base64.RawURLEncoding.EncodeToString(sum[:6])
	}
	return &channel.Credential{
		UID:          uid,
		Nickname:     nick,
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		ExpiresAt:    exp,
		Extra:        map[string]string{"locale": "zh-CN"},
	}, nil
}

func (s *deviceSession) Cancel() { s.done = true }

// Refresh 用 refresh_token 续期（上游会轮换 refresh_token，所以要把新的落盘 —— 落盘由调用方负责）。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if c == nil || strings.TrimSpace(c.RefreshToken) == "" {
		// 没有 refresh token：明确表示刷不了，交给上层按 401 逻辑处理（不要假装刷新成功）。
		return nil, nil
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", c.RefreshToken)
	form.Set("client_id", clientID)

	var out struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	status, raw, err := a.doForm(ctx, c, a.chatBase+epToken, form, &out)
	if err != nil {
		return nil, errs.New(errs.Transport, "续期请求失败").WithChannel(string(channel.Qwen)).WithAccount(c.UID).WithCause(err)
	}
	if out.AccessToken == "" {
		kind := errs.SessionDead // refresh_token 失效 = 这个号必须重新授权
		if status == 429 {
			kind = errs.SoftRate
		}
		msg := out.ErrorDescription
		if msg == "" {
			msg = out.Error
		}
		return nil, errs.New(kind, "续期失败，需要重新授权："+msg).
			WithChannel(string(channel.Qwen)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	nc := *c
	nc.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		nc.RefreshToken = out.RefreshToken // 上游轮换过就存新的
	}
	nc.ExpiresAt = time.Now().Add(2 * time.Hour)
	if out.ExpiresIn > 0 {
		nc.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	if uid, nick := identityOf(out.AccessToken); uid != "" {
		nc.UID, nc.Nickname = uid, nick
	}
	return &nc, nil
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

// identityOf 从 access token（JWT）里读身份，用作用户可见的账号标识。
//
// 只解析 payload、**不验签**：这不是安全判断，只是给面板一个「这是哪个号」的标签。
// 拿不到就退回空串，由调用方补一个稳定后缀（避免把同一个号存成两个账号）。
func identityOf(token string) (uid, nickname string) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return "", ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := claims[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	// 通义的 JWT 身份字段参考实现读的是 `id`（qwen-free-api internal/qbridge/qwen.go
	// decodeJWT：claims{ID:"id", Email, Name}），OAuth 令牌常见的是 `sub`；两个都认，
	// 再退到 email/username。只解析 payload、不验签，仅用于面板标识。
	uid = pick("sub", "id", "uid", "user_id", "userId", "email", "username")
	nickname = pick("username", "name", "nickname", "email")
	return uid, nickname
}

// doForm 发一个表单请求，返回状态码与原始报文（状态码 >=400 不算 Go 层错误 —— 上游的
// 业务错误都带在 body 里，由调用方按 body 判读）。
func (a *Adapter) doForm(ctx context.Context, c *channel.Credential, urlStr string, form url.Values, out any) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// 客户端标识必须给：实测用 Go 默认 UA 打授权端点会被阿里云 WAF 拦成 HTML 页
	// （而带 QwenCode/浏览器 UA 时返回正常 JSON）。这不是签名，只是「别看起来像脚本」。
	req.Header.Set("User-Agent", cliUA)
	req.Header.Set("Origin", chatBase)
	req.Header.Set("Referer", chatBase+"/")
	if c != nil && strings.TrimSpace(c.AccessToken) != "" {
		req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	}
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

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
