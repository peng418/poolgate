package deepseek

// sms_test.go —— 短信验证码登录的状态机与上游交互测试。
//
// 测什么：① 状态机步骤推进/回退是否正确（面板全靠它渲染，错了面板就乱）
//         ② 上游业务码是否被正确翻译成人话（红线一：不静默、不瞎猜）
//         ③ 人机校验（数美控件）那一步是否真的按「挂控件 + 转发 rid」来走
//
// 关于 ③ 的来龙去脉：上一轮曾判定短信这条路「做不了」并关掉，理由是数美的
// shumei_verification 是浏览器里跑出来的令牌，服务端造不出来。真机复查发现结论错了 ——
// **不需要造，把数美控件挂到我们自己的面板里就行**（用户点选 → 控件给 rid → 我们转发）。
// 下面 TestSMSShumeiWidgetBranch / TestSMSSendCarriesShumeiVerification 把这个链路锁住，
// TestSessionSMSPathIsOn 锁住「开关是开着的」—— 谁要改回去，测试会先拦下。

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

// widgetResult 造一份面板回传的人机校验结果（与前端 submitStep 发的形状一致）。
func widgetResult(rid string) string {
	b, _ := json.Marshal(map[string]string{"region": "CN", "rid": rid})
	return string(b)
}

// sendOK 是发码成功的上游信封（官方给的重发窗口是 60 秒）。
const sendOK = `{"code":0,"msg":"","data":{"biz_code":0,"biz_msg":"","biz_data":{"send_window_secs":60}}}`

// loginOK 是「新手机号注册并登入」成功的上游信封 —— **形状照真实响应**：
// token 在 `data.biz_data.user.token`（用户对象内部），不是 `biz_data.user_token`。
// 依据：官方前端把整个登录响应交给用户映射函数，该函数取 `e.token` 当 userToken 存起来
// 供 `Authorization: Bearer` 使用。原先这里写的是 `{"user_token":...}` —— 那是**照密码
// 路径猜的**（夹具注释还写着「与密码路径同形」），于是真机上永远取不到 token。
const loginOK = `{"code":0,"msg":"","data":{"biz_code":0,"biz_msg":"","biz_data":{"user":{"id":1,"mobile_number":"13800138000","token":"tok-from-sms-abcdefghijklmnop","status":0}}}}`

// loginExistingOK 是「手机号已有账号、直接登入」成功的上游信封 —— 就是用户真机撞到的那一次：
// biz_code=1 / biz_msg=LOGIN_TO_EXISTING_ACCOUNT。这个码**不是失败**（官方前端在它下面记的
// 日志是「验证码登录成功」，与 biz_code=0 的「验证码注册成功」并列），所以这条也必须拿到 token。
const loginExistingOK = `{"code":0,"msg":"","data":{"biz_code":1,"biz_msg":"LOGIN_TO_EXISTING_ACCOUNT","biz_data":{"user":{"id":2,"mobile_number":"13800138000","token":"tok-existing-account-1234567890","status":0}}}}`

// guestChallengeOK 是游客 PoW 挑战的桩响应（形状与真上游一致，数值随便取）。
const guestChallengeOK = `{"code":0,"msg":"","data":{"biz_code":0,"biz_msg":"","biz_data":{"guest_challenge":{"algorithm":"DeepSeekHashV1","challenge":"3d7f80d6a5fe3a1aa05384d8599fb89df8438dcdf85c306e4ec2059888a0eb38","salt":"c19bfb47645d6f99e749","signature":"6529368e1b534e6a9679a513ae570a548ccdcb82f5b9be4eb25468ab9be2a149","difficulty":80000,"expire_at":1790426441746,"expire_after":300000,"target_path":"/v0/users/login_by_mobile_sms"}}}}`

