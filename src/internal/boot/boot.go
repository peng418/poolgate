// Package boot 集中渠道注册 —— 「新增渠道 = 一行」的落点。
//
// 六家全部接入：QoderCN / WorkBuddyCN / TraeWork / 千问办公 / QoderCOM / WorkBuddyAI。
// 判据见 docs/03-渠道能力矩阵.md。
//
// 千问办公走**网页端协议**（chat-ws + cookie）：桌面网关 gateway.qwenwork.cn
// 被上游加了闸门（信封 503），但网页域同账号可用 —— 上游改版就换路。
package boot

import (
	"poolgate/internal/adapter/gemini"
	"poolgate/internal/adapter/qodercn"
	"poolgate/internal/adapter/qodercom"
	"poolgate/internal/adapter/qwen"
	"poolgate/internal/adapter/qwenwork"
	"poolgate/internal/adapter/traework"
	"poolgate/internal/adapter/workbuddy"
	"poolgate/internal/adapter/workbuddyai"
	"poolgate/internal/channel"
	"poolgate/internal/registry"
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
