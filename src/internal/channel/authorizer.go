package channel

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrPending 表示授权尚未完成（用户还没在浏览器里点完）。适配器用它表达
// 「继续轮询」，控制台据此显示「等待授权」而不是报错。
//
// 上游对「还没授权」的表达方式千奇百怪：QoderCN 用 HTTP 404/202、
// WorkBuddy 用业务码 11217、Trae 用「本机回调还没收到」。这些差异在适配器内
// 归一成这一个哨兵错误（D4），控制台只认它。
var ErrPending = errors.New("channel: login pending")

// LoginSession 是一次进行中的渠道授权。
//
// 各渠道的授权形态不同（设备流轮询 / 授权码 + 本机回调），但控制台只需要
// 两件事：给用户一个要打开的 URL，以及反复问「好了没」。
type LoginSession interface {
	// AuthURL 是让用户用浏览器打开的授权页地址（唯一必须出示给用户的东西）。
	AuthURL() string

	// Poll 问一次授权结果：
	//   - 还没完成 → 返回 ErrPending
	//   - 完成 → 返回凭证
	//   - 失败 → 返回归一化的 errs.Error（必须带原因，红线一）
	// Poll 可被反复调用，超时与否由调用方（控制台）控制。
	Poll(ctx context.Context) (*Credential, error)

	// Cancel 释放资源（如 Trae/Qwen 的本机回调 server）。必须可重复调用。
	Cancel()
}

// Authorizer 是「能发起授权」的渠道。控制台通过它拿到 LoginSession，
// 不需要知道任何渠道协议细节 —— 这是红线三在授权链路上的落点。
//
// 不是所有渠道都必须实现：没实现的渠道在面板上显示「该渠道暂不支持面板授权」，
// 而不是给一个点了没反应的按钮（F1.1）。
type Authorizer interface {
	StartLogin(ctx context.Context, opts LoginOptions) (LoginSession, error)
}

// LoginOptions 是发起授权时可带的上下文信息。
//
// CallbackBase 是**面板自己的对外地址**（形如 http://192.0.2.10:5014，不含路径）。
// 需要「公开回调」的渠道（TraeWork）拿它把回调从 127.0.0.1 换成面板所在主机 ——
// 这样手机扫码、别的电脑的浏览器都能把授权码送回来，不再要求「浏览器与 PoolGate 同机」。
// 依赖预注册回调的渠道（千问办公）忽略它，理由写在各自的 StartLogin 里。
type LoginOptions struct {
	CallbackBase string
}

// CallbackAcceptor 由「本机回调」类流程实现：允许把用户浏览器地址栏里的回调地址
// 手工喂进来（整串 URL 或只给 code 都行），下一次 Poll 就用它完成兑换。
//
// 为什么需要：有些上游把回调地址锁死在预注册清单里（千问办公实测只接受
// 127.0.0.1:<port>），远端浏览器跳回 127.0.0.1 是打不开的 —— 这时让用户把地址栏
// 那串粘回来，是不改回调地址、不额外开放端口的唯一完成方式。
//
// 没实现这个接口的渠道在面板上不会出现「粘贴回调地址」入口。
type CallbackAcceptor interface {
	AcceptCallback(raw string) error
}

// PasswordAcceptor 由「账号密码直登」类渠道实现：面板给一个账号/密码表单，
// 用户填一次，服务端直接拿它去换 token —— 不需要开浏览器、不需要复制粘贴。
//
// 为什么要有这一层（与 CallbackAcceptor 的区别）：
//   - CallbackAcceptor 解决的是「回调地址回不来」，本质仍是**用户先在自己浏览器登录**
//     （DeepSeek 粘 userToken 就是这类：用户得先登录、再进控制台复制 localStorage）。
//   - PasswordAcceptor 解决的是「连登录这一步都想省掉」——网页版 token 是短期会话令牌，
//     几小时就过期且无刷新接口，每次都要重新登录复制，体验上不可接受。
//
// 安全边界（实现方必须遵守）：
//   - 账号密码**只用于换取 token**，换到后由实现方决定是否留存（留存才能自动刷新续期）；
//   - 面板与日志**永不回显**密码；面板上的密码框一律 type=password 且不落前端状态；
//   - 未实现本接口的渠道，面板不出现账号密码表单（红线三：核心层零渠道专有代码）。
//
// 实现方若同时实现 LoginSession（见 password_session），控制台会在 Poll 时
// 拿到已换好的凭证；这使「表单直登」与现有授权轮询走同一条落盘/入池链路。
type PasswordAcceptor interface {
	// LoginFields 描述本渠道需要用户填哪些字段（面板据此渲染表单）。
	// 例如 DeepSeek 返回 [账号, 密码]；未来某渠道可能是 [手机号, 验证码]。
	// 返回空切片表示本渠道当前不提供直登表单。
	LoginFields() []LoginField

	// AcceptPassword 收用户填的字段值。字段名与 LoginFields 的 Name 一一对应。
	// 返回错误时面板显示原因（红线一：失败必带原因，不静默）。
	AcceptPassword(values map[string]string) error
}

