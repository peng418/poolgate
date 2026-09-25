// login.go TraeWork 的面板授权：授权码 + 本机一次性回调（PKCE）。
//
// 协议移植自 wild-work internal/login_trae（已实测）：
//  1. 起一个只监听 127.0.0.1 的临时 HTTP 服务作为 auth_callback_url；
//  2. 用户在浏览器完成授权，浏览器回调到本机，带回 refreshToken / authCode；
//  3. 用 refreshToken（或 PKCE 的 authCode + code_verifier）换 Cloud-IDE-JWT，
//     再调 GetUserInfo 拿 uid / 昵称。
//
// 与 wild-work 的差别：中间状态放内存（一次性交互，不留临时文件）。
// 回调页在两种情况下给不同内容 —— 拿到凭证是成功态，没拿到就明确写出缺什么（红线一）。
package traework

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const (
	ConsoleHost      = "https://www.trae.cn"
	PluginVersion    = "2.3.73734"
	EpAuthCodeExch   = "/trae/api/v3/oauth/ExchangeToken"
	EpUserInfo       = "/cloudide/api/v3/trae/GetUserInfo"
	callbackPath     = "/authorize"
	callbackLifetime = 5 * time.Minute
)

// callbackInfo 是从浏览器回调里解析出的凭证（任何一个都够走通一条兑换路径）。
type callbackInfo struct {
	RefreshToken string
	AccessToken  string
	AuthCode     string
	Host         string
}

// traeSession 是一次进行中的授权（含本机回调服务）。
type traeSession struct {
	a         *Adapter
	ln        net.Listener
	srv       *http.Server
	authURL   string
	machineID string
	deviceID  string
	verifier  string

	mu       sync.Mutex
	info     callbackInfo
	cbErr    string
	gotCB    bool
	cancel   bool
	closed   bool
	shutdown func()
}

// StartLogin 实现 channel.Authorizer。
//
// 回调地址：优先用面板给的对外地址（opts.CallbackBase）的**主机名**，端口仍取本机
// 临时端口。这样手机扫码、别的电脑的浏览器都能把授权码送回来 —— 原来固定
// 127.0.0.1 时，只有「浏览器与 PoolGate 同一台机器」才可能完成（用户实测踩到）。
// 监听相应改成 0.0.0.0（否则局域网连不进来）。没给 CallbackBase 时退回原来的
// 127.0.0.1 行为，保持向后兼容。
func (a *Adapter) StartLogin(ctx context.Context, opts channel.LoginOptions) (channel.LoginSession, error) {
	bindHost, cbHost := "127.0.0.1", "127.0.0.1"
	if h := callbackHost(opts.CallbackBase); h != "" && !isLoopbackHost(h) {
		// 面板所在机器的局域网地址：浏览器可能来自手机/别的电脑，监听必须放出去。
		// 这是一个**一次性**回调（只收本次授权的授权码，5 分钟后自动关闭），
		// 且收到的凭证必须能通过后面的 PKCE/接口校验才会入池。
		bindHost, cbHost = "0.0.0.0", h
	}
	ln, err := net.Listen("tcp", bindHost+":0")
	if err != nil {
		return nil, errs.New(errs.Transport, "无法启动本机回调监听（授权需要它接浏览器回调）").WithCause(err)
	}
	machineID := randHex(32) // 与官方客户端一致：64 位 hex
	deviceID := randNumericID()
	verifier, challenge := genPKCE()
	port := ln.Addr().(*net.TCPAddr).Port
	callback := "http://" + net.JoinHostPort(cbHost, strconv.Itoa(port)) + callbackPath

	s := &traeSession{a: a, ln: ln, machineID: machineID, deviceID: deviceID, verifier: verifier}
	s.authURL = buildAuthURL(callback, machineID, deviceID, challenge)

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, s.handleCallback)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()

	// 兜底：user 一直没点完，5 分钟后自己关掉，别留着一个监听端口。
	go func() {
		time.Sleep(callbackLifetime)
		s.Cancel()
	}()
	return s, nil
}

