// Package channel 定义渠道适配器契约与能力声明（需求 F2.1/F2.2）。
//
// 红线三：渠道可插拔。核心层零渠道专有代码 —— 新增渠道只需实现 Registry 接口
// 加一行注册；摘除渠道只需把 Spec.Status 改为 Paused（或删掉注册行）。
// 这正是千问办公上游改版时需要的操作粒度（见 docs/03-渠道能力矩阵.md §4）。
package channel

import (
	"context"
	"strings"
	"time"

	"poolgate/internal/errs"
)

// Kind 是渠道的唯一标识，同时用作模型前缀，如 "qodercn/claude-sonnet-4.5"。
type Kind string

// 渠道大类（面板分组用）。
const (
	CategoryCoding = "coding" // 编程助手 / IDE 类
	CategoryChat   = "chat"   // 聊天平台类
)

const (
	QoderCN     Kind = "qodercn"
	WorkBuddyCN Kind = "workbuddy"
	TraeWork    Kind = "traework"
	// 后三家：协议独立实现，与首发三家并存（2026-09-24 全部接入）。
	QwenWork Kind = "qwenwork" // 千问办公（网页端 chat-ws 协议）
	// 通义（Qwen 网页/CLI 渠道）：与千问办公不是一回事 —— 前者是通义官网/Qwen Code CLI，
	// 后者是「千问办公」那个白领助手产品。两个上游协议完全不同，别混。
	Qwen Kind = "qwen"
	// Gemini（Google）：走 Code Assist CLI 的**官方 OAuth**（登录式，不用 key）。
	Gemini Kind = "gemini"
	// DeepSeek（网页版）：登录式（粘贴 userToken），工具调用由网关模拟。
	DeepSeek Kind = "deepseek"
	// Kimi（网页版）：登录式（粘贴浏览器里的 refresh token），工具调用由网关模拟。
	// 与「账号池里的 Kimi」是同一件事：它没有 API Key 形态，只有网页登录态这一条路。
	Kimi Kind = "kimi"

	// ---- 以下为「网页版/客户端登录式」渠道 ----
	//
	// 命名前缀的原则：**一个上游一个前缀**，且不与「接入源」里的 API Key 式来源撞名。
	// 例如智谱清言用 chatglm（域名），把 glm 留给 BigModel 官方 API 那个来源，
	// 这样模型名 `chatglm/glm-4.7` 与 `glm/glm-4.7` 分别指向「网页登录态」与「API Key」，
	// 用户在客户端里一眼能分清自己用的是哪一条。
	ChatGLM     Kind = "chatglm"     // 智谱清言（chatglm.cn 网页版）
	Doubao      Kind = "doubao"      // 豆包（网页版）
	Yuanbao     Kind = "yuanbao"     // 腾讯元宝（网页版）
	CodeBuddy   Kind = "codebuddy"   // 腾讯 CodeBuddy（编程助手，与 WorkBuddy 同族）
	Copilot     Kind = "copilot"     // GitHub Copilot（订阅登录）
	Windsurf    Kind = "windsurf"    // Windsurf / Codeium
	Kiro        Kind = "kiro"        // AWS Kiro
	IFlow       Kind = "iflow"       // iFlow CLI
	Lingma      Kind = "lingma"      // 通义灵码
	Cursor      Kind = "cursor"      // Cursor
	Antigravity Kind = "antigravity" // Google Antigravity
	ChatGPT     Kind = "chatgpt"     // ChatGPT（网页版）
	Anthropic   Kind = "anthropic"   // Anthropic（订阅 OAuth 登录）

	QoderCOM    Kind = "qodercom"    // QoderCOM 国际版（COSY 同框架，仅域名不同）
	WorkBuddyAI Kind = "workbuddyai" // WorkBuddy 国际版（独立域名与模型表）
)

// Status 是渠道的运行状态。暂停的渠道保留账号与适配器，但不向客户端下发模型。
type Status string

const (
	Active      Status = "active"
	Paused      Status = "paused"
	Maintenance Status = "maintenance"
)

// Credential 是一个账号在上游的身份凭证。
//
// 安全约束（D5）：凭证加密落盘、权限 600、永不进入日志与默认导出。
// 因此这里不实现 String()，避免被 fmt 意外打印出来。
type Credential struct {
	UID          string    // 上游账号唯一标识
	Nickname     string    // 展示名（可为空，回退到 UID）
	AccessToken  string    // 访问令牌
	RefreshToken string    // 刷新令牌（可为空）
	ExpiresAt    time.Time // 过期时间（零值表示未知）
	Extra        map[string]string
}

