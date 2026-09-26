// login.go 千问办公的面板授权：OAuth2 授权码 + 本机回调（PKCE）。
//
// 与 wild-work internal/login_qwenwork 同一套流程（已实测）：
//
//	授权页  https://qwenwork.cn/oauth2/auth?client_id=qwenwork-desktop-app&...
//	        → 用户登录并同意 → 303 跳到 http://127.0.0.1:<随机端口>/callback?code=…
//	兑换    POST https://qwenwork.cn/oauth2/token（public client + PKCE，无需密钥）
//
// 本机回调 server 随 LoginSession 起停，Cancel 时释放端口。
//
// 为什么不走扫码：网页版的 QR 登录要阿里设备指纹（bx-ua / bx-umidtoken / bx_et），
// 那串由钉钉/支付宝 SDK 在浏览器端动态生成（实测空值与被拒都返回
// "invalid QR login request"），服务端无法复现。OAuth 授权码这条路不需要它。
package qwenwork

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 授权相关常量（与桌面端一致的 public client，不是密钥）。
const (
	OAuthAuthorizeURL = "https://qwenwork.cn/oauth2/auth"
	OAuthTokenURL     = "https://qwenwork.cn/oauth2/token"
	OAuthClientID     = "qwenwork-desktop-app"
	OAuthScope        = "openid profile email offline_access qwen_work"

	callbackTTL = 5 * time.Minute
)

// makePKCE 生成 verifier 与 S256 challenge。
func makePKCE() (verifier, challenge string) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	buf := make([]byte, 64)
	_, _ = rand.Read(buf)
	var sb strings.Builder
	sb.Grow(64)
	for _, b := range buf {
		sb.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	verifier = sb.String()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// randomState 生成授权 state（Hydra 要求 ≥8 字符）。
func randomState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// qwSession 是一次进行中的千问办公授权。
type qwSession struct {
	a           *Adapter
	verifier    string
	redirectURI string
	authURL     string

	ln     net.Listener
	srv    *http.Server
	mu     sync.Mutex
	code   string
	failed string
	closed bool
}

// StartLogin 实现 channel.Authorizer：起本机回调并返回授权页地址。
func (a *Adapter) StartLogin(ctx context.Context, opts channel.LoginOptions) (channel.LoginSession, error) {
	verifier, challenge := makePKCE()

	// 本机回调：随机空闲端口（与 wild-work 同做法）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errs.New(errs.Transport, "无法启动本机回调端口").WithCause(err).
			WithChannel(string(channel.QwenWork))
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	authURL := fmt.Sprintf(
		"%s?client_id=%s&code_challenge=%s&code_challenge_method=S256&redirect_uri=%s&response_type=code&scope=%s&state=%s",
		OAuthAuthorizeURL, OAuthClientID, challenge,
		url.QueryEscape(redirectURI), url.QueryEscape(OAuthScope), randomState())

	s := &qwSession{a: a, verifier: verifier, redirectURI: redirectURI, authURL: authURL, ln: ln}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			desc := q.Get("error_description")
			if desc == "" {
				desc = e
			}
			s.setFailure(desc)
			_, _ = io.WriteString(w, callbackPage(false, desc))
			return
		}
		code := q.Get("code")
		if code == "" {
			s.setFailure("回调缺少 code 参数")
			_, _ = io.WriteString(w, callbackPage(false, "回调缺少 code 参数"))
			return
		}
		s.setCode(code)
		// 此时还没兑换 token，页面文案用「已收到授权」而不是「登录成功」，
		// 避免兑换失败时误导用户（与 wild-work 一致）。
		_, _ = io.WriteString(w, callbackPage(true, ""))
	})
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()

	// 超时兜底：5 分钟没回调就自己关掉，不留悬挂端口。
	go func() {
		time.Sleep(callbackTTL)
		s.Cancel()
	}()

	return s, nil
}

func (s *qwSession) AuthURL() string { return s.authURL }

func (s *qwSession) setCode(c string) {
	s.mu.Lock()
	s.code = c
	s.mu.Unlock()
}

func (s *qwSession) setFailure(msg string) {
	s.mu.Lock()
	if s.failed == "" {
		s.failed = msg
	}
	s.mu.Unlock()
}

