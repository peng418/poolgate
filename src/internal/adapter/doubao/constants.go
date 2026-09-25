// Package doubao 是「豆包（网页版）」渠道适配器 —— **登录式**（不要 API key）。
//
// 协议取自公开参考实现 doubao2api 的实测结论：
//   - 凭证是浏览器 Cookie 里的 `sessionid` 等一串（用户粘一次），另有设备参数来自 localStorage；
//   - 对话是 `POST /chat/completion?<一长串 query>`，响应是带事件名的 SSE；
//   - **思考与正文靠一个开关块区分**：`block_type=10040` 第一次出现进入思考、第二次出现退出；
//   - **上游没有原生工具调用** → 本渠道走 toolshim 模拟层。
//
// ⚠️ 本渠道有一个**必须说清楚的先天弱点**：字节的请求签名 `a_bogus` 是在浏览器里由它的
// fetch hook 现算的，参考实现是靠真浏览器跑 `fetch()` 才拿到；纯 HTTP 客户端**不带**它。
// 参考实现的纯 HTTP 路径「在上游风控触发之前是可用的」，也就是说：
//   - 能用，但可能被风控拦（表现为 710022002 / 710022004，要求人工验证码）；
//   - 我们**不去伪造** a_bogus（猜一个签名算法只会得到一个更难查的失败），
//     而是把风控拦截明确报出来，让用户知道该做什么（重新登录 / 降低频率）。
package doubao

import "time"

const (
	apiBase = "https://www.doubao.com"
	// epCompletion 是对话端点：注意 query 参数是**签名/风控的一部分**，不能不传。
	epCompletion = "/chat/completion"

	// botID 是「扩展/默认」智能体 id（参考实现里无指定时的取值）。
	botID = "7338286299411103781"

	// 伪装成浏览器版本（参考实现对齐的 Chrome 版本）。
	chromeVersion  = "148.0.0.0"
	chromiumBuild  = "148.0.7816.0"
	pcVersion      = "2.1.7"
	versionCode    = "20800"
	runtimeVersion = "3.5.4"

	httpTimeout = 10 * time.Minute

	// defaultMinIntervalSec 是同账号最小请求间隔出厂默认（秒）。
	// 豆包的风控比 Kimi 更敏感（它要签名），所以节奏更慢一点。
	defaultMinIntervalSec = 5
)

// riskCode 是上游的风控错误码。
//
//	710022002 —— msToken 假/空导致的风控
//	710022004 —— 要求人工过验证码
//
// 两个都不是「等一会儿就好」：前者要清掉伪造的 msToken（我们干脆不传），后者要人去过验证码。
const (
	riskCodeMsToken  = 710022002
	riskCodeCaptcha  = 710022004
	codeLoginExpired = 710012001
)

// webModels 是面板下发的档位。
//
// 上游其实不是「按模型名选模型」，而是靠 `need_deep_think`（0/1/2/3）与 bot_id 决定档位，
// 所以我们把档位映射成可读的名字 —— 用户选的是「要不要深度思考」，不是另一个模型。
var webModels = []struct {
	ID    string
	Name  string
	Think int // need_deep_think
}{
	{ID: "doubao", Name: "豆包（默认）"},
	{ID: "doubao-pro", Name: "豆包 Pro"},
	{ID: "doubao-think", Name: "豆包（深度思考）", Think: 1},
	{ID: "doubao-expert", Name: "豆包（专家模式）", Think: 3},
}

// thinkOf 按档位给出 need_deep_think；认不出的名字回落到默认档（记一笔，不假装认识）。
func thinkOf(id string) (int, bool) {
	for _, m := range webModels {
		if m.ID == id {
			return m.Think, true
		}
	}
	return 0, false
}
