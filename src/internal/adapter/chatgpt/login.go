package chatgpt

// login.go 「登录」= 你在自己的浏览器里登录 chatgpt.com，然后把 accessToken 粘回面板。
//
// 为什么不做自动登录（账号密码 / 无头浏览器）：
//   - 网页登录要过 Cloudflare 与它自己的人机校验，服务端复现不了；
//   - 把 OpenAI 账号密码存在 NAS 上，风险远大于「多一步粘贴」——
//     参考实现里那条「存邮箱密码自动登录」的路我们不抄。
//
// 认的凭证只有一种：**accessToken（JWT）**。它有两面，引导语必须把两面都说清楚：
//   - 好的一面：拿它就能直接对话，不需要 cookie、不需要密码；
//   - 坏的一面：它会过期，而**没有 session cookie 就刷不了**（见 client.go 的 Refresh），
//     过期后必须重新粘一次。面板要提前提示，别让用户以为渠道坏了。
//
// 粘贴进来之后：
//   1. 解出 JWT 的 exp / 邮箱 / account_id，账号身份就有了（UID 用 account_id）；
//   2. **当场**用 /backend-api/me 验一次 —— 把失效的凭证塞进池子，表现是「装上就报错」，
//      用户分不清是自己粘错了还是渠道坏了；
//   3. 顺手把设备指纹写进 Extra（幂等），让它随账号落盘。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const loginPage = "https://chatgpt.com"

// hintText 是给面板的粘贴引导语。
//
// 给两条路：控制台一行命令（最省事、最不容易拿错），以及 Network 面板里手搜
// （对应 gpt4free 用的那条正则 `"accessToken":"…"`）。
// 强调「只读、不上传」：让用户敢在控制台执行这段代码。
const hintText = "在浏览器登录 chatgpt.com 后，按 F12 打开控制台，执行：" +
	"(await (await fetch('/api/auth/session')).json()).accessToken" +
	" —— 把它打印出来的那一整串（eyJ 开头）粘到下面。" +
	"（也可以不用控制台：F12 → Network 里找 accessToken，或任意请求的响应里搜 \"accessToken\":\"…\"。" +
	"这段命令只读你自己的登录态，不会把任何东西发到别处。）"

// session 是一次「等粘贴」的登录会话。
type session struct {
	a  *Adapter
	mu sync.Mutex
	// token 是用户粘回来的 accessToken；uid 由 JWT 里的 account_id 决定，粘完就固定。
	token string
	uid   string
	done  bool
}

// StartLogin 返回一个「等粘贴」的登录会话（面板会显示 AuthURL + Hint）。
func (a *Adapter) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	return &session{a: a}, nil
}

func (s *session) AuthURL() string { return loginPage }

// Hint 实现控制台的可选接口：面板据此切换成「粘贴」形态（不画二维码、不提示扫码）。
func (s *session) Hint() string { return hintText }

// AcceptCallback 收用户粘回来的 accessToken。
//
// 宽容一点：裸串、带引号、`accessToken = …`、整段 session JSON 都认 ——
// 用户是从浏览器里"抠"出来的，格式不统一是常态，为格式吵架没有意义。
func (s *session) AcceptCallback(raw string) error {
	tok := extractAccessToken(raw)
	if tok == "" {
		return errs.New(errs.Parse,
			"没识别出 accessToken：请在 chatgpt.com 的控制台执行面板上给出的那行命令，"+
				"把打印出来的整串（eyJ 开头）粘过来").WithChannel(string(channel.ChatGPT))
	}
	s.mu.Lock()
	s.token, s.uid = tok, uidFromToken(tok)
	s.mu.Unlock()
	return nil
}

// Poll 在用户粘了凭证之后完成组装，并**当场验一次**。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	tok, uid, done := s.token, s.uid, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.ChatGPT))
	}
	if tok == "" {
		return nil, channel.ErrPending // 还没粘：正常的等待态
	}

	cred := &channel.Credential{
		UID:         uid,
		AccessToken: tok,
		ExpiresAt:   tokenExpiry(tok),
		Nickname:    nicknameFromToken(tok),
	}
	// 设备指纹随凭证一起落盘：之后每个请求都从这里读，绝不每请求随机。
	ensureFingerprint(cred)

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