// Cancel 关闭本机回调 server（可重复调用）。
func (s *qwSession) Cancel() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	if s.srv != nil {
		_ = s.srv.Close()
	} else if s.ln != nil {
		_ = s.ln.Close()
	}
}

// Poll 实现 channel.LoginSession：回调到了就兑换 token，没到就继续等。
func (s *qwSession) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	code, failed := s.code, s.failed
	s.mu.Unlock()

	if failed != "" {
		return nil, errs.New(errs.AuthFailed, "授权失败："+failed).
			WithChannel(string(channel.QwenWork)).WithUpstream(failed)
	}
	if code == "" {
		return nil, channel.ErrPending
	}
	return s.a.exchangeToken(ctx, s)
}

// exchangeToken 用授权码兑换 token（Hydra 标准 token endpoint，public client + PKCE）。
func (a *Adapter) exchangeToken(ctx context.Context, s *qwSession) (*channel.Credential, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {s.code},
		"redirect_uri":  {s.redirectURI},
		"client_id":     {OAuthClientID},
		"code_verifier": {s.verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造 token 兑换请求失败").WithCause(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "token 兑换请求失败").WithCause(err).
			WithChannel(string(channel.QwenWork))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		// 兑换失败：把上游原话带出来（红线一），不写「请重试」。
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "token 兑换失败").
			WithChannel(string(channel.QwenWork)).WithUpstream(truncate(string(raw), 300))
	}

	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return nil, errs.New(errs.Parse, "token 响应无法解析或缺 access_token").
			WithChannel(string(channel.QwenWork)).WithUpstream(truncate(string(raw), 300))
	}

	uid, nickname := parseJWTIdentity(tok.AccessToken)
	if uid == "" {
		uid, nickname = parseJWTIdentity(tok.IDToken)
	}
	if uid == "" {
		// 拿不到 uid 就不能落盘（凭证文件名要靠它）—— 如实报错，不编造。
		return nil, errs.New(errs.Parse, "授权成功但无法从 token 解出 uid").
			WithChannel(string(channel.QwenWork)).WithUpstream(truncate(tok.IDToken, 200))
	}

	cred := channel.Credential{
		UID:          uid,
		Nickname:     nickname,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
	}
	if tok.ExpiresIn > 0 {
		cred.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}

	// 关键一步（抄 wild-work 的实测结论）：OAuth 兑换出来的 access token
	// **网页域不认**（JWT 校验直接 401 invalid-credential），必须用 refresh_token
	// 去桌面网关换 device_token，那才是能被 qwenwork.cn 接受的凭证。
	//
	// 不换的话就会得到用户实测那种状态：面板上「授权成功」，但之后每次
	// 取余额 / 列模型 / 对话都是 401，重新登录多少次都一样。
	if cred.RefreshToken != "" {
		dt, rotated, exp, derr := a.fetchDeviceToken(ctx, cred.RefreshToken)
		if derr != nil {
			// 换不到 device_token 就没有可用凭证：如实失败，别把死凭证塞进池子。
			s.Cancel()
			return nil, derr
		}
		cred.AccessToken = dt
		cred.RefreshToken = rotated
		cred.ExpiresAt = exp
		// device_token 里才有 username（OAuth 兑换的那个实测没有）。
		if cred.Nickname == "" {
			if _, nick := parseJWTIdentity(dt); nick != "" {
				cred.Nickname = nick
			}
		}
	}

	s.Cancel() // 拿到凭证即释放端口
	return &cred, nil
}

// parseJWTIdentity 从 JWT 的 payload 里解出 uid 与显示名。
// 只做 base64 解码读字段，不验签 —— token 是上游刚发给我们的。
// jwtClaims 解出 JWT 的 payload claims；解不开返回 false（不猜、编造）。
func jwtClaims(token string) (map[string]any, bool) {
	if token == "" {
		return nil, false
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 兼容带 padding 的编码
		raw2, err2 := base64.URLEncoding.DecodeString(parts[1])
		if err2 != nil {
			return nil, false
		}
		raw = raw2
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil, false
	}
	return claims, true
}

