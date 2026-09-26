package deepseek

// sms_test.go —— 短信验证码登录的状态机与上游交互测试。
//
// 测什么：① 状态机步骤推进/回退是否正确（这是面板全靠它渲染的东西，错了面板就乱）
//         ② 上游业务码是否被正确翻译成人话（红线一：不静默、不瞎猜）
//         ③ 图验分支是否真的会切到 image_challenge 那一屏

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
)

// newTestAdapter 造一个把上游指到 httptest 服务、PoW 用桩解的适配器。
// 与 deepseek_test.go 里的写法一致（那里是内联的），这里抽成函数给本文件复用。
func newTestAdapter(t *testing.T, base string) *Adapter {
	t.Helper()
	a := New()
	a.base = base
	a.SetSolver(&stubSolver{})
	return a
}

// TestSMSCaptchaBranch 上游要图验时，状态机应当切到 image_challenge 那一屏。
//
// 这是用户明确要求的能力（「如果有图片验证之类的，是必须有的」）——
// 虽然 DeepSeek 登录场景目前不开图验，但上游是按风控随时能开的，
// 所以这条分支必须有测试锁住，不能等真被拦了才发现没实现。
func TestSMSCaptchaBranch(t *testing.T) {
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/create_sms_verification_code":
			attempt++
			if attempt == 1 {
				// 第一次：上游要人机校验。
				io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":2,"biz_msg":"RECAPTCHA_VERIFY_FAILED"}}`)
				return
			}
			// 第二次（用户填过图验后）：正常发码。
			io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"send_window_secs":60}}}`)
		case "/users/login_by_mobile_sms":
			io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"user_token":"tok-after-captcha-1234567890"}}}`)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()

	// 填手机号 → 上游要图验 → 应当落到 image_challenge 屏（而不是报错退出）。
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("要图验时不该直接报错（那只是换条路继续），实际：%v", err)
	}
	v := f.Step()
	if v.Stage != channel.StageImageChallenge {
		t.Fatalf("stage = %q, 想要 %q", v.Stage, channel.StageImageChallenge)
	}
	if len(v.Fields) != 1 || v.Fields[0].Name != channel.FieldCaptcha {
		t.Fatalf("图验屏字段应当用保留名 %q，实际：%+v", channel.FieldCaptcha, v.Fields)
	}
	if !v.Fields[0].Required {
		t.Fatal("图片验证码应当必填")
	}

	// 填图验字符 → 应当重新发码并前进到验证码屏。
	if err := f.Submit(map[string]string{channel.FieldCaptcha: "AB12"}); err != nil {
		t.Fatalf("提交图验失败：%v", err)
	}
	if v = f.Step(); v.Stage != channel.StageInput || v.Fields[0].Name != fieldSMSCode {
		t.Fatalf("过图验后应当到验证码屏，实际：stage=%q fields=%+v", v.Stage, v.Fields)
	}

	// 走完登录。
	if err := f.Submit(map[string]string{fieldSMSCode: "123456"}); err != nil {
		t.Fatalf("提交验证码失败：%v", err)
	}
	cred, err := f.credential()
	if err != nil {
		t.Fatalf("组装凭证失败：%v", err)
	}
	if cred.AccessToken != "tok-after-captcha-1234567890" {
		t.Fatalf("token = %q", cred.AccessToken)
	}
	if attempt != 2 {
		t.Fatalf("发码应当调了 2 次（图验前 + 图验后），实际 %d", attempt)
	}
}

// TestSMSCaptchaImageIsHonestWhenAbsent 上游要图验但我们拿不到图时，
// 视图里**如实**为空，不编一张假图（红线一）。
func TestSMSCaptchaImageIsHonestWhenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":2,"biz_msg":"RECAPTCHA_VERIFY_FAILED"}}`)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("要图验时不该报错，实际：%v", err)
	}
	v := f.Step()
	if v.Stage != channel.StageImageChallenge {
		t.Fatalf("stage = %q, 想要 image_challenge", v.Stage)
	}
	if v.ImageURL != "" {
		t.Fatalf("拿不到图时 ImageURL 应当为空串（诚实），实际 %q", v.ImageURL)
	}
	// 但用户仍然能填（有些上游是「图上没字、只需勾选」，保留输入口不至于卡死）。
	if len(v.Fields) == 0 {
		t.Fatal("图验屏至少要留一个输入口，不能把用户卡住")
	}
}

