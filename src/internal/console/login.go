// login.go 控制台渠道授权：设备流/回调流的编排层。
//
// 这里只做三件事 —— 起一次授权、反复问结果、必要时取消；协议细节全在适配器的
// channel.LoginSession 里（红线三：核心层零渠道专有代码）。
//
// 与「管理员密码登录」是两套东西（store/admin.go 注释）：控制台登录证明你是管理员，
// 渠道授权证明上游账号可用。别把两者混成一个接口。
package console

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/registry"
)

// LoginTimeout 是一次授权的有效期。超时后必须明确告诉用户「超时了，重来」，
// 而不是让面板一直转圈（红线一：失败必带原因）。
const LoginTimeout = 5 * time.Minute

// channelLoginReq 是 POST /api/login/start 的请求体。
// 注意与 admin 密码登录的 loginReq 区分：两套登录是完全不同的东西。
type channelLoginReq struct {
	Channel string `json:"channel"`
}

// loginStartResp 返回给前端的授权页地址。
type loginStartResp struct {
	Channel string `json:"channel"`
	AuthURL string `json:"auth_url"`
	Expires string `json:"expires_at"`
	// CallbackBase 是这次授权给上游的回调基址（TraeWork 用它把 127.0.0.1 换成面板地址）。
	// 面板如实告诉用户「回调打到哪台机器」——这决定了扫码/别的浏览器能不能完成。
	CallbackBase string `json:"callback_base,omitempty"`
	// PasteHint 是给「粘贴式」渠道（实现 CallbackAcceptor）的引导语，如 DeepSeek：
	// 它的凭证是用户在自己浏览器登录后粘回来的 userToken，面板据此切换引导文案。
	PasteHint string `json:"paste_hint,omitempty"`
}

// loginPollResp 是轮询结果：pending / ok 两态，失败直接走结构化错误。
type loginPollResp struct {
	Status  string         `json:"status"` // pending | ok
	Channel string         `json:"channel,omitempty"`
	Account map[string]any `json:"account,omitempty"`
	Message string         `json:"message,omitempty"`
	// Balance 是授权成功后顺手拉到的余额；BalanceError 非空表示没拉到及原因。
	//
	// 为什么在授权这一步就拉：新号刚入池时余额是「未知」，用户看到的是
	// 「刚加完号，余额栏一片未知」—— 以为没接上，其实只是没人去问过上游。
	// 加号是「这个号能用多少」的唯一自然时机，别把这件事推给用户再点一次。
	Balance      map[string]any `json:"balance,omitempty"`
	BalanceError string         `json:"balance_error,omitempty"`
}

// activeLogin 是一次进行中的授权。
type activeLogin struct {
	kind   channel.Kind
	sess   channel.LoginSession
	until  time.Time
	mgr    *loginManager
	cancel context.CancelFunc
}

// loginManager 保证「同一时间只有一次授权在进行」。
//
// 同时开两次授权会让用户不知道该点哪个页面，也会让凭证归属含糊 —— 上游一次只该
// 授权一个账号。第二次请求直接告诉用户「已有授权进行中」，而不是默默覆盖。
type loginManager struct {
	mu     sync.Mutex
	active *activeLogin
	save   func(channel.Kind, channel.Credential) (string, error)
	pool   func(channel.Kind, channel.Credential)
	now    func() time.Time
}

func (m *loginManager) start(ctx context.Context, kind channel.Kind, opts channel.LoginOptions) (string, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if m.active != nil {
		if now.Before(m.active.until) {
			return "", time.Time{}, errs.New(errs.Parse, "已有渠道授权在进行中，请先在浏览器完成或点取消").
				WithChannel(string(m.active.kind))
		}
		// 上一轮已经超时：先清掉，别让过期会话挡住新授权。
		m.active.sess.Cancel()
		m.active = nil
	}

	auth, ok := authorizerFor(kind)
	if !ok {
		return "", time.Time{}, errs.New(errs.Parse, "渠道 "+string(kind)+" 不支持面板授权，请手动导入凭证").
			WithChannel(string(kind))
	}

	sess, err := auth.StartLogin(ctx, opts)
	if err != nil {
		if ee, ok := err.(*errs.Error); ok {
			return "", time.Time{}, ee.WithChannel(string(kind))
		}
		return "", time.Time{}, errs.New(errs.AuthFailed, "发起授权失败").WithChannel(string(kind)).WithCause(err)
	}

	until := now.Add(LoginTimeout)
	m.active = &activeLogin{kind: kind, sess: sess, until: until, mgr: m}
	log.Printf("console: 渠道 %s 授权已发起，%s 内有效", kind, LoginTimeout)
	return sess.AuthURL(), until, nil
}

