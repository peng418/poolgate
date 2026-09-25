// Package channel 定义渠道适配器契约与能力声明（需求 F2.1/F2.2）。
//
// 红线三：渠道可插拔。核心层零渠道专有代码 —— 新增渠道只需实现 Registry 接口
// 加一行注册；摘除渠道只需把 Spec.Status 改为 Paused（或删掉注册行）。
// 这正是千问办公上游改版时需要的操作粒度（见 docs/03-渠道能力矩阵.md §4）。
package channel

import (
	"context"
	"time"

	"poolgate/internal/errs"
)

// Kind 是渠道的唯一标识，同时用作模型前缀，如 "qodercn/claude-sonnet-4.5"。
type Kind string

const (
	QoderCN     Kind = "qodercn"
	WorkBuddyCN Kind = "workbuddy"
	TraeWork    Kind = "traework"
	// 后三家：协议独立实现，与首发三家并存（2026-09-24 全部接入）。
	QwenWork    Kind = "qwenwork"    // 千问办公（网页端 chat-ws 协议）
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

	Tools      bool
	Images     bool
	Reasoning  bool
	SSEOnly    bool // 上游只有流式 → 非流式由本地聚合成
	CheckinCap bool // 是否支持签到

	Docs string // 渠道说明链接（可为空）
}

// Downstream 为 true 表示该渠道当前应向客户端下发模型。
func (s Spec) Downstream() bool { return s.Status == Active }

// ChatRequest 是一次对话请求的标准形态。上游协议差异（COSY 签名、信封、
// QoderEncoding 编码、嵌套 SSE）全部由适配器消化，网关与路由只见到这个结构。
//
// 字段是 OpenAI chat.completions 请求的归一子集：M1 只发文本对话；
// 工具/图片在后续里程碑按 Spec 能力位做清洗（F3.7）。
type ChatRequest struct {
	Model       string // 客户端请求的模型名（含渠道前缀剥离后的部分）
	Messages    []Message
	MaxTokens   int
	Temperature *float64
	Stream      bool // 是否要求流式返回
}

// Message 是一条对话消息。role 取 system / user / assistant / tool；
// 适配器负责把 developer 改写成 system（qodercn 的实测要求）。
type Message struct {
	Role    string
	Content string
}

// ToolCall 是标准 OpenAI tool_call（工具调用）。适配器把上游任意工具调用形态
// （如 Trae 的 function_call 字段）归一成这个结构。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // 恒 "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall 是工具调用的函数名与参数。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
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