// TestSMSFlowStepProgression 走一遍正常流程：
// 起始屏（填手机号）→ 填手机号 → 验证码屏 → 填码 → 完成。
func TestSMSFlowStepProgression(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/users/create_sms_verification_code":
			// 发码成功：官方给的重发窗口是 60 秒。
			io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_msg":"","biz_data":{"send_window_secs":60}}}`)
		case "/users/login_by_mobile_sms":
			// 登录成功：token 在 data.biz_data.user_token（与密码路径同形）。
			io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_msg":"","biz_data":{"user_token":"tok-from-sms-abcdefghijklmnop"}}}`)
		case "/users/auth_token/check_device":
			io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{}}}`)
		default:
			t.Errorf("没预料到的请求路径 %s（body=%s）", r.URL.Path, body)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()

	// ① 起始屏：要手机号（+ 可选区号），按钮是「发送验证码」。
	v := f.Step()
	if v.Stage != channel.StageInput {
		t.Fatalf("起始 stage = %q, 想要 %q", v.Stage, channel.StageInput)
	}
	if v.SubmitLabel != "发送验证码" {
		t.Fatalf("起始按钮 = %q, 想要「发送验证码」", v.SubmitLabel)
	}
	if len(v.Fields) != 2 || v.Fields[0].Name != fieldMobile {
		t.Fatalf("起始字段不对：%+v", v.Fields)
	}
	if !v.Fields[0].Required {
		t.Fatal("手机号应当是必填")
	}

	// ② 填手机号 → 应当发码并前进到「输入验证码」屏。
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}
	v = f.Step()
	if v.Stage != channel.StageInput {
		t.Fatalf("发码后 stage = %q, 想要 %q", v.Stage, channel.StageInput)
	}
	if !strings.Contains(v.Title, "验证码") {
		t.Fatalf("发码后标题 = %q, 想要含「验证码」", v.Title)
	}
	if v.SubmitLabel != "登录" {
		t.Fatalf("验证码屏按钮 = %q, 想要「登录」", v.SubmitLabel)
	}
	if len(v.Fields) != 1 || v.Fields[0].Name != fieldSMSCode {
		t.Fatalf("验证码屏字段不对：%+v", v.Fields)
	}
	// 上游给的 60 秒重发窗口应当反映到视图上（面板据此显示倒计时）。
	if v.ResendAfterSecs != 60 {
		t.Fatalf("ResendAfterSecs = %d, 想要 60（上游给的值没带上来）", v.ResendAfterSecs)
	}

	// ③ 填验证码 → 应当换到 token 并标记完成。
	if err := f.Submit(map[string]string{fieldSMSCode: "123456"}); err != nil {
		t.Fatalf("提交验证码失败：%v", err)
	}
	if v = f.Step(); v.Stage != channel.StageDone {
		t.Fatalf("提交验证码后 stage = %q, 想要 %q（失败原因：%s）", v.Stage, channel.StageDone, v.Message)
	}

	// ④ credential 应当给出 token + 设备 id，并且**不落盘密码/验证码**。
	cred, err := f.credential()
	if err != nil {
		t.Fatalf("组装凭证失败：%v", err)
	}
	if cred.AccessToken != "tok-from-sms-abcdefghijklmnop" {
		t.Fatalf("token 不对：%q", cred.AccessToken)
	}
	if cred.Extra["device_id"] == "" {
		t.Fatal("device_id 应当被填上")
	}
	if cred.Extra["login_via"] != "sms" {
		t.Fatalf("login_via = %q, 想要 sms", cred.Extra["login_via"])
	}
	for _, leak := range []string{"password", "sms_verification_code", "code"} {
		if _, ok := cred.Extra[leak]; ok {
			t.Fatalf("凭证 Extra 里不该有 %q（短信路径不该把验证码/密码落盘）", leak)
		}
	}

	// ⑤ 上游调用序列正确：先发码，再登录。
	want := []string{"/users/create_sms_verification_code", "/users/login_by_mobile_sms"}
	if len(paths) != len(want) {
		t.Fatalf("调用序列 = %v, 想要 %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("调用序列 = %v, 想要 %v", paths, want)
		}
	}
}

// TestSMSSendPayloadUsesOfficialFieldNames 确认发码请求体的字段名是官方那一套。
//
// 为什么值得锁死：这套字段名是从 DeepSeek 官方前端 bundle 里读出来的，
// 早期按命名习惯猜的 mobile / phone_number 全不认（实测返回 MOBILE_NUMBER_REQUIRED）。
// 谁要是「顺手改得更顺眼」，这条测试会立刻拦下。
func TestSMSSendPayloadUsesOfficialFieldNames(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"send_window_secs":60}}}`)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}

	if got["mobile_number"] != "13800138000" {
		t.Fatalf("mobile_number = %v, 想要 13800138000（官方字段名）", got["mobile_number"])
	}
	if got["scenario"] != "login" {
		t.Fatalf("scenario = %v, 想要 login（register 会触发图验）", got["scenario"])
	}
	if got["device_id"] == "" || got["device_id"] == nil {
		t.Fatal("device_id 必须带上")
	}
	// 不能出现猜错的老字段名。
	for _, wrong := range []string{"mobile", "phone_number", "phone"} {
		if _, ok := got[wrong]; ok {
			t.Fatalf("请求体里不该出现 %q（官方不认这个名）", wrong)
		}
	}
}