// AccountState 是账号运行态的最小事实集：谁、能不能用、为什么、什么时候可能恢复。
//
// 为什么要它：面板上「该渠道无可用账号，无法探测」这种结论必须能自己解释原因 ——
// 「在冷却，到 XX 点自己恢复」和「已禁用，要重新授权」是两种完全不同的处置，
// 只给一句「无可用账号」，用户连该等还是该动手都判断不了
// （实测 2026-09-26：面板就只有那一句，用户无从下手）。
type AccountState struct {
	UID      string
	Disabled bool
	Reason   string    // 触发 Disabled 或冷却的原因（errs.Kind 字符串）
	Until    time.Time // 冷却截止；零值 = 未冷却
	// SoftCooling 为真表示这是「推断类软冷却」（连接中断 / 空流）：该号仍会被选用，
	// 一次成功即自动解除 —— 面板要把它显示成「降级使用」而不是「下线」（④）。
	SoftCooling bool
	// ErrCount / ErrThreshold 是「连续错误 n/m」的展示口径（④）。
	// 门槛机制：推断类错误连续到 ErrThreshold 次才冷却（成功即清零）。
	ErrCount     int
	ErrThreshold int
}

// Balance 是账号余额快照。未知字段保持零值，由 UI 显示「未知」而不是猜数字（F4.2）。
type Balance struct {
	Credits   int64
	ExpiresAt time.Time
	Known     bool // false 表示上游没给出可信余额
}

// CheckinResult 是签到结果。
type CheckinResult struct {
	OK         bool
	Message    string // 上游原话摘要
	Reward     string // 如 "+100 Credits"
	NoActivity bool   // 该渠道无签到活动（UI 显示「无活动」，不显示为失败）
}

// ModelInfo 描述一个模型及其能力位。来源必须标注，未知时不猜数字。
type ModelInfo struct {
	ID            string
	DisplayName   string
	ContextWindow int
	Source        Source // 能力位的来源
	Tools         Cap
	Images        Cap
	Reasoning     Cap
}

// Source 标注数据来源，防止把估算值当上游值展示。
type Source string

const (
	SourceUpstream Source = "upstream" // 上游明确给出
	SourceLocal    Source = "local"    // 本地能力声明推导
	SourceUnknown  Source = "unknown"  // 未知
)

// Cap 是三态能力位：支持 / 不支持 / 未知。用三态而非布尔，
// 是因为「不知道」和「不支持」在网关清洗时处置不同（后者可直接拒绝，前者不猜）。
type Cap int8

const (
	CapUnknown Cap = 0
	CapYes     Cap = 1
	CapNo      Cap = -1
)