// smsServer 起一个「发码必成功、登录必成功」的桩上游，并把收到的请求体/路径/请求头记下来。
//
// 注意 paths 记的是**全部**请求，包括登录类接口前面那一步取游客 PoW 挑战
// （/users/create_guest_challenge）—— 那是真实协议的一部分，不该在测试里藏起来。
func smsServer(t *testing.T) (*httptest.Server, *[]map[string]any, *[]string, *[]http.Header) {
	t.Helper()
	bodies := &[]map[string]any{}
	paths := &[]string{}
	hdrs := &[]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		*bodies = append(*bodies, m)
		*paths = append(*paths, r.URL.Path)
		*hdrs = append(*hdrs, r.Header.Clone())
		switch r.URL.Path {
		case "/users/create_guest_challenge":
			io.WriteString(w, guestChallengeOK)
		case "/users/create_sms_verification_code":
			io.WriteString(w, sendOK)
		case "/users/login_by_mobile_sms":
			io.WriteString(w, loginOK)
		case "/users/auth_token/check_device":
			io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{}}}`)
		default:
			t.Errorf("没预料到的请求路径 %s（body=%s）", r.URL.Path, raw)
			http.NotFound(w, r)
		}
	}))
	return srv, bodies, paths, hdrs
}

// bodyFor 取出某个路径对应的请求体（测试里不按下标硬取，免得插一步就全错位）。
func bodyFor(t *testing.T, paths *[]string, bodies *[]map[string]any, path string) map[string]any {
	t.Helper()
	for i, p := range *paths {
		if p == path {
			return (*bodies)[i]
		}
	}
	t.Fatalf("调用序列 %v 里没有 %s", *paths, path)
	return nil
}

// headerFor 取出某个路径对应的请求头。
func headerFor(t *testing.T, paths *[]string, hdrs *[]http.Header, path string) http.Header {
	t.Helper()
	for i, p := range *paths {
		if p == path {
			return (*hdrs)[i]
		}
	}
	t.Fatalf("调用序列 %v 里没有 %s", *paths, path)
	return nil
}

// TestSMSShumeiWidgetBranch 人机校验那一屏必须是「挂数美控件」，不是「显示一张图」。
//
// 这是本功能的核心：数美是「空间点选」交互题（点击坐标 + 数美服务器校验），
// 服务端既拿不到唯一正确答案、也造不出合法凭据，所以只能把控件原样挂到面板里，
// 把控件给的 rid 转发出去。这条测试把整条链路（控件屏 → rid → 发码）钉死。
func TestSMSShumeiWidgetBranch(t *testing.T) {
	srv, bodies, paths, hdrs := smsServer(t)
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()

	// ① 填手机号：**不该**打上游（上游要求先有校验凭据才肯发码，先试一次是白跑）。
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号不该报错，实际：%v", err)
	}
	if len(*paths) != 0 {
		t.Fatalf("填完手机号不该先打上游，实际调了 %v", *paths)
	}

	// ② 应当落在「人机校验」屏，且声明的是数美控件。
	v := f.Step()
	if v.Stage != channel.StageWidget {
		t.Fatalf("stage = %q, 想要 %q", v.Stage, channel.StageWidget)
	}
	if v.Widget != channel.WidgetShumei {
		t.Fatalf("widget = %q, 想要 %q", v.Widget, channel.WidgetShumei)
	}
	if v.ImageURL != "" {
		t.Fatalf("数美是交互控件、不是静态图，ImageURL 应当为空，实际 %q", v.ImageURL)
	}
	if len(v.Fields) != 0 {
		t.Fatalf("控件屏由前端挂载器渲染，不该带普通输入字段，实际：%+v", v.Fields)
	}

	// ③ 控件给结果前不许提交（否则就是让用户白点一次才知道没生效）。
	err := f.Submit(map[string]string{})
	if err == nil {
		t.Fatal("没有控件结果时提交应当报错（红线一）")
	}
	if !strings.Contains(err.Error(), "人机校验") {
		t.Fatalf("错误文案应当点明是「人机校验」还没做完，实际：%v", err)
	}
	if v = f.Step(); v.Stage != channel.StageWidget {
		t.Fatalf("空提交后应当仍停在控件屏，实际 stage=%q", v.Stage)
	}

	// ④ 交控件结果 → 应当带凭据去发码，并推进到「输入验证码」屏。
	rid := "2026092620051945f0f652c9a10a8884"
	if err := f.Submit(map[string]string{channel.FieldWidgetResult: widgetResult(rid)}); err != nil {
		t.Fatalf("提交控件结果失败：%v", err)
	}
	if v = f.Step(); v.Stage != channel.StageInput || v.Fields[0].Name != fieldSMSCode {
		t.Fatalf("过校验后应当到验证码屏，实际：stage=%q fields=%+v", v.Stage, v.Fields)
	}
	if !equalPaths(*paths, []string{"/users/create_guest_challenge", "/users/create_sms_verification_code"}) {
		t.Fatalf("发码这一步的调用序列 = %v，想要先取游客 PoW 挑战再发码", *paths)
	}
	// 关键：发码请求里必须带着控件给的凭据。
	verify, ok := bodyFor(t, paths, bodies, "/users/create_sms_verification_code")["shumei_verification"].(map[string]any)
	if !ok {
		t.Fatalf("发码请求里 shumei_verification 应当是对象，实际 %#v",
			bodyFor(t, paths, bodies, "/users/create_sms_verification_code")["shumei_verification"])
	}
	// 也必须带上 PoW 头：不带的话上游 100% 回 40300「Missing Header」
	// （真机就是这么翻车的 —— 用户看到「验证码登录失败：上游原话：Missing Header」）。
	if h := headerFor(t, paths, hdrs, "/users/create_sms_verification_code").Get("X-DS-Guest-PoW-Response"); h == "" {
		t.Fatal("发码请求缺 X-DS-Guest-PoW-Response 头（上游会回 Missing Header）")
	}
	if verify["rid"] != rid {
		t.Fatalf("shumei_verification.rid = %v, 想要 %q", verify["rid"], rid)
	}
	if verify["region"] != "CN" {
		t.Fatalf("shumei_verification.region = %v, 想要 CN", verify["region"])
	}

	// ⑤ 走完登录。
	if err := f.Submit(map[string]string{fieldSMSCode: "123456"}); err != nil {
		t.Fatalf("提交验证码失败：%v", err)
	}
	cred, err := f.credential()
	if err != nil {
		t.Fatalf("组装凭证失败：%v", err)
	}
	if cred.AccessToken != "tok-from-sms-abcdefghijklmnop" {
		t.Fatalf("token = %q", cred.AccessToken)
	}
}

// TestSMSWidgetConfigIsComplete 控件屏必须把挂载数美所需的参数**全部**带出来。
//
// 为什么单列一条：这些值（organization / mode / 脚本地址）是 DeepSeek 自己的数美租户参数，
// 从前端 bundle 里读出来的；少一个前端就挂不起来，而失败现场只会是一句
// 「initSMCaptcha is not a function」，很难查。所以把契约锁在测试里。
func TestSMSWidgetConfigIsComplete(t *testing.T) {
	a := newTestAdapter(t, "http://127.0.0.1:1")
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}
	v := f.Step()
	if v.Stage != channel.StageWidget {
		t.Fatalf("stage = %q, 想要 widget", v.Stage)
	}
	if len(v.WidgetConfig) == 0 {
		t.Fatal("控件屏必须带 WidgetConfig，否则前端无从挂载")
	}
	var cfg map[string]string
	if err := json.Unmarshal(v.WidgetConfig, &cfg); err != nil {
		t.Fatalf("WidgetConfig 应当是合法 JSON 对象，实际 %s（%v）", v.WidgetConfig, err)
	}
	for _, k := range []string{"organization", "appId", "mode", "lang", "region", "script"} {
		if cfg[k] == "" {
			t.Errorf("WidgetConfig 缺 %q（前端会挂不起来）：%s", k, v.WidgetConfig)
		}
	}
	if cfg["mode"] != "spatial_select" {
		t.Errorf("mode = %q, 想要 spatial_select（点选题；换成 slide 就不是 DeepSeek 用的那种了）", cfg["mode"])
	}
	if cfg["region"] != "CN" {
		t.Errorf("region = %q, 想要 CN（它要原样填进 shumei_verification.region）", cfg["region"])
	}
	if !strings.Contains(cfg["script"], "smcp.min.js") {
		t.Errorf("script = %q, 想要数美控件 smcp.min.js 的地址", cfg["script"])
	}
}

// TestSMSSendCarriesShumeiVerification 发码请求体的字段名与凭据形状都要锁死。
//
// 为什么值得锁死：这套字段名是从 DeepSeek 官方前端 bundle 里读出来的 ——
// 早期按命名习惯猜的 mobile / phone_number 全不认（实测返回 MOBILE_NUMBER_REQUIRED）；
// 而 shumei_verification 的**形状**（{region, rid} 对象，不是字符串、不是 null）
// 也是实测出来的：传 null 过校验但被风控拦、传 {} 被 422 索要 rid/region。
func TestSMSSendCarriesShumeiVerification(t *testing.T) {
	srv, bodies, paths, _ := smsServer(t)
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}
	if err := f.Submit(map[string]string{channel.FieldWidgetResult: widgetResult("RID-XYZ")}); err != nil {
		t.Fatalf("提交控件结果失败：%v", err)
	}
	// 发码只该发一次（取 PoW 挑战那一步不算发码）。
	sent := 0
	for _, p := range *paths {
		if p == "/users/create_sms_verification_code" {
			sent++
		}
	}
	if sent != 1 {
		t.Fatalf("应当只发了一次码，实际 %d 次（调用序列 %v）", sent, *paths)
	}
	got := bodyFor(t, paths, bodies, "/users/create_sms_verification_code")

	if got["mobile_number"] != "13800138000" {
		t.Errorf("mobile_number = %v, 想要 13800138000（官方字段名）", got["mobile_number"])
	}
	if got["scenario"] != "login" {
		t.Errorf("scenario = %v, 想要 login", got["scenario"])
	}
	if got["locale"] != "zh_CN" {
		t.Errorf("locale = %v, 想要 zh_CN", got["locale"])
	}
	if got["device_id"] == "" || got["device_id"] == nil {
		t.Error("device_id 必须带上")
	}
	// 数美凭据必须是对象，且只认这两个键。
	if _, ok := got["shumei_verification"].(map[string]any); !ok {
		t.Fatalf("shumei_verification 必须是对象（传 null/字符串会被上游拒），实际 %#v", got["shumei_verification"])
	}
	// turnstile 只给非大陆用，大陆场景必须是空串 —— 填错会让上游按错误的分支校验。
	if got["turnstile_token"] != "" {
		t.Errorf("turnstile_token = %v, 想要空串（大陆场景用的是数美）", got["turnstile_token"])
	}
	// 不能出现猜错的老字段名。
	for _, wrong := range []string{"mobile", "phone_number", "phone"} {
		if _, ok := got[wrong]; ok {
			t.Errorf("请求体里不该出现 %q（官方不认这个名）", wrong)
		}
	}
}

// TestSMSSubmitWidgetResultValidation 控件结果不合法时当场拦下，不打上游。
func TestSMSSubmitWidgetResultValidation(t *testing.T) {
	srv, _, paths, _ := smsServer(t)
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"空白", "   ", "人机校验"},
		{"不是 JSON", "not-json", "重新点"},
		{"缺 rid", `{"region":"CN"}`, "rid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := a.startSMS()
			if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
				t.Fatalf("提交手机号失败：%v", err)
			}
			err := f.Submit(map[string]string{channel.FieldWidgetResult: c.value})
			if err == nil {
				t.Fatalf("非法控件结果 %q 应当被拦下", c.value)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误文案应当含 %q，实际：%v", c.want, err)
			}
			if v := f.Step(); v.Stage != channel.StageWidget {
				t.Fatalf("被拒后应当仍停在控件屏（用户重点一次即可），实际 stage=%q", v.Stage)
			}
		})
	}
	if len(*paths) != 0 {
		t.Fatalf("控件结果不合法时不该打上游，实际 %v", *paths)
	}
}

// TestSMSStaleCredentialStaysOnWidgetStep 上游不认这次凭据时：留在控件屏让用户重来，
// 而不是把他弹回开头重填手机号。
//
// 真实成因：rid 是一次性的（用过/过期就失效）。这种失败必须让用户**原地**再点一次题。
func TestSMSStaleCredentialStaysOnWidgetStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 上游说凭据不认（实测形状：200 + biz_code=2 RECAPTCHA_VERIFY_FAILED）。
		io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":2,"biz_msg":"RECAPTCHA_VERIFY_FAILED"}}`)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}
	// 凭据不认不算「提交失败」——状态机换一屏继续，所以不返回错误。
	if err := f.Submit(map[string]string{channel.FieldWidgetResult: widgetResult("RID-STALE")}); err != nil {
		t.Fatalf("凭据失效应当留在原屏重试、而不是报错退出，实际：%v", err)
	}
	v := f.Step()
	if v.Stage != channel.StageWidget {
		t.Fatalf("凭据失效后应当仍停在控件屏，实际 stage=%q", v.Stage)
	}
	if !strings.Contains(v.Message, "重新点") {
		t.Fatalf("必须告诉用户「重新点一次题目」，实际：%q", v.Message)
	}
}