// TestSMSCodeErrorStaysOnSameStep 验证码错时：状态机**不前进**，且错误里带人话。
//
// 这条对应红线一：失败必须有原因、且用户能原地重试（不用重走一遍流程）。
func TestSMSCodeErrorStaysOnSameStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/create_sms_verification_code":
			io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"send_window_secs":60}}}`)
		case "/users/login_by_mobile_sms":
			io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":7,"biz_msg":"SMS_EXPIRED","biz_data":null}}`)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}

	err := f.Submit(map[string]string{fieldSMSCode: "000000"})
	if err == nil {
		t.Fatal("验证码过期时应当返回错误（红线一：不静默）")
	}
	if !strings.Contains(err.Error(), "过期") {
		t.Fatalf("错误里应当把 SMS_EXPIRED 翻成人话，实际：%v", err)
	}
	// 状态机必须还停在「输入验证码」那一屏，用户可直接改了重试。
	if v := f.Step(); v.Stage != channel.StageInput || v.Fields[0].Name != fieldSMSCode {
		t.Fatalf("失败后应当留在验证码屏，实际：stage=%q fields=%+v", v.Stage, v.Fields)
	}
	if !strings.Contains(f.Step().Message, "过期") {
		t.Fatalf("失败原因应当留在视图上给面板显示，实际：%q", f.Step().Message)
	}
}

// TestSMSMobileValidation 手机号明显不对时当场拦下（不发请求）。
func TestSMSMobileValidation(t *testing.T) {
	var called int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":0}}`)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	// 注意：带横线/空格的写法**应当被接受**（用户从通讯录粘出来的常常是这种），
	// 规范化后就是合法号码 —— 所以下面只列真正非法的形状。
	for _, bad := range []string{"", "abc", "12345", "1380013800", "23800138000", "1380013800a"} {
		f := a.startSMS()
		if err := f.Submit(map[string]string{fieldMobile: bad}); err == nil {
			t.Fatalf("手机号 %q 应当被拦下", bad)
		}
	}
	if called != 0 {
		t.Fatalf("手机号不合法时不该发请求，实际发了 %d 次", called)
	}

	// 带国别前缀/横线/空格的合法号码应当被规范化后通过。
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "+86 138-0013-8000"}); err != nil {
		t.Fatalf("带 +86 与横线的号码应当被接受，实际：%v", err)
	}
	if called != 1 {
		t.Fatalf("规范化后的号码应当发出 1 次请求，实际 %d", called)
	}
}

// TestSMSMessageTranslations 只翻实测过的业务码，没见过的原样带上。
func TestSMSMessageTranslations(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		want string
	}{
		{0, "SMS_EXPIRED", "过期"},
		{0, "SMS_SEND_TOO_FREQUENT", "太频繁"},
		{0, "MOBILE_NUMBER_REQUIRED", "没收到手机号"},
		{0, "RECAPTCHA_VERIFY_FAILED", "人机校验"},
		{0, "SOME_NEW_CODE_FROM_UPSTREAM", "SOME_NEW_CODE_FROM_UPSTREAM"}, // 不认识就原样带出
		{40003, "", "Authorization Failed"},
	}
	for _, c := range cases {
		got := smsMessage(c.code, c.msg)
		if !strings.Contains(got, c.want) {
			t.Errorf("smsMessage(%d, %q) = %q, 想要含 %q", c.code, c.msg, got, c.want)
		}
	}
}