// LoginField 描述直登表单里的一个输入项。
type LoginField struct {
	// Name 是字段键（AcceptPassword 收到的 map 里的 key），如 "account" / "password"。
	Name string `json:"name"`
	// Label 是面板上显示的中文标签，如 "邮箱或手机号"。
	Label string `json:"label"`
	// Type 是输入框类型：text / password。
	Type string `json:"type"`
	// Placeholder 是输入框占位提示，可为空。
	Placeholder string `json:"placeholder,omitempty"`
	// Required 表示必填。
	Required bool `json:"required"`
}

// ── 验证码类授权（OTP） ────────────────────────────────────────────────

// LoginStage 是一次授权流程当前处在哪一步。面板据此决定显示什么。
//
// 设计要点：面板**不需要知道**渠道内部有几步、每步叫什么 —— 它只按 Stage 渲染
// 当前这一屏，把渠道声明的字段渲染出来，再调 Submit 交回去。加一个新渠道或给
// 某个渠道加/减一步，核心层与前端都不改代码（红线三）。
type LoginStage string

const (
	// StageIdle 表示流程还没开始（面板显示「开始授权」）。
	StageIdle LoginStage = "idle"
	// StageInput 表示正在等用户填字段（含首次填表与第二步填验证码）。
	StageInput LoginStage = "input"
	// StageImageChallenge 表示上游要图片验证码：面板显示 ImageURL 的图 + 一个输入框，
	// 用户看图填字后 Submit 交回。
	StageImageChallenge LoginStage = "image_challenge"
	// StageWidget 表示这一步要用户在**渠道自带的交互控件**里完成。
	//
	// 面板按 StepView.Widget 的名字挂载对应控件；控件自己渲染、自己与它的服务器交互，
	// 成功后的结果由面板用保留键 FieldWidgetResult 交回。
	//
	// 为什么不能复用 StageImageChallenge：有些人机校验**根本不是一张静态图**。
	// 例如数美的「空间点选」题（题目形如「点击图中最小的蓝色三棱锥」），答案是
	// 一组点击坐标，并且校验发生在数美自己的服务器上、凭据是一次性令牌 ——
	// 服务端既拿不到唯一正确答案，也无法凭空造出合法院验令牌。
	//
	// 正解是**把控件原样挂到面板里**：用户看到的就是上游自己那一屏，控件把令牌
	// 交给面板，面板交给服务端，服务端再拿它去换正式凭据。这样既不需要逆向控件
	// 的加密协议，也不需要伪造任何东西 —— 走的就是真实的登录流程，
	// 只是发生在我们自己的界面里。
	StageWidget LoginStage = "widget"
	// StageSending 表示正在等上游把验证码发出去（面板显示「发送中」并可轮询）。
	StageSending LoginStage = "sending"
	// StageDone 表示授权已拿到凭证，控制台会走正常落盘/入池。
	StageDone LoginStage = "done"
	// StageFailed 表示授权失败（Message 里是原因，红线一）。
	StageFailed LoginStage = "failed"
)

