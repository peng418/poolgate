package channel

import (
	"context"
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
