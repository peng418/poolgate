package windsurf

// protocol.go GetChatMessageRequest / GetUserStatusRequest 的构造，以及
// GetChatMessageResponse 的字段号常量。
//
// 字段号全部来自参考实现（WindsurfAPI/src/devin-connect.js 的实测抓包结论），
// 不是「按名字顺序猜的」——这两者差别很大：prost 允许字段号留空，声明顺序 ≠ wire tag。
// 凡是参考实现自己都标了「未确认/来自别人 .proto 的声明顺序」的坐标，我们一律不发
// （见包注释要点 3）。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"poolgate/internal/channel"
)

// GetChatMessageRequest 的顶层字段号（参考实现 buildGetChatMessageRequest）。
//
// 只列我们真的会写的：
//
//	#1  ClientMetadata（鉴权元数据，含 session token 与 732 hex 指纹）
//	#2  system_prompt（system 消息全部上提到这里，不再出现在 #3 序列里）
//	#3  repeated ChatMessage（对话正文）
//	#7  常量 5
//	#8  CompletionConfig（max_tokens / 上下文 / 温度 …）
//	#10 repeated ToolDef（原生工具定义，见下）
//	#15 ModelConfig（会话配置 id + 轮次）
//	#16 session_id（会话 id，字符串）
//	#20 常量 1
//	#21 model selector（字符串，如 "swe-1-6-slow"）
//
// 故意**不写**的字段：
//   - #11/#12 tool_choice / disable_parallel（参考实现里也是默认关的**未确认坐标**，
//     它自己注明是「别人 .proto 的声明顺序」而非抓包，我们同样不猜）；
//   - #13 system_prompt_cache_options（同上，默认关）；
//   - #22 request_id（实测只有第 2 轮起才出现，我们无状态、按第 1 轮形态发）。
const (
	reqFieldMetadata    = 1
	reqFieldSystem      = 2
	reqFieldChatMessage = 3
	reqFieldConst7      = 7
	reqFieldCompletion  = 8
	reqFieldTools       = 10
	reqFieldModelConfig = 15
	reqFieldSessionID   = 16
	reqFieldConst20     = 20
	reqFieldModel       = 21
)

// ToolDef（请求 #10 里每个工具定义）的子 tag。
//
// ★ 来源：参考实现 WindsurfAPI/src/devin-connect.js:394-397 的 DEFAULT_DEF_TAGS ——
// 它标注为**付费实弹标定**（2026-07-04，teams 账号 claude-opus-4-8）：按
// name=1 / description=2 / parameters=3 发原生 ToolDef，#10 里的定义被模型正确理解、
// 并真的回出了原生 tool_calls（见该文件 :359-381 的结论段）。所以这是**已标定**的
// tag，不是猜的（schema 是 parameters 的别名，同号，:433）。
//
// 于是我们不再走 toolshim，改为原生透传（Spec.Tools=true）。
const (
	toolDefFieldName        = 1
	toolDefFieldDescription = 2
	toolDefFieldParameters  = 3
	schemaFieldAlias        = toolDefFieldParameters // 参考实现里 schema 与 parameters 同号
)

// ChatMessage 里原生工具历史用到的字段号（参考实现 encodeChatMessage /
// encodeAssistantToolCall，devin-connect.js:301-357）：
//
//	#6 = 一个 ToolCall 子消息（仅 assistant 轮）：子 tag #1 id / #2 name / #3 argsJSON
//	#7 = tool_call_id（仅 source=4 的 tool_result 轮）
//
// 子 tag 与响应侧 ChatToolCall 的 id/name/arguments_json 同号，是同一套逆向坐标。
const (
	cmFieldToolCall   = 6
	cmFieldToolCallID = 7

	callFieldID        = 1
	callFieldName      = 2
	callFieldArguments = 3
)

// ClientMetadata 的字段号。token 在这里是**单份**（#3），双写只发生在 HTTP 头。
const (
	metaFieldName        = 1
	metaFieldVersion     = 2
	metaFieldToken       = 3
	metaFieldLocale      = 4
	metaFieldPlatform    = 5
	metaFieldVersionAlt  = 7
	metaFieldNameAlt     = 12
	metaFieldFingerprint = 31
)