func (s *traeSession) AuthURL() string { return s.authURL }

// Cancel 关掉回调服务。可重复调用。
func (s *traeSession) Cancel() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cancel = true
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	_ = s.ln.Close()
}

// handleCallback 接浏览器回调。支持 GET（query）与 POST（JSON body）两种形态：
// 新版授权页登录成功后以 POST 到 auth_callback_url。
func (s *traeSession) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10)); len(body) > 0 {
			var m map[string]any
			if json.Unmarshal(body, &m) == nil {
				q := r.URL.Query()
				for k, v := range m {
					if val, ok := v.(string); ok && q.Get(k) == "" {
						q.Set(k, val)
					}
				}
				r.URL.RawQuery = q.Encode()
			}
		}
	}
	info := parseCallback(r.URL.String())
	if info.Host == "" {
		info.Host = s.a.oauth
	}

	s.mu.Lock()
	s.info = info
	s.gotCB = true
	if info.RefreshToken == "" && info.AccessToken == "" && info.AuthCode == "" {
		s.cbErr = "回调里没有携带任何凭证（既没有 refreshToken，也没有 authCode）"
	}
	cbErr := s.cbErr
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if cbErr == "" {
		_, _ = io.WriteString(w, callbackPage(true, "授权成功，可以关闭此页面回到 PoolGate 面板。"))
	} else {
		_, _ = io.WriteString(w, callbackPage(false, "授权页没有带回凭证："+cbErr))
	}
	go s.Cancel()
}

// Poll 实现 channel.LoginSession：回调到了就兑换凭证，没到就继续等。
func (s *traeSession) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	got, info, cbErr, cancelled := s.gotCB, s.info, s.cbErr, s.cancel
	s.mu.Unlock()

	if cancelled && !got {
		return nil, errs.New(errs.AuthFailed, "授权已取消")
	}
	if !got {
		return nil, channel.ErrPending
	}
	if cbErr != "" {
		return nil, errs.New(errs.AuthFailed, cbErr).WithChannel(string(channel.TraeWork))
	}

	host := info.Host
	if host == "" {
		host = s.a.oauth
	}
	cred := channel.Credential{
		RefreshToken: info.RefreshToken,
		AccessToken:  info.AccessToken,
		Extra:        map[string]string{"machine_id": s.machineID, "device_id": s.deviceID},
	}

	switch {
	case info.AuthCode != "":
		// PKCE 新流程：authCode + verifier + 设备公钥换 token。
		res, err := s.a.exchangeAuthCode(ctx, host, info.AuthCode, s.verifier, s.machineID, s.deviceID)
		if err != nil {
			return nil, err
		}
		cred.AccessToken, cred.RefreshToken, cred.ExpiresAt = res.token, res.refresh, res.expiresAt
		if res.host != "" {
			host = res.host
		}
	case info.RefreshToken != "":
		// 标准路径：用 refreshToken 换 access token。
		tmp := &channel.Credential{UID: "", RefreshToken: info.RefreshToken, Extra: map[string]string{"api_host": host}}
		refreshed, err := s.a.Refresh(ctx, tmp)
		if err != nil {
			return nil, err
		}
		cred.AccessToken, cred.RefreshToken, cred.ExpiresAt = refreshed.AccessToken, refreshed.RefreshToken, refreshed.ExpiresAt
	default:
		// 兜底路径：回调只给了 userJwt.Token（后面刷新会失败，但本轮能用）。
		if cred.AccessToken == "" {
			return nil, errs.New(errs.AuthFailed, "回调未提供可用凭证").WithChannel(string(channel.TraeWork))
		}
	}
	cred.Extra["api_host"] = host

	uid, nickname, err := s.a.getUserInfo(ctx, host, &cred)
	if err != nil {
		return nil, err
	}
	if uid == "" {
		return nil, errs.New(errs.Parse, "上游未返回 UserID，无法确定账号归属").
			WithChannel(string(channel.TraeWork))
	}
	cred.UID = uid
	cred.Nickname = nickname
	return &cred, nil
}

