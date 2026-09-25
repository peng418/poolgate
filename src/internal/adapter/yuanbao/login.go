package yuanbao

// login.go 「登录」= 你把浏览器里元宝那次请求的 `x-uskey` 粘回来。
//
// 元宝的鉴权就是「那一份请求头」，没有签名、没有 nonce —— 所以粘贴这一步比豆包简单得多。
// 我们只要 x-uskey（cookie 顺带带上更接近真实客户端，但两份参考实现都表明
// 真正被校验的只有 x-uskey）。
//
// 与 Kimi / 智谱不同：元宝**没有**可以用来验凭证的轻量接口，所以这里只做格式检查，
// 凭证真伪由第一次对话确认（401 会被归一成 SessionDead，面板会把账号标成需要重新登录）。

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const loginPage = "https://yuanbao.tencent.com/chat/"

// hintText 是给面板的粘贴引导语。
const hintText = "在浏览器登录腾讯元宝（yuanbao.tencent.com）并**随便发一条消息**，" +
	"然后 F12 → Network → 找到 yuanbao.tencent.com/api 的请求 → Request Headers → " +
	"复制 `x-uskey` 的值粘到下面（整段请求头一起粘也行，我们会自己挑）。" +
	"注意：元宝没有轻量校验接口，所以这里只检查格式，凭证是否有效由第一次对话确认。"

type session struct {
	a    *Adapter
	mu   sync.Mutex
	raw  string
	uid  string
	done bool
}

// StartLogin 返回一个「等粘贴」的登录会话。
func (a *Adapter) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	return &session{a: a}, nil
}

func (s *session) AuthURL() string { return loginPage }

// Hint 实现控制台的可选接口：面板据此切换成「粘贴」形态。
func (s *session) Hint() string { return hintText }

// AcceptCallback 收用户粘回来的内容（整段请求头 / JSON / 裸 uskey 都认）。
func (s *session) AcceptCallback(raw string) error {
	cr, err := parseCred(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.raw, s.uid = strings.TrimSpace(raw), "yuanbao-"+shortHash(cr.uskey)
	s.mu.Unlock()
	return nil
}

// Poll 组装凭证（格式已在 AcceptCallback 里检查过；有效性由首次对话确认）。
func (s *session) Poll(_ context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	raw, uid, done := s.raw, s.uid, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.Yuanbao))
	}
	if strings.TrimSpace(raw) == "" {
		return nil, channel.ErrPending // 还没粘：正常的等待态
	}
	cr, err := parseCred(raw)
	if err != nil {
		return nil, err
	}
	cred := &channel.Credential{
		UID:      uid,
		Nickname: "腾讯元宝账号",
		// AccessToken 存**用户粘回来的原文**（可能是整段请求头，也可能只是 uskey）：
		// Chat 时还要从里面取 cookie 与 UA，只存 uskey 会把这些信息丢掉。
		AccessToken: raw,
		Extra:       map[string]string{"uskey": cr.uskey},
	}
	if cr.cookie != "" {
		cred.Extra["cookie"] = cr.cookie
	}
	if cr.ua != "" {
		cred.Extra["user_agent"] = cr.ua
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

// shortHash 给账号一个稳定短标识（面板显示用）。
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}
