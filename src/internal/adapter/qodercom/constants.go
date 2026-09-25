// Package qodercom 是 PoolGate 的 QoderCOM（国际版，qoder.com / qoder.sh）适配器。
//
// 与 QoderCN 同一套 COSY 框架（公钥、签名串格式、QoderEncoding 完全相同），
// 只有域名表不同 —— 因此实现由 qodercn 代码级复制后改域名，而不是另写一套。
//
// 上游差异（2026-09-24 实测）：
//   - 业务 API openapi.qoder.sh、推理 api1.qoder.sh、模型表 api2.qoder.sh；
//   - 无 legacy daily-check-in 签到路径（实测 404），只有 campaigns 活动路径。
package qodercom

// 上游域名与端点（COM only）。
const (
	OpenAPIBase = "https://openapi.qoder.sh" // 业务 API（dt- Bearer，无签名）
	GatewayBase = "https://api1.qoder.sh"    // 推理网关（COSY 签名）
	ModelsBase  = "https://api2.qoder.sh"    // 模型列表（COSY 签名）

	EpQuotaUsage = "/api/v2/quota/usage"
	EpUserInfo   = "/api/v1/userinfo"
	EpDTRefresh  = "/api/v1/deviceToken/refresh"
	EpPlan       = "/api/v2/user/plan"
	EpModels     = "/algo/api/v2/model/list?Encode=1"
	EpChat       = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

	clientUA = "Go-http-client/2.0"
)