// CompletionConfig 的字段号（schema 校准值）。
//
// 这里有一个历史坑值得留一笔（照抄参考实现的血泪）：#2 与 #3 曾经标反过，
// 结果「调用方给的 max_tokens」被写进了 max_newlines 那个空转字段，输出上限
// 被钉死在上下文窗口上 —— 表面看是「免费模型不理会 max_tokens」，实际是发错了字段。
// 现在：#2 = max_tokens（真上限），#3 = max_newlines（这里我们放上下文窗口，
// 与参考实现一致：一个大的 max_newlines 是 no-op）。
const (
	compFieldVersion     = 1
	compFieldMaxTokens   = 2
	compFieldMaxNewlines = 3
	compFieldTemperature = 5
	compFieldTopK        = 7
	compFieldTopP        = 8
)

// ChatMessage 的字段号。
//
//	#1 uuid（每条消息一个，随机）
//	#2 source（1=user, 2=assistant, 4=tool_result）
//	#3 text
//
// source=4 是上游客观的第四种 source：走原生工具时，`role:"tool"` 的结果会以
// source=4 发出，并用 #7 回指被调用的那次 tool_call（参考实现 encodeChatMessage 的
// TOOL_RESULT 分支，devin-connect.js:130、:1184-1195）。
const (
	cmFieldUUID   = 1
	cmFieldSource = 2
	cmFieldText   = 3

	sourceUser       = 1
	sourceAssistant  = 2
	sourceToolResult = 4
)

// ModelConfig 的字段号：#1 会话配置 uuid（同一会话内稳定）、#2 轮次、#3 常量 4。
// 我们无状态，每请求都按「第 1 轮」发（与参考实现的 turn-1 抓包一致）。
const (
	mcFieldSessionUUID = 1
	mcFieldTurn        = 2
	mcFieldConst3      = 3
)

// GetChatMessageResponse 的顶层字段号。
//
// ★ 这里必须照参考实现**校准后的常量**，而不是文件头注释里那句旧话：
//
//	devin-connect.js:1333  FIELD = { CONTENT: 3, FINISH: 5, META: 7, REASONING: 9 }
//
// 同一个文件顶部的注释还写着「text deltas in response field #9」，那是**旧代码的误读**，
// 参考实现自己在下标处写了纠正：「Earlier code read #9 as the content — that was the
// thinking stream. The answer the caller actually wants is #3.」历史台账也印证：
// docs/HISTORY-LEDGER-2026-06-connect.md:164「decodeFrame() 把 GetChatMessageResponse
// 拆出原生分离的 #3 content / #9 reasoning / #5 finish / #7 usage」。
//
// 所以：正文在 #3，思考在 #9。把 #9 当正文会让客户端只看到模型的思考草稿。
// 至于「思考签名」那一类（delta_signature / thinking_id）是**另一个未标定的 tag**，
// 参考实现默认不解码，我们同样不猜。
const (
	respFieldContent   = 3
	respFieldFinish    = 5
	respFieldToolCalls = 6
	respFieldMeta      = 7
	respFieldReasoning = 9
)

// ChatToolCall（响应 #6 repeated）的子 tag。
//
// ★ 来源：参考实现 WindsurfAPI/src/devin-connect.js:1489-1492 的 DEFAULT_CALL_TAGS，
// 由 devin.exe 反汇编钉死（encode_raw @0x1442fe1f0 + merge_field 跳转表）：
//
//	outer=6（顶层 repeated delta_tool_calls）, id=1, name=2, arguments_json=3,
//	invalid_json_str=4, invalid_json_err=5, is_custom_tool_call=6
//
// 注意同一份参考实现里有**互相矛盾**的两条备注：文件头 :1462-1470 说「响应侧
// ChatToolCall 没有 name 字段」（只有 id + arguments），而 :1489 又把 name=2 标成
// 反汇编钉死。处置：照抄 name=2，但解不到 name 时用「本轮只下发了一个工具」反查兜底，
// 再退回 "unknown" —— 与参考实现 decodeToolCalls / devin-connect-openai.js:672 一致。
const (
	callFieldInvalidJSONStr = 4
	callFieldInvalidJSONErr = 5
)

// 响应 #7 metadata 子消息里的用量字段号。
//
//	#2 prompt tokens（"新鲜"输入）    #3 completion tokens
//	#4 cache_write tokens            #5 cache_read tokens
//
// #4/#5 是参考实现在**付费账号**上做过 live A/B 校准的（读=5、写=4），免费账号上
// 这两个计数器恒为 0，而 protobuf 不编码 0 值标量，所以字段自然缺席 —— 解不到就是
// 「没有缓存」，不会误判。这里照抄它的读数，让 usage 里的缓存明细与参考实现一致。
const (
	metaUsagePrompt     = 2
	metaUsageCompletion = 3
	metaUsageCacheWrite = 4
	metaUsageCacheRead  = 5
)

