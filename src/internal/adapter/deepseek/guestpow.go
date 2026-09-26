// guestpow.go 「游客 PoW」：登录类接口的前置请求头。
//
// 为什么需要它（2026-09-26 真机实测定案）：
//
//	不带这个头去打 login_by_mobile_sms，上游 100% 回
//	  {"code":40300,"msg":"Missing Header","data":null}
//	用户在面板上看到的就是「验证码登录失败：上游原话：Missing Header」——
//	看着像我们漏了什么参数，其实漏的是一个**请求头**。
//
// 40300 的真名从官方 bundle 的枚举里读出来是 POW_HEADER_ERROR，对应头是
// `X-DS-Guest-PoW-Response`，值 = base64(JSON({salt, answer}))（bundle 里 encoder 就是 btoa）。
//
// 挑战从哪来：POST /users/create_guest_challenge {"target_path": "<要调的那个接口>"}
//
//	→ data.biz_data.guest_challenge = {algorithm:"DeepSeekHashV1", challenge, salt,
//	  signature, difficulty, expire_at, expire_after, target_path}
//
// 两个坑，都别踩：
//
//  1. **难度是按接口各自定的**，实测 login_by_mobile_sms 的 difficulty=80000，
//     而 create_sms_verification_code 只有 20。所以必须「要打哪个接口就现取哪个挑战」，
//     不能取一次缓存起来复用（复用 = 难度对不上/挑战过期，白解）。
//  2. **挑战有效期 300 秒**（expire_after=300000），且是一次性的：重试要重新取。
//
// 哪些接口需要（下面 needsGuestPow 的白名单，照抄官方 bundle 里那一条：
// ["/v0/users/register","/v0/users/register_by_mobile","/v0/users/login_by_mobile_sms",
// "/api/v0/users/create_sms_verification_code","/api/v0/users/create_email_verification_code"]）。
// 本适配器用到其中两个：发码 + 短信登录。
package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// guestPowPath 是取游客挑战的接口。
const guestPowPath = "/users/create_guest_challenge"

// guestPowAPIPrefix 用来把适配器内部的相对路径拼成 target_path。
//
// 上游会把这个值规范化后再回显（实测发 "/api/v0/users/login_by_mobile_sms"，
// 回显 target_path 是 "/v0/users/login_by_mobile_sms"），所以两种写法它都认；
// 这里用带 /api/v0 的全路径，语义最明确。
const guestPowAPIPrefix = "/api/v0"

// needsGuestPow 判断某个接口是否必须带游客 PoW 头（白名单与官方前端一致）。
//
// 为什么不用「凡是登录类都要」这种推断：发码接口实测不带也能偶尔过
// （它的 difficulty 只有 20，风控松），但登录接口不带必挂。白名单是上游的实际契约，
// 按契约来，不按感觉来。
func needsGuestPow(path string) bool {
	switch path {
	case "/users/login_by_mobile_sms",
		"/users/create_sms_verification_code",
		"/users/create_email_verification_code",
		"/users/register",
		"/users/register_by_mobile":
		return true
	}
	return false
}

// guestPow 走完整的一轮：取挑战 → 解出来 → 返回 `X-DS-Guest-PoW-Response` 的值。
func (a *Adapter) guestPow(ctx context.Context, s smsSnapshot, path string) (string, error) {
	ch, err := a.guestChallenge(ctx, s, path)
	if err != nil {
		return "", err
	}
	solver, err := a.ensureSolver(ctx, guestCred(s))
	if err != nil {
		return "", err
	}
	powCtx, cancel := context.WithTimeout(ctx, powTimeout)
	defer cancel()
	h, err := solver.guestPowHeader(powCtx, ch)
	if err != nil {
		return "", errs.New(errs.Parse, "解游客 PoW 失败："+err.Error()).
			WithChannel(string(channel.DeepSeek)).WithCause(err)
	}
	return h, nil
}

// guestChallenge 取一次游客挑战（targetPath 用适配器内部的相对路径，如
// "/users/login_by_mobile_sms"）。
func (a *Adapter) guestChallenge(ctx context.Context, s smsSnapshot, path string) (powChallenge, error) {
	body, _ := json.Marshal(map[string]any{"target_path": guestPowAPIPrefix + path})
	c := guestCred(s)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+guestPowPath, strings.NewReader(string(body)))
	if err != nil {
		return powChallenge{}, errs.New(errs.Transport, "构造游客 PoW 请求失败").
			WithChannel(string(channel.DeepSeek)).WithCause(err)
	}
	for k, v := range clientHeaders(c) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return powChallenge{}, errs.New(errs.Transport, "取游客 PoW 挑战失败（上游请求失败）").
			WithChannel(string(channel.DeepSeek)).WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return powChallenge{}, errs.New(a.Classify(resp.StatusCode, raw),
			fmt.Sprintf("取游客 PoW 挑战失败（HTTP %d）", resp.StatusCode)).
			WithChannel(string(channel.DeepSeek)).WithUpstream(truncate(string(raw), 200))
	}
	var out struct {
		Data struct {
			BizData struct {
				Guest powChallenge `json:"guest_challenge"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Data.BizData.Guest.Salt == "" {
		// 交给 bizError：它会把信封级的码/消息带出来（红线一，不吞原因）。
		return powChallenge{}, bizError(raw, "取游客 PoW 挑战失败")
	}
	return out.Data.BizData.Guest, nil
}

// guestCred 造一个「还没有凭证」的临时身份，只为了复用 clientHeaders 的客户端指纹
// 与按出口取 http.Client（登录这一步本来就没有 Authorization）。
func guestCred(s smsSnapshot) *channel.Credential {
	return &channel.Credential{Extra: map[string]string{"device_id": s.deviceID}}
}

// powHeaderRejected 判断上游这条响应是不是在说「缺 PoW 头 / PoW 结果不认」。
//
// 它认三种写法，因为上游在不同层用不同措辞：
//   - 40300 POW_HEADER_ERROR（信封 code=40300，msg 是 "Missing Header"）
//   - 40301 INVALID_POW_RESPONSE（信封 code=40301）
//   - 文案里出现 MISSING HEADER / POW_HEADER（防它改码不改文案，或反过来）
//
// 只在「我们这次确实没带上 powErr」时才会用到它（见 authCall），
// 所以误判的代价只是错误文案更具体，不会改变正常路径的行为。
func powHeaderRejected(raw []byte) bool {
	code, msg, _ := parseBiz(raw)
	if code == 40300 || code == 40301 {
		return true
	}
	u := strings.ToUpper(msg)
	return strings.Contains(u, "MISSING HEADER") || strings.Contains(u, "POW_HEADER")
}
