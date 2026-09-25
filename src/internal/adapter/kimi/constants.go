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
	// epModels 是模型目录（上游的可用模型清单）。
	epModels = "/apiv2/kimi.gateway.config.v1.ConfigService/GetAvailableModels"
	// epRefresh 用 refresh token 换 access token（GET，Bearer 传 refresh token）。
	epRefresh = "/api/auth/token/refresh"
	// epSubscription 是账号订阅状态（用于校验凭证是否还有效）。
	epSubscription = "/apiv2/kimi.gateway.order.v1.SubscriptionService/GetSubscription"

	// scenario 是对话场景标识。参考实现只有一个常量（SCENARIO_K2D5），
	// 所有模型档位都用它 —— 上游按 models 里的模型名与 scenario 共同决定路由。
	scenario = "SCENARIO_K2D5"

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
	"X-Msh-Platform": "web",
}

// webModels 是面板下发的模型清单（Kimi 的档位不是「模型不同」，而是**同一模型的开关组合**：
// 开不开思考、开不开联网搜索。后缀就是这个意思，别把它当成不同的模型去理解）。
//
// 上游还有 `-agent` / `-agent-swarm` 这类智能体档，协议形态与普通对话不同（超出本适配器范围），
// 故意不列 —— 不下发不存在的档位，比下发一个点了就报错的档位好。
var webModels = []struct {
	ID    string
	Name  string
	Think bool
	// Search 是「开联网搜索」：请求体里 tools 带 TOOL_TYPE_SEARCH 即开。
	Search bool
}{
	{ID: "kimi-k2.6", Name: "Kimi K2.6"},
	{ID: "kimi-k2.6-thinking", Name: "Kimi K2.6（思考）", Think: true},
	{ID: "kimi-k2.6-search", Name: "Kimi K2.6（联网搜索）", Search: true},
	{ID: "kimi-k2.6-thinking-search", Name: "Kimi K2.6（思考 + 联网搜索）", Think: true, Search: true},
}

// modelSpec 是一个模型档位解析后的形态。
type modelSpec struct {
	scenario string
	thinking bool
	search   bool
	known    bool
}

// specOf 解析客户端给的模型名。
//
// 后缀可以叠加且顺序不定（`-thinking-search` 与 `-search-thinking` 等价），逐个剥掉即可。
// 版本号段不参与判断：上游用什么 scenario 由版本决定，我们现在只知道 K2D5 这一个 ——
// 所以**只对认得出的形态放行**，认不出就回落到默认档并由调用方记一笔（不假装认识）。
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
