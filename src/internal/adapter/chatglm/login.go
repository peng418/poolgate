package chatglm

// login.go 「登录」= 你在浏览器登录智谱清言（chatglm.cn），把 Cookie 里的
// `chatglm_refresh_token` 粘回面板。
//
// 为什么不扫码/密码：登录流程要过它自己的人机校验，服务端复现不了；把账号密码存在 NAS 上
// 风险更大。粘一个 refresh_token 已经足够 —— 它本身是长期凭证，且**不落任何密码**。
//
// 顺便说清一个容易混的点：要粘的是 **refresh_token**（长期），不是 access_token。
// 两者都是不透明串、看不出区别，所以我们两个都收（access_token 也能先跑起来），
// 但引导语里明确推荐 refresh_token —— 它过期后我们能自动续，access_token 过期只能重粘。

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

const loginPage = "https://chatglm.cn"

// hintText 是给面板的粘贴引导语。
//
// 直接给「在哪找」比让用户翻 Cookie 列表快得多：F12 → Application → Cookies → chatglm.cn
// → 找 chatglm_refresh_token → 复制 Value。也给一行控制台替代写法（只读，不上传）。
const hintText = "在浏览器登录智谱清言（chatglm.cn）后，按 F12 → Application → Cookies → https://chatglm.cn，" +
	"找到 `chatglm_refresh_token` 并复制它的 Value 粘到下面（推荐）。" +
	"也可以在控制台执行 document.cookie.split('; ').find(c=>c.startsWith('chatglm_refresh_token=')) 取出来。"

type session struct {
	a  *Adapter
	mu sync.Mutex
	// token 是用户粘回来的凭证（refresh 优先）；uid 由它派生。
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

// AcceptCallback 收用户粘回来的凭证（裸串 / 带引号 / 整段 JSON / `cookie 名=值` 都认）。
func (s *session) AcceptCallback(raw string) error {
	tok := extractToken(raw)
	if tok == "" {
		return errs.New(errs.Parse,
			"没识别出凭证：请复制 chatglm_refresh_token 的 Value（`chatglm_refresh_token=…` 这种整行也认）").
			WithChannel(string(channel.ChatGLM))
	}
	s.mu.Lock()
	s.token, s.uid = tok, "chatglm-"+shortHash(tok)
	s.mu.Unlock()
	return nil
}

// Poll 在用户粘了凭证之后完成组装，并**当场验一次**（换 access token 成功即有效）。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	tok, uid, done := s.token, s.uid, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.ChatGLM))
	}
	if tok == "" {
		return nil, channel.ErrPending // 还没粘：正常的等待态
	}

	cred := &channel.Credential{
		UID:          uid,
		Nickname:     "智谱清言账号",
		RefreshToken: tok, // 约定：粘进来的当 refresh token；换出来的 access token 由 check 填上
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

// extractToken 从粘贴内容里抽凭证。
//
// 认这些形态（用户会怎么粘是没法规定的，全认下来最省事）：
//
//	裸串 / "带引号" / {"refresh_token":"…"} / chatglm_refresh_token=… / Cookie: …chatglm_refresh_token=…
func extractToken(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "{") {
		var obj struct {
			RefreshToken string `json:"refresh_token"`
			AccessToken  string `json:"access_token"`
			Token        string `json:"token"`
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
	// `chatglm_refresh_token=xxxx`（含前面带 Cookie: 前缀的情况）→ 只取值。
	if i := strings.Index(s, "chatglm_refresh_token="); i >= 0 {
		s = s[i+len("chatglm_refresh_token="):]
		if j := strings.IndexAny(s, "; \t\n"); j >= 0 {
			s = s[:j]
		}
	}
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	s = strings.TrimSpace(s)
	// 令牌形态：较长的不透明串；挡掉明显误粘的内容（含空白的多词文本）。
	if len(s) < 20 || strings.ContainsAny(s, " \t\n") {
		return ""
	}
	return s
}

// shortHash 给账号一个稳定短标识（面板显示用，不泄露凭证本身）。
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}