// ---------------------------------------------------------------------------
// 构造
// ---------------------------------------------------------------------------

// newDeviceFingerprint 生成 MetaFieldFingerprint（#31）的值：732 个 hex 字符（366 字节）。
//
// 上游只校验长度/形状，不校验内容 —— 所以随机就够（参考实现的默认行为就是每请求
// randomBytes(366)）。这里保持「每请求随机」而不是「按账号派生稳定值」，
// 是与参考实现默认路径字节等价的行为。
func newDeviceFingerprint() string {
	b := make([]byte, fingerprintBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 读失败几乎不可能，但仍不能返回短串（短串会被上游判 internal）。
		// 用一个确定性的兜底填充，绝不 panic、也绝不发短指纹。
		for i := range b {
			b[i] = byte(i * 7)
		}
	}
	return hex.EncodeToString(b)
}

// newUUID 生成一个 RFC4122 v4 字符串（消息 id / 会话 id 用）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", 0)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// buildClientMetadata 组装 ClientMetadata（请求字段 #1）。
//
// token 在这里**单份**（#3）；732 hex 指纹固定在 #31。
func buildClientMetadata(token string) []byte {
	b := make([]byte, 0, 1024)
	b = appendStringField(b, metaFieldName, clientName)
	b = appendStringField(b, metaFieldVersion, clientVersion)
	b = appendStringField(b, metaFieldToken, token)
	b = appendStringField(b, metaFieldLocale, locale)
	b = appendStringField(b, metaFieldPlatform, platform)
	b = appendStringField(b, metaFieldVersionAlt, clientVersion)
	b = appendStringField(b, metaFieldNameAlt, clientName)
	b = appendStringField(b, metaFieldFingerprint, newDeviceFingerprint())
	return b
}

// buildCompletionConfig 组装 CompletionConfig（请求字段 #8）。
func buildCompletionConfig(maxTokens int, temperature *float64) []byte {
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	temp := defaultTemperature
	if temperature != nil {
		temp = *temperature
	}
	if temp < minTemperature {
		temp = minTemperature // 见 minTemperature 的说明：0 会被上游判 internal
	}
	b := make([]byte, 0, 64)
	b = appendVarintField(b, compFieldVersion, 1)
	b = appendVarintField(b, compFieldMaxTokens, uint64(maxTokens))
	b = appendVarintField(b, compFieldMaxNewlines, defaultContextWindow)
	b = appendFixed64Field(b, compFieldTemperature, temp)
	b = appendVarintField(b, compFieldTopK, defaultTopK)
	b = appendFixed64Field(b, compFieldTopP, defaultTopP)
	return b
}

// buildModelConfig 组装 ModelConfig（请求字段 #15）。
func buildModelConfig() []byte {
	b := make([]byte, 0, 48)
	b = appendStringField(b, mcFieldSessionUUID, newUUID())
	b = appendVarintField(b, mcFieldTurn, 1)
	b = appendVarintField(b, mcFieldConst3, 4)
	return b
}

// buildChatMessage 组装一条纯文本 ChatMessage（请求 #3 的一个元素）。
func buildChatMessage(source int, text string) []byte {
	b := make([]byte, 0, len(text)+40)
	b = appendStringField(b, cmFieldUUID, newUUID())
	b = appendVarintField(b, cmFieldSource, uint64(source))
	b = appendStringField(b, cmFieldText, text)
	return b
}

// buildToolCallMessage 组装一条只带原生工具调用的 assistant 轮（ChatMessage #6）。
//
// 参考实现 encodeAssistantToolCall（devin-connect.js:328-357）：请求侧的 ToolCall 是
// protobuf 子消息 {#1 id, #2 name, #3 argsJSON}，**只有 args 的值**是 JSON 字符串。
// 一条消息只放一个 tool_call（参考实现按每个 call 各发一条）。
func buildToolCallMessage(tc channel.ToolCall) []byte {
	args := trimSpace(tc.Function.Arguments)
	if args == "" {
		args = "{}"
	}
	id := trimSpace(tc.ID)
	if id == "" {
		// 没给 id 就补一个：后面的 tool_result 要靠它才能对上号。
		id = "call_" + shortHash(tc.Function.Name+args)
	}
	name := trimSpace(tc.Function.Name)
	if name == "" {
		name = "unknown" // 与参考实现 encodeToolDef/encodeAssistantToolCall 的兜底一致
	}
	sub := make([]byte, 0, len(args)+64)
	sub = appendStringField(sub, callFieldID, id)
	sub = appendStringField(sub, callFieldName, name)
	sub = appendStringField(sub, callFieldArguments, args)

	b := make([]byte, 0, len(sub)+40)
	b = appendStringField(b, cmFieldUUID, newUUID())
	b = appendVarintField(b, cmFieldSource, sourceAssistant)
	b = appendBytesField(b, cmFieldToolCall, sub)
	return b
}

