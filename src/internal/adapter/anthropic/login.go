package anthropic

// login.go 订阅 OAuth 授权码流程（PKCE + 手工粘回 code）。
//
// 与 gemini/oauth.go 同一个骨架（PKCE S256 + 粘回 code），但有一处**刻意的**不同：
// 不使用本机回调 server。理由很实际 —— 网关跑在 NAS 上，用户平常在另一台机器上浏览，
// 本机 127.0.0.1 的回调浏览器根本跳不到；而 Claude 的授权页在授权完成后会落到
// platform.claude.com 的一个**展示授权码的页面**（形如 `code#state`），
// 把它复制粘贴回来，是「不额外开端口、不要公网回调」就能走完的路。
// 这正是 teamclaude 的 manual 路径，也是我们把 redirect_uri 固定成 oauthRedirect 的原因。
//
// 认三种粘贴形态（用户怎么粘是没法规定的，全认下来最省事）：
//
//	https://platform.claude.com/oauth/code/callback?code=…&state=…   整条回调地址
//	xxxxx#yyyyy                                                       授权页显示的 code#state
//	xxxxx                                                             裸授权码（state 用本次会话的）
//
// 安全边界：授权码就算从别处拿到也没用 —— 兑换时必须带上本进程生成的 code_verifier
// （PKCE），所以裸 code 也敢收；state 能对上就核一遍，对不上直接拒（CSRF）。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// hintText 是给面板的粘贴引导语（控制台通过可选接口 Hint() 取它，见 channel 的 CallbackAcceptor）。
const hintText = "点开上面的授权链接，用你的 Claude 订阅账号（Pro/Max）登录并点「Authorize」。" +
	"授权完成后浏览器会跳到一个显示授权码的页面，把整串码（形如 `xxxx#yyyy`）复制粘贴到下面；" +
	"粘贴整条回调地址也可以。"

// 编译期契约断言：Channel 是主契约，Authorizer/CallbackAcceptor 是面板授权链路要求的
// 两个可选接口（少了任何一个，面板上就不会出现「添加账号」或「粘贴授权码」入口）。
var (
	_ channel.Channel          = (*Adapter)(nil)
	_ channel.Authorizer       = (*Adapter)(nil)
	_ channel.CallbackAcceptor = (*session)(nil)
)

type session struct {
	a *Adapter
	// verifier/state/authURL 在 StartLogin 时一次性生成，之后不变。
	verifier string
	state    string
	authURL  string

	mu   sync.Mutex
	code string // 用户粘回来的授权码
	// gotState 是粘贴内容里带的 state（裸 code 时为空）。兑换时优先用它 ——
	// 授权码与 state 是配对签发的，用会话里的 state 去配一个别处来的 code 会被上游拒。
	gotState string
	done     bool
}

// StartLogin 生成授权地址（带 PKCE + state），并把会话挂在内存里等粘贴。
func (a *Adapter) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	verifier, challenge := makePKCE()
	state := randHex(32)

	q := url.Values{}
	q.Set("code", "true") // 参考实现都带它：让授权页走「显示授权码」这条分支
	q.Set("client_id", oauthClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", oauthRedirect)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	// scope 单独拼：Anthropic 的授权页要求冒号**不编码**（auth2api 实测踩过这个坑），
	// 而 url.Values.Encode() 会把 ":" 变成 "%3A"。其它参数照常用 Encode。
	authURL := a.authorizeURL + "?" + q.Encode() + "&scope=" + scopeParam()

	return &session{a: a, verifier: verifier, state: state, authURL: authURL}, nil
}

func (s *session) AuthURL() string { return s.authURL }

// Hint 实现控制台的可选接口：面板据此切换成「粘贴」形态（不画二维码、不提示扫码）。
func (s *session) Hint() string { return hintText }

