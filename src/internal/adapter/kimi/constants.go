// Package kimi 是「Kimi 网页版」渠道适配器 —— **登录式**（不要 API key）。
//
// 协议取自公开参考实现 kimi2api（2026-09 仍在更新）对 www.kimi.com 的实测结论：
//   - 对话不是 OpenAI 格式，而是 **Connect 协议**（gRPC-Web 变体）：请求体是
//     「5 字节信封（0x00 + 4 字节大端长度）+ JSON」，Content-Type 为 application/connect+json；
//     响应也是同样的二进制帧流，`flag&0x80` 的帧是 trailer（跳过）。
//   - **全部消息被拼成一条 user 文本**发给上游（上游只吃单轮），工具调用与工具结果
//     用 `[function_calls]…[/function_calls]` / `[TOOL_RESULT for id]` 这类标记表达。
//   - **上游没有原生工具调用**（只有一个内置联网搜索开关）→ 本渠道走 toolshim 模拟层。
//   - 登录用「你在浏览器登录 Kimi 后，把凭证粘回来」：refresh token 是长期凭证，
//     服务端拿它换 access token（JWT），不需要浏览器、不需要密码。
package kimi

import (
	"strings"
	"time"
)

// 端点（www.kimi.com）。
const (
	apiBase = "https://www.kimi.com"

	// epChat 是对话端点。注意路径里的 `apiv2/<service>/<Method>` 是 Connect 的形态。
	epChat = "/apiv2/kimi.gateway.chat.v1.ChatService/Chat"
	// epModels 是模型目录（上游的可用模型清单）。**不是 Connect 信封**：
	// 参考实现用普通 JSON 调它（kimi2api app/kimi/model_catalog.py:253-265，body `{}`）。
	epModels = "/apiv2/kimi.gateway.config.v1.ConfigService/GetAvailableModels"
	// epRefresh 用 refresh token 换 access token（GET，Bearer 传 refresh token）。
	epRefresh = "/api/auth/token/refresh"
	// epSubscription 是账号订阅状态（用于校验凭证是否还有效）。
	epSubscription = "/apiv2/kimi.gateway.order.v1.SubscriptionService/GetSubscription"

	// scenario 是对话场景标识。参考实现只有一个常量（SCENARIO_K2D5），
	// 所有模型档位都用它 —— 上游按 models 里的模型名与 scenario 共同决定路由。
	scenario = "SCENARIO_K2D5"

	// scenarioOKComputer 是 agent 档（`-agent` / `-agent-swarm`）的场景。
	// 参考实现按目录响应里的 scenario 走（model_catalog.py:126-141），
	// 已知的 agent 档就是 SCENARIO_OK_COMPUTER（tests/test_model_catalog.py:23-36）。
	scenarioOKComputer = "SCENARIO_OK_COMPUTER"

	// kimiPlusIDAgent / agentMode* 是 agent 档要带的产品参数：
	// 参考实现只在 model_spec 里非空时才写进请求体（client.py:332-335），
	// 值取自目录响应（`kimiPlusId: "ok-computer"`、`agentMode: TYPE_NORMAL/TYPE_ULTRA`）。
	kimiPlusIDAgent = "ok-computer"
	agentModeNormal = "TYPE_NORMAL"
	agentModeUltra  = "TYPE_ULTRA"

	// defaultModel 是模型名认不出来时的兜底档（宁可给默认档，也不让请求失败，
	// 但要能在日志里看出来 —— 与 DeepSeek 渠道同一个处理原则）。
	defaultModel = "kimi-k2.6"
)

const (
	httpTimeout = 10 * time.Minute

	// defaultMinIntervalSec 是同账号最小请求间隔出厂默认（秒）。
	//
	// 参考实现默认「并发 2、最小间隔 0.5s」。我们取更保守的值：网页端反爬按账号算，
	// 同号猛打既容易撞限流，也是被封号最直接的线索。
	defaultMinIntervalSec = 2
)

