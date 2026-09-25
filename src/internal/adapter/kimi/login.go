package kimi

// login.go 「登录」= 你在自己的浏览器里登录 Kimi，然后把凭证粘回面板。
//
// 为什么不做扫码/密码自动登录：
//   - 网页登录要过它自己的设备指纹与人机校验，服务端复现不了；
//   - 把邮箱密码存在 NAS 上，风险远大于「多一步粘贴」——参考实现里那条自动登录的路我们不抄。
//
// 认两种凭证，都能在浏览器存储里拿到：
//   - refresh token（长期、推荐）：我们拿它去换 access token，换完不发散、不需要回写；
//   - access token（JWT）：也能直接用，但它会过期，而**没有 refresh token 就刷不了** ——
//     过期后必须重新粘一次。面板的引导语把这点说清楚。

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const loginPage = "https://www.kimi.com"

// hintText 是给面板的粘贴引导语。
//
// 为什么要给一段 JS：上游把令牌放在 localStorage 里的**哪个键**会变（而且可能不止一处），
// 与其让用户翻遍存储，不如让他执行一行代码把候选值打出来 —— 这段代码只读、不上传任何东西。
const hintText = "在浏览器登录 Kimi（www.kimi.com）后，按 F12 打开控制台，执行：" +
	"Object.entries(localStorage).filter(([k,v])=>/token|auth/i.test(k)||/^(eyJ|cpmt_)/.test(String(v))).map(([k,v])=>k+' = '+v)" +
	" —— 把其中 refresh token（或 access token）那一整串值粘到下面。"

type session struct {
	a  *Adapter
	mu sync.Mutex
	// token 是用户粘回来的凭证（refresh 或 access）；uid 由它派生，粘完就固定。
	token string
	uid   string
	done  bool
}

// StartLogin 返回一个「等粘贴」的登录会话。
func (a *Adapter) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	return &session{a: a}, nil
}

func (s *session) AuthURL() string { return loginPage }

// Hint 实现控制台的可选接口：面板据此切换成「粘贴」形态（不画二维码、不提示扫码）。
func (s *session) Hint() string { return hintText }

// AcceptCallback 收用户粘回来的凭证（裸串 / 带引号 / 整段 JSON 都认）。
func (s *session) AcceptCallback(raw string) error {
	tok := extractToken(raw)
	if tok == "" {
		return errs.New(errs.Parse,
			"没识别出 Kimi 凭证：请在浏览器控制台执行面板上给出的那行代码，"+
				"把 refresh token（或 access token）整串粘过来").WithChannel(string(channel.Kimi))
	}
	s.mu.Lock()
	s.token, s.uid = tok, "kimi-"+shortHash(tok)
	s.mu.Unlock()
	return nil
}

// Poll 在用户粘了凭证之后完成组装，并**当场验一次**。
//
// 为什么要当场验：把一个已失效的凭证塞进池子，用户看到的是「装上了但一调用就报错」，
// 分不清是自己粘错了还是渠道坏了。这里直接拿订阅接口问一句，错了当场说清楚。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	tok, uid, done := s.token, s.uid, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.Kimi))
	}
	if tok == "" {
		return nil, channel.ErrPending // 还没粘：正常的等待态
	}

	cred := &channel.Credential{UID: uid, Nickname: "Kimi 账号"}
	if isAccessToken(tok) {
		cred.AccessToken = tok
	} else {
		// refresh token：先换一次 access token，换成功说明它还有效。
		cred.RefreshToken = tok
	}
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

// extractToken 从粘贴内容里抽凭证：裸串 / "带引号" / JSON（token、access_token、refresh_token 都认）。
func extractToken(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "{") {
		var obj struct {
			Token        string `json:"token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Value        string `json:"value"`
		}
		if json.Unmarshal([]byte(s), &obj) == nil {
			for _, v := range []string{obj.RefreshToken, obj.AccessToken, obj.Token, obj.Value} {
				if strings.TrimSpace(v) != "" {
					s = strings.TrimSpace(v)
					break
				}
			}
		}
	}
	// 用户可能粘成 `key = value`（我们引导语里的输出格式就是 `k+' = '+v`）。
	if i := strings.Index(s, " = "); i >= 0 {
		s = strings.TrimSpace(s[i+3:])
	}
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	s = strings.TrimSpace(s)
	// 令牌形态：JWT（三段点分）或上游的不透明串；挡掉明显误粘的内容（含空白的多词文本）。
	if len(s) < 20 || strings.ContainsAny(s, " \t\n") {
		return ""
	}
	return s
}

// shortHash 给账号一个稳定短标识（面板显示用，不泄露令牌本身）。
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}
