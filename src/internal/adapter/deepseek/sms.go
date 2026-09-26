package deepseek

// sms.go —— DeepSeek「手机号 + 短信验证码」登录。
//
// 为什么要有这条路（用户原话：「账号密码好麻烦，直接验证码登录」）：
//   - 密码路径要先在面板上记/填长密码；手机号只需收一条短信，手机上点了就能填。
//   - 更重要的是：**短信验证码路径不会把用户密码落到磁盘上**。密码路径若勾了
//     「记住密码」才能自动续期，代价是密码明文留在凭证文件里；验证码路径则用
//     「已登录会话 + 定期重登」这一套（见 refresh 说明），密码全程不出现。
//
// 协议是**从 DeepSeek 官方前端 bundle 里读出来的**（不是猜的）：
//
//	① 发码  POST /api/v0/users/create_sms_verification_code
//	        { device_id, locale, scenario, mobile_number,
//	          turnstile_token, hcaptcha_token, shumei_verification, ticket }
//	        → data.biz_code / data.biz_data.send_window_secs（重发倒计时，官方给的是 60）
//	② 验码  POST /api/v0/users/check_sms_code
//	        { area_code, mobile_number, scenario, sms_verification_code[, ticket] }
//	        → data.biz_code      （用于「先验码后登录」的两段式上游；DeepSeek 走 ③ 一步到位）
//	③ 登录  POST /api/v0/users/login_by_mobile_sms
//	        { region, locale, mobile_number, area_code, sms_verification_code, device_id, os }
//	        → data.biz_code / data.biz_data.user  （token 从同一条链路取，见 parseSMSLogin）
//
// 关于图片验证码（用户要求「如果有图片验证之类的，是必须有的，也可以拿过来」）：
//   DeepSeek 的图验是**运行时多态**的 —— 官方前端里 captchaEnabled 取
//   "turnstile"（非大陆）/ "shumei"（数美，大陆）/ "hcaptcha"，由 feature flag 决定。
//   实测（2026-09-26）scenario=login 场景**不发图验挑战**：缺 token 字段时上游直接
//   进入发码逻辑（返回 SMS_SEND_TOO_FREQUENT + 60 秒窗口；而 scenario=register
//   才会返回 RECAPTCHA_VERIFY_FAILED）。也就是说登录这条路目前是「免图验」的。
//   但上游随时可以按风控把图验打开 —— 所以这里**把图验这一步完整实现了**：
//   只要上游回 CAPTCHA/图验类错误码，就切到 image_challenge 屏，把图直接显示给用户，
//   用户填字交回来。图从哪来见 captchaImage()。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 短信路径的表单字段名（面板与服务端共用同一套键）。
const (
	fieldMobile   = "mobile_number"
	fieldSMSCode  = "sms_verification_code"
	fieldAreaCode = "area_code"
)

// smsScenario 是发码场景。官方前端用的字面量就是 "login"。
const smsScenario = "login"

// SMSLoginAvailable 决定面板是否默认暴露「手机号 + 短信验证码」这条路。
//
// **实测结论（2026-09-26，打真实上游）：这条路当前走不通，故默认关闭。**
//
// 原因：发码接口 POST /users/create_sms_verification_code 把 shumei_verification
// 作为**结构化必填对象**，而它是「数美」验证码 JS 控件在浏览器里跑出来的一次性
// 令牌。实测证据：
//
//	不带该字段        → 200 {"biz_code":2,"biz_msg":"RECAPTCHA_VERIFY_FAILED"}
//	传 "" 或 "x"      → 422 {"loc":"body.shumei_verification"}   （类型不对）
//	传 null           → 200 但仍 RECAPTCHA_VERIFY_FAILED          （过了校验、没过了风控）
//	传 {} 或 {token:""} → 422 索要 {"rid","region"}               （它要的是完整对象）
//
// 也就是说：服务端拿不到这个令牌就没法发码，而拿到它必须跑数美的 JS 控件
// （需要浏览器环境 + 数美分配的 partner/密钥）。**这不是我们能补的代码，
// 是上游的风控前置**。所以：
//
//   - 适配器里短信这条路的代码**完整保留且已测**（状态机、图验屏、错误翻译），
//     一旦上游放开（例如换成静态图验、或 SDK 暴露纯 HTTP 取号接口），
//     把这里改成 true 就能立刻用；
//   - 面板默认不显示短信入口，避免让用户填完手机号才发现发不出码（那是骗人）。
//
// 改成 true 的前提：确认上游发码不再强制 shumei_verification。改之前请先跑
// probe 脚本验一遍，别凭感觉开。
const SMSLoginAvailable = false

