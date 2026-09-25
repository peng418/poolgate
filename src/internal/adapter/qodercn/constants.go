// Package qodercn 是 PoolGate 的 QoderCN 渠道适配器：实现 channel.Channel，
// 把 QoderCN 上游协议（COSY 签名、QoderEncoding、嵌套 SSE 信封）消化成本地标准
// OpenAI chunk 流。协议实现移植自 wild-work internal/qodercn（已实测跑通），
// 适配 PoolGate 的 channel/errs 契约。
package qodercn

// 上游域名与端点（CN only）。
const (
	OpenAPIBase = "https://openapi.qoder.com.cn" // 业务 API（dt- Bearer，无签名）
	GatewayBase = "https://gateway.qoder.com.cn" // 推理网关（COSY 签名）

	EpQuotaUsage = "/api/v2/quota/usage"
	EpUserInfo   = "/api/v1/userinfo"
	EpDTRefresh  = "/api/v1/deviceToken/refresh"
	EpModels     = "/algo/api/v2/model/list?Encode=1"
	EpChat       = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

	clientUA = "Go-http-client/2.0"
)
