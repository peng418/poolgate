// Package boot 集中渠道注册 —— 「新增渠道 = 一行」的落点。
//
// 六家全部接入：QoderCN / WorkBuddyCN / TraeWork / 千问办公 / QoderCOM / WorkBuddyAI。
// 判据见 docs/03-渠道能力矩阵.md。
//
// 千问办公走**网页端协议**（chat-ws + cookie）：桌面网关 gateway.qwenwork.cn
// 被上游加了闸门（信封 503），但网页域同账号可用 —— 上游改版就换路。
package boot

import (
	"poolgate/internal/adapter/anthropic"
	"poolgate/internal/adapter/antigravity"
	"poolgate/internal/adapter/chatglm"
	"poolgate/internal/adapter/chatgpt"
	"poolgate/internal/adapter/codebuddy"
	"poolgate/internal/adapter/copilot"
	"poolgate/internal/adapter/deepseek"
	"poolgate/internal/adapter/doubao"
	"poolgate/internal/adapter/gemini"
	"poolgate/internal/adapter/iflow"
	"poolgate/internal/adapter/kimi"
	"poolgate/internal/adapter/kiro"
	"poolgate/internal/adapter/lingma"
	"poolgate/internal/adapter/qodercn"
	"poolgate/internal/adapter/qodercom"
	"poolgate/internal/adapter/qwen"
	"poolgate/internal/adapter/qwenwork"
	"poolgate/internal/adapter/traework"
	"poolgate/internal/adapter/windsurf"
	"poolgate/internal/adapter/workbuddy"
	"poolgate/internal/adapter/workbuddyai"
	"poolgate/internal/adapter/yuanbao"
	"poolgate/internal/channel"
	"poolgate/internal/registry"
	"poolgate/internal/store"
)

// RegisterChannels 注册全部渠道声明与实现。
func RegisterChannels() {
	qc := qodercn.New()
	registry.Register(qc, qc.Spec())

	wb := workbuddy.New()
	registry.Register(wb, wb.Spec())

	tw := traework.New()
	registry.Register(tw, tw.Spec())

	qw := qwenwork.New()
	registry.Register(qw, qw.Spec())

	qm := qodercom.New()
	registry.Register(qm, qm.Spec())

	wa := workbuddyai.New()
	registry.Register(wa, wa.Spec())

	// 通义（Qwen）：设备码登录 + Qwen Code CLI 端点（原生工具调用，无需浏览器指纹）。
	// 与「千问办公」（qwenwork）是两个完全不同的上游，别混。
	qn := qwen.New()
	registry.Register(qn, qn.Spec())

	// Gemini（Google Code Assist）：官方 OAuth 的登录式渠道（原生工具调用、1000 请求/天）。
	gm := gemini.New()
	registry.Register(gm, gm.Spec())

	// DeepSeek（网页版）：粘贴 userToken 登录（面板引导）+ 服务端解 PoW；工具调用由网关模拟。
	ds := deepseek.New()
	registry.Register(ds, ds.Spec())

	// Kimi（网页版）：粘贴浏览器里的 refresh token 登录（服务端换 access token）；
	// 上游是 Connect/gRPC-Web 二进制帧协议，思考单独分流，工具调用由网关模拟。
	km := kimi.New()
	registry.Register(km, km.Spec())

	// 智谱清言（网页版）：粘贴 chatglm_refresh_token 登录（服务端换 access token）；
	// 每个请求都要带一个自算签名（智谱的时间戳变换 + md5），是最容易随官网改版失效的一环。
	cg := chatglm.New()
	registry.Register(cg, cg.Spec())

	// 豆包（网页版）：粘贴整行 Cookie 登录。注意它有一个先天弱点 —— 请求签名 a_bogus
	// 是浏览器现算的，本渠道**不带**它（不去猜签名算法），所以可能被风控要求人工验证码；
	// 这一点写在 Spec.Docs 里，用户加账号前就能看到。工具调用由网关模拟。
	db := doubao.New()
	registry.Register(db, db.Spec())

	// 腾讯元宝（网页版）：粘贴浏览器请求头里的 x-uskey 登录；历史拼成单轮 prompt；
	// 思考单独分流（type=think 帧）；工具调用由网关模拟。
	yb := yuanbao.New()
	registry.Register(yb, yb.Spec())

	// ---- 0.5.0：登录式渠道批量接入（详见 docs/03 §7 与 src/README 的版本记录）----
	//
	// 共同前提：凭证来自用户**自己浏览器里的登录态**（粘一次），服务端不复现登录流程 ——
	// 不存密码、不在 NAS 上跑无头浏览器。能力位按事实标注：上游有原生工具调用的走 Tools，
	// 只会上网聊天的走 toolshim 模拟（对客户端合同不变）。

	// ChatGPT（网页版）：粘贴 accessToken；对话前要过 sentinel 挑战并自算 PoW；
	// **无原生工具调用** → toolshim 模拟。
	cpgt := chatgpt.New()
	registry.Register(cpgt, cpgt.Spec())

	// Anthropic（订阅 OAuth）：Claude Pro/Max 的授权码流程，Messages API 原生工具调用。
	// 封号风险写在 Spec.Docs 里（官方禁止第三方客户端用订阅令牌）。
	an := anthropic.New()
	registry.Register(an, an.Spec())

	// 腾讯 CodeBuddy：与 WorkBuddy 同一个后端，差在出站身份（X-Domain / 刷新来源 / 极简头）；
	// 上游 OpenAI 兼容且原生支持工具调用。
	cb := codebuddy.New()
	registry.Register(cb, cb.Spec())

	// GitHub Copilot：GitHub 设备码授权 → 换 copilotToken；上游就是标准 OpenAI 协议 + 原生 tools，
	// 但必须伪装成 VS Code 的 Copilot 插件。
	cp := copilot.New()
	registry.Register(cp, cp.Spec())

	// AWS Kiro：粘贴刷新令牌（桌面版或企业版 SSO 两模式）；上游是 AWS Event Stream 二进制帧，
	// 不是 SSE；原生支持工具调用。
	kr := kiro.New()
	registry.Register(kr, kr.Spec())

	// iFlow CLI（心流）：粘贴 apiKey，每请求带 HMAC-SHA256 签名；上游 OpenAI 兼容 + 原生 tools。
	fl := iflow.New()
	registry.Register(fl, fl.Spec())

	// 通义灵码：粘贴 IDE 登录缓存（服务端解密出 COSY 凭证）；远端接口支持原生 tools。
	lm := lingma.New()
	registry.Register(lm, lm.Spec())

	// Google Antigravity：Google OAuth 登录式；一个上游下发 Gemini / Claude / GPT-OSS，原生 tools。
	// 风险（违反 Google ToS、社区有封号记录）写在 Spec.Docs 里。
	ag := antigravity.New()
	registry.Register(ag, ag.Spec())

	// Windsurf（Codeium）：粘贴 session token；**只走直连云那条路**
	// （不走依赖本地语言服务器的 Cascade —— 网关在 NAS 上，不会去跑 IDE 的语言服务器）；
	// 上游是 protobuf/Connect 协议，工具定义的两个子 tag 未标定 → 走 toolshim 模拟。
	ws := windsurf.New()
	registry.Register(ws, ws.Spec())

	reserveRegisteredKinds()
}

