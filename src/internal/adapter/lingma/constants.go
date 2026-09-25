// Package lingma 是「通义灵码（阿里云 Lingma）」渠道适配器 —— **登录式**（不要 API key）。
//
// 协议取自公开参考实现 lingma-proxy（2026-09）对 lingma.alibabacloud.com 的实测结论：
//   - 鉴权是自研的 **COSY 签名**（同一套体系，QoderCN 渠道也用它，但两者细节不同）：
//     请求头 `Authorization: Bearer COSY.<base64(payload)>.<md5(requestId…)>`，
//     md5 原文是 `payloadBase64 \n cosy_key \n date \n body \n path`，
//     其中 path **必须去掉 `/algo` 前缀**（签名用 `/api/v2/...`，请求打到 `/algo/api/v2/...`）。
//     配套头 `Cosy-Key` / `Cosy-Machineid` / `Cosy-Machineos` / `Cosy-User` / `Cosy-Clientip` 等。
//   - 凭据来自 **IDE 的登录缓存**，不是接口签发：`cache/user` 是 AES-128-CBC 密文
//     （密钥与 IV 都是 machineID 前 16 字节），解出来是 `{key, encrypt_user_info, uid, …}`；
//     machineID 在 `cache/id` 里。所以「登录」= 让用户把这两个文件的内容粘回来，服务端解密。
//   - 对话端点是 `POST /algo/api/v2/service/pro/sse/agent_chat_generation`，
//     响应是**两层嵌套 SSE**：外层 `{"body":"<json字符串>","statusCodeValue":N}`，
//     body 里再套一层 OpenAI 风格的 `choices[].delta`。
//   - 上游**支持原生 tools/tool_calls**（请求体透传、响应解析 `delta.tool_calls`）→ Spec.Tools=true，
//     不走 toolshim 模拟层。
//
// 与 QoderCN 的关系：COSY「算法」同源（md5 原文与 payload 字段逐字一致），但**凭据来源不同** ——
// QoderCN 是 OAuth 设备流现场签发 cosy_key（RSA 包裹的临时密钥），灵码是复用 IDE 已经拿到的那把 key。
// 我们在本包内自成一套 `cosy.go`，与仓库既有惯例一致（qodercn/qodercom 也是各自持有一份，
// 没有导出可复用的实现；见 cosy.go 头部说明）。
//
// 封号风险：这条路的本质是**用第三方客户端复用 IDE 的登录态**，上游的客户端校验与风控都指向
// 「只允许官方 IDE 使用」。所以默认请求间隔取得保守，且 Spec.Docs 里对用户明说风险。
package lingma

import "time"

// 上游域名与端点。
const (
	// baseURL 是官方默认域名（参考实现也支持企业专属域名，这里只做官方默认，
	// 需要换域名时改这一处即可 —— 与 qodercn 的 base/gateway 双字段不同，灵码只有一个域）。
	baseURL = "https://lingma.alibabacloud.com"

	// epChat 对话端点。查询串不参与签名（签名只覆盖 Path），固定带上即可。
	epChat = "/algo/api/v2/service/pro/sse/agent_chat_generation"
	// chatQuery 是对话端点必须带的查询串（参考实现硬编码，缺了上游不认）。
	chatQuery = "?FetchKeys=llm_model_result&AgentId=agent_common"

	// epModels 模型目录：动态拉取，**不写死列表**（模型名与档位随上游账号权益变）。
	epModels = "/algo/api/v2/model/list"
)

// COSY 协议常量。cosyVersion 同时出现在签名 payload 与 `Cosy-Version` 头里，
// 两处必须一致 —— 上游按它校验 payload 形态。
const (
	// cosyVersion 是 IDE 客户端协议版本（参考实现实测值 2.11.2）。
	cosyVersion = "2.11.2"

	// cosyClientType 是 `Cosy-Clienttype`：参考实现为 IDE 形态取 2（QoderCN 走 5）。
	cosyClientType = "2"

	// cosyClientIP 是 `Cosy-Clientip`。参考实现填了一个**保留测试网段**的固定值，
	// 不是真实出口 IP —— 我们不猜、也不填本机地址（那反而会暴露自己不是官方客户端）。
	cosyClientIP = "198.18.0.1"

	// loginVersion 是 `Login-Version` 头（参考实现固定 v2）。
	loginVersion = "v2"

	// appcode 是 `Appcode` 头（参考实现固定 cosy）。
	appcode = "cosy"
)

// userAgent 是我们自己的 UA。参考实现用 "lingma-proxy/remote" 也能过 ——
// 说明上游不强制官方 IDE 的 UA，那就如实标识自己，不冒充（不伪装成 IDE 的 UA）。
const userAgent = "poolgate-lingma/1.0"

const (
	// httpTimeout 给足：长思考 + SSE 慢上游，与 kimi 渠道同一取值理由。
	httpTimeout = 10 * time.Minute

	// defaultMinIntervalSec 是同账号最小请求间隔出厂默认（秒）。
	//
	// 走 IDE 登录态的渠道天生比 API key 渠道更容易被风控盯上（上游只看得到「官方 IDE」的
	// 流量特征）。参考实现默认并发 2，我们取更保守的单号 2 秒 —— 同号猛打是最直接的封号线索。
	defaultMinIntervalSec = 2
)

// 签名与设备相关的小常量。
const (
	// algoPrefix 是路径前缀：请求打 /algo/...，但**签名必须去掉它**。
	// 这是本协议最容易写错的一处 —— 带上 /algo 会得到 403 Signature invalid。
	algoPrefix = "/algo"

	// aesBlockSize 是 AES-128 的块大小，也是 cache/user 的密钥/IV 长度。
	aesBlockSize = 16
)
