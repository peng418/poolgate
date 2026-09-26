package deepseek

// login.go 「登录」有两条路，用户自己选：
//
//	① 账号密码直登（推荐）：面板上填一次邮箱/手机号 + 密码，服务端直接调
//	   POST /users/login 换 userToken。之后 token 过期时，只要当时选了「记住密码」，
//	   就能自动重新登录续期 —— 用户再也无需开浏览器、无需复制任何东西。
//
//	② 粘贴 userToken（原有方式，保留）：在自己的浏览器登录 DeepSeek，把
//	   localStorage 里的 userToken 粘回面板。不落盘密码，但 token 一过期就得重来。
//
// 为什么以前不做直登、现在做了：
//   旧注释担心「存密码的风险远大于多一点操作」。但那笔账漏算了网页版 token 的性质 ——
//   它是**短期会话令牌**（实测几小时即 40003 失效），且 Refresh 拿不到轮换（几乎恒为空），
//   于是「多一点的粘贴操作」实际变成了「每隔几小时手工重登一次」，不可用。
//   现在把选择权交给用户：想省事就给密码（本地加密落盘、面板不回显），
//   不想给就继续粘贴。两条路都能走通，不强迫。
//
// 设备指纹：密码登录接口同样要 device_id，且数美风控只在**异常设备**上触发。
// 由账号（而非 token）派生稳定设备 id —— 同一账号每次登录都是同一台「设备」，
// 既避免了「每次登录换设备」的风控特征，又保证不同账号互不相同。

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const loginPage = "https://chat.deepseek.com"

// 直登表单的字段名（面板与服务端共用同一套键）。
const (
	fieldAccount  = "account"
	fieldPassword = "password"
	fieldRemember = "remember"
)

type session struct {
	a  *Adapter
	mu sync.Mutex
	// flow 是短信验证码登录的状态机（非 nil 时走这条路，面板按 Step() 渲染每一步）。
	// 用户点「添加账号 → DeepSeek」默认就是这条路 —— 它是三条路里最省事的：
	// 不用开浏览器、不用复制 token，也不用输密码。
	flow *smsFlow
	// token 是用户粘回来（或直登换回）的 userToken；deviceID 由账号派生（每账号一个）。
	token    string
	deviceID string
	// 直登路径用：账号 + 密码 + 是否记住（记住才写进凭证的 Extra 供自动续期）。
	account  string
	password string
	remember bool
	// usedPaste 标记这次走的是粘贴路径（旧 UI 直接调 AcceptCallback）。
	usedPaste bool
	done      bool
}

// StartLogin 返回「去这儿登录」的地址（粘贴路径用），并按可用性决定默认路径。
//
// 路径优先级：
//
//	① 短信验证码（SMSLoginAvailable 时）—— 最省事：只需手机号 + 收一条短信，
//	   **密码全程不落盘**。中间那道数美人机校验由面板挂控件完成（见 sms.go），
//	   用户点一下就过。**这是当前主推**；
//	② 账号密码直登 —— 不想收短信时用；勾「记住密码」可自动续期，代价是密码留存；
//	③ 粘贴 userToken —— 既不想给密码、也不想收短信时的兜底。
func (a *Adapter) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	s := &session{a: a}
	if SMSLoginAvailable {
		s.flow = a.startSMS()
	}
	return s, nil
}

func (s *session) AuthURL() string { return loginPage }

// Hint 是给面板的粘贴引导语（粘贴路径）。实现后控制台在 /api/login/start 的
// 响应里带上 paste_hint，面板据此切换成「粘贴」形态。
//
// 文案必须诚实：短信这条路已经实测打通（含数美点选那一步），所以把它列为首选；
// 密码路径作为不想收短信时的备选。
func (s *session) Hint() string {
	return "推荐：填手机号 → 点一下人机校验题 → 收短信填验证码即可登录；这条路不会把你的密码存到磁盘上。\n" +
		"不想收短信：填 DeepSeek 的账号和密码点「直接登录」，勾上「记住密码」后 token 过期会自动重登。\n" +
		"或者：在浏览器登录 DeepSeek 后，于控制台执行 JSON.parse(localStorage.getItem(\"userToken\")).value，把结果整段粘到输入框里"
}