// AcceptCallback 收用户粘回来的授权码（整条回调地址 / code#state / 裸 code 都认）。
func (s *session) AcceptCallback(raw string) error {
	code, gotState := extractAuthCode(raw, s.state)
	if code == "" {
		return errs.New(errs.Parse,
			"没识别出授权码：请把授权完成页显示的那串码（形如 `xxxx#yyyy`）整段粘过来").
			WithChannel(string(channel.Anthropic))
	}
	if gotState != "" && gotState != s.state {
		// state 对不上：要么粘错了别的会话的码，要么是被构造过的输入 —— 都直接拒。
		return errs.New(errs.AuthFailed, "授权码的 state 与本次登录不匹配，请重新点「授权」再粘一次").
			WithChannel(string(channel.Anthropic))
	}
	s.mu.Lock()
	s.code, s.gotState = code, gotState
	s.mu.Unlock()
	return nil
}

// Poll 在用户粘了码之后完成兑换 + 身份识别 + **当场验一次**。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	code, gotState, done := s.code, s.gotState, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次授权已经完成过了").WithChannel(string(channel.Anthropic))
	}
	if code == "" {
		return nil, channel.ErrPending // 还没粘码：正常的等待态，不是错误
	}
	state := gotState
	if state == "" {
		state = s.state
	}

	tok, err := s.a.exchangeCode(ctx, code, state, s.verifier)
	if err != nil {
		return nil, err
	}

	cred := &channel.Credential{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.expiry(),
	}
	// 身份：优先用令牌端点随响应给的 account（auth2api 观测到的字段）；
	// 拿不到再问一次账号档案接口（teamclaude 的做法）。两条都拿不到就退回
	// 「由 refresh token 派生的短标识」—— 至少不冲突、也不泄露令牌本身。
	email, name, uuid := tok.Account.Email, tok.Account.DisplayName, tok.Account.UUID
	if email == "" || uuid == "" {
		if pe, pn, pu := s.a.fetchProfile(ctx, cred); pe != "" || pu != "" {
			email, name, uuid = firstNonEmpty(pe, email), firstNonEmpty(pn, name), firstNonEmpty(pu, uuid)
		}
	}
	cred.UID = firstNonEmpty(uuid, email, "anthropic-"+shortHash(firstNonEmpty(cred.RefreshToken, cred.AccessToken)))
	cred.Nickname = firstNonEmpty(name, email, "Claude 订阅账号")

	// 兑换成功 ≠ 能推理（可能有账号但没订阅、或授权范围不含 inference）。
	// 这里拿 /v1/models 问一句，失败就**不把凭证塞进池子** —— 否则用户看到的是
	// 「装上了但一调用就报错」，分不清是自己弄错了还是渠道坏了。
	if err := s.a.checkCredential(ctx, cred); err != nil {
		return nil, err
	}

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

// scopeParam 把 scope 拼成授权页要的形态：空格用 "+"、冒号**保持原样**。
func scopeParam() string {
	return strings.ReplaceAll(url.QueryEscape(oauthScopes), "%3A", ":")
}

// tokenResp 是 OAuth 令牌端点的响应（授权码兑换与 refresh 共用）。
type tokenResp struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	// Account 是令牌端点在兑换响应里附带的账号信息（auth2api 观测到 email_address/uuid）。
	// 有就用，省一次档案请求；没有就回落 fetchProfile。
	Account struct {
		UUID        string `json:"uuid"`
		Email       string `json:"email_address"`
		DisplayName string `json:"display_name"`
	} `json:"account"`
}

// expiry 换算过期时间。上游没给 expires_in 时保守按 1 小时 —— 宁可早刷，不要晚刷
// （晚刷的代价是每个请求先撞一次 401）。
func (t tokenResp) expiry() time.Time {
	if t.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	return time.Now().Add(time.Hour)
}

// exchangeCode 用授权码换令牌。
func (a *Adapter) exchangeCode(ctx context.Context, code, state, verifier string) (tokenResp, error) {
	payload := map[string]any{
		"code":          code,
		"state":         state,
		"grant_type":    "authorization_code",
		"client_id":     oauthClientID,
		"redirect_uri":  oauthRedirect,
		"code_verifier": verifier,
	}
	var out tokenResp
	status, raw, err := a.postJSON(ctx, nil, a.tokenURL, payload, &out)
	if err != nil {
		return out, errs.New(errs.Transport, "换取令牌失败（连不上上游）").
			WithChannel(string(channel.Anthropic)).WithCause(err)
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
			WithChannel(string(channel.Anthropic)).WithUpstream(truncate(string(raw), 200))
	}
	return out, nil
}

