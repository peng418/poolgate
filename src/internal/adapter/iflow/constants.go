// Package iflow 是「iFlow CLI（心流）」渠道适配器 —— **登录式**（粘贴 apiKey，不是账号密码）。
//
// 协议事实全部取自公开参考实现 iflow2api（2026-09），本文件与注释里的每个字段都能在
// 那份实现里找到出处，不凭记忆发明：
//   - 上游是**标准 OpenAI 兼容**接口：POST {base}/chat/completions，单层 SSE，
//     以 `data: [DONE]` 收尾，响应里就是 OpenAI 的 chat.completion.chunk（含 tool_calls）。
//   - 请求**必须带 HMAC-SHA256 签名**：密钥 = apiKey，原文 = `{user-agent}:{session-id}:{timestamp}`
//     （user-agent 恒为 iFlow-Cli，timestamp 是**毫秒**），结果放 x-iflow-signature / x-iflow-timestamp。
//     原文与头的确切拼法见 proxy.py 的 generate_signature / _get_headers —— 这里照抄，不改写。
//   - `user-agent: iFlow-Cli` 是解锁 GLM / DeepSeek / Kimi 高级模型的关键：
//     换成别的 UA，这些模型会被上游拒绝或降级（参考实现把它单列为 IFLOW_CLI_USER_AGENT）。
//   - 凭证是 apiKey，**长期有效、不需要刷新**；失效只表现为 401/签名错误 → SessionDead
//     （提示用户重新粘贴，而不是去查网络）。
//   - **上游原生支持 tools/tool_calls**，所以 Spec 是 Tools=true + ToolsShim=false，
//     不经过 internal/toolshim 那一层。
package iflow

import "time"

// 端点与协议常量。来源：iflow2api/proxy.py。
const (
	// apiBase 是默认 API 基址（settings.json 里的 baseUrl 也常是它）。
	apiBase = "https://apis.iflow.cn/v1"
	// epChat 是对话端点。上游把 OpenAI 的 chat.completions 原样实现。
	epChat = "/chat/completions"

	// cliUserAgent 是伪装的客户端 UA。**它同时参与签名**（签名原文的第一段），
	// 所以这里是唯一出处，改它要连着签名一起改，不能只改头。
	cliUserAgent = "iFlow-Cli"
	// cliVersion 只在 Aone 内网端点（ducky.code.alibaba-inc.com）才需要，
	// 我们只打 apis.iflow.cn，不追加那组头 —— 留着它是为了标注出处，不做他用。
	cliVersion = "0.5.13"

	// 官方 CLI 在 chat/completions 上会补齐的默认参数（参考实现按抓包结论对齐）。
	// partial body 里没给这些字段时用它们，给了就以调用方为准。
	defaultTemperature  = 0.7
	defaultTopP         = 0.95
	defaultMaxNewTokens = 8192

	// defaultModel 是本地清单里的兜底档（模型名认不出、或凭证探活时用）。
	defaultModel = "glm-4.6"

	httpTimeout = 10 * time.Minute

	// defaultMinIntervalSec 是同账号最小请求间隔出厂默认（秒）。
	//
	// 参考实现自身的限速是 60 请求/分钟，换算下来正好 1 秒/次；这里取同一个值：
	// iFlow 是**官方 API**（不是网页反爬），节流主要是为了不把并发打到官方限流阈值上。
	defaultMinIntervalSec = 1
)

// localModels 是面板下发的模型清单（本地硬编码，Source=local，如实标注）。
//
// 为什么硬编码：参考实现明确写「iFlow API 没有公开的 /models 端点」，清单来自
// iflow-cli 源码里的 SUPPORTED_MODELS。上游既然不提供目录接口，我们就不能假装
// 从上游读到了 —— 照抄清单并标 SourceLocal，比猜一个不存在的接口诚实。
//
// Think 只用于给 ModelInfo.Reasoning 一个依据（是否要开思考参数），不参与请求路由。
var localModels = []struct {
	ID    string
	Name  string
	Think bool
}{
	{ID: "glm-4.6", Name: "GLM-4.6", Think: true},
	{ID: "glm-4.7", Name: "GLM-4.7", Think: true},
	{ID: "glm-5", Name: "GLM-5（推荐）", Think: true},
	{ID: "iFlow-ROME-30BA3B", Name: "iFlow ROME 30B（快速）", Think: false},
	{ID: "deepseek-v3.2-chat", Name: "DeepSeek-V3.2", Think: true},
	{ID: "qwen3-coder-plus", Name: "Qwen3-Coder-Plus", Think: false},
	{ID: "kimi-k2", Name: "Kimi-K2", Think: false},
	{ID: "kimi-k2-thinking", Name: "Kimi-K2-Thinking", Think: true},
	{ID: "kimi-k2.5", Name: "Kimi-K2.5", Think: true},
	{ID: "kimi-k2-0905", Name: "Kimi-K2-0905", Think: false},
	{ID: "minimax-m2.5", Name: "MiniMax-M2.5", Think: false},
	{ID: "qwen-vl-max", Name: "Qwen-VL-Max", Think: false},
}