// TestSMSFlowStepProgression 走一遍完整正常流程：
// 填手机号 → 人机校验 → 发码 → 验证码屏 → 填码 → 完成。
func TestSMSFlowStepProgression(t *testing.T) {
	srv, _, paths, _ := smsServer(t)
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()

	// ① 起始屏：要手机号（+ 可选区号）。
	v := f.Step()
	if v.Stage != channel.StageInput {
		t.Fatalf("起始 stage = %q, 想要 %q", v.Stage, channel.StageInput)
	}
	if v.SubmitLabel != "下一步" {
		t.Fatalf("起始按钮 = %q, 想要「下一步」（填完号才发码，不叫「发送验证码」）", v.SubmitLabel)
	}
	if len(v.Fields) != 2 || v.Fields[0].Name != fieldMobile {
		t.Fatalf("起始字段不对：%+v", v.Fields)
	}
	if !v.Fields[0].Required {
		t.Fatal("手机号应当是必填")
	}

	// ② 填手机号 → 应当进人机校验屏。
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}
	if v = f.Step(); v.Stage != channel.StageWidget {
		t.Fatalf("填号后 stage = %q, 想要 %q", v.Stage, channel.StageWidget)
	}

	// ③ 过校验 → 发码 → 验证码屏。
	if err := f.Submit(map[string]string{channel.FieldWidgetResult: widgetResult("RID-1")}); err != nil {
		t.Fatalf("提交控件结果失败：%v", err)
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

	// ④ 填验证码 → 应当换到 token 并标记完成。
	if err := f.Submit(map[string]string{fieldSMSCode: "123456"}); err != nil {
		t.Fatalf("提交验证码失败：%v", err)
	}
	if v = f.Step(); v.Stage != channel.StageDone {
		t.Fatalf("提交验证码后 stage = %q, 想要 %q（失败原因：%s）", v.Stage, channel.StageDone, v.Message)
	}

	// ⑤ credential 应当给出 token + 设备 id，并且**不落盘密码/验证码/rid**。
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
	for _, leak := range []string{"password", "sms_verification_code", "code", "rid", "captcha"} {
		if _, ok := cred.Extra[leak]; ok {
			t.Fatalf("凭证 Extra 里不该有 %q（短信路径不该把验证码/密码/校验凭据落盘）", leak)
		}
	}

	// ⑥ 上游调用序列正确：每打一个登录类接口，前面都要先取一次游客 PoW 挑战
	//    （挑战是「按接口 + 一次性」的，所以是 4 步而不是 2 步）。
	want := []string{
		"/users/create_guest_challenge", "/users/create_sms_verification_code",
		"/users/create_guest_challenge", "/users/login_by_mobile_sms",
	}
	if !equalPaths(*paths, want) {
		t.Fatalf("调用序列 = %v, 想要 %v", *paths, want)
	}
}