// ---------------------------------------------------------------------------
// 兑换与账号信息
// ---------------------------------------------------------------------------

type authCodeResult struct {
	token     string
	refresh   string
	expiresAt time.Time
	host      string
}

// exchangeAuthCode 用 AuthCode + CodeVerifier + 设备公钥换 Cloud-IDE-JWT。
// 依次尝试 api.trae.cn / 回调 host / api.trae.com.cn（与 wild-work 实测一致）。
func (a *Adapter) exchangeAuthCode(ctx context.Context, callbackHost, authCode, verifier, machineID, deviceID string) (*authCodeResult, error) {
	if strings.TrimSpace(authCode) == "" || strings.TrimSpace(verifier) == "" {
		return nil, errs.New(errs.AuthFailed, "AuthCode 或 codeVerifier 缺失，无法兑换").WithChannel(string(channel.TraeWork))
	}
	pubPEM, err := devicePublicKeyPEM()
	if err != nil {
		return nil, errs.New(errs.Transport, "生成设备公钥失败").WithCause(err)
	}
	body, _ := json.Marshal(map[string]any{
		"ClientID":     ClientID,
		"AuthCode":     authCode,
		"CodeVerifier": verifier,
		"DeviceInfo": map[string]any{
			"DeviceID":        deviceID,
			"MachineID":       machineID,
			"PlatformCode":    "SOLO_PC",
			"DeviceType":      "PC",
			"DeviceName":      deviceName(),
			"DeviceModel":     DeviceBrand,
			"ClientVersion":   IdeVersion,
			"DevicePublicKey": pubPEM,
			"DeviceBrand":     DeviceBrand,
			"OSInfo":          "windows",
			"OSVersion":       OSVersion,
		},
		"IDEVersion": IdeVersion,
	})

	var lastErr string
	for _, origin := range authCodeOrigins(callbackHost) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+EpAuthCodeExch, strings.NewReader(string(body)))
		if err != nil {
			lastErr = err.Error()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Trae/"+IdeVersion)
		resp, err := a.http.Do(req)
		if err != nil {
			lastErr = origin + " => " + err.Error()
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			lastErr = fmt.Sprintf("%s => HTTP %d %s", origin, resp.StatusCode, truncate(string(raw), 160))
			continue
		}
		res, perr := parseAuthCodeResult(raw)
		if perr != nil {
			lastErr = origin + " => " + perr.Error()
			continue
		}
		res.host = origin
		return res, nil
	}
	return nil, errs.New(errs.AuthFailed, "AuthCode 兑换失败").
		WithChannel(string(channel.TraeWork)).WithUpstream(truncate(lastErr, 200))
}

// authCodeOrigins 候选兑换 origin：授权回调 host 优先，再回退 api.trae.cn / api.trae.com.cn。
func authCodeOrigins(callbackHost string) []string {
	out := []string{}
	for _, h := range []string{strings.TrimSpace(callbackHost), UgHost, OAuthHost} {
		h = strings.TrimRight(h, "/")
		if h == "" {
			continue
		}
		dup := false
		for _, v := range out {
			if v == h {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, h)
		}
	}
	return out
}