// jwtExpiry 从 JWT 的 payload 里取 exp。取不到返回零值。
//
// 为什么单独要这个（2026-09-27 真机事故）：上游给的 expires_in 是「多久该来换一次」的节奏提示，
// 不是令牌的真实寿命。千问办公实测 expires_in ≈ 10 分钟，而 device_token 的 JWT exp 是 7 天后；
// 把 10 分钟当到期时间 → 「临期才续期」永远成立 → 每个请求都去续一次，
// 而这个渠道的 refresh token 是一次性轮换的：越续越容易断链。
func jwtExpiry(token string) time.Time {
	claims, ok := jwtClaims(token)
	if !ok {
		return time.Time{}
	}
	switch v := claims["exp"].(type) {
	case float64:
		if v > 0 {
			return time.Unix(int64(v), 0)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}

func parseJWTIdentity(token string) (uid, nickname string) {
	claims, ok := jwtClaims(token)
	if !ok {
		return "", ""
	}
	get := func(k string) string {
		s, _ := claims[k].(string)
		return s
	}
	uid = get("user_id")
	if uid == "" {
		uid = get("sub")
	}
	nickname = get("username")
	if nickname == "" {
		nickname = get("email")
	}
	return uid, nickname
}

// callbackPage 是本机回调的落地页：告诉用户「可以关掉了」。
func callbackPage(ok bool, msg string) string {
	title := "授权已完成"
	body := "已收到授权，正在完成登录…这个页面可以关掉了。"
	if !ok {
		title = "授权未完成"
		body = msg
	}
	icon := "✓"
	color := "#0ca30c"
	if !ok {
		icon = "×"
		color = "#d03b3b"
	}
	return fmt.Sprintf(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;
font:14px/1.6 system-ui,-apple-system,"Segoe UI","PingFang SC",sans-serif;
background:#f9f9f7;color:#0b0b0b}
main{background:#fff;border:1px solid rgba(11,11,11,.1);border-radius:12px;padding:28px 32px;max-width:420px}
.ico{width:38px;height:38px;border-radius:50%%;display:grid;place-items:center;
background:%s;color:#fff;font-size:20px;font-weight:700;margin-bottom:14px}
h1{font-size:17px;margin:0 0 6px}p{margin:0;color:#52514e;font-size:13px}
</style></head><body><main><div class="ico">%s</div><h1>%s</h1><p>%s</p></main></body></html>`,
		title, color, icon, title, body)
}

// AcceptCallback 实现 channel.CallbackAcceptor。
//
// 千问办公的回调地址被上游锁死在**预注册清单**里（实测：把 redirect_uri 换成局域网
// 地址直接 302 invalid_request「does not match any of the OAuth 2.0 Client's
// pre-registered redirect urls」），所以「让手机扫码」这条路对它无效 —— 面板里
// 出的二维码扫了也回不来。用户从别的机器打开面板时，唯一不改回调地址的完成方式
// 就是：授权页跳回 127.0.0.1 打不开时，把地址栏里那串地址粘回面板。
//
// 接受三种输入：整串回调 URL、只有 query 的一段、或者光秃秃的 code。
func (s *qwSession) AcceptCallback(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errs.New(errs.Parse, "回调内容为空")
	}
	// 只贴了 code：既没有 query 标记，也不像一条地址（含 :// 或 /）。
	// 「像地址但没有 code」的情况必须走下面的报错分支 —— 否则会把一段没用的 URL
	// 当成 code 塞进去，兑换时才失败（更难查）。
	if !strings.ContainsAny(raw, "?=&") && !strings.Contains(raw, "://") && !strings.Contains(raw, "/") {
		s.setCode(raw)
		return nil
	}
	u := raw
	if !strings.Contains(u, "?") {
		u = "/callback?" + strings.TrimPrefix(raw, "?")
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return errs.New(errs.Parse, "这段内容不是可解析的回调地址").WithCause(err).
			WithUpstream(truncate(raw, 200))
	}
	q := parsed.Query()
	if e := q.Get("error"); e != "" {
		desc := q.Get("error_description")
		if desc == "" {
			desc = e
		}
		s.setFailure(desc)
		return nil
	}
	code := q.Get("code")
	if code == "" {
		return errs.New(errs.Parse, "这段内容里没有 code 参数（授权码）").
			WithUpstream(truncate(raw, 200))
	}
	s.setCode(code)
	return nil
}