func (m *loginManager) poll(ctx context.Context) (*activeLogin, *channel.Credential, error) {
	m.mu.Lock()
	al := m.active
	if al == nil {
		m.mu.Unlock()
		return nil, nil, errs.New(errs.Parse, "没有进行中的渠道授权")
	}
	if m.now().After(al.until) {
		m.mu.Unlock()
		m.finish(al)
		return nil, nil, errs.New(errs.AuthFailed, "授权超时（"+LoginTimeout.String()+"），请重新发起").
			WithChannel(string(al.kind))
	}
	m.mu.Unlock()

	cred, err := al.sess.Poll(ctx)
	if err != nil {
		if errors.Is(err, channel.ErrPending) {
			return al, nil, nil // 还没授权完：这是正常态，不是错误
		}
		// 真失败：结束这次授权，把原因交给用户（红线一）。
		m.finish(al)
		if ee, ok := err.(*errs.Error); ok {
			return nil, nil, ee.WithChannel(string(al.kind))
		}
		return nil, nil, errs.New(errs.AuthFailed, "授权失败").WithChannel(string(al.kind)).WithCause(err)
	}
	if cred == nil {
		return al, nil, nil
	}
	if cred.UID == "" {
		m.finish(al)
		return nil, nil, errs.New(errs.Parse, "上游未返回账号标识（uid），无法落盘").WithChannel(string(al.kind))
	}

	// 成功：先落盘再入池。落盘失败必须报错 —— 否则重启后面板里会出现一个
	// 「能用但重启就没了」的幽灵账号。
	if m.save != nil {
		if _, err := m.save(al.kind, *cred); err != nil {
			m.finish(al)
			return nil, nil, errs.New(errs.Parse, "授权成功但凭证落盘失败："+err.Error()).WithChannel(string(al.kind))
		}
	}
	if m.pool != nil {
		m.pool(al.kind, *cred)
	}
	log.Printf("console: 渠道 %s 授权成功，账号 %s 已入池", al.kind, cred.UID)
	m.finish(al)
	// 返回 al 而不是 nil：调用方要用它标出这是哪个渠道的授权（al 本身已经释放）。
	return al, cred, nil
}

// acceptCallback 把用户手工粘贴的回调地址交给当前这次授权（未实现该能力的渠道报错说清）。
func (m *loginManager) acceptCallback(raw string) (channel.Kind, error) {
	m.mu.Lock()
	al := m.active
	m.mu.Unlock()
	if al == nil {
		return "", errs.New(errs.Parse, "没有进行中的渠道授权（先点「授权」再粘贴）")
	}
	acc, ok := al.sess.(channel.CallbackAcceptor)
	if !ok {
		return al.kind, errs.New(errs.Parse,
			"渠道 "+string(al.kind)+" 的授权不需要手工回填：请在浏览器里打开授权页完成").
			WithChannel(string(al.kind))
	}
	if err := acc.AcceptCallback(raw); err != nil {
		if ee, ok := err.(*errs.Error); ok {
			return al.kind, ee.WithChannel(string(al.kind))
		}
		return al.kind, errs.New(errs.Parse, "回调内容无法识别").WithCause(err).
			WithChannel(string(al.kind))
	}
	log.Printf("console: 渠道 %s 收到手工回填的回调，等待下次轮询兑换", al.kind)
	return al.kind, nil
}

func (m *loginManager) cancel() (channel.Kind, error) {
	m.mu.Lock()
	al := m.active
	m.active = nil
	m.mu.Unlock()
	if al == nil {
		return "", errs.New(errs.Parse, "没有进行中的渠道授权")
	}
	al.sess.Cancel()
	return al.kind, nil
}

// finish 结束一次授权（幂等：只有还是当前那次才清）。
func (m *loginManager) finish(al *activeLogin) {
	m.mu.Lock()
	if m.active == al {
		m.active = nil
	}
	m.mu.Unlock()
	al.sess.Cancel()
}

