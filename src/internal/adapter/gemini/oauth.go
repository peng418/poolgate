package gemini

// oauth.go 三段auth：① user-code 授权换令牌；② 刷新；③ **开通 Code Assist**。
//
// 第三段最容易漏：只换令牌不开通，调对话会 412/403（Google 要求先 loadCodeAssist /
// onboardUser 把「项目 + 档位」定下来）。这一段的结果缓存进凭证，后续直接用。

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

// StartLogin 生成授权地址（带 PKCE + state）。用户在浏览器里点完后，页面上会显示一串 code。
func (a *Adapter) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	verifier, challenge := makePKCE()
	state := randHex(32)
	u := url.Values{}
	u.Set("client_id", clientID)
	u.Set("redirect_uri", redirectURI)
	u.Set("response_type", "code")
	u.Set("scope", scope)
	u.Set("access_type", "offline") // 要 refresh_token
	u.Set("prompt", "consent")
	u.Set("state", state)
	u.Set("code_challenge", challenge)
	u.Set("code_challenge_method", "S256")
	return &session{
		a: a, verifier: verifier, state: state,
		authURL: epAuth + "?" + u.Encode(), createdAt: time.Now(),
	}, nil
}

func (s *session) AuthURL() string { return s.authURL }

// AcceptCallback 收用户粘回来的授权码（整串回调地址也行，自动抽 code）。
//
// 为什么需要它：NAS 上没有桌面浏览器，Google 的 user-code 流就是「授权完给一串码，
// 你自己拿回程序里」。控制台已有一条通用的「粘贴」通道（CallbackAcceptor），
// 这里实现它即可，不需要额外端口、也不需要公网回调。
func (s *session) AcceptCallback(raw string) error {
	code := extractCode(raw)
	if code == "" {
		return errs.New(errs.Parse, "没识别出授权码：请把授权页显示的那串代码整段粘过来").
			WithChannel(string(channel.Gemini))
	}
	s.mu.Lock()
	s.code = code
	s.mu.Unlock()
	return nil
}

// Poll 在用户粘了码之后完成兑换 + 开通。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	code, done := s.code, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次授权已经完成过了").WithChannel(string(channel.Gemini))
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
	// 身份：拿 userinfo 的 email 作账号标识（拿不到就退回 user-<随机>，至少不冲突）
	if email, name := s.a.fetchIdentity(ctx, cred); email != "" {
		cred.UID, cred.Nickname = email, name
	}
	if cred.UID == "" {
		cred.UID = "google-" + randHex(6)
		cred.Nickname = cred.UID
	}
	// 开通（拿到 project）。失败就是失败 —— 不把「没开通的凭证」塞进池子，
	// 否则用户装完第一次调用收到 412 却不知道为什么（红线一）。
	project, err := s.a.ensureProject(ctx, cred)
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