// Spec 能力声明。UI、网关清洗、模型下发全部读它 —— 不读渠道专有逻辑。
type Spec struct {
	Kind        Kind
	DisplayName string
	Status      Status

	Tools bool
	// ToolsShim 表示上游没有原生工具调用，但**网关会代做模拟**：
	// 把 tools 定义翻成提示词发给上游，再把模型输出的标记解析回结构化 tool_calls
	// （见 internal/toolshim）。对客户端来说仍是标准的 tools/tool_calls 合同。
	//
	// 与 Tools 互斥使用：Tools=true 走原生透传；两个都 false 时网关明确拒绝带 tools
	// 的请求（F3.7）—— 静默丢弃会让模型把工具调用写成文本，客户端只看到乱码。
	ToolsShim bool
	// ToolsIgnore 表示上游协议里**没有放工具定义的位置**，但该渠道的定位就是「纯文本入口」：
	// 客户端（coding agent）习惯性带上 tools 时**不报错**，网关把这批 tools 丢掉、
	// 按纯文本转发，客户端拿到的是模型的自然语言回答（不会有 tool_calls）。
	//
	// 用它的前提是「本渠道的工具不需要我们代做」—— 千问办公就是这一类：工具由上游
	// 自己那套内置能力承担，客户端塞进来的工具定义它本来也不认。
	// 打开它必须同时落一条日志（不静默，见 gateway 的 tools 分支），否则客户端会
	// 以为自己的工具真的挂上了。
	//
	// 与 Tools / ToolsShim 互斥；三者都 false 时网关**明确拒绝**（默认档，F3.7）。
	ToolsIgnore bool
	Images      bool
	Reasoning   bool
	SSEOnly     bool // 上游只有流式 → 非流式由本地聚合成
	CheckinCap  bool // 是否支持签到
	// Category 是渠道的「大类」，面板按它分组展示：互相隔离、不混在一起。
	//
	//   CategoryCoding —— 编程助手 / IDE 类（订阅额度池，扫码或设备码授权后按额度用）
	//   CategoryChat   —— 聊天平台类（网页/CLI 登录的通义、元宝、豆包这种）
	//
	// 分开的理由不是好看：两类的**额度模型、风控强度、能不能调工具**都不同，
	// 混在一起用户会拿「余额」去理解「限速」，也会以为它们可以互相顶替。
	Category string
	// DefaultMinIntervalSec 是该渠道「同一账号两次请求的最小间隔」的**出厂默认**（秒）。
	//
	// 为什么放 Spec 而不是只放设置：网页渠道（反爬敏感）天生就该慢一点，
	// 默认值为 0 等于把「设一个安全节奏」这件事推给用户，而用户不知道自己在冒什么险。
	// 设置里的 account_min_interval_sec 可覆盖它（见 docs/06 防封号设计）。
	DefaultMinIntervalSec int
	// RefreshCadence 是「主动续期的节奏」：0 = 老行为（只在临期时续）；非 0 = 距上次成功续期
	// 满这个时长就续一次（请求路径 + 后台心跳都按它判）。
	//
	// 为什么要有这个声明（2026-09-27 真机事故）：有的渠道发的是**一次性 refresh token**
	// （每续一次就轮换一次），闲置久了还会失效（实测 ~31 小时后上游回 invalid_grant
	//「令牌未激活」），所以必须定期去续；而它的 expires_in(≈10 分钟) 只是「多久该换一次」的提示，
	// 不能当到期时间用（真实寿命在令牌的 JWT exp 里）。有了这个声明，池子才能**按渠道**决定
	// 什么时候续，而不是「按请求」——后者会把一次性 rt 轮换到断链。
	RefreshCadence time.Duration

	Docs string // 渠道说明链接（可为空）
}

// Downstream 为 true 表示该渠道当前应向客户端下发模型。
func (s Spec) Downstream() bool { return s.Status == Active }

// ChatRequest 是一次对话请求的标准形态。上游协议差异（COSY 签名、信封、
// QoderEncoding 编码、嵌套 SSE）全部由适配器消化，网关与路由只见到这个结构。
//
// 字段是 OpenAI chat.completions 请求的归一子集：文本对话 + 工具调用。
// 图片仍按 Spec 能力位做清洗（F3.7）。
type ChatRequest struct {
	Model       string // 客户端请求的模型名（含渠道前缀剥离后的部分）
	Messages    []Message
	MaxTokens   int
	Temperature *float64
	Stream      bool // 是否要求流式返回

	// Tools 是客户端（OpenAI 形态）的工具定义，适配器原样放进上游请求体。
	// 为空 = 客户端没要工具调用。渠道不支持时必须由网关**明确拒绝**，
	// 不允许静默丢弃 —— 丢掉 tools 会让上游把工具调用写成普通文本，
	// 客户端拿不到 tool_calls，表现成「模型不回复/回一堆乱码」（红线一、F3.7）。
	Tools []map[string]any
	// ToolChoice 是 tool_choice 原样值（"auto" / "none" / {"type":"function",…}）。
	// nil = 客户端没指定，按上游默认。
	ToolChoice any
}

// ForwardTools 返回应当转发给上游的工具定义。
//
// 唯一的过滤是「客户端显式说了 tool_choice:"none"」—— 那是明确的「本轮不要用工具」，
// 不是静默丢弃：网关已按渠道能力位校验过，能走到这里的渠道都是支持工具的。
func (r ChatRequest) ForwardTools() []map[string]any {
	if len(r.Tools) == 0 || toolChoiceIsNone(r.ToolChoice) {
		return nil
	}
	return r.Tools
}

// toolChoiceIsNone 判断 tool_choice 是否显式要求「不用工具」。
func toolChoiceIsNone(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "none")
	case map[string]any:
		s, _ := t["type"].(string)
		return strings.EqualFold(strings.TrimSpace(s), "none")
	}
	return false
}

// Message 是一条对话消息。role 取 system / user / assistant / tool；
// 适配器负责把 developer 改写成 system（qodercn 的实测要求）。
type Message struct {
	Role    string
	Content string

	// ToolCalls 仅 assistant 消息使用：本轮的若干个工具调用，回传给上游时
	// 必须原样带上，否则模型看到的是「自己上次什么都没调用」，会重复调用同一个工具。
	ToolCalls []ToolCall
	// ToolCallID 仅 role=="tool" 使用：这条结果对应哪一次工具调用。
	ToolCallID string
	// Name 仅 role=="tool" 使用：函数名（部分上游要求 tool 消息带 name）。
	Name string
}