// buildToolResultMessage 组装一条 source=4 的 tool_result 轮。
//
// 参考实现 devin-connect.js:1184-1195：role=tool 的结果以 source=4 发出，文本进 #3，
// 并用 #7 tool_call_id 回指前面那条 assistant #6。
func buildToolResultMessage(callID, text string) []byte {
	if text == "" {
		text = "[tool result]"
	}
	b := make([]byte, 0, len(text)+64)
	b = appendStringField(b, cmFieldUUID, newUUID())
	b = appendVarintField(b, cmFieldSource, sourceToolResult)
	b = appendStringField(b, cmFieldText, text)
	b = appendStringField(b, cmFieldToolCallID, callID)
	return b
}

// buildToolDef 组装一个 ToolDef 子消息（请求 #10 的一个元素）。
//
// ★ 两处「不这么做就会被上游拒掉」的细节（全部抄自参考实现 encodeToolDef，
// devin-connect.js:594-609）：
//
//  1. 顶层 description 写的是**工具名**，不是真实描述。上游 server.codeium.com 会对
//     工具描述做 MCP 指纹匹配，命中已知工具签名就整个请求回 permission_denied
//     （"Unable to process request due to an MCP configuration issue."）。实测 Cursor
//     的 21 个工具里 8 个会触发；模型是按名字识别工具的，描述正文可去（:572-592）。
//  2. parameters 的 JSON Schema 先规整：丢掉 $schema、压平顶层组合子、去掉所有
//     description 注解（同上，指纹需要「描述 + 参数描述」组合才命中，去掉任一层不够）。
//
// 形状不合要求的工具（不是 function / 没名字）直接跳过 —— 宁可少发一个定义，也不发
// 一个会让上游整请求报错的畸形 ToolDef。
func buildToolDef(tool map[string]any) []byte {
	fn, _ := tool["function"].(map[string]any)
	if fn == nil {
		return nil
	}
	name, _ := fn["name"].(string)
	if trimSpace(name) == "" {
		return nil
	}
	b := make([]byte, 0, 256)
	b = appendStringField(b, toolDefFieldName, name)
	if desc, _ := fn["description"].(string); desc != "" {
		b = appendStringField(b, toolDefFieldDescription, name) // ← 写名字，见上
	}
	if params, ok := fn["parameters"]; ok {
		b = appendStringField(b, schemaFieldAlias, string(mustJSON(normalizeToolSchema(params))))
	}
	return b
}

// buildChatRequest 组装完整的 GetChatMessageRequest。
//
// tools 是已经过 ForwardTools() 过滤的客户端工具定义（OpenAI 形态）；为空则不发 #10。
//
// 字段顺序与参考实现一致：除 #10 外按字段号升序，而 #10 ToolDefs 是**追加在最后**的
// （参考实现 devin-connect.js:1279-1315 先拼完 #1..#21 再把 tools push 到末尾——该字段
// 是后加进协议里的）。protobuf 与字段顺序无关，这里照抄它的字节序以便逐字节对照。
func buildChatRequest(token, model, sessionID string, msgs []channel.Message, tools []map[string]any, maxTokens int, temperature *float64) []byte {
	system, chats := packMessages(msgs)
	if sessionID == "" {
		sessionID = newUUID()
	}
	// ★ 空 system + 有 tools 的兜底。参考实现实测（devin-connect.js:1230-1242）：上游对
	// 「声明了 tools 但 system 为空/缺失」的 Claude 系请求直接回 internal error
	// （"an internal error occurred (trace ID …)"）；只要有一个字符的 system 就能过。
	// 纯 API 客户端很容易不发 system，所以这里补一句无害的话。
	if system == "" && len(tools) > 0 {
		system = toolsGuardSystem
	}
	b := make([]byte, 0, 2048)
	b = appendBytesField(b, reqFieldMetadata, buildClientMetadata(token))
	b = appendStringField(b, reqFieldSystem, system) // 即使为空也写（与参考实现一致）
	for _, cm := range chats {
		b = appendBytesField(b, reqFieldChatMessage, cm)
	}
	b = appendVarintField(b, reqFieldConst7, 5)
	b = appendBytesField(b, reqFieldCompletion, buildCompletionConfig(maxTokens, temperature))
	b = appendBytesField(b, reqFieldModelConfig, buildModelConfig())
	b = appendStringField(b, reqFieldSessionID, sessionID)
	b = appendVarintField(b, reqFieldConst20, 1)
	b = appendStringField(b, reqFieldModel, model)
	for _, tool := range tools {
		if td := buildToolDef(tool); td != nil {
			b = appendBytesField(b, reqFieldTools, td)
		}
	}
	return b
}

