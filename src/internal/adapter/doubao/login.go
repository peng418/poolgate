package doubao

// login.go 「登录」= 你把浏览器里豆包的整行 Cookie 粘回来。
//
// 与 Kimi / 智谱那两条不一样：**豆包没有可以用来验凭证的轻量接口**（没有 /me、没有订阅查询），
// 唯一的出口就是对话端点 —— 用它来「验一下」等于真发一次对话（浪费额度、还可能触发风控）。
// 所以这里只做**格式检查**（sessionid 在不在），凭证真伪由第一次对话暴露：
// 401/风控码会被归一成 SessionDead，面板上会把这个账号标成「需要重新登录」。
// 这个取舍写在引导语里，用户就不会以为是登录流程出了问题。

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const loginPage = "https://www.doubao.com/chat/"

// hintText 是给面板的粘贴引导语。
const hintText = "在浏览器登录豆包（www.doubao.com）后，按 F12 → Network → 点任意一个 doubao.com 的请求 → " +
	"在 Request Headers 里复制整行 `cookie` 的值，粘到下面（至少要有 sessionid）。" +
	"想固定设备号的话可以粘 JSON：{\"cookie\":\"…\",\"device_id\":\"…\",\"web_id\":\"…\"}。" +
	"注意：豆包没有轻量校验接口，所以这里只检查格式，凭证是否还有效由第一次对话确认。"

type session struct {
	a  *Adapter
	mu sync.Mutex
	// raw 是用户粘回来的原文；uid 由 sessionid 派生（面板显示用，不泄露 Cookie）。
	raw  string
	uid  string
	done bool
}

// StartLogin 返回一个「等粘贴」的登录会话。
func (a *Adapter) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	return &session{a: a}, nil
}

func (s *session) AuthURL() string { return loginPage }

// Hint 实现控制台的可选接口：面板据此切换成「粘贴」形态（不画二维码、不提示扫码）。
func (s *session) Hint() string { return hintText }

// AcceptCallback 收用户粘回来的 Cookie（同时做格式检查，粘坏了当场说清楚）。
func (s *session) AcceptCallback(raw string) error {
	c, err := parseCred(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.raw, s.uid = strings.TrimSpace(raw), "doubao-"+shortHash(c.sessionid)
	s.mu.Unlock()
	return nil
}

// Poll 组装凭证（格式已在 AcceptCallback 里检查过；有效性由首次对话确认）。
func (s *session) Poll(_ context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	raw, uid, done := s.raw, s.uid, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.Doubao))
	}
	if strings.TrimSpace(raw) == "" {
		return nil, channel.ErrPending // 还没粘：正常的等待态
	}
	c, err := parseCred(raw)
	if err != nil {
		return nil, err
	}
	cred := &channel.Credential{
		UID:         uid,
		Nickname:    "豆包账号",
		AccessToken: c.cookie, // Cookie 串本身就是凭证
		Extra: map[string]string{
			"device_id": c.deviceID,
			"web_id":    c.webID,
			"fp":        c.fp,
		},
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