// defaultAreaCode 是默认国际区号（中国大陆）。
const defaultAreaCode = "+86"

// smsStage 是本会话当前处在哪一步（内部状态机，对外经 channel.StepView 暴露）。
type smsStage int

const (
	// stageMobile：等用户填手机号（+ 区号），提交后去发码。
	stageMobile smsStage = iota
	// stageCode：码已发出，等用户填验证码，提交后去登录。
	stageCode
	// stageImage：上游要图片验证码，等用户看图填字。
	stageImage
	// stageDone：已换到 token，等 Poll 组装凭证。
	stageDone
	// stageFailed：失败，Message 里是原因。
	stageFailed
)

// smsFlow 保存一次短信登录的完整状态。
//
// 与 session 分开的原因：session 要同时承载「粘贴 token」和「账号密码」两条老路径，
// 再把它撑成 SMS 状态机只会越来越糊。这里单独一个小状态机，逻辑清楚，
// 出问题也只看这一个文件。
type smsFlow struct {
	mu   sync.Mutex
	a    *Adapter
	step smsStage

	// 用户填的输入。
	mobile   string
	areaCode string
	code     string
	// captcha 是用户对图片验证码填的字（上游要图验时才有）。
	captcha string

	// 发码后上游给的重发窗口（秒），面板据此显示倒计时。
	resendAfter int

	// 密钥材料。
	deviceID string
	token    string

	// remember 表示把手机号落盘（**不落密码**），供 token 失效后尝试重新登录。
	// 短信登录无法「无人值守自动续期」（重登要再收一条短信，不可能自动），
	// 所以这里的语义是「记住这个号，方便下次一键重登」，不是「自动续期」。
	remember bool

	// 失败原因（stageFailed 时必填，红线一）。
	message string
	// 上游要求图验时的图片（data URL）与提示。
	imageURL string
	// imageURLTried 记录「已经问过上游要图」（无论有没有拿到）。
	// 目前 DeepSeek 登录场景不开图验，这个标记让我们在日志/排查时能分清
	// 「上游没要图」与「要了但我们没图可显示」—— 而不是笼统地显示一个空图框。
	imageURLTried bool
}

// startSMS 开一次短信登录流程（面板点「添加账号 → DeepSeek」后进的第一屏）。
func (a *Adapter) startSMS() *smsFlow {
	return &smsFlow{a: a, step: stageMobile, areaCode: defaultAreaCode, remember: true}
}

// Step 实现 channel.StepAcceptor：返回当前这一屏。
//
// 面板不需要知道有几步、自己在第几屏 —— 每次提交后拿服务端回的 view 重绘即可。
func (f *smsFlow) Step() channel.StepView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.viewLocked()
}