// refreshToken 用 refresh token 换新的 access token。
//
// 上游**可能轮换** refresh token（响应里给了新的就用新的），所以返回整个 tokenResp，
// 由调用方决定怎么落盘 —— 只拿 access token 会让下一次刷新用旧的、可能已被作废。
func (a *Adapter) refreshToken(ctx context.Context, c *channel.Credential) (tokenResp, error) {
	payload := map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": strings.TrimSpace(c.RefreshToken),
		"client_id":     oauthClientID,
	}
	var out tokenResp
	status, raw, err := a.postJSON(ctx, c, a.tokenURL, payload, &out)
	if err != nil {
		return out, errs.New(errs.Transport, "刷新令牌失败（连不上上游）").
			WithChannel(string(channel.Anthropic)).WithAccount(uidOf(c)).WithCause(err)
	}
	if out.AccessToken == "" {
		kind := errs.SessionDead
		if status == 429 {
			kind = errs.SoftRate // 限流不是「凭证坏了」，别把好号判死
		}
		msg := out.ErrorDescription
		if msg == "" {
			msg = out.Error
		}
		return out, errs.New(kind, "刷新失败，需要重新授权："+msg).
			WithChannel(string(channel.Anthropic)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	return out, nil
}

// fetchProfile 取账号档案（email/uuid/显示名）。失败不影响授权，只是少了个好标题 ——
// 所以错误全部吞掉，返回空串由调用方回落。
//
// 注意这个端点只要 Authorization: Bearer，**不带** anthropic-beta（teamclaude 的用法）。
func (a *Adapter) fetchProfile(ctx context.Context, c *channel.Credential) (email, name, uuid string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.apiBase+epProfile, nil)
	if err != nil {
		return "", "", ""
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.AccessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", cliUA)
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", ""
	}
	var out struct {
		Account struct {
			UUID        string `json:"uuid"`
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
		} `json:"account"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return "", "", ""
	}
	name = strings.TrimSpace(out.Account.DisplayName)
	if name == "" {
		name = strings.TrimSpace(out.Account.Email)
	}
	return strings.TrimSpace(out.Account.Email), name, strings.TrimSpace(out.Account.UUID)
}

// extractAuthCode 从粘贴内容里抽 (code, state)。
//
// defaultState 是本次会话生成的 state：裸 code 不知道自己配哪个 state，就用它
// （PKCE 才是真正的绑定，见文件头）。
func extractAuthCode(raw, defaultState string) (code, state string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ""
	}

	// 形态 1：整条回调地址（可能在中间被空格/换行截过，所以不假设它一定能 Parse 成 URL）。
	if i := strings.Index(s, "code="); i >= 0 {
		rest := s[i+len("code="):]
		code = rest
		if j := strings.IndexAny(rest, "&# \t\n\r"); j >= 0 {
			code = rest[:j]
		}
		if k := strings.Index(rest, "state="); k >= 0 {
			st := rest[k+len("state="):]
			if j := strings.IndexAny(st, "&# \t\n\r"); j >= 0 {
				st = st[:j]
			}
			state = st
		}
		if c := unescape(code); c != "" {
			return c, unescape(state)
		}
	}

	// 形态 2：授权页显示的 `code#state`。
	if i := strings.Index(s, "#"); i >= 0 {
		c := strings.TrimSpace(s[:i])
		st := strings.TrimSpace(s[i+1:])
		if c != "" {
			return c, st
		}
	}

	// 形态 3：裸授权码。挡掉明显误粘的内容（含空白的多词文本）。
	if strings.ContainsAny(s, " \t\n") {
		return "", ""
	}
	return s, defaultState
}

func unescape(s string) string {
	if s == "" {
		return ""
	}
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
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

// randHex 生成 n 字节的 hex 随机串（state 用）。
func randHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// shortHash 给账号一个稳定短标识（面板显示用，不泄露凭证本身）。
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
