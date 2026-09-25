// Package yuanbao 是「腾讯元宝（网页版）」渠道适配器 —— **登录式**（不要 API key）。
//
// 协议取自**两份互相独立的公开实现**（Python 的 yuanbao-free-api 与 Rust 的
// yuanbao-chat2api），它们在请求体上完全一致，所以这一版可以照着实现而不是猜：
//   - 凭证是浏览器请求头里的 `x-uskey`（外加同一请求的 cookie），用户粘一次；
//     没有签名、没有 nonce —— 这是本渠道比豆包省事的地方；
//   - 对话分两步：先建会话（conversation/create）拿到 id，再 POST /api/chat/{id}；
//   - 响应是 SSE，**帧结构明确**：`{"type":"think","content":"…"}` 是思考、
//     `{"type":"text","msg":"…"}` 是正文（注意两者字段名不一样）、`{"stopReason":"…"}` 是结束；
//   - 历史没有多轮概念，**全部拼进一个 prompt 字符串**；
//   - **上游没有原生工具调用** → 本渠道走 toolshim 模拟层。
package yuanbao

import (
	"strings"
	"time"
)

const (
	apiBase = "https://yuanbao.tencent.com"

	// epCreate 建会话：返回的 id 就是后面聊天要用的 conversation id。
	epCreate = "/api/user/agent/conversation/create"
	// epChatPrefix 拼上 conversation id 就是对话端点。
	epChatPrefix = "/api/chat/"

	// agentID 是默认智能体（参考实现的默认取值）。
	agentID = "naQivTmsDa"

	httpTimeout = 10 * time.Minute

	// defaultMinIntervalSec 是同账号最小请求间隔出厂默认（秒）。
	defaultMinIntervalSec = 3
)

// webModels 是面板下发的档位（公开模型名 → 上游 chatModelId）。
//
// 上游是用 `chatModelId` 指定模型的；这是两款实现的共同结论。
var webModels = []struct {
	ID     string // 客户端用的名字
	Name   string
	Upward string // 上游 chatModelId
	Search bool   // 是否开联网搜索（supportFunctions）
}{
	{ID: "deepseek-v3", Name: "元宝 · DeepSeek V3", Upward: "deep_seek_v3"},
	{ID: "deepseek-r1", Name: "元宝 · DeepSeek R1（推理）", Upward: "deep_seek", Search: false},
	{ID: "hunyuan", Name: "元宝 · 混元", Upward: "hunyuan_gpt_175B_0404"},
	{ID: "hunyuan-t1", Name: "元宝 · 混元 T1（推理）", Upward: "hunyuan_t1"},
	{ID: "deepseek-v3-search", Name: "元宝 · DeepSeek V3（联网）", Upward: "deep_seek_v3", Search: true},
}

// modelSpec 是一个档位解析后的形态。
type modelSpec struct {
	upward string
	search bool
	known  bool
}

// specOf 解析客户端给的模型名；认不出就回落到 DeepSeek V3（由调用方记一笔）。
func specOf(id string) modelSpec {
	s := strings.ToLower(strings.TrimSpace(id))
	for _, m := range webModels {
		if m.ID == s {
			return modelSpec{upward: m.Upward, search: m.Search, known: true}
		}
	}
	return modelSpec{upward: "deep_seek_v3"}
}