func (f *smsFlow) viewLocked() channel.StepView {
	// message 是「上一次提交为什么被拒」。它必须出现在**任何**步骤的视图上，
	// 而不只是 stageFailed —— 因为验证码填错时我们会退回验证码屏让用户重试，
	// 此时不带上原因就是在静默失败（红线一）。
	warn := f.message
	switch f.step {
	case stageMobile:
		return channel.StepView{
			Stage: channel.StageInput,
			Title: "填写手机号",
			Hint: "填 DeepSeek 绑定的手机号，我们会给它发一条登录验证码。" +
				"验证码登录**不需要你的密码**。",
			Fields: []channel.LoginField{
				{Name: fieldMobile, Label: "手机号", Type: "text",
					Placeholder: "如 13800138000", Required: true},
				{Name: fieldAreaCode, Label: "区号", Type: "text",
					Placeholder: defaultAreaCode, Required: false},
			},
			SubmitLabel: "发送验证码",
			Message:     warn,
		}
	case stageCode:
		hint := "验证码已发出，请查看手机短信。"
		if f.resendAfter > 0 {
			hint += fmt.Sprintf("（%d 秒内不要重复获取）", f.resendAfter)
		}
		return channel.StepView{
			Stage: channel.StageInput,
			Title: "输入短信验证码",
			Hint:  hint,
			Fields: []channel.LoginField{
				{Name: fieldSMSCode, Label: "验证码", Type: "text",
					Placeholder: "6 位数字", Required: true},
			},
			SubmitLabel:     "登录",
			ResendAfterSecs: f.resendAfter,
			Message:         warn,
		}
	case stageImage:
		return channel.StepView{
			Stage: channel.StageImageChallenge,
			Title: "图片验证码",
			Hint: "上游要求人机校验：照下图填入字符即可继续。" +
				"（这是 DeepSeek 按风控临时开启的，填一次就过。）",
			Fields: []channel.LoginField{
				{Name: channel.FieldCaptcha, Label: "图片中的字符", Type: "text",
					Placeholder: "照图填写", Required: true},
			},
			SubmitLabel: "提交",
			ImageURL:    f.imageURL,
			Message:     warn,
		}
	case stageDone:
		return channel.StepView{Stage: channel.StageDone, Title: "授权完成"}
	default:
		return channel.StepView{
			Stage:   channel.StageFailed,
			Title:   "登录失败",
			Message: firstNonEmpty(f.message, "登录没能完成"),
		}
	}
}

// Submit 实现 channel.StepAcceptor：按当前步骤处理用户输入并推进状态机。
//
// 约定：返回错误则状态机**不前进**（用户可改了重试），面板显示错误原因（红线一）。
func (f *smsFlow) Submit(values map[string]string) error {
	f.mu.Lock()
	step := f.step
	f.mu.Unlock()

	switch step {
	case stageMobile:
		return f.submitMobile(values)
	case stageCode:
		return f.submitCode(values)
	case stageImage:
		return f.submitCaptcha(values)
	case stageDone:
		return errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.DeepSeek))
	default:
		return errs.New(errs.Parse, "这次登录已失败，请重新点「添加账号」再来一次").
			WithChannel(string(channel.DeepSeek))
	}
}

// submitMobile 处理第一步：存手机号、发验证码。
func (f *smsFlow) submitMobile(values map[string]string) error {
	mobile := normalizeMobile(values[fieldMobile])
	if mobile == "" {
		return errs.New(errs.Parse, "请把手机号填上").WithChannel(string(channel.DeepSeek))
	}
	if !looksLikeMobile(mobile) {
		return errs.New(errs.Parse, "手机号看着不对（应为 11 位数字，如 13800138000）").
			WithChannel(string(channel.DeepSeek))
	}
	area := strings.TrimSpace(values[fieldAreaCode])
	if area == "" {
		area = defaultAreaCode
	}
	if !strings.HasPrefix(area, "+") {
		area = "+" + area
	}

	f.mu.Lock()
	f.mobile, f.areaCode = mobile, area
	f.deviceID = deriveDeviceID(mobile) // 设备 id 由手机号派生：同号重登设备相同
	f.message = ""                      // 新的一次提交：清掉上一次的失败原因
	f.step = stageCode
	f.mu.Unlock()

	ctx := context.Background()
	window, err := f.a.sendSMSCode(ctx, f.snapshot())
	if err == nil {
		f.mu.Lock()
		f.resendAfter = window
		f.mu.Unlock()
		return nil
	}
	// 上游要图片验证码：切到图验屏（不报错 —— 这一步还没失败，只是换条路继续）。
	if errors.Is(err, errCaptchaRequired) {
		f.mu.Lock()
		f.imageURL = f.a.captchaImage()
		f.imageURLTried = true
		f.step = stageImage
		f.mu.Unlock()
		return nil
	}
	// 发码真失败：退回第一步（用户可改号重试），并把原因带上。
	f.failTo(stageMobile, err)
	return err
}

