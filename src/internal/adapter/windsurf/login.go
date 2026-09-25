package windsurf

// login.go 「登录」= 你在自己的 Windsurf/Devin 客户端里登录后，把 **session token** 粘回面板。
//
// 为什么不做邮箱密码登录：参考实现那条路（Auth1：邮箱密码 → PostAuth → 换出
// `devin-session-token$…`）要求把密码存在网关上，风险远大于「多一步粘贴」。
// 我们也不跑本地语言服务器（见包注释）。所以这里只实现「粘贴」这一条路。
//
// 实现 channel.CallbackAcceptor：控制台已有通用的粘贴通道，用户粘一次 session token，
// 我们落盘凭证；**不存任何密码**。

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

const loginPage = "https://windsurf.com"

// hintText 是给面板的粘贴引导语。
//
// 要点写清楚三件事：要粘的是什么形状、从哪拿、我们不做密码登录。
// session token 是长期凭证，形如 `devin-session-token$<JWT>`。
const hintText = "在 Windsurf / Devin 客户端登录后，取出它的本地登录态里的 session token —— " +
	"形如 `devin-session-token$…` 的一整串（参考实现 WindsurfAPI 的账号池里存的 apiKey 也是这串，" +
	"可先用它登录一次再复制出来）。把这一整串粘到下面的输入框即可。" +
	"注意：本渠道**不做**邮箱密码登录（不在网关上存密码），也不运行本地语言服务器。"

type session struct {
	a  *Adapter
	mu sync.Mutex
	// token 是用户粘回来的 session token；uid 由它派生，粘完就固定。
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

// AcceptCallback 收用户粘回来的凭证（裸串 / 带引号 / 整段 JSON / `key = value` 都认）。
func (s *session) AcceptCallback(raw string) error {
	tok := extractSessionToken(raw)
	if tok == "" {
		return errs.New(errs.Parse,
			"没识别出 session token：请把形如 `devin-session-token$…` 的那一整串粘过来").
			WithChannel(string(channel.Windsurf))
	}
	s.mu.Lock()
	s.token, s.uid = tok, "windsurf-"+shortHash(tok)
	s.mu.Unlock()
	return nil
}

// Poll 在用户粘了凭证之后完成组装，并**当场用额度接口验一次**。
//
// 为什么要当场验：把一个已失效的凭证塞进池子，用户看到的是「装上了但一调用就报错」，
// 分不清是自己粘错了还是渠道坏了。这里直接问一次 GetUserStatus（零推理、不烧额度），
// 错了当场说清楚；成功时顺带把 plan 名写进昵称，免费/付费一眼可见。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	tok, uid, done := s.token, s.uid, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.Windsurf))
	}
	if tok == "" {
		return nil, channel.ErrPending // 还没粘：正常的等待态
	}

	cred := &channel.Credential{UID: uid, Nickname: "Windsurf 账号", AccessToken: tok}
	plan, err := s.a.checkCredential(ctx, cred)
	if err != nil {
		return nil, err
	}
	if plan != "" {
		cred.Nickname = "Windsurf（" + plan + "）"
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

// extractSessionToken 从粘贴内容里抽凭证：裸串 / "带引号" / JSON / `key = value` 都认。
//
// 挡掉明显误粘的内容：太短、或者含空白的多词文本（session token 是没有空白的单串）。
func extractSessionToken(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "{") {
		var obj struct {
			Token       string `json:"token"`
			APIKey      string `json:"apiKey"`
			AccessToken string `json:"access_token"`
			Session     string `json:"session_token"`
			Value       string `json:"value"`
		}
		if json.Unmarshal([]byte(s), &obj) == nil {
			for _, v := range []string{obj.Session, obj.Token, obj.APIKey, obj.AccessToken, obj.Value} {
				if strings.TrimSpace(v) != "" {
					s = strings.TrimSpace(v)
					break
				}
			}
		}
	}
	// 用户可能粘成 `key = value` 或 `key: value`。
	if i := strings.Index(s, " = "); i >= 0 {
		s = strings.TrimSpace(s[i+3:])
	}
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	s = strings.TrimSpace(s)
	if len(s) < 20 || strings.ContainsAny(s, " \t\r\n") {
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