// reserveRegisteredKinds 把注册表里全部渠道名登记为「保留名」。
//
// 为什么放在注册流程里而不是维护一张手写清单：清单一定会漏（漏掉的那次，用户就能用一个
// 同名来源把内置渠道静默顶掉）。这里从注册表直接取，新增渠道自动进保留集合。
func reserveRegisteredKinds() {
	kinds := make([]string, 0, 16)
	for _, e := range registry.All() {
		kinds = append(kinds, string(e.Spec.Kind))
	}
	store.ReserveKinds(kinds...)
}

// QoderCN 返回注册的 QoderCN 适配器。未注册返回 nil。
func QoderCN() *qodercn.Adapter {
	c, ok := registry.Get(channel.QoderCN)
	if !ok {
		return nil
	}
	a, _ := c.(*qodercn.Adapter)
	return a
}

// QoderCOM 返回注册的 QoderCOM 适配器。未注册返回 nil。
func QoderCOM() *qodercom.Adapter {
	c, ok := registry.Get(channel.QoderCOM)
	if !ok {
		return nil
	}
	a, _ := c.(*qodercom.Adapter)
	return a
}

// EnsureFingerprint 为需要 COSY 机器指纹的渠道补齐指纹（幂等）。
//
// QoderCN 与 QoderCOM 同一套 COSY 框架，都必须带机器指纹才能过上游校验；
// 指纹缺失时由这里生成并回填到凭证 Extra。
func EnsureFingerprint(kind channel.Kind, c *channel.Credential) {
	switch kind {
	case channel.QoderCN:
		if a := QoderCN(); a != nil {
			a.EnsureFingerprint(c)
		}
	case channel.QoderCOM:
		if a := QoderCOM(); a != nil {
			a.EnsureFingerprint(c)
		}
	}
}