// LoginFields 声明直登表单的字段。控制台据此渲染「账号 / 密码」表单
// （实现 channel.PasswordAcceptor，红线三：核心层零渠道专有代码）。
func (s *session) LoginFields() []channel.LoginField {
	return []channel.LoginField{
		{Name: fieldAccount, Label: "邮箱或手机号", Type: "text",
			Placeholder: "DeepSeek 账号（邮箱 / 手机号）", Required: true},
		{Name: fieldPassword, Label: "密码", Type: "password",
			Placeholder: "DeepSeek 登录密码", Required: true},
		{Name: fieldRemember, Label: "记住密码（token 过期后自动重新登录续期）",
			Type: "text", Placeholder: "on", Required: false},
	}
}

// ── 短信验证码路径：把 StepAcceptor 转发给 flow ─────────────────────────
//
// 就是薄薄一层转发。这样 session 同时满足 channel.StepAcceptor（默认路径）、
// channel.PasswordAcceptor（可选：账号密码）与 channel.CallbackAcceptor（可选：
// 粘贴 token），控制台按「谁实现了什么」决定面板给哪些入口，核心层无需分支。

// Step 实现 channel.StepAcceptor：默认走短信流程；若用户已经改用「账号密码」或
// 「粘贴 token」，则不再暴露短信步骤（避免两套输入同时挂在一次授权上）。
func (s *session) Step() channel.StepView {
	s.mu.Lock()
	f := s.flow
	manual := s.account != "" || s.token != ""
	s.mu.Unlock()
	if manual || f == nil {
		return channel.StepView{Stage: channel.StageIdle}
	}
	return f.Step()
}

// Submit 实现 channel.StepAcceptor：转发给短信流程。
func (s *session) Submit(values map[string]string) error {
	s.mu.Lock()
	f := s.flow
	manual := s.account != "" || s.token != ""
	s.mu.Unlock()
	if manual {
		return errs.New(errs.Parse,
			"这次授权已经改用「账号密码 / 粘贴 token」了；要改用验证码登录请重新点「添加账号」").
			WithChannel(string(channel.DeepSeek))
	}
	if f == nil {
		return errs.New(errs.Parse, "这次授权没有短信验证码流程").
			WithChannel(string(channel.DeepSeek))
	}
	return f.Submit(values)
}

// AcceptPassword 收面板填的账号密码（直登路径）。
func (s *session) AcceptPassword(values map[string]string) error {
	acc := strings.TrimSpace(values[fieldAccount])
	pw := values[fieldPassword]
	if acc == "" || pw == "" {
		return errs.New(errs.Parse, "请把账号和密码都填上").WithChannel(string(channel.DeepSeek))
	}
	s.mu.Lock()
	s.account = acc
	s.password = pw
	s.remember = values[fieldRemember] == "on" || values[fieldRemember] == "true" || values[fieldRemember] == "1"
	s.usedPaste = false
	// 改用密码路径了：停掉短信流程，免得两条路同时挂在一次授权上。
	s.flow = nil
	s.mu.Unlock()
	return nil
}

