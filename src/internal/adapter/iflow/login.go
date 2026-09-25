package iflow

// login.go 「登录」= 你在装了 iFlow CLI 的机器上把 apiKey 取出来，粘回面板。
//
// 为什么不做扫码/OAuth 自动登录：
//   - iFlow CLI 的登录态落在 ~/.iflow/settings.json 里，那是**本机文件**，服务端拿不到；
//   - 让用户把 iFlow 账号密码存在 NAS 上，风险远大于「多一步复制粘贴」。
//
// 所以实现 channel.CallbackAcceptor（控制台已有通用的「粘贴」通道）+ Hint()，
// 面板据此切换成「粘贴」形态；我们只存 apiKey，**不存任何账号密码**。
// 凭证在 Poll 里**当场验一次**，避免把一个坏 key 塞进池子（那会变成「装上就报 401」）。

import (
	"context"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// loginPage 是让用户去登录 iFlow / 取 apiKey 的入口。
const loginPage = "https://iflow.cn"

// hintText 是给面板的粘贴引导语 —— 必须写清楚「去哪拿 apiKey」。
//
// apiKey 不是网页 storage 里的东西，而是 iFlow CLI 落盘的配置项，所以引导语要给出
// **文件路径与字段名**，Windows 用户还要看到 Windows 路径（用户里有一半是 Windows）。
const hintText = "在装了 iFlow CLI 的机器上完成一次登录后，打开配置文件：" +
	"Linux/macOS 是 ~/.iflow/settings.json，Windows 是 C:\\Users\\<你的用户名>\\.iflow\\settings.json；" +
	"把其中 \"apiKey\" 这一项的值（整串）复制出来粘到下面。注意别把整个文件里的其它密钥一起粘进来。"

type session struct {
	a  *Adapter
	mu sync.Mutex
	// key 是用户粘回来的 apiKey；uid 由它派生，粘完就固定。
	key  string
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

// AcceptCallback 收用户粘回来的 apiKey（裸串 / 带引号 / 整段 settings.json 都认）。
func (s *session) AcceptCallback(raw string) error {
	key := extractAPIKey(raw)
	if key == "" {
		return errs.New(errs.Parse,
			"没识别出 apiKey：请打开 ~/.iflow/settings.json，把 \"apiKey\" 那一项的值整串粘过来").
			WithChannel(string(channel.IFlow))
	}
	s.mu.Lock()
	s.key, s.uid = key, "iflow-"+shortHash(key)
	s.mu.Unlock()
	return nil
}

// Poll 在用户粘了 apiKey 之后完成组装，并**当场验一次**。
//
// 为什么要当场验：把一个已失效的 key 塞进池子，用户看到的是「装上了但一调用就 401」，
// 分不清是自己粘错了还是渠道坏了。这里真打一次最小对话请求，错了当场说清楚。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	key, uid, done := s.key, s.uid, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.IFlow))
	}
	if strings.TrimSpace(key) == "" {
		return nil, channel.ErrPending // 还没粘：正常的等待态
	}

	cred := &channel.Credential{
		UID:         uid,
		Nickname:    "iFlow 账号",
		AccessToken: key, // apiKey 存 AccessToken 字段；没有 refresh 一说（见 Refresh）
	}
	if err := s.a.probeCredential(ctx, cred); err != nil {
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