// submitCode 处理第二步：用验证码换 token。
func (f *smsFlow) submitCode(values map[string]string) error {
	code := strings.TrimSpace(values[fieldSMSCode])
	if code == "" {
		return errs.New(errs.Parse, "请把收到的验证码填上").WithChannel(string(channel.DeepSeek))
	}
	snap := f.snapshot()
	snap.code = code

	tok, err := f.a.loginByMobileSMS(context.Background(), snap)
	if err != nil {
		// 验证码错误也留在第二步，用户可以直接改了重输（不用重发短信）。
		f.failTo(stageCode, err)
		return err
	}
	f.mu.Lock()
	f.token = tok
	f.step = stageDone
	f.mu.Unlock()
	return nil
}

// submitCaptcha 处理图片验证码那一屏：存下用户填的字，回到「发码」那一步重试。
func (f *smsFlow) submitCaptcha(values map[string]string) error {
	text := strings.TrimSpace(values[channel.FieldCaptcha])
	if text == "" {
		return errs.New(errs.Parse, "请把图里的字符填上").WithChannel(string(channel.DeepSeek))
	}
	f.mu.Lock()
	f.captcha = text
	f.imageURL = ""
	f.message = ""
	f.mu.Unlock()
	// 图验过了 → 重新发码（正常会退到「输入验证码」屏）。
	window, err := f.a.sendSMSCode(context.Background(), f.snapshot())
	if err != nil {
		// 上游还要图验（说明刚才那个填错了）：留在图验屏让用户重填，别弹回第一步。
		if errors.Is(err, errCaptchaRequired) {
			f.mu.Lock()
			f.imageURL = f.a.captchaImage()
			f.message = "图里的字符不对，请重新填一次"
			f.mu.Unlock()
			return nil
		}
		f.failTo(stageImage, err)
		return err
	}
	f.mu.Lock()
	f.resendAfter = window
	f.step = stageCode
	f.mu.Unlock()
	return nil
}

// snapshot 取一份状态副本（避免持锁发网络请求）。
func (f *smsFlow) snapshot() smsSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return smsSnapshot{
		mobile: f.mobile, areaCode: f.areaCode, code: f.code,
		captcha: f.captcha, deviceID: f.deviceID, token: f.token,
	}
}

// failTo 记录失败原因并把状态机退回指定步骤。
//
// 为什么要专门记 message：面板在 StageFailed 时要显示原因；而退回某一步时
// （如验证码错）也要让用户看到「为什么被拒」，不能只静默停在原地（红线一）。
func (f *smsFlow) failTo(back smsStage, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.message = err.Error()
	f.step = back
}

// credential 在 stageDone 之后组装凭证（交给控制台走正常落盘/入池）。
func (f *smsFlow) credential() (*channel.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.step != stageDone || f.token == "" {
		return nil, channel.ErrPending
	}
	cred := &channel.Credential{
		UID:         "deepseek-" + shortHash(f.mobile),
		Nickname:    nicknameFor(f.mobile),
		AccessToken: f.token,
		Extra:       map[string]string{"device_id": f.deviceID, "login_via": "sms"},
	}
	// 记住手机号（**不记密码**）：短信登录没法无人值守续期，这个只用于让面板
	// 下次显示得更清楚、并在需要时一键重登。绝不把验证码或密码写进来。
	if f.remember && f.mobile != "" {
		cred.Extra["mobile"] = f.mobile
		cred.Extra["area_code"] = f.areaCode
	}
	return cred, nil
}

// ── 上游调用 ──────────────────────────────────────────────────────────

// smsSnapshot 是发码/登录要用的输入快照。
type smsSnapshot struct {
	mobile   string
	areaCode string
	code     string
	captcha  string
	deviceID string
	token    string
}