// TestSMSLoginExistingAccountGetsTokenFromUserObject 真机回归锁。
//
// 现场（2026-09-26，用户真机）：填完短信验证码点「登录」，面板显示
// 「验证码登录失败：上游原话：LOGIN_TO_EXISTING_ACCOUNT」—— 看着像上游拒绝，
// 其实上游当我们**成功**了（LOGIN_TO_EXISTING_ACCOUNT = 「这号已有账号，直接登入」），
// 是旧解析只找了 biz_data.user_token 而漏掉了 user 对象里的 token。
//
// 这条测试把「已有账号」这条真实路径钉死：必须拿到 token、必须走到 done。
func TestSMSLoginExistingAccountGetsTokenFromUserObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/create_guest_challenge":
			io.WriteString(w, guestChallengeOK)
		case "/users/create_sms_verification_code":
			io.WriteString(w, sendOK)
		case "/users/login_by_mobile_sms":
			io.WriteString(w, loginExistingOK)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}
	if err := f.Submit(map[string]string{channel.FieldWidgetResult: widgetResult("RID-1")}); err != nil {
		t.Fatalf("提交控件结果失败：%v", err)
	}
	if err := f.Submit(map[string]string{fieldSMSCode: "123456"}); err != nil {
		t.Fatalf("已有账号这条路必须被当成成功（真机就是这么挂的）：%v", err)
	}
	if v := f.Step(); v.Stage != channel.StageDone {
		t.Fatalf("stage = %q, 想要 %q（message=%s）", v.Stage, channel.StageDone, v.Message)
	}
	cred, err := f.credential()
	if err != nil {
		t.Fatalf("组装凭证失败：%v", err)
	}
	if cred.AccessToken != "tok-existing-account-1234567890" {
		t.Fatalf("token 必须取 data.biz_data.user.token，实际：%q", cred.AccessToken)
	}
}