// ToolCall 是标准 OpenAI tool_call（工具调用）。适配器把上游任意工具调用形态
// （如 Trae 的 function_call 字段）归一成这个结构。
type ToolCall struct {
	// Index 是流式分片里的序号（OpenAI delta 规范）：同一个工具调用的 name 与
	// arguments 可能分几片到达，客户端靠它拼回去。**不 omitempty** —— OpenAI 的
	// 流式帧里 index=0 也是显式出现的，缺了它客户端可能把分片当成多个调用。
	Index    int          `json:"index"`
	ID       string       `json:"id"`
	Type     string       `json:"type"` // 恒 "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall 是工具调用的函数名与参数。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
}

// ParseOpenAIToolCalls 解析 OpenAI 形态的 tool_calls 数组：
// [{"index":0,"id":"call_x","type":"function","function":{"name":…,"arguments":…}}]。
// 上游本身是 OpenAI 兼容 SSE 的适配器（WorkBuddy 两家、COSY 两家）共用它，
// 顺带兼容 SOLO 的 "function_call" 键名 —— 少写四份几乎一样的解析。
func ParseOpenAIToolCalls(raw any) []ToolCall {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]any)
		if !ok {
			fn, ok = m["function_call"].(map[string]any)
		}
		if !ok {
			continue
		}
		tc := ToolCall{Type: "function"}
		if v, ok := m["id"].(string); ok {
			tc.ID = v
		}
		if v, ok := m["type"].(string); ok && v != "" {
			tc.Type = v
		}
		if v, ok := m["index"].(float64); ok {
			tc.Index = int(v)
		}
		if v, ok := fn["name"].(string); ok {
			tc.Function.Name = v
		}
		if v, ok := fn["arguments"].(string); ok {
			tc.Function.Arguments = v
		}
		out = append(out, tc)
	}
	return out
}

// ChunkChoice 是 ChatCompletionChunk.Choices 的元素类型（具名，便于适配器复用）。
type ChunkChoice struct {
	Index int `json:"index"`
	Delta struct {
		Role             string     `json:"role,omitempty"`
		Content          string     `json:"content,omitempty"`
		ReasoningContent string     `json:"reasoning_content,omitempty"`
		ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	} `json:"delta"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// ChatCompletionChunk 是标准 OpenAI chat.completion.chunk 的流式单元。
// 适配器把上游任意形态归一成这个结构，核心层只认它（F2.4）。
type ChatCompletionChunk struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []ChunkChoice  `json:"choices"`
	Usage   map[string]any `json:"usage,omitempty"`
}

// Stream 是一次流式对话的读取接口：每次 Next 返回一个归一化 chunk；
// 流结束返回 io.EOF。协议差异、信封剥壳、[DONE] 判读都发生在 Next 内部。
type Stream interface {
	Next() (ChatCompletionChunk, error)
	Close() error
}

// Channel 是一个渠道的完整能力面。实现方只关心「怎么和这个上游说话」。
//
// 单向依赖：Adapter 不知道池里有几个号，不知道 HTTP 之外的任何东西。
type Channel interface {
	Kind() Kind
	Spec() Spec

	// 凭证生命周期。Login 可能需要弹浏览器（OAuth 授权码/设备流）。
	Login(ctx context.Context) (*Credential, error)
	Refresh(ctx context.Context, c *Credential) (*Credential, error)

	// 目录与账务
	Models(ctx context.Context, c *Credential) ([]ModelInfo, error)
	Balance(ctx context.Context, c *Credential) (Balance, error)
	Checkin(ctx context.Context, c *Credential) (CheckinResult, error)

	// Chat 发起一次对话，返回归一化的标准 OpenAI chunk 流（流式/非流式均走 Stream，
	// SSEOnly 渠道由适配器保证 chunk 化）。req.Model 是剥离渠道前缀后的模型名。
	// 错误应归一为 errs.Error —— 见 Classify。
	Chat(ctx context.Context, c *Credential, req ChatRequest) (Stream, error)

	// Classify 把上游错误归一成有限枚举 —— D4 的落点。
	// classify 失败时也应返回 Parse 而不是 nil，避免上层把未知当成功。
	Classify(status int, body []byte) errs.Kind
}