// parseAuthCodeResult 解析兑换响应：token 在 Result.Token/AccessToken 等位置（上游形态不止一种）。
func parseAuthCodeResult(data []byte) (*authCodeResult, error) {
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("兑换响应无法解析: %w", err)
	}
	for _, container := range []any{v["Result"], v["result"], v["data"], v} {
		m, ok := container.(map[string]any)
		if !ok {
			continue
		}
		token := firstNonEmptyStr(m, "AccessToken", "accessToken", "access_token", "Token", "token")
		if token == "" {
			continue
		}
		res := &authCodeResult{
			token:   token,
			refresh: firstNonEmptyStr(m, "RefreshToken", "refreshToken", "refresh_token"),
		}
		if ts := firstInt64(m, "TokenExpireAt", "tokenExpireAt", "ExpiresAt", "expiresAt", "expiredAt"); ts > 0 {
			res.expiresAt = time.Unix(normalizeExpires(ts), 0)
		}
		return res, nil
	}
	return nil, fmt.Errorf("兑换响应缺少 token: %s", truncate(string(data), 160))
}

// getUserInfo 取 uid / 昵称。
func (a *Adapter) getUserInfo(ctx context.Context, host string, c *channel.Credential) (uid, nickname string, err error) {
	if host == "" {
		host = a.oauth
	}
	body := `{"ReqSource":"IDE","IDEVersion":"` + IdeVersion + `"}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+EpUserInfo, strings.NewReader(body))
	if err != nil {
		return "", "", errs.New(errs.Transport, "构造账号信息请求失败").WithCause(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Trae/"+IdeVersion)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+c.AccessToken)
	req.Header.Set("X-Cloudide-Token", c.AccessToken)
	req.Header.Set("X-Ide-Token", c.AccessToken)
	if c.UID != "" {
		req.Header.Set("X-Uid", c.UID)
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return "", "", errs.New(errs.Transport, "查询账号信息失败").WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", errs.New(a.Classify(resp.StatusCode, raw), "查询账号信息被拒").
			WithUpstream(truncate(string(raw), 200))
	}
	var out struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", errs.New(errs.Parse, "账号信息无法解析").WithUpstream(truncate(string(raw), 200)).WithCause(err)
	}
	return out.Result.UserID, out.Result.ScreenName, nil
}

// ---------------------------------------------------------------------------
// 回调解析与工具
// ---------------------------------------------------------------------------

// parseCallback 从回调 URL 提取凭证。
// 优先级：refreshToken → userJwt.RefreshToken → userJwt.Token → authCodeInfo.AuthCode。
func parseCallback(rawURL string) callbackInfo {
	var info callbackInfo
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return info
	}
	q := u.Query()
	info.Host = q.Get("host")
	if info.RefreshToken = q.Get("refreshToken"); info.RefreshToken != "" {
		return info
	}
	userJwt := parseJSONParam(q.Get("userJwt"))
	if info.RefreshToken = getStr(userJwt, "RefreshToken"); info.RefreshToken != "" {
		return info
	}
	if info.AccessToken = getStr(userJwt, "Token"); info.AccessToken != "" {
		return info
	}
	if raw := q.Get("authCodeInfo"); raw != "" {
		info.AuthCode = authCodeFromInfo(raw)
	}
	return info
}

// authCodeFromInfo 从 authCodeInfo（原始 code 或 JSON 对象）提取 AuthCode。
func authCodeFromInfo(raw string) string {
	raw = strings.TrimSpace(raw)
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) == nil {
		for _, container := range []any{m, m["Result"], m["result"]} {
			if mm, ok := container.(map[string]any); ok {
				if s := getStr(mm, "AuthCode"); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return raw // 非 JSON：本身就是 code
}

// parseJSONParam 解回调里 URL 编码的 JSON 参数（容错再解一层 percent-encoding）。
func parseJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func getStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func buildAuthURL(callback, machineID, deviceID, challenge string) string {
	u, _ := url.Parse(ConsoleHost + "/authorization")
	v := u.Query()
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", PluginVersion)
	v.Set("auth_type", "local")
	v.Set("client_id", ClientID)
	v.Set("redirect", "0")
	v.Set("login_trace_id", newUUID())
	v.Set("auth_callback_url", callback)
	v.Set("machine_id", machineID)
	v.Set("device_id", deviceID)
	v.Set("x_device_id", deviceID)
	v.Set("x_machine_id", machineID)
	v.Set("x_device_brand", DeviceBrand)
	v.Set("x_device_type", "windows")
	v.Set("x_os_version", OSVersion)
	v.Set("x_app_version", IdeVersion)
	v.Set("x_app_type", "stable")
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	v.Set("hide_saas_login", "true")
	v.Set("channel_name", "common")
	v.Set("click_id", "TRAE SOLOSetup-stable-"+PluginVersion)
	u.RawQuery = v.Encode()
	return u.String()
}

// genPKCE 生成 PKCE verifier + S256 challenge（verifier 必须与授权 URL 配对保存）。
func genPKCE() (verifier, challenge string) {
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// devicePublicKeyPEM 生成一次性 ECDSA P256 公钥 PEM（AuthCode 交换必需）。
func devicePublicKeyPEM() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

func deviceName() string {
	for _, k := range []string{"USERNAME", "USER"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "PC"
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// randNumericID 生成 15 位数字设备 ID（对齐官方客户端 device_id 格式）。
func randNumericID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	n := uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
	return fmt.Sprintf("%015d", n%900000000000000+100000000000000)
}

// newUUID 生成 v4 UUID（授权 URL 的 login_trace_id 用）。
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// callbackPage 是浏览器里看到的那一页。成功与失败必须看起来不一样，
// 且失败要写清原因 —— 让用户知道是「没点完」还是「上游拒绝」。
func callbackPage(ok bool, msg string) string {
	color := "#0ca30c"
	title := "授权成功"
	if !ok {
		color = "#d03b3b"
		title = "授权未完成"
	}
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>` + title + ` · PoolGate</title></head>
<body style="font:15px/1.6 -apple-system,'PingFang SC','Microsoft YaHei',sans-serif;max-width:520px;margin:80px auto;padding:0 20px">
<h1 style="color:` + color + `;font-size:20px">` + title + `</h1>
<p>` + html.EscapeString(msg) + `</p>
<p style="color:#898781;font-size:13px">本页面由 PoolGate 本机临时回调服务生成，可以安全关闭。</p>
</body></html>`
}