// TestSMSNormalizeMobile / TestSMSLooksLikeMobile 纯函数边界。
func TestSMSNormalizeAndValidate(t *testing.T) {
	norm := map[string]string{
		" 13800138000 ":     "13800138000",
		"+8613800138000":    "13800138000",
		"0086 138-0013-8000": "13800138000",
	}
	for in, want := range norm {
		if got := normalizeMobile(in); got != want {
			t.Errorf("normalizeMobile(%q) = %q, 想要 %q", in, got, want)
		}
	}
	ok := []string{"13800138000", "19912345678"}
	for _, s := range ok {
		if !looksLikeMobile(s) {
			t.Errorf("looksLikeMobile(%q) 应当是 true", s)
		}
	}
	bad := []string{"", "1380013800", "23800138000", "1380013800a", "1380013800012"}
	for _, s := range bad {
		if looksLikeMobile(s) {
			t.Errorf("looksLikeMobile(%q) 应当是 false", s)
		}
	}
}

// TestParseBizDistinguishesOuterAndInnerCode 外层 code 与 data.biz_code 要分清。
//
// 上游形状是「HTTP 200 + 外层 code=0 + 真正的结果在 data.biz_code」——
// 只看外层就会把失败当成功（红线一）。这条把这个形状锁住。
func TestParseBizDistinguishesOuterAndInnerCode(t *testing.T) {
	// 内层失败（外层 0）：必须取内层码。
	code, msg, _ := parseBiz([]byte(`{"code":0,"msg":"","data":{"biz_code":7,"biz_msg":"SMS_EXPIRED"}}`))
	if code != 7 || msg != "SMS_EXPIRED" {
		t.Fatalf("内层失败时取到 (%d,%q), 想要 (7,SMS_EXPIRED)", code, msg)
	}
	// 外层失败（缺 token）：取外层码。
	code, msg, _ = parseBiz([]byte(`{"code":40002,"msg":"Missing Token","data":null}`))
	if code != 40002 || !strings.Contains(msg, "Missing Token") {
		t.Fatalf("外层失败时取到 (%d,%q), 想要 (40002,Missing Token)", code, msg)
	}
	// 成功。
	code, _, _ = parseBiz([]byte(`{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"send_window_secs":60}}}`))
	if code != 0 {
		t.Fatalf("成功时 code = %d, 想要 0", code)
	}
}

// TestIsCaptchaChallenge 图验判定：只认图验类错误，别把别的失败也当图验。
func TestIsCaptchaChallenge(t *testing.T) {
	yes := []string{"RECAPTCHA_VERIFY_FAILED", "CAPTCHA_REQUIRED", "TURNSTILE_FAILED", "SHUMEI_BLOCK"}
	for _, m := range yes {
		if !isCaptchaChallenge(2, m) {
			t.Errorf("isCaptchaChallenge(2,%q) 应当是 true", m)
		}
	}
	no := []string{"SMS_EXPIRED", "SMS_SEND_TOO_FREQUENT", "PASSWORD_OR_USER_NAME_IS_WRONG"}
	for _, m := range no {
		if isCaptchaChallenge(0, m) {
			t.Errorf("isCaptchaChallenge(0,%q) 应当是 false", m)
		}
	}
}

// TestSessionSMSPathIsGatedOff 确认默认**不开**短信入口。
//
// 为什么这条重要：短信路径实测被上游数美控件挡死（见 SMSLoginAvailable 的注释）。
// 如果谁无意间把开关打开、或改成默认走短信，用户就会填完手机号才发现发不出码 ——
// 那是骗人。这条测试把「默认关闭」锁住，改开关必须是有意识的行为。
func TestSessionSMSPathIsGatedOff(t *testing.T) {
	a := newTestAdapter(t, "http://127.0.0.1:1")
	sessIface, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	s, ok := sessIface.(*session)
	if !ok {
		t.Fatalf("StartLogin 返回了 %T, 想要 *session", sessIface)
	}
	if s.flow != nil {
		t.Fatal("默认不该开短信流程（上游数美控件挡着，开了就是让用户白填）")
	}
	// 面板拿到的是 idle（不显示任何验证码步骤屏），走的是账号密码/粘贴两条路。
	if v := s.Step(); v.Stage != channel.StageIdle {
		t.Fatalf("默认应当是 idle，实际 stage=%q", v.Stage)
	}
	// 但仍然实现了 StepAcceptor（能力在，只是没开）—— 面板据此知道该渠道支持多步。
	var _ channel.StepAcceptor = s
}