// AcceptCallback 收用户粘回来的 userToken（裸串 / 带引号 / 整段 JSON 都认）。
func (s *session) AcceptCallback(raw string) error {
	tok := extractUserToken(raw)
	if tok == "" {
		return errs.New(errs.Parse,
			"没识别出 userToken：可以在上面直接填账号密码直登；或在浏览器登录 DeepSeek 后，于控制台执行 "+
				"`JSON.parse(localStorage.getItem(\"userToken\")).value`，把结果整段粘过来").
			WithChannel(string(channel.DeepSeek))
	}
	s.mu.Lock()
	s.token = tok
	s.usedPaste = true
	// 改用粘贴路径了：停掉短信流程，避免两条路同时挂在一次授权上。
	s.flow = nil
	// 粘贴路径不落盘密码：设备 id 由 token 派生（与旧行为一致，保证老凭证的 device_id 不变）。
	if s.deviceID == "" {
		s.deviceID = deriveDeviceID(tok)
	}
	s.mu.Unlock()
	return nil
}

// Poll 在用户填了表单 / 粘了 token / 走完短信流程之后完成凭证组装
// （并做一次轻量的有效性检查）。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	flow := s.flow
	tok, dev, acc, pw, remember, usedPaste, done := s.token, s.deviceID, s.account, s.password, s.remember, s.usedPaste, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.DeepSeek))
	}

	// 短信验证码路径：状态机走到 stageDone 才算数，否则一律「还在等」。
	// 这里用 ErrPending 表达等待，而不是报错 —— 面板仍在让用户填验证码。
	if flow != nil {
		cred, err := flow.credential()
		if err != nil {
			return nil, err // 未完成时就是 channel.ErrPending
		}
		tok = cred.AccessToken
		dev = cred.Extra["device_id"]
		// 短信路径的账号字段填手机号：后面 nickname / checkToken 都要用。
		acc = ""
		if err := s.a.checkToken(ctx, cred); err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.done = true
		s.mu.Unlock()
		return cred, nil
	}

	// 直登路径：有账号密码就先换 token（换完覆盖 s.token，供后续 checkToken）。
	if acc != "" && pw != "" {
		ntok, ndev, err := s.a.passwordLogin(ctx, acc, pw)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.token = ntok
		s.deviceID = ndev
		s.mu.Unlock()
		tok, dev = ntok, ndev
		usedPaste = false
	}

	if tok == "" {
		return nil, channel.ErrPending // 还没填：正常的等待态
	}

	cred := &channel.Credential{
		UID:         "deepseek-" + shortHash(accountSeed(acc, tok)),
		Nickname:    nicknameFor(acc),
		AccessToken: tok,
		Extra:       map[string]string{"device_id": dev},
	}
	// 直登 + 记住密码：把账号密码写进 Extra，供 token 过期后自动重新登录续期。
	// 粘贴路径不写（用户明确选择不落盘密码）。
	if !usedPaste && acc != "" && remember {
		cred.Extra["account"] = acc
		cred.Extra["password"] = pw
	}

	// 用 check_device 做一次「这串 token 还能用吗」的确认：不能就当场报错，
	// 免得把一个已失效的凭证塞进池子（那会变成「装上就报 401」）。
	if err := s.a.checkToken(ctx, cred); err != nil {
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

// ── 直登：换 token ─────────────────────────────────────────────

// passwordLogin 调 POST /users/login 用账号密码换 userToken。
//
// 实测（2026-09-26）该接口要求 body 至少含 device_id / os / password，
// 账号字段既认 email 也认 mobile；成功返回 data.biz_data.user_token
// （字段名在不同版本间有过变化，这里做宽容解析）。
func (a *Adapter) passwordLogin(ctx context.Context, account, password string) (token, deviceID string, err error) {
	deviceID = deriveDeviceID(account)
	body, _ := json.Marshal(map[string]any{
		"device_id": deviceID,
		"os":        "android",
		"email":     account,
		"mobile":    account,
		"password":  password,
	})
	// 直登请求还没有凭证，借用一次性凭证走同一套 do()（拿到一致的 UA / 头）。
	tmp := &channel.Credential{Extra: map[string]string{"device_id": deviceID}}
	resp, err := a.do(ctx, tmp, http.MethodPost, a.base+"/users/login", body, "")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	tok, msg := parseLoginToken(raw)
	if tok == "" {
		// 红线一：失败必须留证据。摘要是脱敏过的（token/密码/手机号换成 <已隐藏>）。
		dsLogf("账号密码直登失败：%s · 上游信封 %s", firstNonEmpty(msg, "上游未返回 token"),
			loginEnvelopeSummary(raw, 400))
		return "", "", errs.New(errs.AuthFailed,
			"账号密码直登失败："+firstNonEmpty(msg, "上游未返回 token")).
			WithChannel(string(channel.DeepSeek))
	}
	return tok, deviceID, nil
}

// parseLoginToken 从登录类响应里抽 userToken，并给出失败原因。
//
// **token 的位置是实机 + 官方 bundle 一起定出来的**：官方前端把登录响应映射成
// 用户对象的那个函数写着
//
//	{token: e.token, id: e.id, email: e.email, mobile: e.mobile || e.mobile_number || "", …}
//
// 也就是 **token 在 `data.biz_data.user.token`（用户对象内部）**，前端拿到后存起来
// 当 `Authorization: Bearer` 用（`ww(user)` 里 `user.token && setPersistedCurrentUser(...)`）。
//
// 踩过的坑：早先这里只读 `biz_data.user_token`（**那是照密码路径猜的**，不是抓包来的），
// 于是「手机号 + 短信验证码」登录在真机上永远失败 —— 上游其实把它当成功（见 sms.go
// 里对 biz_code 0/1 的处理），报出来的却是上游原话 `LOGIN_TO_EXISTING_ACCOUNT`，
// 看着像业务拒绝，实际是**我们把 token 漏掉了**。
//
// 三个位置按「官方确认过的在前」的顺序宽容解析，任一处给到即可。
func parseLoginToken(raw []byte) (token, msg string) {
	var out struct {
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
		BizCode int    `json:"biz_code"`
		Data    struct {
			BizCode int    `json:"biz_code"`
			BizMsg  string `json:"biz_msg"`
			BizData struct {
				UserToken string `json:"user_token"`
				Token     string `json:"token"`
				User      struct {
					Token string `json:"token"`
				} `json:"user"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "上游响应无法解析"
	}
	tok := firstNonEmpty(out.Data.BizData.User.Token, out.Data.BizData.UserToken, out.Data.BizData.Token)
	if tok != "" {
		return tok, ""
	}
	// 没拿到 token：把上游给的原因翻成人话。
	switch {
	case out.Data.BizCode == 2 || strings.Contains(strings.ToUpper(out.Data.BizMsg), "PASSWORD_OR_USER_NAME_IS_WRONG"):
		return "", "账号或密码不对（DeepSeek 原话：PASSWORD_OR_USER_NAME_IS_WRONG）"
	case strings.Contains(strings.ToUpper(out.Data.BizMsg), "RISK"):
		return "", "被风控拦截（DeepSeek 原话：" + out.Data.BizMsg + "）；请改用「粘贴 userToken」方式"
	}
	return "", firstNonEmpty(out.Data.BizMsg, out.Msg, fmt.Sprintf("上游业务码 %d", out.Data.BizCode))
}

// loginEnvelopeSummary 把上游登录信封压成一行**可进日志的摘要**（红线一：失败要留证据）。
//
// 为什么要它：真机上「上游为什么这么说」常常要靠响应的**结构**才能断（这轮就是靠
// `biz_data.user` 里有没有 token 才定案）。但信封里同时躺着 token / 手机号 /
// 验证码 —— 这些一律不能进日志。
//
// 做法：按 key 名脱敏（含 token/password/手机号/验证码的敏感键保留键名、值换成
// `[已隐藏]`），其余原样保留并限长，最后截断到 max 字节。宁可少记，不可泄露。
//
// 标记用方括号而不是尖括号：`json.Marshal` 会把 `<` `>` 转义成 `\u003c`/`\u003e`，
// 尖括号标记在日志里根本看不见（这条是跑测试才发现的）。
func loginEnvelopeSummary(raw []byte, max int) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		// 不是 JSON（HTML/WAF 页等）：只给长度和一个极短片段，避免把整页刷进日志。
		return fmt.Sprintf("<非 JSON 响应 %d 字节> %s", len(raw), truncate(string(raw), 80))
	}
	redactKeys := []string{"token", "password", "passwd", "secret",
		"sms_verification_code", "mobile", "phone", "authorization"}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			out := make(map[string]any, len(t))
			for k, val := range t {
				lk := strings.ToLower(k)
				if sensitiveKey(lk, redactKeys) {
					out[k] = "[已隐藏]"
					continue
				}
				out[k] = walk(val)
			}
			return out
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		case string:
			return truncate(t, 64)
		default:
			return x
		}
	}
	b, err := json.Marshal(walk(v))
	if err != nil {
		return "<信封无法序列化>"
	}
	return truncate(string(b), max)
}

// sensitiveKey 判断一个已小写的 key 是否属于「值不可入日志」的类别。
//
// 注意 biz_code 这类**状态码**要留住（它正是排查要看的），所以不按 "code" 一概过滤，
// 只有 sms_verification_code 这种一次性凭据才脱敏。
func sensitiveKey(lk string, redactKeys []string) bool {
	for _, r := range redactKeys {
		if strings.Contains(lk, r) {
			return true
		}
	}
	return false
}

// Refresh 尝试续期。
//
// 两条路：
//   - 凭证里存了账号密码（直登 + 记住密码）→ 直接重新登录换新 token（这是最可靠的续期）；
//   - 否则退回 check_device（网页端只在它返回 rotate 时才换 token，实测几乎恒为空）。
//
// 拿不到新 token 时返回 (nil, nil) —— 明确表示「刷不了」，让上层按 401 处理，
// 而不是假装成功（红线一）。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if c == nil {
		return nil, nil
	}
	// 直登可续期路径。
	if acc, pw := c.Extra["account"], c.Extra["password"]; acc != "" && pw != "" {
		tok, dev, err := a.passwordLogin(ctx, acc, pw)
		if err != nil {
			return nil, err
		}
		nc := *c
		nc.AccessToken = tok
		extra := map[string]string{}
		for k, v := range c.Extra {
			extra[k] = v
		}
		extra["device_id"] = dev
		nc.Extra = extra
		return &nc, nil
	}
	// 粘贴路径：只能看 check_device 有没有轮换。
	before := c.AccessToken
	nc := *c
	if err := a.checkToken(ctx, &nc); err != nil {
		return nil, err
	}
	if nc.AccessToken == before {
		return nil, nil
	}
	return &nc, nil
}

// checkToken 用 check_device 验证 token（粘贴路径唯一可能轮换 token 的地方）。
func (a *Adapter) checkToken(ctx context.Context, c *channel.Credential) error {
	body, _ := json.Marshal(map[string]any{"device_id": c.Extra["device_id"], "device_model": deviceModel})
	resp, err := a.do(ctx, c, http.MethodPost, a.base+"/users/auth_token/check_device", body, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Data struct {
			BizData struct {
				Rotate struct {
					Token string `json:"token"`
				} `json:"rotate"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// 解析不了不等于 token 坏：保守放行，交给第一次真正调用去暴露问题。
		return nil
	}
	// 上游若在信封里报了鉴权失败，这里就该拦下（别把一个坏 token 放进池子）。
	if code, msg := envelopeAuthError(raw); code != 0 {
		return errs.New(errs.SessionDead, "凭证校验失败："+msg).WithChannel(string(channel.DeepSeek))
	}
	if tok := strings.TrimSpace(out.Data.BizData.Rotate.Token); tok != "" {
		c.AccessToken = tok // 上游轮换就换成新的
	}
	return nil
}

// envelopeAuthError 从 200 信封里挑出鉴权类错误码（40002 缺 token / 40003 无效）。
// 返回 (0, "") 表示没有这类错误。
func envelopeAuthError(raw []byte) (int, string) {
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, ""
	}
	if env.Code == 40002 || env.Code == 40003 {
		return env.Code, firstNonEmpty(env.Msg, "Authorization Failed")
	}
	return 0, ""
}

// extractUserToken 从粘贴内容里抽 token：裸串 / "带引号" / {"value":"…"} 都认。
func extractUserToken(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "{") {
		var obj struct {
			Value string `json:"value"`
			Token string `json:"token"`
		}
		if json.Unmarshal([]byte(s), &obj) == nil {
			if v := firstNonEmpty(obj.Value, obj.Token); v != "" {
				s = v
			}
		}
	}
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	s = strings.TrimSpace(s)
	// userToken 是一串较长的 JWT（三段点分）或类似长度的不透明串；挡掉明显误粘的内容
	if len(s) < 20 || strings.ContainsAny(s, " \t\n") {
		return ""
	}
	return s
}

// accountSeed 决定账号 UID 的种子：直登用账号（重登后 UID 不变，可去重）；
// 粘贴路径用 token（与旧行为一致，老账号 UID 保持不变）。
func accountSeed(account, token string) string {
	if account != "" {
		return account
	}
	return token
}

// nicknameFor 给面板一个能认出来的展示名。
func nicknameFor(account string) string {
	if account == "" {
		return "DeepSeek 账号"
	}
	return "DeepSeek " + maskAccount(account)
}

// maskAccount 把账号打码显示（保留首尾，中间打星）—— 面板上一眼能认出是哪个号，
// 又不会把完整账号（含手机号）明着摆在页面上。
func maskAccount(account string) string {
	at := strings.Index(account, "@")
	if at > 0 { // 邮箱：保留前 2 位与域名
		name, domain := account[:at], account[at:]
		if len(name) > 2 {
			name = name[:2] + "***"
		}
		return name + domain
	}
	if len(account) >= 7 { // 手机号：保留前 3 后 2
		return account[:3] + "****" + account[len(account)-2:]
	}
	return account
}

// deriveDeviceID 由种子派生一个**稳定**的 RFC4122 v4 设备 UUID（FNV-1a 双哈希）。
//
// 为什么要派生而不是随机：设备 id 是上游用来区分「同一个客户端」的，每账号必须固定且互不相同
// （共用设备 id 会被判异常）；由账号/token 派生保证「同一个账号重登后还是同一个 id」，
// 而不同账号天然不同。
func deriveDeviceID(seed string) string {
	h1 := fnv.New64a()
	_, _ = h1.Write([]byte(seed))
	h2 := fnv.New64a()
	_, _ = h2.Write([]byte(seed + "#deepseek-device"))
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], h1.Sum64())
	binary.BigEndian.PutUint64(b[8:16], h2.Sum64())
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// shortHash 给账号一个稳定短标识（面板显示用，不泄露 token）。
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// 编译期断言：三条登录路径的能力都必须在。
//
//	① StepAcceptor     —— 手机号 + 短信验证码（默认路径，含图验那一屏）
//	② PasswordAcceptor —— 账号 + 密码（可选）
//	③ CallbackAcceptor —— 粘贴 userToken（可选兜底）
var (
	_ channel.StepAcceptor     = (*session)(nil)
	_ channel.PasswordAcceptor = (*session)(nil)
	_ channel.CallbackAcceptor = (*session)(nil)
)

// dsLogf 是本包统一的日志出口（带渠道前缀，便于在网关日志里筛）。
//
// 单独包一层的原因：这里要保证「日志里只有事件、没有用户输入」—— 手机号、
// 验证码、密码、token 一律不入日志。直接把 log.Printf 收在这里，审起来只这一处。
func dsLogf(format string, args ...any) {
	log.Printf("deepseek: "+format, args...)
}