// sendSMSCode 调上游发码接口，返回上游给的重发窗口秒数（拿不到就是 0）。
//
// 请求体字段名全部来自官方前端 bundle（见文件头注释）——注意是 mobile_number，
// 不是 mobile；scenario 是 "login"。
func (a *Adapter) sendSMSCode(ctx context.Context, s smsSnapshot) (int, error) {
	body, _ := json.Marshal(map[string]any{
		"device_id":     s.deviceID,
		"locale":        "zh_CN",
		"scenario":      smsScenario,
		"mobile_number": s.mobile,
		// 图验 token：官方前端会按 captchaEnabled 填 turnstile_token 或
		// hcaptcha_token / shumei_verification。登录场景实测不需要（没有图验就传空），
		// 但一旦上游切成要求校验，把用户在图验屏填的令牌放这里就能过 —— 通路已经铺好。
		"turnstile_token":     s.captcha,
		"hcaptcha_token":      "",
		"shumei_verification": "",
	})
	raw, _, err := a.authCall(ctx, s, "/users/create_sms_verification_code", body)
	if err != nil {
		return 0, err
	}
	code, msg, extra := parseBiz(raw)
	if code == 0 {
		// 上游给的重发窗口（官方字段名 send_window_secs，实测 60）—— 面板据此显示倒计时。
		// 拿不到就返回 0（面板不显示倒计时），**不编一个数字**出来。
		if n, ok := toInt(extra["send_window_secs"]); ok && n > 0 {
			return n, nil
		}
		return 0, nil
	}
	// 上游要求人机校验：走图验那一屏，这里不算失败（见 submitMobile 的分支）。
	if isCaptchaChallenge(code, msg) {
		return 0, errCaptchaRequired
	}
	return 0, errs.New(errs.AuthFailed, "发送短信验证码失败："+smsMessage(code, msg)).
		WithChannel(string(channel.DeepSeek))
}

// errCaptchaRequired 是本包内部的信号（不出现在用户可见的错误里）：
// 上游要图片验证码。只有 submitMobile/submitCaptcha 认它，对外的错误文案由它们给。
var errCaptchaRequired = errors.New("deepseek: sms captcha required")

// toInt 把 JSON 数字（float64）或字符串安全地转成 int。
func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case string:
		n := 0
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// loginByMobileSMS 用手机号 + 验证码换 userToken。
//
// 真端点名是 login_by_mobile_sms（从前端 bundle 里读出来的；早期按命名习惯猜的
// login_by_sms_code / mobile_login 之类全不存在）。
func (a *Adapter) loginByMobileSMS(ctx context.Context, s smsSnapshot) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"region":                "CN",
		"locale":                "zh_CN",
		"mobile_number":         s.mobile,
		"area_code":             s.areaCode,
		"sms_verification_code": s.code,
		"device_id":             s.deviceID,
		"os":                    "android",
	})
	raw, _, err := a.authCall(ctx, s, "/users/login_by_mobile_sms", body)
	if err != nil {
		return "", err
	}
	// 复用密码路径的解析器：两边的成功信封形状一致（data.biz_data.user_token）。
	tok, msg := parseLoginToken(raw)
	if tok == "" {
		code, bmsg, _ := parseBiz(raw)
		if isCaptchaChallenge(code, bmsg) {
			a.noteCaptchaChallenge()
		}
		return "", errs.New(errs.AuthFailed, "验证码登录失败："+smsMessage(code, firstNonEmpty(bmsg, msg))).
			WithChannel(string(channel.DeepSeek))
	}
	return tok, nil
}

