// Package chatglm 是「智谱清言（chatglm.cn）网页版」渠道适配器 —— **登录式**（不要 API key）。
//
// 协议取自公开参考实现 GLM-Free-API 的实测结论：
//   - 凭证是浏览器 Cookie 里的 `chatglm_refresh_token`（用户粘一次），服务端拿它换 access_token；
//   - **每个请求都要带一个自算签名**（`X-Sign` / `X-Timestamp` / `X-Nonce`，见 sign.go）——
//     这是本渠道最脆的一环：官网改算法或换密钥，签名就会失效；
//   - 对话端点是 `backend-api/assistant/stream`，请求体只接受**一条** user 消息（历史要拼成一段文本）；
//   - 响应是 SSE，每帧带 `parts[]`（按 logic_id 增量更新），正文在 item.type=="text"，
//     思考在 item.type=="think" → reasoning_content；
//   - **上游没有原生工具调用** → 本渠道走 toolshim 模拟层。
package chatglm

import (
	"strings"
	"time"
)

const (
	apiBase = "https://chatglm.cn"

	// epRefresh 用 refresh_token 换 access_token。
	epRefresh = "/chatglm/user-api/user/refresh"
	// epStream 是对话端点（SSE）。
	epStream = "/chatglm/backend-api/assistant/stream"
	// epDelete 删除会话。参考实现每轮对话后都会删（免得在用户的智谱对话列表里堆记录）。
	//
	// 我们**暂不调用它**：它的请求体形态在我们参考的版本里没有被验证过（路径存在，参数按猜的写）。
	// 宁可留下会话记录，也不要发一个没验证过的请求去改用户账号上的数据 ——
	// 真要清理时，先确认参数形态再打开这里。
	epDelete = "/chatglm/backend-api/assistant/conversation/delete"

	// defaultAssistantID 是「GLM4」默认智能体 id（参考实现里的默认值）。
	// 上游其实是用 assistant_id 决定用哪个智能体/模型档位。
	defaultAssistantID = "65940acff94777010aa6b796"

	// signSecret 是签名密钥。参考实现源码里写明「官网变化记得更新」——
	// 也就是说它是**会变**的，所以做成可覆盖：凭证 Extra["sign_secret"] 优先，
	// 其次是这个编译进二进制的默认值（改默认值需要重新发版）。
	signSecret = "8a1317a7468aa3ad86e997d08f3f31cb"

	// accessTokenTTL 是 access_token 的缓存时长（参考实现按 3600 秒缓存）。
	accessTokenTTL = 3600 * time.Second

	httpTimeout = 10 * time.Minute

	// defaultMinIntervalSec 是同账号最小请求间隔出厂默认（秒）。
	// 网页端反爬按账号算，同号猛打既容易撞限流，也是被封号最直接的线索。
	defaultMinIntervalSec = 3
)

// fakeHeaders 是伪装成智谱清言网页前端的固定头（照参考实现抄）。
//
// 其中 X-Exp-Groups 是一长串实验分组标记：看着像噪声，但它是网页客户端身份的一部分，
// 少了对不上官网的形态。X-App-Fr 写 browser_extension 也是网页插件客户端的真实取值。
var fakeHeaders = map[string]string{
	"Accept":             "text/event-stream",
	"Accept-Language":    "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6",
	"App-Name":           "chatglm",
	"Cache-Control":      "no-cache",
	"Origin":             "https://chatglm.cn",
	"Pragma":             "no-cache",
	"Sec-Ch-Ua":          `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
	"Sec-Ch-Ua-Mobile":   "?0",
	"Sec-Ch-Ua-Platform": `"Windows"`,
	"Sec-Fetch-Dest":     "empty",
	"Sec-Fetch-Mode":     "cors",
	"Sec-Fetch-Site":     "same-origin",
	"X-App-Fr":           "browser_extension",
	"X-App-Platform":     "pc",
	"X-App-Version":      "0.0.1",
	"X-Device-Brand":     "",
	"X-Device-Model":     "",
	"X-Exp-Groups": "na_android_config:exp:NA,na_4o_config:exp:4o_A,tts_config:exp:tts_config_a," +
		"na_glm4plus_config:exp:open,mainchat_server_app:exp:A,mobile_history_daycheck:exp:a," +
		"desktop_toolbar:exp:A,chat_drawing_server:exp:A,drawing_server_cogview:exp:cogview4," +
		"app_welcome_v2:exp:A,chat_drawing_streamv2:exp:A,mainchat_rm_fc:exp:add,mainchat_dr:exp:open," +
		"chat_auto_entrance:exp:A,drawing_server_hi_dream:control:A,homepage_square:exp:close," +
		"assistant_recommend_prompt:exp:3,app_home_regular_user:exp:A,memory_common:exp:enable," +
		"mainchat_moe:exp:300,assistant_greet_user:exp:greet_user,app_welcome_personalize:exp:A," +
		"assistant_model_exp_group:exp:glm4.5,ai_wallet:exp:ai_wallet_enable",
	"X-Lang": "zh",
}

// userAgents 是一个小型 UA 池：每次请求随机取一个。
// 单一 UA 高频出现是最容易被风控认出来的形态，参考实现也是这么做的。
var userAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:124.0) Gecko/20100101 Firefox/124.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:124.0) Gecko/20100101 Firefox/124.0",
}

// webModels 是面板下发的档位。
//
// 上游的「思考 / 沉思」不是不同的模型，而是请求体里的 chat_mode（zero / deep_research）——
// 参考实现靠**模型名里带不带 think/zero/deepresearch** 来判定。我们把这个约定显式列出来，
// 让用户在客户端里能直接选到（否则他只能猜该写什么名字）。
var webModels = []struct {
	ID    string
	Name  string
	Think bool
	Deep  bool
}{
	{ID: "glm-4.7", Name: "GLM-4.7"},
	{ID: "glm-4.7-think", Name: "GLM-4.7（推理模式）", Think: true},
	{ID: "glm-4.7-deepresearch", Name: "GLM-4.7（沉思 / DeepResearch）", Deep: true},
	{ID: "glm-4.6", Name: "GLM-4.6"},
	{ID: "glm-4.6v", Name: "GLM-4.6V（视觉模型，本渠道只走文本）"},
}

// modelSpec 是一个档位解析后的形态。
type modelSpec struct {
	assistantID string
	chatMode    string // "" | "zero" | "deep_research"
	thinking    bool
}

// specOf 解析客户端给的模型名。
//
// 两条规则都来自参考实现的实测行为：
//   - 名字是 24 位以上的小写字母数字 → 当成**智能体 id**（用户可以把任意智能体接进来）；
//   - 名字里含 think/zero → chat_mode=zero（推理）；含 deepresearch → deep_research（沉思）。
func specOf(model string) modelSpec {
	s := strings.ToLower(strings.TrimSpace(model))
	out := modelSpec{assistantID: defaultAssistantID}
	if isAgentID(s) {
		return modelSpec{assistantID: s}
	}
	switch {
	case strings.Contains(s, "deepresearch"):
		out.chatMode, out.thinking = "deep_research", true
	case strings.Contains(s, "think"), strings.Contains(s, "zero"):
		out.chatMode, out.thinking = "zero", true
	}
	return out
}

// isAgentID 判断是不是 24 位以上的小写字母数字串（上游的智能体 id 形态）。
func isAgentID(s string) bool {
	if len(s) < 24 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