// StepView 是「当前这一步」的完整描述：面板拿它就能把一个屏渲染出来。
//
// 这是把「多步授权」变成一个**通用状态机**的关键：不管是账号密码、手机号验证码、
// 还是「先过图验再发短信」，对面板而言都只是「一组字段 + 一句提示 + 可选的图片」。
type StepView struct {
	// Stage 是当前步骤类型。
	Stage LoginStage `json:"stage"`
	// Title 是本步骤的标题，如「填写手机号」「输入短信验证码」。
	Title string `json:"title,omitempty"`
	// Hint 是本步骤的说明文字（面板显示在字段上方）。
	Hint string `json:"hint,omitempty"`
	// Fields 是本步骤要用户填的字段（面板按顺序渲染）。
	Fields []LoginField `json:"fields,omitempty"`
	// SubmitLabel 是提交按钮文字，如「发送验证码」「登录」。空则面板用默认「提交」。
	SubmitLabel string `json:"submit_label,omitempty"`
	// ImageURL 仅在 StageImageChallenge 时有值：图片验证码的图片地址。
	// 用 data URL（data:image/png;base64,…）直接内联，面板 <img src> 即可显示，
	// 不用另开静态路由、也不依赖面板与网关同机。
	ImageURL string `json:"image_url,omitempty"`
	// Widget 仅在 StageWidget 时有值：控件标识（面板据此选挂载器）。取值见 Widget* 常量。
	Widget string `json:"widget,omitempty"`
	// WidgetConfig 是控件的初始化参数，**原样透传**给前端，核心层不解释其内容
	// （渠道自有参数不进核心层，红线三）。由渠道声明，形如
	// {"organization":"…","appId":"…","mode":"spatial_select","region":"CN","script":"https://…"}。
	WidgetConfig json.RawMessage `json:"widget_config,omitempty"`
	// Message 是失败/提示信息（StageFailed 时必填，红线一）。
	Message string `json:"message,omitempty"`
	// ResendAfterSecs > 0 时面板显示「N 秒后可重发」。上游通常在响应里给这个窗口。
	ResendAfterSecs int `json:"resend_after_secs,omitempty"`
}

// StepAcceptor 由「多步 / 验证码」类渠道实现：把授权流程暴露成一个可查询的
// 状态机，面板每一步都调 Step() 拿当前屏、调 Submit() 交这一步的输入。
//
// 为什么在 PasswordAcceptor 之外还要这一层：
//   - PasswordAcceptor 假设「一步完事」（填账号密码，直接换 token）。
//   - 手机号验证码是**至少两步**（先发码、再验码），中间还可能插一步图片验证码。
//   - 与其在核心层写死「第几步该显示什么」，不如让渠道自己说「我现在这一步要什么」。
//
// 安全边界（与 PasswordAcceptor 一致）：密码/验证码只用于换取 token；面板与日志
// 永不回显；实现方自己决定是否留存（留存才能自动续期）。
//
// 与 PasswordAcceptor 的关系：两者可以并存。同时实现的渠道，面板优先走 StepAcceptor
// （更通用，能表达密码登录、验证码登录等多条路）。旧渠道不受影响。
type StepAcceptor interface {
	// Step 返回当前步骤的视图。任何时候都可调用（面板轮询也用它）。
	Step() StepView

	// Submit 交回用户在**当前步骤**填的值，并推进状态机（或原地报错）。
	//   - values 的 key 与 Step().Fields[].Name 对应；图片验证码步骤用保留键 FieldCaptcha，
	//     交互控件步骤（StageWidget）用保留键 FieldWidgetResult。
	//   - 返回错误时：面板显示错误，状态机**不前进**（用户可改了重试）。
	//   - 返回 nil 时：状态机可能进入下一步，也可能直接完成 —— 面板再调 Step() 看。
	Submit(values map[string]string) error
}

// FieldCaptcha 是图片验证码输入的保留字段名。
//
// 约定：上游要图片验证码时，面板在 StageImageChallenge 下把用户填的字用这个 key
// 交回（配合 Step().ImageURL 显示的图）。之所以用保留名而不是让渠道自定义：
// 这一屏的形状是固定的（一张图 + 一个输入框），核心层与前端只需认识这一个约定，
// 各渠道不必各造一套；渠道在 Submit 里把它映射成上游要的字段名即可。
const FieldCaptcha = "captcha_code"

// WidgetShumei 是数美（Shumei）人机校验控件的标识。
//
// 为什么这个名字要放在核心层：它是一个**跨层约定**。适配器声明
// StepView.Widget = WidgetShumei 并给出初始化参数（WidgetConfig），前端据此加载
// 数美的 smcp.min.js 并调用它的 initSMCaptcha。名字与握手方式只写在一处，
// 两边不会对不上；而**具体参数**（organization / mode / 脚本地址）仍然由渠道给出，
// 核心层与前端都不认识「数美」的业务含义（红线三）。
const WidgetShumei = "shumei"

// FieldWidgetResult 是「交互控件结果」的保留字段名。
//
// 约定：StageWidget 下，面板把控件成功后的结果用这个 key 交回，值是一段 **JSON 字符串**，
// 内容由渠道自己定义，核心层只负责搬运。
//
// 与 FieldCaptcha 的区别（别混用）：
//   - FieldCaptcha 传的是用户**看图敲进去的字**，服务端拿去填上游的字符校验字段；
//   - FieldWidgetResult 传的是**控件自己产生的一次性令牌**（数美给的是 rid），
//     它由控件厂商的服务器签发并校验，服务端只做转发，不参与生成。
const FieldWidgetResult = "captcha_result"