// TestSMSCodeErrorStaysOnSameStep 验证码错时：状态机**不前进**，且错误里带人话。
//
// 这条对应红线一：失败必须有原因、且用户能原地重试（不用重走一遍流程）。
func TestSMSCodeErrorStaysOnSameStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/create_sms_verification_code":
			io.WriteString(w, sendOK)
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
	if err := f.Submit(map[string]string{channel.FieldWidgetResult: widgetResult("RID-1")}); err != nil {
		t.Fatalf("提交控件结果失败：%v", err)
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
	srv, _, paths, _ := smsServer(t)
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
	if len(*paths) != 0 {
		t.Fatalf("手机号不合法时不该发请求，实际发了 %v", *paths)
	}

	// 带国别前缀/横线/空格的合法号码应当被规范化后通过，并推进到人机校验屏。
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "+86 138-0013-8000"}); err != nil {
		t.Fatalf("带 +86 与横线的号码应当被接受，实际：%v", err)
	}
	if v := f.Step(); v.Stage != channel.StageWidget {
		t.Fatalf("合法号码应当推进到人机校验屏，实际 stage=%q", v.Stage)
	}
	if f.mobile != "13800138000" {
		t.Fatalf("规范化后的号码 = %q, 想要 13800138000", f.mobile)
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

// TestSMSNormalizeAndValidate 纯函数边界。
func TestSMSNormalizeAndValidate(t *testing.T) {
	norm := map[string]string{
		" 13800138000 ":      "13800138000",
		"+8613800138000":     "13800138000",
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
	code, _, _ = parseBiz([]byte(sendOK))
	if code != 0 {
		t.Fatalf("成功时 code = %d, 想要 0", code)
	}
}

// TestIsCaptchaChallenge 人机校验判定：只认校验类错误，别把别的失败也当校验。
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

// TestSessionSMSPathIsOn 确认短信入口是**开着**的，且第一屏就是「填手机号」。
//
// 这条与上一版的 TestSessionSMSPathIsGatedOff 正好相反 —— 上一版锁的是「默认关」，
// 理由是当时判定数美控件做不了。真机复查证明那条路走得通（把控件挂过来即可），
// 于是改成默认开。谁要再关回去，会先被这条测试拦下，必须是有意识的行为。
func TestSessionSMSPathIsOn(t *testing.T) {
	if !SMSLoginAvailable {
		t.Fatal("短信登录已实测可用（数美控件挂载），不该再关掉")
	}
	a := newTestAdapter(t, "http://127.0.0.1:1")
	sessIface, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("StartLogin 失败：%v", err)
	}
	s, ok := sessIface.(*session)
	if !ok {
		t.Fatalf("StartLogin 返回了 %T, 想要 *session", sessIface)
	}
	if s.flow == nil {
		t.Fatal("默认应当开短信流程（这是主推路径：密码不落盘）")
	}
	if v := s.Step(); v.Stage != channel.StageInput || !strings.Contains(v.Title, "手机号") {
		t.Fatalf("短信路径第一屏应当是「填手机号」，实际 stage=%q title=%q", v.Stage, v.Title)
	}
	// 能力断言：面板据此知道该渠道支持多步授权。
	var _ channel.StepAcceptor = s
}

// TestSessionRoutesStepAndStopsOnManualPath 确认两条路不会同时挂在一次授权上。
//
// 验证「改用账号密码后短信步骤必须停掉」—— 否则面板会同时挂着手机号表单和密码表单，
// 用户不知道该填哪个。
func TestSessionRoutesStepAndStopsOnManualPath(t *testing.T) {
	a := newTestAdapter(t, "http://127.0.0.1:1") // 不真发请求，只看路由
	s := &session{a: a, flow: a.startSMS()}
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
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}
	err := f.Submit(map[string]string{channel.FieldWidgetResult: widgetResult("RID-1")})
	if err == nil {
		t.Fatal("上游 422 时应当返回错误（红线一：不静默）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "数美") {
		t.Fatalf("应当把 shumei_verification 翻成人话并点明是人机校验，实际：%v", err)
	}
	// 用户得拿到一个能自己执行的动作：重新点一次题。
	if !strings.Contains(msg, "重新点") {
		t.Fatalf("应当给出可执行动作（重新点一次题目），实际：%v", err)
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

// equalPaths 比较两条调用序列（测试里统一用它，避免散落的 len+下标断言）。
func equalPaths(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestSMSLoginCarriesGuestPowHeader 短信登录那一步必须带「游客 PoW」头。
//
// 这条是**真机 Bug 的回归锁**：0.9.0 上线后用户实际登录时收到
// 「验证码登录失败：上游原话：Missing Header」。
// 上游的 40300 真名是 POW_HEADER_ERROR，缺的就是 `X-DS-Guest-PoW-Response`
// —— 发码那一步当时「碰巧」过了（它的挑战难度只有 20，风控松），
// 登录那一步（难度 80000）必挂。所以这里把登录这一步单独钉死，别再靠运气。
func TestSMSLoginCarriesGuestPowHeader(t *testing.T) {
	srv, bodies, paths, hdrs := smsServer(t)
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}
	if err := f.Submit(map[string]string{channel.FieldWidgetResult: widgetResult("RID-POW")}); err != nil {
		t.Fatalf("提交控件结果失败：%v", err)
	}
	if err := f.Submit(map[string]string{fieldSMSCode: "123456"}); err != nil {
		t.Fatalf("提交验证码失败：%v", err)
	}

	h := headerFor(t, paths, hdrs, "/users/login_by_mobile_sms").Get("X-DS-Guest-PoW-Response")
	if h == "" {
		t.Fatal("登录请求缺 X-DS-Guest-PoW-Response 头 —— 上游会直接回 40300 Missing Header")
	}
	// 桩求解器给的是固定值：能对上就证明走的是**游客**求解路径（不是聊天那条 6 字段载荷）。
	if h != "c3R1Yi1ndWVzdA==" {
		t.Fatalf("X-DS-Guest-PoW-Response = %q, 想要游客载荷的形状（见 pow.go guestPowHeader）", h)
	}

	// 取挑战时必须点名要调的接口：上游按 target_path 决定难度和用途，
	// 传错了就是「难度对不上/用途不符」，白解一次。
	var lastTarget string
	for i, p := range *paths {
		if p == "/users/create_guest_challenge" {
			lastTarget, _ = (*bodies)[i]["target_path"].(string)
		}
	}
	if lastTarget != "/api/v0/users/login_by_mobile_sms" {
		t.Fatalf("最后一次取挑战的 target_path = %q，想要 /api/v0/users/login_by_mobile_sms", lastTarget)
	}
}

// TestSMSPowFailureExplainsItself 取不到 PoW 挑战、而上游又因此拒绝时，错误里必须带真因。
//
// 对应红线一的一个具体痛点：上游只说 "Missing Header"，用户（和我们）根本看不出
// 是哪个头、是谁的问题。所以只要是我们这边没带上 PoW，就必须把「为什么没带上」说出来。
func TestSMSPowFailureExplainsItself(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/create_guest_challenge":
			// 挑战接口坏掉：没有 guest_challenge（上游偶尔会这样回业务错误）。
			io.WriteString(w, `{"code":0,"msg":"","data":{"biz_code":1001,"biz_msg":"SYSTEM_BUSY"}}`)
		case "/users/login_by_mobile_sms":
			// 真机就是这个形状：HTTP 200 + 信封 code=40300 + msg="Missing Header"。
			io.WriteString(w, `{"code":40300,"msg":"Missing Header","data":null}`)
		case "/users/create_sms_verification_code":
			io.WriteString(w, sendOK)
		}
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}
	if err := f.Submit(map[string]string{channel.FieldWidgetResult: widgetResult("RID-1")}); err != nil {
		t.Fatalf("提交控件结果失败：%v", err)
	}
	err := f.Submit(map[string]string{fieldSMSCode: "123456"})
	if err == nil {
		t.Fatal("上游回 40300 时必须报错（红线一：不静默）")
	}
	msg := err.Error()
	// ① 必须说清是「PoW 请求头」的事，而不是把 Missing Header 原样丢出来。
	if !strings.Contains(msg, "PoW") {
		t.Fatalf("错误里应当点明是 PoW 请求头的问题，实际：%v", err)
	}
	// ② 也必须带上「为什么没带上」 —— 这正是用户拿去自查/报障的关键信息。
	if !strings.Contains(msg, "SYSTEM_BUSY") {
		t.Fatalf("错误里应当带上取挑战失败的原始原因，实际：%v", err)
	}
	// ③ 状态机不前进：用户原地重试即可。
	if v := f.Step(); v.Stage != channel.StageInput {
		t.Fatalf("失败后应当留在验证码屏，实际 stage=%q", v.Stage)
	}
}

// TestSMSResendGoesBackToCaptcha 「重新获取」必须把用户带回人机校验那一步。
//
// 为什么不是「原地再调一次发码」：数美凭据是一次性的，发码必须带一个新凭据。
// 所以重发的真实含义就是「再点一次题」。这条锁住这个语义 ——
// 否则界面上的「重新获取」会变成一个点了没反应（或报错）的假按钮。
func TestSMSResendGoesBackToCaptcha(t *testing.T) {
	srv, _, paths, _ := smsServer(t)
	defer srv.Close()

	a := newTestAdapter(t, srv.URL)
	f := a.startSMS()
	if err := f.Submit(map[string]string{fieldMobile: "13800138000"}); err != nil {
		t.Fatalf("提交手机号失败：%v", err)
	}
	if err := f.Submit(map[string]string{channel.FieldWidgetResult: widgetResult("RID-1")}); err != nil {
		t.Fatalf("提交控件结果失败：%v", err)
	}
	if v := f.Step(); v.Stage != channel.StageInput || v.ResendAfterSecs != 60 {
		t.Fatalf("应当先到验证码屏且带上重发窗口，实际 stage=%q resend=%d", v.Stage, v.ResendAfterSecs)
	}

	// 点「重新获取」：交上来的是一个动作，不是验证码。
	if err := f.Submit(map[string]string{fieldStepAction: actionResend}); err != nil {
		t.Fatalf("「重新获取」不该报错，实际：%v", err)
	}
	v := f.Step()
	if v.Stage != channel.StageWidget {
		t.Fatalf("「重新获取」后应当回到人机校验屏（凭据要重新签），实际 stage=%q", v.Stage)
	}
	if v.Widget != channel.WidgetShumei {
		t.Fatalf("回到的应当是数美控件屏，实际 widget=%q", v.Widget)
	}
	// 而且**不该**在这一步打上游：重发是下一屏（过校验）才发生的事。
	if !equalPaths(*paths, []string{"/users/create_guest_challenge", "/users/create_sms_verification_code"}) {
		t.Fatalf("「重新获取」不该自己打上游，实际调用序列 %v", *paths)
	}
	// 重发窗口清掉，免得新一屏还挂着上一轮的倒计时。
	if v.ResendAfterSecs != 0 {
		t.Fatalf("回到控件屏后不该还带重发窗口，实际 %d", v.ResendAfterSecs)
	}

	// 重新过校验后应当还能正常发码（第二次发码，证明往回退没有把流程锁死）。
	if err := f.Submit(map[string]string{channel.FieldWidgetResult: widgetResult("RID-2")}); err != nil {
		t.Fatalf("重发路径上再过一次校验应当成功，实际：%v", err)
	}
	if v := f.Step(); v.Stage != channel.StageInput || v.ResendAfterSecs != 60 {
		t.Fatalf("重发后应当回到验证码屏，实际 stage=%q resend=%d", v.Stage, v.ResendAfterSecs)
	}
}

// 编译期：四条能力都得在（与 login.go 的断言重复一次，但放在这里更显眼）。
var (
	_ channel.StepAcceptor     = (*smsFlow)(nil)
	_ channel.StepAcceptor     = (*session)(nil)
	_ channel.PasswordAcceptor = (*session)(nil)
	_ channel.CallbackAcceptor = (*session)(nil)
)
