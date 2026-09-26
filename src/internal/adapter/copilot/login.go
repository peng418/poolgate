package copilot

// login.go GitHub **设备码**授权（RFC 8628）+ 一条「粘 token」兜底路径。
//
// 流程（与参考实现一致）：
//  1. POST https://github.com/login/device/code  {client_id, scope} → 拿到 device_code / user_code /
//     verification_uri / expires_in / interval；
//  2. 用户在自己的浏览器打开 verification_uri（默认 https://github.com/login/device）并输入 user_code；
//  3. 我们反复 POST https://github.com/login/oauth/access_token 换 githubToken：
//     未完成时上游回 200 + {"error":"authorization_pending"}（或 slow_down），我们归一成 channel.ErrPending。
//
// 为什么用「设备码」而不是「OAuth 回调」：回调要求 PoolGate 能被浏览器访问到（还要处理
// redirect_uri 白名单、HTTPS、公网/局域网地址可达性一堆事），而设备码只需要**出一个站**，
// 用户在哪台设备上完成授权都行 —— 密码始终只在用户自己的浏览器里输入，我们拿到的只是令牌。
//
// 为什么不做「邮箱密码自动授权」：那条路要把用户的 GitHub 密码存在 NAS 上（公开参考实现里
// 有这种玩法），对一个自用的账号池网关来说风险远大于「多一步在浏览器输码」。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// defaultDeviceTTL 是上游没给 expires_in 时的设备码有效期兜底（GitHub 实测 900 秒）。
const defaultDeviceTTL = 15 * time.Minute

// defaultPollInterval 是上游没给 interval 时的轮询间隔兜底。
//
// 5 秒不是我们拍的：RFC 8628 §3.5 规定未给 interval 时按 5 秒，三份参考实现也都以此为准
// （copilot-api poll-access-token.ts:17 用 interval 再 +1 秒；gpt4free oauthFlow.py:60 写
// `device_auth.get("interval", 5)`；BYOKEY crates/auth/src/provider/copilot.rs:25 的 default_expires_in
// 与 device_code.rs:98-101 用 dc.interval 睡）。
const defaultPollInterval = 5 * time.Second

// slowDownStep 是上游回 slow_down 时轮询间隔的增量。
//
// RFC 8628 §3.5：收到 slow_down 必须把间隔 +5 秒；gpt4free oauthFlow.py:79-80 是 `interval += 5`，
// BYOKEY 的 `apply_slow_down` 默认也是 +5.0（crates/auth/src/flow/device_code.rs:55-58,120-122）。
// 参考实现 copilot-api 没区分 slow_down（一律 interval+1），我们按 RFC + 另两家处理，
// 因为控制台是固定节奏轮询，不主动降速就会一直撞 slow_down。
const slowDownStep = 5 * time.Second

// deviceSession 是一次进行中的设备码授权。
type deviceSession struct {
	a *Adapter

	mu sync.Mutex
	// deviceCode 是上游给的设备码（机密，只用于轮询换 token；不要展示，也绝不进日志）。
	deviceCode string
	// userCode 是要显示给用户、让他输进 GitHub 授权页的那串码。
	userCode  string
	authURL   string
	expiresAt time.Time
	// interval 是两次「真的向上游问一次」之间的最小间隔（来自设备码响应的 interval）。
	interval time.Duration
	// nextPoll 是下一次允许真正打上游的时刻；不到点就只回 ErrPending，不碰上游。
	nextPoll time.Time
	// pasted 是兜底路径的 token：用户没走设备码，直接把已有的 GitHub token 粘回来。
	pasted string
	done   bool
}

// 编译期断言：登录会话必须满足面板会用到的三个能力
// （LoginSession + 粘凭证兜底 + 引导语）。
var (
	_ channel.LoginSession       = (*deviceSession)(nil)
	_ channel.CallbackAcceptor   = (*deviceSession)(nil)
	_ interface{ Hint() string } = (*deviceSession)(nil)
)