// TestSessionRoutesStepAndStopsOnManualPath 确认两条路不会同时挂在一次授权上。
//
// 注意：这里直接构造带 flow 的会话（模拟开关打开的情形），来验证「改用账号密码后
// 短信步骤必须停掉」—— 否则面板会同时挂着手机号表单和密码表单，用户不知道该填哪个。
func TestSessionRoutesStepAndStopsOnManualPath(t *testing.T) {
	a := newTestAdapter(t, "http://127.0.0.1:1") // 不真发请求，只看路由
	s := &session{a: a, flow: a.startSMS()}     // 显式打开短信流程
	// 短信流程在时：Step() 应当给出「填手机号」那一屏。
	if v := s.Step(); v.Stage != channel.StageInput || !strings.Contains(v.Title, "手机号") {
		t.Fatalf("短信流程应当是起始屏，实际：stage=%q title=%q", v.Stage, v.Title)
	}
	// 改用账号密码后，短信步骤应当消失（避免两套输入同时挂着）。
	if err := s.AcceptPassword(map[string]string{fieldAccount: "a@b.com", fieldPassword: "pw"}); err != nil {
		t.Fatalf("AcceptPassword 失败：%v", err)
	}
	if v := s.Step(); v.Stage != channel.StageIdle {
		t.Fatalf("改用账号密码后短信步骤应当停止，实际 stage=%q", v.Stage)
	}
	if err := s.Submit(map[string]string{fieldMobile: "13800138000"}); err == nil {
		t.Fatal("改用账号密码后再交短信步骤应当报错（不该静默混用两条路）")
	}
}

// TestAuthCallTranslatesValidationError 上游 422 必须翻成人话。
//
// 真机实测：发码接口对缺 shumei_verification 的请求回
// 422 {"detail":[{"loc":"body.shumei_verification"}]}。
// 不翻译的话用户只看到「上游返回错误」，完全不知道卡在哪 —— 这条锁住翻译。
func TestAuthCallTranslatesValidationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"detail":[{"loc":"body.shumei_verification","type":"value_error"}]}`)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()
	err := f.Submit(map[string]string{fieldMobile: "13800138000"})
	if err == nil {
		t.Fatal("上游 422 时应当返回错误（红线一：不静默）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "数美") {
		t.Fatalf("应当把 shumei_verification 翻成人话并点明是人机校验，实际：%v", err)
	}
	// 用户得知道「这不是我填错了」，而是环境做不到。
	if !strings.Contains(msg, "浏览器") {
		t.Fatalf("应当说明需要在浏览器环境完成，实际：%v", err)
	}
}

// TestDescribeValidationUnknownField 不认识的字段原样列出（不猜）。
func TestDescribeValidationUnknownField(t *testing.T) {
	got := describeValidation([]byte(`{"detail":[{"loc":"body.some_new_field"}]}`))
	if !strings.Contains(got, "some_new_field") {
		t.Fatalf("未知字段应当原样带出，实际 %q", got)
	}
	// 完全解析不了时也得给句话，不能空着。
	if got := describeValidation([]byte(`not json`)); got == "" {
		t.Fatal("解析失败时也要有说明，不能返回空串")
	}
	// 已知字段给人话。
	if got := describeValidation([]byte(`{"detail":[{"loc":"body.mobile_number"}]}`)); !strings.Contains(got, "手机号") {
		t.Fatalf("手机号字段应当翻译，实际 %q", got)
	}
}

// 编译期：三条能力都得在（与 login.go 的断言重复一次，但放在这里更显眼）。
var (
	_ channel.StepAcceptor     = (*smsFlow)(nil)
	_ channel.StepAcceptor     = (*session)(nil)
	_ channel.PasswordAcceptor = (*session)(nil)
	_ channel.CallbackAcceptor = (*session)(nil)
)