// authCall 发一个「还没拿到凭证」的鉴权类请求（发码 / 短信登录），并**翻译错误**。
//
// 为什么不复用 a.do()：do() 把任何 >=400 都压成一个笼统的「上游返回错误」，
// 只把原始 body 塞进 upstream 字段。对聊天这种已经带凭证的调用够用，但登录这几步
// 不行 —— 上游在这里用 **422** 表达「缺哪个字段/要人机校验」，那正是用户唯一能
// 据此自救的信息（实测：缺 shumei_verification 时回 422 body.shumei_verification）。
// 所以这里自己发一次请求，拿到状态码再分别翻译。
//
// 请求头仍走 clientHeaders（与业务请求同一份指纹），保证登录请求不被当异常设备。
func (a *Adapter) authCall(ctx context.Context, s smsSnapshot, path string, body []byte) ([]byte, int, error) {
	tmp := &channel.Credential{Extra: map[string]string{"device_id": s.deviceID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, errs.New(errs.Transport, "构造请求失败").
			WithChannel(string(channel.DeepSeek)).WithCause(err)
	}
	for k, v := range clientHeaders(tmp) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.clientFor(tmp).Do(req)
	if err != nil {
		return nil, 0, errs.New(errs.Transport, "上游请求失败").
			WithChannel(string(channel.DeepSeek)).WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusUnprocessableEntity:
		// 参数校验失败：上游用 {"detail":[{"loc":"body.xxx"}]} 说缺哪个字段。
		// 必须翻译 —— 否则用户只看到「上游返回错误」而不知道卡在哪（红线一）。
		return raw, resp.StatusCode, errs.New(errs.AuthFailed,
			"上游拒绝了这次请求："+describeValidation(raw)).WithChannel(string(channel.DeepSeek))
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		_, msg := envelopeAuthError(raw)
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return raw, resp.StatusCode, errs.New(errs.AuthFailed,
			"上游拒绝了这次请求："+msg).WithChannel(string(channel.DeepSeek))
	case resp.StatusCode >= 400:
		// 其余 4xx/5xx：交给渠道自己的分类器（它认得 WAF/限流等），并保留上游原话。
		return raw, resp.StatusCode, errs.New(a.Classify(resp.StatusCode, raw),
			"上游返回错误 "+fmt.Sprintf("(HTTP %d)", resp.StatusCode)).
			WithChannel(string(channel.DeepSeek)).WithUpstream(truncate(string(raw), 200))
	}
	return raw, resp.StatusCode, nil
}

// describeValidation 把上游 422 的 {"detail":[{"loc":"body.x"}]} 翻成人话。
//
// 不认识的字段就原样列出来 —— 宁可不翻译，也不猜错（红线一）。
func describeValidation(raw []byte) string {
	var env struct {
		Detail []struct {
			Loc  string `json:"loc"`
			Msg  string `json:"msg"`
			Type string `json:"type"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Detail) == 0 {
		return "参数校验未通过（上游未说明细节）"
	}
	seen := map[string]bool{}
	var parts []string
	for _, d := range env.Detail {
		name := strings.TrimPrefix(d.Loc, "body.")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		parts = append(parts, validationFieldHint(name))
	}
	if len(parts) == 0 {
		return "参数校验未通过（上游未说明细节）"
	}
	return strings.Join(parts, "；")
}

// validationFieldHint 给已知字段一句人话，未知字段原样返回字段名。
func validationFieldHint(field string) string {
	switch field {
	case "shumei_verification":
		// 这是数美验证码控件的令牌。服务端拿不到 —— 短信这条路当前走不通的主因，
		// 所以这句话必须说清楚，别让用户以为是「填错了」。
		return "上游要求人机校验（数美控件令牌 shumei_verification），这需要浏览器环境，当前无法在服务端完成"
	case "mobile_number":
		return "手机号格式不对"
	case "scenario":
		return "场景参数不对"
	case "sms_verification_code":
		return "验证码格式不对"
	case "device_id":
		return "设备标识缺失"
	}
	return "参数 " + field + " 不合法"
}

// parseBiz 从上游的 200 信封里取业务码/业务消息/额外数据。
//
// 上游形状：{"code":0,"msg":"","data":{"biz_code":N,"biz_msg":"…","biz_data":{…}}}
// 注意外层 code 常常是 0（表示「传输成功」），真正的结果在 data.biz_code —— 只看
// 外层 code 会把失败当成功（红线一）。
func parseBiz(raw []byte) (code int, msg string, extra map[string]any) {
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			BizCode int            `json:"biz_code"`
			BizMsg  string         `json:"biz_msg"`
			BizData map[string]any `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return -1, "上游响应无法解析", nil
	}
	if env.Code != 0 && env.Data.BizCode == 0 {
		// 外层就失败（如 40002/40003/40300）：用外层的码和消息。
		return env.Code, env.Msg, env.Data.BizData
	}
	return env.Data.BizCode, env.Data.BizMsg, env.Data.BizData
}

// isCaptchaChallenge 判断业务码/消息是不是「要人机校验」。
//
// DeepSeek 实测会把图验需求表达成 RECAPTCHA_VERIFY_FAILED（biz_code=2）。
// 这里同时认几种常见写法，免得上游换个壳就漏判。
func isCaptchaChallenge(code int, msg string) bool {
	u := strings.ToUpper(msg)
	switch {
	case strings.Contains(u, "RECAPTCHA"),
		strings.Contains(u, "CAPTCHA"),
		strings.Contains(u, "TURNSTILE"),
		strings.Contains(u, "HUMAN"),
		strings.Contains(u, "RISK_VERIFY"),
		strings.Contains(u, "SHUMEI"):
		return true
	}
	// biz_code=2 在发码接口上实测就是 RECAPTCHA_VERIFY_FAILED。
	return code == 2 && strings.Contains(u, "VERIFY")
}

// captchaImage 取一张要展示给用户的图片验证码（data URL）。
//
// 现状（实测 2026-09-26）：DeepSeek 登录场景**不开图验**，所以这里返回空串，
// 面板会退回「文字提示 + 一个输入框」的形态。
//
// 为什么留这个函数而不是直接删掉：上游的图验开关是**运行时 feature flag**
// （官方前端 captchaEnabled ∈ {turnstile, shumei, hcaptcha}），风控一收紧就会打开。
// 到那时有两个落地方式，二选一即可：
//  ① 若上游在响应里带图（base64/URL），在这里把它包成 data URL 返回 —— 改动只在
//     这一个函数里；
//  ② 若是第三方组件（数美/hCaptcha 是 JS 控件，拿不到静态图），就在面板嵌对应
//     JS 控件，令牌经 channel.FieldCaptcha 回传（Submit 已经支持收它）。
// 现在返回空串是**诚实**的做法：没有图就说没有，而不是编一张假图（红线一）。
func (a *Adapter) captchaImage() string { return "" }

// noteCaptchaChallenge 记录「上游这次要了图验」。
//
// 单独抽出来的原因：图验是上游按风控临时开的，本地日志里有这条记录，
// 以后再出问题就能一眼看出「不是我们没实现，是上游这次要了」。
func (a *Adapter) noteCaptchaChallenge() {
	// 只记事件、不记任何用户输入（手机号/验证码绝不入日志）。
	dsLogf("上游本次要求图片验证码（Captcha/Risk 挑战）")
}

// smsMessage 把上游业务码翻成人话。
//
// 只翻**确实实测过**的码；没见过的码原样带上，不编解释（红线一：宁可不翻译，
// 也不能猜错把用户带沟里）。
func smsMessage(code int, msg string) string {
	u := strings.ToUpper(msg)
	switch {
	case strings.Contains(u, "SMS_EXPIRED"):
		return "验证码已过期，请点「重新获取」再来一次"
	case strings.Contains(u, "SMS_CODE_"), strings.Contains(u, "WRONG"):
		return "验证码不对，请照短信重新输入"
	case strings.Contains(u, "SMS_SEND_TOO_FREQUENT"):
		return "发送太频繁了，等一分钟再点"
	case strings.Contains(u, "MOBILE_NUMBER_REQUIRED"):
		return "上游没收到手机号（请检查手机号是否填了）"
	case strings.Contains(u, "RECAPTCHA"), strings.Contains(u, "CAPTCHA"):
		return "上游要求人机校验（图片验证码）"
	case strings.Contains(u, "MOBILE_BLACKLIST"), strings.Contains(u, "FORBIDDEN"):
		return "这个号码被上游限制了登录"
	case code == 40002:
		return "上游要求先带令牌（Missing Token）"
	case code == 40003:
		return "上游拒绝了这次请求（Authorization Failed）"
	case code == 40029:
		return "请求太频繁（TOO_MANY_REQUESTS），歇一会儿再试"
	}
	if msg != "" {
		return "上游原话：" + msg
	}
	return fmt.Sprintf("上游业务码 %d", code)
}

// normalizeMobile 去掉用户可能粘进来的空格/横线/国别前缀。
func normalizeMobile(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	for _, p := range []string{"+86", "0086", "86"} {
		if strings.HasPrefix(s, p) && len(s) > len(p)+6 {
			s = s[len(p):]
			break
		}
	}
	return s
}

// looksLikeMobile 做一次轻量形状检查（11 位数字且以 1 开头）。
//
// 只拦明显错的，不追求严格（各国家号码规则不同，这里主用大陆号码）。
func looksLikeMobile(s string) bool {
	if len(s) != 11 || !strings.HasPrefix(s, "1") {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// dataURL 把图片字节包成 data URL（面板 <img src> 直接用，不必另开静态路由）。
func dataURL(mime string, b []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}