// HintProvider 返回当前授权会话的粘贴引导语（DeepSeek 这类「粘回凭证」的渠道实现它）；
// 没有进行中的授权或渠道不给引导语时 ok=false。
func (m *loginManager) HintProvider() (string, bool) {
	m.mu.Lock()
	al := m.active
	m.mu.Unlock()
	if al == nil {
		return "", false
	}
	hp, ok := al.sess.(interface{ Hint() string })
	if !ok || hp.Hint() == "" {
		return "", false
	}
	return hp.Hint(), true
}

// authorizerFor 从注册表取该渠道的授权能力。没注册或没实现都不算错误，
// 但要能说清楚「为什么这个渠道点不了授权」。
func authorizerFor(kind channel.Kind) (channel.Authorizer, bool) {
	ch, ok := registry.Get(kind)
	if !ok {
		return nil, false
	}
	a, ok := ch.(channel.Authorizer)
	return a, ok
}

// ---------------------------------------------------------------------------
// HTTP 处理
// ---------------------------------------------------------------------------

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req channelLoginReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	if req.Channel == "" {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "缺少 channel 字段"))
		return
	}
	kind := channel.Kind(req.Channel)
	if _, ok := registry.GetSpec(kind); !ok {
		writeErr(w, http.StatusNotFound, errs.New(errs.Parse, "未知渠道 "+req.Channel).WithChannel(req.Channel))
		return
	}

	// 回调地址按「面板自己的对外地址」给：手机扫码、别的电脑的浏览器都能回到这台机器
	// （TraeWork 用它把 127.0.0.1 换成局域网地址；千问办公忽略它 —— 它的回调是预注册死的）。
	base := s.panelBase(r)
	url, until, err := s.login.start(r.Context(), kind, channel.LoginOptions{CallbackBase: base})
	if err != nil {
		writeErrFromErr(w, err)
		return
	}
	// 粘贴式渠道（实现 CallbackAcceptor 且会话暴露引导语，如 DeepSeek 粘 userToken）
	// 把引导语带给前端：这类渠道的「授权」不是扫码/点授权页，而是用户在自己的浏览器
	// 登录后把凭证粘回面板 —— 引导语里必须写清楚拿什么、怎么拿。
	var pasteHint string
	if hp, ok := s.login.HintProvider(); ok {
		pasteHint = hp
	}
	writeJSON(w, http.StatusOK, loginStartResp{Channel: req.Channel, AuthURL: url, Expires: until.Format(time.RFC3339), CallbackBase: base, PasteHint: pasteHint})
}

func (s *Server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 GET/POST"))
		return
	}
	al, cred, err := s.login.poll(r.Context())
	if err != nil {
		writeErrFromErr(w, err)
		return
	}
	if cred == nil {
		writeJSON(w, http.StatusOK, loginPollResp{Status: "pending", Channel: string(al.kind), Message: "等待浏览器完成授权"})
		return
	}
	// 入池后立刻拉一次余额：让「授权成功」这四个字后面跟着真实数字。
	// 余额失败只作为附注返回 —— 授权本身已经成功，不能因为余额查不到就报错。
	bal, balErr := s.refreshOneBalance(r.Context(), al.kind, cred.UID)
	writeJSON(w, http.StatusOK, loginPollResp{
		Status:       "ok",
		Channel:      string(al.kind),
		Account:      map[string]any{"uid": cred.UID, "nickname": cred.Nickname},
		Balance:      bal,
		BalanceError: balErr,
	})
}

func (s *Server) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	kind, err := s.login.cancel()
	if err != nil {
		writeErrFromErr(w, err)
		return
	}
	log.Printf("console: 渠道 %s 授权已取消", kind)
	writeJSON(w, http.StatusOK, map[string]any{"status": "cancelled", "channel": string(kind)})
}

// handleLoginCallback 收用户手工粘贴的回调地址（本机回调被网络挡住时的兜底）。
//
// 只在「当前这次授权确实实现了 CallbackAcceptor」时可用；不做任何放宽 ——
// 粘进来的 code 仍然要走原来的 PKCE 兑换，服务端不额外信任任何东西。
func (s *Server) handleLoginCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req struct {
		Raw string `json:"raw"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	kind, err := s.login.acceptCallback(req.Raw)
	if err != nil {
		writeErrFromErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": string(kind)})
}