// Refresh 用 refresh_token 换新的 access token（Google 可能轮换 refresh_token，新的要落盘）。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if c == nil || strings.TrimSpace(c.RefreshToken) == "" {
		return nil, nil // 刷不了：交给上层按 401 处理，不假装成功
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("refresh_token", c.RefreshToken)
	var out tokenResp
	status, raw, err := a.postForm(ctx, c, a.tokenURL, form, &out)
	if err != nil {
		return nil, errs.New(errs.Transport, "刷新令牌失败").WithChannel(string(channel.Gemini)).
			WithAccount(c.UID).WithCause(err)
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
		return nil, errs.New(kind, "刷新失败，需要重新授权："+msg).
			WithChannel(string(channel.Gemini)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	nc := *c
	nc.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		nc.RefreshToken = out.RefreshToken
	}
	nc.ExpiresAt = out.expiry()
	return &nc, nil
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
	_, raw, err := a.postForm(ctx, nil, a.tokenURL, form, &out)
	if err != nil {
		return out, errs.New(errs.Transport, "换取令牌失败").WithChannel(string(channel.Gemini)).WithCause(err)
	}
	if out.AccessToken == "" {
		msg := out.ErrorDescription
		if msg == "" {
			msg = out.Error
		}
		return out, errs.New(errs.AuthFailed, "授权码兑换失败："+msg).
			WithChannel(string(channel.Gemini)).WithUpstream(truncate(string(raw), 200))
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

// onboardPollMax / onboardPollEvery 是开通长任务的轮询上限与间隔。
//
// 取 10 次 × 5 秒（上限约 50 秒）：与 gpt4free GeminiCLI.py:566-567 的
// attempts=10 / delay_ms=5000 一致，足够绝大多数账号开完，又不至于把面板的
// 「等待授权」卡死太久（geminicli2api auth.py:497-510 是不设上限的 while True，
// 照抄会把授权流程永久挂住，故不采用）。
const (
	onboardPollMax   = 10
	onboardPollEvery = 5 * time.Second
)

// ensureProject 跑开通流程，返回可用的 project id。
//
// 逻辑（照官方 CLI）：先 loadCodeAssist；已有档位+项目就直接用；
// 否则按 allowedTiers 里的默认档调 onboardUser（**free-tier 不能带 project**，
// 带了会 Precondition Failed），返回的是长任务，没做完就轮询。
func (a *Adapter) ensureProject(ctx context.Context, c *channel.Credential) (string, error) {
	if p := strings.TrimSpace(c.Extra["project"]); p != "" {
		return p, nil
	}
	// metadata：三份参考实现都带 duetProject（geminicli2api utils.py:32-38、
	// gpt4free GeminiCLI.py:582-587、AIClient2API gemini-core.js:471-475），
	// 开通阶段还没有项目，按 AIClient2API 同款用空串。
	// platform 保持 PLATFORM_UNSPECIFIED：geminicli2api 会按主机算成 LINUX_AMD64，
	// 而 AIClient2API 直接用 PLATFORM_UNSPECIFIED —— 两份冲突，按后者（现状）不动。
	meta := map[string]any{
		"ideType":     "IDE_UNSPECIFIED",
		"platform":    "PLATFORM_UNSPECIFIED",
		"pluginType":  pluginType,
		"duetProject": "",
	}
	body, _ := json.Marshal(map[string]any{"metadata": meta})
	var loaded struct {
		CurrentTier             map[string]any   `json:"currentTier"`
		AllowedTiers            []map[string]any `json:"allowedTiers"`
		CloudAICompanionProject string           `json:"cloudaicompanionProject"`
		IneligibleTiers         []map[string]any `json:"ineligibleTiers"`
	}
	if err := a.postJSONInto(ctx, c, a.codeAssistBase+"/v1internal:loadCodeAssist", body, &loaded); err != nil {
		return "", err
	}
	if len(loaded.CurrentTier) > 0 && strings.TrimSpace(loaded.CloudAICompanionProject) != "" {
		return loaded.CloudAICompanionProject, nil
	}
	// 不满足条件时上游会给出枚举原因（地区/年龄/需验证）——原样透给用户，别吞。
	if len(loaded.IneligibleTiers) > 0 {
		t := loaded.IneligibleTiers[0]
		reason, _ := t["reasonCode"].(string)
		msg, _ := t["reasonMessage"].(string)
		if reason != "" {
			return "", errs.New(errs.AuthFailed,
				fmt.Sprintf("这个 Google 账号暂时不可用（%s：%s）", reason, msg)).
				WithChannel(string(channel.Gemini)).WithAccount(c.UID)
		}
	}
	tier := freeTier
	for _, t := range loaded.AllowedTiers {
		if def, _ := t["isDefault"].(bool); def {
			if id, _ := t["id"].(string); id != "" {
				tier = id
			}
			break
		}
	}
	payload := map[string]any{"tierId": tier, "metadata": meta}
	if tier != freeTier && strings.TrimSpace(loaded.CloudAICompanionProject) != "" {
		payload["cloudaicompanionProject"] = loaded.CloudAICompanionProject
	}
	onbBody, _ := json.Marshal(payload)
	var lro onboardResp
	if err := a.postJSONInto(ctx, c, a.codeAssistBase+"/v1internal:onboardUser", onbBody, &lro); err != nil {
		return "", err
	}
	// 长任务：没 done 就**重发同一个 onboardUser**，不是去 GET 操作名。
	// 三份参考实现都是重发：geminicli2api auth.py:497-510（while True 再 POST）、
	// gpt4free GeminiCLI.py:594-635（attempts=10、每次 sleep 5s）、
	// AIClient2API gemini-core.js:523-533（循环重发，最长 60s）。
	// 过去走的是自创路径 GET /v1internal/{name} —— 那个端点没有任何参考实现用过。
	for i := 0; !lro.Done && i < onboardPollMax; i++ {
		select {
		case <-ctx.Done():
			return "", errs.New(errs.Transport, "等待开通超时").WithChannel(string(channel.Gemini)).WithCause(ctx.Err())
		case <-time.After(onboardPollEvery):
		}
		var next onboardResp
		if err := a.postJSONInto(ctx, c, a.codeAssistBase+"/v1internal:onboardUser", onbBody, &next); err != nil {
			return "", err
		}
		lro = next
	}
	if id := lro.Response.CloudAICompanionProject.ID; id != "" {
		return id, nil
	}
	return "", errs.New(errs.AuthFailed, "开通没拿到项目 id（可能这个账号需要先在浏览器里完成验证）").
		WithChannel(string(channel.Gemini)).WithAccount(c.UID)
}

// onboardResp 是 onboardUser 的长任务响应。
//
// done 为假时表示还没开完，要重发同一请求（见 ensureProject 的说明）。
type onboardResp struct {
	Done     bool `json:"done"`
	Response struct {
		CloudAICompanionProject struct {
			ID string `json:"id"`
		} `json:"cloudaicompanionProject"`
	} `json:"response"`
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

func (a *Adapter) postJSONInto(ctx context.Context, c *channel.Credential, urlStr string, body []byte, out any) error {
	return a.doJSONInto(ctx, c, http.MethodPost, urlStr, body, out)
}

func (a *Adapter) doJSONInto(ctx context.Context, c *channel.Credential, method, urlStr string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, rd)
	if err != nil {
		return errs.New(errs.Transport, "构造请求失败").WithChannel(string(channel.Gemini)).WithCause(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", cliUA)
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return errs.New(errs.Transport, "上游请求失败").WithChannel(string(channel.Gemini)).
			WithAccount(c.UID).WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return errs.New(a.Classify(resp.StatusCode, raw), "上游返回错误").
			WithChannel(string(channel.Gemini)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return errs.New(errs.Parse, "上游响应无法解析").WithChannel(string(channel.Gemini)).
				WithUpstream(truncate(string(raw), 200))
		}
	}
	return nil
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

// extractCode 从用户粘回来的内容里抽出授权码：整串 URL 也行，裸 code 也行。
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
	if u, err := url.QueryUnescape(s); err == nil {
		s = u
	}
	s = strings.TrimSpace(s)
	// Google 的 code 形如 4/0Axxxxxxxx...；不做严格校验，只挡明显不是的（太短/含空格）
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