// deviceCodeResp 是设备码申请响应。
type deviceCodeResp struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// accessTokenResp 是令牌轮询响应。注意「未完成」也是 200，错误在 error 字段里。
type accessTokenResp struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Interval         int    `json:"interval"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// StartLogin 申请设备码，把「用户要打开的授权页 + 要输入的码」交出去。
//
// 面板只能展示 AuthURL，而 GitHub 的授权页**没有**一个「把 user_code 带在地址里直接完成」的
// 变体（Qwen 那种 verification_uri_complete 在 GitHub 不存在），所以 user_code 通过
// Hint() 交给面板显示 —— 见 Hint 的注释。
func (a *Adapter) StartLogin(ctx context.Context, _ channel.LoginOptions) (channel.LoginSession, error) {
	dc, err := a.requestDeviceCode(ctx)
	if err != nil {
		return nil, err
	}
	auth := strings.TrimSpace(dc.VerificationURI)
	if auth == "" {
		auth = githubBase + "/login/device"
	}
	ttl := time.Duration(dc.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultDeviceTTL
	}
	// 轮询节奏取自上游给的 interval；缺失/非法时按 RFC 8628 的 5 秒兜底。
	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	now := time.Now()
	return &deviceSession{
		a:          a,
		deviceCode: dc.DeviceCode,
		userCode:   dc.UserCode,
		authURL:    auth,
		expiresAt:  now.Add(ttl),
		interval:   interval,
		// 首次轮询也要等满 interval（参考实现 copilot-api 就是这样）——
		// 授权页还没打开就抢先问，只会白白撞上游的限流。
		nextPoll: now.Add(interval),
	}, nil
}

// requestDeviceCode 申请设备码。
func (a *Adapter) requestDeviceCode(ctx context.Context) (*deviceCodeResp, error) {
	body, _ := json.Marshal(map[string]any{"client_id": clientID, "scope": scope})
	status, raw, err := a.do(ctx, nil, http.MethodPost, a.githubBase+epDeviceCode, stdHeaders(), body)
	if err != nil {
		return nil, errs.New(errs.Transport, "申请 GitHub 设备码失败（连不上 github.com）").
			WithChannel(string(channel.Copilot)).WithCause(err)
	}
	var out deviceCodeResp
	_ = json.Unmarshal(raw, &out)
	if status >= 400 || strings.TrimSpace(out.DeviceCode) == "" {
		return nil, errs.New(a.Classify(status, raw), "GitHub 拒绝了设备码申请").
			WithChannel(string(channel.Copilot)).WithUpstream(truncate(string(raw), 200))
	}
	return &out, nil
}

func (s *deviceSession) AuthURL() string { return s.authURL }

// Hint 是给面板的引导语。
//
// 为什么把 user_code 放在这里：设备码流程要求用户在 GitHub 授权页**手动输入**那串码，
// 而适配器能自定义展示给用户的文案只有这一处（面板会把 Hint 显示在授权步骤的说明行里，
// AuthURL 单独画成链接/二维码）。不写的话用户只看到一个 github.com/login/device 链接，
// 到了页面上不知道自己该输什么。
//
// 顺带说明兜底路径：某些环境（无浏览器 / 公司网络拦了授权页）走不通设备码，此时可以用
// 已有的 GitHub token 直接完成（见 AcceptCallback）。
func (s *deviceSession) Hint() string {
	code := strings.TrimSpace(s.userCode)
	if code == "" {
		code = "（见授权页）"
	}
	return "在浏览器打开 " + s.authURL + " 并输入设备码 " + code +
		" 完成 GitHub 授权（设备码约 15 分钟内有效，完成后这里会自动继续）。" +
		"若你已有 GitHub token（例如本机执行 gh auth token 得到的），也可以直接粘到下面的输入框。"
}

// AcceptCallback 收用户粘回来的 GitHub token（裸串 / JSON / `key = value` 都认）。
//
// 这是**兜底**，不是主路径：主路径是设备码（用户不必理解 token 是什么）。保留它的理由：
// 设备码要在浏览器里手动输码，无浏览器或授权页被网络挡掉时走不通；而 GitHub token 本身
// 长期有效，用户在自己的机器上一条命令就能拿到。
func (s *deviceSession) AcceptCallback(raw string) error {
	tok := extractToken(raw)
	if tok == "" {
		return errs.New(errs.Parse,
			"没识别出 GitHub token：请粘贴 ghp_… / github_pat_… 这类整串值"+
				"（设备码本身不行，那串码是要输进 GitHub 授权页的）").
			WithChannel(string(channel.Copilot))
	}
	s.mu.Lock()
	s.pasted = tok
	s.mu.Unlock()
	return nil
}

// Poll 问一次「授权好了没」。
//
// 三种结果，与 channel.LoginSession 的约定一一对应：
//   - 还没完成 → channel.ErrPending（控制台继续轮询，这是正常态）；
//   - 完成 → 凭证（当场验过）；
//   - 失败 → 归一化的 errs.Error（必须带原因，红线一）。
func (s *deviceSession) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	pasted, done, deviceCode, expiresAt := s.pasted, s.done, s.deviceCode, s.expiresAt
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次授权已经完成过了").WithChannel(string(channel.Copilot))
	}

	var githubToken string
	if pasted != "" {
		githubToken = pasted
	} else {
		if time.Now().After(expiresAt) {
			return nil, errs.New(errs.SessionDead, "设备码已过期（约 15 分钟），请重新发起授权").
				WithChannel(string(channel.Copilot))
		}
		// 轮询节奏在这里兜住：控制台是固定节奏反复调 Poll 的（前端每 2 秒一次），
		// 而上游要求两次轮询至少隔 interval 秒。不拦的话 github.com 的设备码端点
		// 会被打快，上游回 slow_down 甚至限流 —— mock 测不出来，真上游才现形。
		// 交叉验证（2026-09）：BYOKEY 就是每轮先 `sleep(interval)` 再 poll
		// （crates/auth/src/flow/device_code.rs:100-101），与这里的门限一致。
		now := time.Now()
		s.mu.Lock()
		if now.Before(s.nextPoll) {
			s.mu.Unlock()
			return nil, channel.ErrPending // 还没到点：不碰上游，对控制台仍是「等」
		}
		s.nextPoll = now.Add(s.interval)
		s.mu.Unlock()

		out, status, raw, err := s.a.pollAccessToken(ctx, deviceCode)
		if err != nil {
			return nil, err
		}
		switch {
		case strings.TrimSpace(out.AccessToken) != "":
			githubToken = out.AccessToken
		case out.Error == "authorization_pending":
			// 用户还没在浏览器里点完。
			return nil, channel.ErrPending
		case out.Error == "slow_down":
			// 上游明确要求放慢：把间隔永久 +5 秒并重新计时（RFC 8628 §3.5；gpt4free 同）。
			s.mu.Lock()
			s.interval += slowDownStep
			s.nextPoll = time.Now().Add(s.interval)
			s.mu.Unlock()
			return nil, channel.ErrPending
		case out.Error == "expired_token":
			return nil, errs.New(errs.SessionDead, "设备码已过期，请重新发起授权").
				WithChannel(string(channel.Copilot))
		case out.Error == "access_denied":
			return nil, errs.New(errs.AuthFailed, "你在 GitHub 授权页拒绝了这次授权").
				WithChannel(string(channel.Copilot))
		case out.Error == "incorrect_device_code":
			return nil, errs.New(errs.SessionDead, "GitHub 不认这个设备码（可能已经用过了），请重新发起授权").
				WithChannel(string(channel.Copilot))
		default:
			msg := out.ErrorDescription
			if msg == "" {
				msg = out.Error
			}
			if msg == "" {
				msg = truncate(string(raw), 200)
			}
			return nil, errs.New(s.a.Classify(status, raw), "换取 GitHub token 失败："+msg).
				WithChannel(string(channel.Copilot)).WithUpstream(truncate(string(raw), 200))
		}
	}

	cred, err := s.a.buildCredential(ctx, githubToken)
	if err != nil {
		// 组装/校验失败时**不置 done**：粘错 token 的用户可以改一个再试；
		// 设备码路径下这多半是「账号没开通 Copilot」，报错已带原因。
		return nil, err
	}
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	return cred, nil
}

// Poll 的辅助：发一次轮询请求并解析。
func (a *Adapter) pollAccessToken(ctx context.Context, deviceCode string) (accessTokenResp, int, []byte, error) {
	body, _ := json.Marshal(map[string]any{
		"client_id":   clientID,
		"device_code": deviceCode,
		"grant_type":  deviceGrant,
	})
	status, raw, err := a.do(ctx, nil, http.MethodPost, a.githubBase+epAccessToken, stdHeaders(), body)
	if err != nil {
		return accessTokenResp{}, 0, nil, errs.New(errs.Transport, "轮询 GitHub 令牌失败").
			WithChannel(string(channel.Copilot)).WithCause(err)
	}
	var out accessTokenResp
	_ = json.Unmarshal(raw, &out)
	return out, status, raw, nil
}

// Cancel 释放这次授权。必须可重复调用（控制台在结束、超时、取消三条路上都会调它）。
func (s *deviceSession) Cancel() {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
}