// fakeHeaders 是伪装成 Kimi 网页前端的固定头。
//
// 为什么必须有：这些头在 kimi2api 里是硬编码的实测结论（Accept-Language、R-Timezone、
// Sec-Ch-Ua 一套），缺了会被当成非浏览器客户端。UA 版本号会随上游放宽/收紧而失效 ——
// 换 UA 时只改这里一处。
//
// **故意不抄**参考实现里的 `Accept-Encoding: gzip, deflate, br, zstd`（protocol.py:17）：
// Go 只在「自己加 Accept-Encoding」时才透明解压 gzip，一旦我们手动声明，就得自己解 br/zstd —
// 而这个渠道的响应是二进制帧流，解压错一层会变成「一直转圈」。让 Go 自己协商 gzip 最稳。
var fakeHeaders = map[string]string{
	"Accept":             "*/*",
	"Accept-Language":    "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7",
	"Cache-Control":      "no-cache",
	"Pragma":             "no-cache",
	"Origin":             "https://www.kimi.com",
	"R-Timezone":         "Asia/Shanghai",
	"Sec-Ch-Ua":          `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
	"Sec-Ch-Ua-Mobile":   "?0",
	"Sec-Ch-Ua-Platform": `"Windows"`,
	"Sec-Fetch-Dest":     "empty",
	"Sec-Fetch-Mode":     "cors",
	"Sec-Fetch-Site":     "same-origin",
	"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	// Priority 在参考实现里是硬编码的伪装头（kimi2api app/kimi/protocol.py:34），
	// 属于 Chrome 指纹的一部分，漏掉就是「少一个头」的客户端。
	"Priority":       "u=1, i",
	"X-Msh-Platform": "web",
}

// fallbackModels 是模型目录拉不到时的兜底清单（正常情况下以**上游目录为准**，见 models.go）。
//
// 来源：参考实现公开列出的档位 —— kimi2api README「模型和参数」一节，以及
// tests/test_model_catalog.py 里对目录响应生成规则的断言。Kimi 的档位不是「不同模型」，
// 而是**同一模型的开关组合**（开不开思考、开不开联网搜索）+ agent 档的产品 id，别当成不同模型理解。
//
// 为什么 fallback 也把 agent 档列上：拉目录失败（网络/凭证/上游改版）时若只列基础档，
// 客户端就再也点不到 agent 档；而 agent 档的请求形态与普通档**没有区别**，
// 只是多带 kimiplusId/agentMode 两个字段（client.py:332-335）。能否用是账号订阅的问题，
// 由上游目录回答；兜底表只保证「名字认得出、请求发得对」。
var fallbackModels = []localModel{
	{ID: "kimi-k2.6", Name: "Kimi K2.6",
		Spec: modelSpec{scenario: scenario, known: true}},
	{ID: "kimi-k2.6-thinking", Name: "Kimi K2.6（思考）",
		Spec: modelSpec{scenario: scenario, thinking: true, known: true}},
	{ID: "kimi-k2.6-search", Name: "Kimi K2.6（联网搜索）",
		Spec: modelSpec{scenario: scenario, search: true, known: true}},
	{ID: "kimi-k2.6-thinking-search", Name: "Kimi K2.6（思考 + 联网搜索）",
		Spec: modelSpec{scenario: scenario, thinking: true, search: true, known: true}},
	{ID: "kimi-k2.6-agent", Name: "Kimi K2.6（Agent）",
		Spec: modelSpec{scenario: scenarioOKComputer, kimiPlusID: kimiPlusIDAgent, agentMode: agentModeNormal, known: true}},
	{ID: "kimi-k2.6-agent-swarm", Name: "Kimi K2.6（Agent Swarm）",
		Spec: modelSpec{scenario: scenarioOKComputer, kimiPlusID: kimiPlusIDAgent, agentMode: agentModeUltra, known: true}},
}

// localModel 是模型表的一项：面板形态（ID/Name）+ 对话时要用的档位参数。
// 目录拉回来的和兜底表用的是同一个结构 —— 这样「列表里有的档位」与「Chat 认得出来的档位」
// 天然一致，不会出现「面板能选但发出去是别的档」这种最糟的组合。
type localModel struct {
	ID   string
	Name string
	Spec modelSpec
}

// modelSpec 是一个模型档位解析后的形态。
type modelSpec struct {
	scenario string
	thinking bool
	search   bool
	// kimiPlusID / agentMode 只有 agent 档非空：参考实现在非空时才写进请求体
	// （client.py:332-335，键名是 kimiplusId / agentMode）。
	kimiPlusID string
	agentMode  string
	known      bool
}

// specOf 解析客户端给的模型名（**兜底路径**：目录里的档位先查 specFor 的表，见 models.go）。
//
// 后缀可以叠加且顺序不定（`-thinking-search` 与 `-search-thinking` 等价），逐个剥掉即可。
// 版本号段不参与判断：上游用什么 scenario 由版本决定，我们现在只知道 K2D5 这一个 ——
// 所以**只对认得出的形态放行**，认不出就回落到默认档并由调用方记一笔（不假装认识）。
// agent 档不在这里识别：它的 scenario/kimiPlusId/agentMode 只能从上游目录（或兜底表）知道，
// 光看名字猜会发出一个「agent 名 + 普通场景」的错请求。
func specOf(id string) modelSpec {
	base := strings.ToLower(strings.TrimSpace(id))
	thinking, search := false, false
	for {
		switch {
		case strings.HasSuffix(base, "-thinking"):
			thinking, base = true, strings.TrimSuffix(base, "-thinking")
		case strings.HasSuffix(base, "-search"):
			search, base = true, strings.TrimSuffix(base, "-search")
		default:
			goto done
		}
	}
done:
	rest, ok := strings.CutPrefix(base, "kimi-k")
	if !ok || !isVersion(rest) {
		// 认不出的名字：按默认档（开思考）走，由调用方记一笔，绝不假装认识。
		return modelSpec{scenario: scenario, thinking: true}
	}
	return modelSpec{scenario: scenario, thinking: thinking, search: search, known: true}
}

// isVersion 判断剥掉前缀剩下的段是不是版本号：只由数字与点组成，且至少有一位数字。
// 「kimi-k2.6-vision」这类带别名的形态不认 —— 形态不明的档位可能落到别的上游能力上，
// 猜错比不认更贵。
func isVersion(s string) bool {
	if s == "" || !strings.ContainsAny(s, "0123456789") {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}