// firstNonEmptyStr 返回第一个非空的字符串字段（上游响应字段名不止一种写法）。
func firstNonEmptyStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// firstInt64 返回第一个能转成整数的字段（时间戳字段同样有多种写法）。
func firstInt64(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n
			}
		}
	}
	return 0
}

// callbackHost 从面板给的对外基址里取出主机名（http://192.0.2.10:5014 → 192.0.2.10）。
func callbackHost(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// isLoopbackHost 报告主机名是不是本机回环地址。
func isLoopbackHost(h string) bool {
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return strings.HasPrefix(strings.ToLower(h), "127.")
}

// AcceptCallback 实现 channel.CallbackAcceptor：把用户浏览器地址栏里的回调地址
// （或整段 query）手工喂进来。用于局域网回调被防火墙挡住、或用户从别的网络完成
// 授权的情形 —— 与「本机回调」是同一套解析，不额外放宽任何校验。
func (s *traeSession) AcceptCallback(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errs.New(errs.Parse, "回调地址为空")
	}
	u := raw
	if !strings.Contains(u, "?") {
		// 用户可能只贴了 code / authCode 之类的一小段
		u = callbackPath + "?" + strings.TrimPrefix(raw, "?")
	}
	info := parseCallback(u)
	if info.AuthCode == "" && info.AccessToken == "" && info.RefreshToken == "" {
		return errs.New(errs.Parse, "这段内容里没有可用的授权信息（TraeWork 的回调里应含 authCodeInfo / refreshToken / userJwt / accessToken）").
			WithUpstream(truncate(raw, 200))
	}
	if info.Host == "" {
		info.Host = s.a.oauth
	}
	s.mu.Lock()
	s.info = info
	s.gotCB = true
	s.mu.Unlock()
	return nil
}
