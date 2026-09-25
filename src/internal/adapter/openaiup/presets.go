package openaiup

// presets.go 主流平台的预设模板。
//
// 面板「添加接入源」直接列出来：选中即带出 base_url，并把「免费档能拿到什么 / 免费额度 /
// 要不要实名」写在旁边 —— 用户注册前就该看到这些，省得填完才发现免费层不给旗舰模型。
//
// 数据来源：docs/05-可接入平台调研.md §5 与 §9（官方页面原文，2026-09-25 实测）。
// 请注意这里是**只读的展示数据**，不参与任何鉴权或路由判断。

// Preset 是一个平台的预设。
type Preset struct {
	Name          string   `json:"name"`             // 建议的模型前缀（用户可改）
	DisplayName   string   `json:"display_name"`     // 面板显示名
	BaseURL       string   `json:"base_url"`         // 不含 /chat/completions
	SupportsTools bool     `json:"supports_tools"`   // 该上游是否支持工具调用（决定 Spec.Tools）
	FreeTier      string   `json:"free_tier"`        // 免费档能拿到什么
	RealName      string   `json:"real_name"`        // 实名 / 绑卡要求
	Models        []string `json:"models,omitempty"` // 上游没有 /v1/models 时要手填
	Note          string   `json:"note,omitempty"`   // 其它提醒
}

// Presets 返回全部预设（顺序即面板里的展示顺序：先国内、后海外；先免费额度明确的）。
func Presets() []Preset {
	return []Preset{
		{
			Name: "google", DisplayName: "Google AI Studio（Gemini）",
			BaseURL:       "https://generativelanguage.googleapis.com/v1beta/openai",
			SupportsTools: true,
			FreeTier:      "输入输出 token 全免费；免费层只给 Flash 系（Pro 系标 Not available）",
			RealName:      "不用实名、不绑卡（要 Google 账号）",
			Note:          "限速未公开，只在 AI Studio 控制台可见；免费层的输入会被 Google 用于改进服务",
		},
		{
			Name: "bailian", DisplayName: "阿里百炼（通义千问）",
			BaseURL:       "https://dashscope.aliyuncs.com/compatible-mode/v1",
			SupportsTools: true,
			FreeTier:      "每模型约 100 万 token、90 天有效；免费档就能用 qwen3.8-max 旗舰",
			RealName:      "免费额度不需要实名（超额才要）",
			Note:          "国内直连；qwen3.8-max/flash 是动态限流",
		},
		{
			Name: "openrouter", DisplayName: "OpenRouter",
			BaseURL:       "https://openrouter.ai/api/v1",
			SupportsTools: true,
			FreeTier:      "24 个 :free 模型；20 RPM / 50 次每日（累计充值 $10 后 1000 次/日）",
			RealName:      "不用实名、不绑卡",
			Note:          "最适合当多路兜底：模型多、OpenAI 兼容、支持 tools",
		},
		{
			Name: "glm", DisplayName: "智谱 GLM",
			BaseURL:       "https://open.bigmodel.cn/api/paas/v4",
			SupportsTools: true,
			FreeTier:      "GLM-4.7-Flash 永久免费（200K 上下文）",
			RealName:      "需实名",
		},
		{
			Name: "volc", DisplayName: "火山方舟（豆包）",
			BaseURL:       "https://ark.cn-beijing.volces.com/api/v3",
			SupportsTools: true,
			FreeTier:      "每个模型送 50 万 tokens",
			RealName:      "需实名",
			Note:          "豆包模型的官方出口；模型名要用方舟的 endpoint id 或模型名",
		},
		{
			// 名字用 deepseek-api 而不是 deepseek：deepseek 已经被「DeepSeek 网页版」
			// 这个登录式渠道占用了（0.4.9 起）。两者是同一个上游的两条路：
			// 网页登录态走内置渠道，官方 API（按量付费）走接入源 —— 前缀必须能区分开。
			Name: "deepseek-api", DisplayName: "DeepSeek 官方 API",
			BaseURL:       "https://api.deepseek.com/v1",
			SupportsTools: true,
			FreeTier:      "无免费额度（按量付费）",
			RealName:      "注册即用",
			Note:          "与内置的「DeepSeek 网页版」渠道是两个上游：这条要 API Key，按量付费",
		},
		{
			Name: "moonshot", DisplayName: "Kimi（Moonshot）",
			BaseURL:       "https://api.moonshot.cn/v1",
			SupportsTools: true,
			FreeTier:      "少量体验金",
			RealName:      "需实名",
		},
		{
			Name: "siliconflow", DisplayName: "硅基流动 SiliconFlow",
			BaseURL:       "https://api.siliconflow.cn/v1",
			SupportsTools: true,
			FreeTier:      "注册送额度；部分模型免费",
			RealName:      "需实名（实名后才能用全部免费模型）",
		},
		{
			Name: "modelscope", DisplayName: "魔搭 ModelScope",
			BaseURL:       "https://api-inference.modelscope.cn/v1",
			SupportsTools: true,
			FreeTier:      "每日免费额度",
			RealName:      "需实名",
		},
		{
			Name: "minimax", DisplayName: "MiniMax",
			BaseURL:       "https://api.minimaxi.com/v1",
			SupportsTools: true,
			FreeTier:      "体验金",
			RealName:      "注册即用",
		},
		{
			Name: "groq", DisplayName: "Groq",
			BaseURL:       "https://api.groq.com/openai/v1",
			SupportsTools: true,
			FreeTier:      "免费档（限速）",
			RealName:      "不用实名",
			Note:          "速度极快；免费档只给开源权重模型",
		},
		{
			Name: "together", DisplayName: "Together AI",
			BaseURL:       "https://api.together.xyz/v1",
			SupportsTools: true,
			FreeTier:      "注册送 $1",
			RealName:      "需海外支付方式",
		},
		{
			Name: "mistral", DisplayName: "Mistral",
			BaseURL:       "https://api.mistral.ai/v1",
			SupportsTools: true,
			FreeTier:      "免费实验层（限速）",
			RealName:      "需手机验证",
		},
		{
			Name: "xai", DisplayName: "xAI Grok",
			BaseURL:       "https://api.x.ai/v1",
			SupportsTools: true,
			FreeTier:      "无（部分地区有 $150/月额度，需数据共享）",
			RealName:      "需海外支付方式",
		},
		{
			Name: "nvidia", DisplayName: "NVIDIA NIM",
			BaseURL:       "https://integrate.api.nvidia.com/v1",
			SupportsTools: true,
			FreeTier:      "免费 credits",
			RealName:      "不用实名",
		},
		{
			Name: "perplexity", DisplayName: "Perplexity",
			BaseURL:       "https://api.perplexity.ai",
			SupportsTools: true,
			FreeTier:      "无免费层（Pro 订阅者另有额度）",
			RealName:      "需海外支付方式",
			Models:        []string{"sonar", "sonar-pro"},
			Note:          "上游没有 /v1/models，模型名必须手填",
		},
		{
			Name: "custom", DisplayName: "自定义（自己填 base_url）",
			BaseURL:       "",
			SupportsTools: true,
			FreeTier:      "—",
			RealName:      "—",
			Note:          "任何 OpenAI 兼容端点都行（vLLM / LM Studio / 自建网关…）",
		},
	}
}