// toolsGuardSystem 是「有工具但没 system 提示词」时的兜底 system。
// 文案照抄参考实现 devin-connect.js:1241。
const toolsGuardSystem = "You are a helpful assistant. Use the available tools when appropriate."

// buildUserStatusRequest 组装 GetUserStatusRequest（unary，字段 #1 = ClientMetadata）。
func buildUserStatusRequest(token string) []byte {
	return appendBytesField(nil, 1, buildClientMetadata(token))
}

// ---------------------------------------------------------------------------
// 消息编排
// ---------------------------------------------------------------------------

// packMessages 把内部消息契约拆成「系统提示词（#2）+ ChatMessage 序列（#3）」。
//
// 三件必须做对的事：
//  1. system / developer 消息**上提到 #2**，并从对话序列里消失（参考实现的默认形态）。
//     上游对 #2 有独立的策略路径，混在对话里语义不同。
//  2. **合并连续同角色纯文本轮**。上游的请求校验会拒绝「同 source 连续 >= 3 条」
//     （回 invalid_argument / "an internal error occurred"），而客户端把一次回复
//     拆成多条存下来是常见行为。合并后文本完全一致，只是把长度压到阈值以下。
//     带 tool_calls / tool_result 的消息**不合并** —— 那是有结构的轮，合并会改语义。
//  3. 空的 assistant 轮直接丢掉（上游对空 assistant 文本不友好），但 user 轮照发。
//
// 工具历史走**原生**编码（与参考实现的 nativeToolCall 分支一致，devin-connect.js:1146-1219）：
//   - assistant 带 tool_calls → 先发一条纯文本 source=2（若有正文），再为每个 call 发一条
//     只带 #6 的 source=2；两者都不降级成文本（降级会让模型看不懂自己调过什么）。
//   - role=tool 且带 tool_call_id → source=4 的 tool_result 轮（文本进 #3，id 进 #7）。
//     **没有 id** 的 tool 消息无法与调用对上号，才退化成 user 文本（参考实现同款兜底）。
func packMessages(msgs []channel.Message) (system string, out [][]byte) {
	var systems []string
	merged := make([]channel.Message, 0, len(msgs))

	for _, m := range msgs {
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		if role == "system" {
			if t := trimSpace(m.Content); t != "" {
				systems = append(systems, t)
			}
			continue
		}
		m.Role = role
		// 连续同角色、且都是「纯文本」时合并。
		if n := len(merged); n > 0 && sameTextTurn(merged[n-1]) && sameTextTurn(m) && merged[n-1].Role == m.Role {
			a := trimSpace(merged[n-1].Content)
			b := trimSpace(m.Content)
			if b != "" {
				if a != "" {
					merged[n-1].Content = a + "\n\n" + b
				} else {
					merged[n-1].Content = b
				}
			}
			continue
		}
		merged = append(merged, m)
	}

	for _, m := range merged {
		text := trimSpace(m.Content)
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				if text != "" {
					out = append(out, buildChatMessage(sourceAssistant, text))
				}
				for _, tc := range m.ToolCalls {
					out = append(out, buildToolCallMessage(tc))
				}
				continue
			}
			if text == "" {
				continue // 空 assistant 轮：丢掉
			}
			out = append(out, buildChatMessage(sourceAssistant, text))
		case "tool":
			if m.ToolCallID != "" {
				out = append(out, buildToolResultMessage(m.ToolCallID, text))
				continue
			}
			// 没有 id：对不上是哪次调用，只能退化成 user 文本。
			if text == "" {
				text = "[空结果]"
			}
			out = append(out, buildChatMessage(sourceUser, "[tool result]: "+text))
		default:
			// user（以及任何未知 role 的降级）：原样作为 user 轮。
			out = append(out, buildChatMessage(sourceUser, text))
		}
	}
	return joinNonEmpty(systems, "\n"), out
}