// ---------------------------------------------------------------------------
// 凭证解析
// ---------------------------------------------------------------------------

var (
	reAccessToken     = regexp.MustCompile(`"accessToken"\s*:\s*"([^"]+)"`)
	reAccessTokenAsgn = regexp.MustCompile(`accessToken["']?\s*[=:]\s*["']?([A-Za-z0-9._\-]+)`)
)

// extractAccessToken 从粘贴内容里抠出 accessToken。
func extractAccessToken(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// 整段 session JSON（含 accessToken / user / expires 等字段）。
	if strings.HasPrefix(s, "{") {
		var obj struct {
			AccessToken  string `json:"accessToken"`
			AccessToken2 string `json:"access_token"`
			Token        string `json:"token"`
			Value        string `json:"value"`
		}
		if json.Unmarshal([]byte(s), &obj) == nil {
			for _, v := range []string{obj.AccessToken, obj.AccessToken2, obj.Token, obj.Value} {
				if strings.TrimSpace(v) != "" {
					s = strings.TrimSpace(v)
					break
				}
			}
		}
	}
	// 不是 JSON 但对得上正则（用户从 DevTools 里连着上下文一起复制了）。
	if m := reAccessToken.FindStringSubmatch(s); len(m) > 1 {
		s = m[1]
	}
	// `accessToken = eyJ…` / `accessToken: eyJ…` 这类带键名的形态
	// （引导语里那行命令的输出粘下来常带前缀）。只有真的出现键名才动它 ——
	// 不按裸 "=" 切，是因为 base64 令牌自身可能带填充 `=`，切了就把凭证切坏了。
	if m := reAccessTokenAsgn.FindStringSubmatch(s); len(m) > 1 {
		s = m[1]
	}
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	s = strings.TrimSpace(s)

	// 形状校验：JWT（三段点分）或足够长的不透明串；挡掉明显误粘的多词文本。
	if len(s) < 20 || strings.ContainsAny(s, " \t\n") {
		return ""
	}
	return s
}

// parseJWTClaims 解出 JWT 的 payload。**不校验签名** —— 我们只用它读 exp 与身份标识，
// 真正的校验在上游（签名错了上游自然 401）。
func parseJWTClaims(tok string) (map[string]any, bool) {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 {
		return nil, false
	}
	seg := strings.TrimRight(parts[1], "=")
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(seg); err != nil {
			return nil, false
		}
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil, false
	}
	return claims, true
}

// uidFromToken 取账号 UID：优先 JWT 里的 chatgpt_account_id，取不到就用令牌哈希。
func uidFromToken(tok string) string {
	if claims, ok := parseJWTClaims(tok); ok {
		if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			if id := strings.TrimSpace(strAny(auth["chatgpt_account_id"], "")); id != "" {
				return "chatgpt-" + id
			}
		}
		if sub := strings.TrimSpace(strAny(claims["sub"], "")); sub != "" {
			return "chatgpt-" + sub
		}
	}
	return "chatgpt-" + shortHash(tok)
}

// nicknameFromToken 取展示名（邮箱最直观；没有就退回 plan 或默认名）。
func nicknameFromToken(tok string) string {
	claims, ok := parseJWTClaims(tok)
	if !ok {
		return "ChatGPT 账号"
	}
	if email := strings.TrimSpace(strAny(claims["email"], "")); email != "" {
		return email
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if plan := strings.TrimSpace(strAny(auth["chatgpt_plan_type"], "")); plan != "" {
			return "ChatGPT（" + plan + "）"
		}
	}
	return "ChatGPT 账号"
}

// tokenExpiry 取 JWT 的 exp；取不到返回零值（表示「未知」，不当成已过期）。
func tokenExpiry(tok string) time.Time {
	claims, ok := parseJWTClaims(tok)
	if !ok {
		return time.Time{}
	}
	exp, ok := claims["exp"].(float64)
	if !ok || exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(exp), 0)
}

// shortHash 给账号一个稳定短标识（面板显示用，不泄露令牌本身）。
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}