// sameTextTurn 判断一条消息是否「可合并的纯文本轮」：user/assistant、无工具字段。
func sameTextTurn(m channel.Message) bool {
	if m.Role != "user" && m.Role != "assistant" {
		return false
	}
	return len(m.ToolCalls) == 0 && m.ToolCallID == ""
}

// ---------------------------------------------------------------------------
// ToolDef 参数 schema 规整（参考实现 normalizeToolSchema 的移植）
// ---------------------------------------------------------------------------

// normalizeToolSchema 把一个 OpenAI/MCP 形态的 function parameters JSON Schema 规整成
// 上游能接受、且不触发 MCP 指纹拦截的最小合法形状。
//
// 逐条对齐参考实现 WindsurfAPI/src/devin-connect.js:462-548：
//   - 非对象 / 数组 / null → 规范的 {type:"object", properties:{}}（:463-466）；
//   - 丢 $schema 元键（上游 schema 从不带它，:468-469）；
//   - 顶层 oneOf/anyOf/allOf 组合子：删掉，并在原本没有 properties 时，从第一个
//     type:object 变体里回收 properties/required/additionalProperties/description
//     （:495-530）——否则把裸组合子硬压成空对象会丢掉真实参数；
//   - 强制 type=object、properties 是对象（:476-481）；
//   - required 必须是「只含真实属性名的 string[]」，否则整个删掉（:482-491）；
//   - 最后递归去掉所有 description 注解（名叫 description 的属性保留，:532-548）。
//
// 入参用 any（不假定形状）：客户端发的 schema 可能是任意 JSON，全部按 map/slice 走。
func normalizeToolSchema(schema any) any {
	obj, ok := schema.(map[string]any)
	if !ok || obj == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		out[k] = v
	}
	delete(out, "$schema")
	stripTopLevelCombinators(out)
	if t, _ := out["type"].(string); t != "object" {
		out["type"] = "object"
	}
	if props, ok := out["properties"].(map[string]any); !ok || props == nil {
		out["properties"] = map[string]any{}
	}
	if req, ok := out["required"]; ok {
		arr, isArr := req.([]any)
		if !isArr {
			delete(out, "required")
		} else {
			props := out["properties"].(map[string]any)
			kept := make([]any, 0, len(arr))
			for _, r := range arr {
				name, isStr := r.(string)
				if !isStr {
					continue
				}
				if _, exists := props[name]; exists {
					kept = append(kept, name)
				}
			}
			if len(kept) > 0 {
				out["required"] = kept
			} else {
				delete(out, "required")
			}
		}
	}
	return stripSchemaDescriptions(out, false)
}

// stripTopLevelCombinators 就地删掉顶层 oneOf/anyOf/allOf，并（仅在根上没有自己的
// properties 时）从第一个 type:object 变体里回收真实参数。与参考实现 or_insert 语义
// 一致：绝不用变体覆盖根上已有的键（devin-connect.js:505-530）。
func stripTopLevelCombinators(out map[string]any) {
	_, hadProps := out["properties"]
	recovered := false
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		variants, ok := out[key].([]any)
		if !ok {
			continue
		}
		delete(out, key)
		if hadProps || recovered {
			continue
		}
		for _, v := range variants {
			ov, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := ov["type"].(string); t != "object" {
				continue
			}
			for _, field := range []string{"properties", "required", "additionalProperties", "description"} {
				if _, has := out[field]; has {
					continue
				}
				if val, exists := ov[field]; exists {
					out[field] = val
				}
			}
			recovered = true
			break
		}
	}
}

// stripSchemaDescriptions 递归去掉 schema 注解里的 description。
//
// 只删「和 type/properties/required 同级的注解 description」，保留名字就叫 description
// 的**属性**（真实参数，如 Cursor 的 Task 工具有一个 description 属性）——靠 inProperties
// 区分层次。数组元素按参考实现重置为 false（devin-connect.js:539-548）。
func stripSchemaDescriptions(value any, inProperties bool) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = stripSchemaDescriptions(v[i], false)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			if k == "description" && !inProperties {
				continue
			}
			out[k] = stripSchemaDescriptions(child, k == "properties")
		}
		return out
	default:
		return value
	}
}

// mustJSON 序列化规整后的 schema。规整后是纯 map/slice/标量，不可能失败；真失败了
// 也返回一个空对象 schema，绝不 panic（这是请求构造热路径）。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func joinNonEmpty(parts []string, sep string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}
