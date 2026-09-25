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
//	#15 ModelConfig（会话配置 id + 轮次）
//	#16 session_id（会话 id，字符串）
//	#20 常量 1
//	#21 model selector（字符串，如 "swe-1-6-slow"）
//
// 故意**不写**的字段：
//   - #10 tools（ToolDef 的子字段 tag 未标定，猜错静默失败 —— 本渠道走 toolshim）；
//   - #11/#12 tool_choice / disable_parallel（参考实现里也是默认关的未确认坐标）；
//   - #13 system_prompt_cache_options（同上，默认关）；
//   - #22 request_id（实测只有第 2 轮起才出现，我们无状态、按第 1 轮形态发）。
const (
	reqFieldMetadata    = 1
	reqFieldSystem      = 2
	reqFieldChatMessage = 3
	reqFieldConst7      = 7
	reqFieldCompletion  = 8
	reqFieldModelConfig = 15
	reqFieldSessionID   = 16
	reqFieldConst20     = 20
	reqFieldModel       = 21
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
//	#2 source（1=user, 2=assistant；4=tool_result 是上游的另一种 source，
//	           但本渠道把工具结果降级成 user 文本，所以不产出 4）
//	#3 text
const (
	cmFieldUUID   = 1
	cmFieldSource = 2
	cmFieldText   = 3

	sourceUser      = 1
	sourceAssistant = 2
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
	respFieldMeta      = 7
	respFieldReasoning = 9
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

// buildChatMessage 组装一条 ChatMessage（请求 #3 的一个元素）。
func buildChatMessage(source int, text string) []byte {
	b := make([]byte, 0, len(text)+40)
	b = appendStringField(b, cmFieldUUID, newUUID())
	b = appendVarintField(b, cmFieldSource, uint64(source))
	b = appendStringField(b, cmFieldText, text)
	return b
}

// buildChatRequest 组装完整的 GetChatMessageRequest。
//
// 字段顺序按字段号升序，与参考实现的抓包一致。
func buildChatRequest(token, model, sessionID string, msgs []channel.Message, maxTokens int, temperature *float64) []byte {
	system, chats := packMessages(msgs)
	if sessionID == "" {
		sessionID = newUUID()
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
	return b
}

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
// 说明：走 toolshim 时，网关已经把 tools 定义并进一条 system 消息、把工具历史
// 改写成纯文本了（internal/toolshim.BuildRequest）。这里再兜一层 role=tool 与
// assistant.tool_calls 的降级，是为了「客户端只发工具历史、不发 tools」这种
// toolshim 不介入的情况（那时 role=tool 会原样到这里）。
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
			// 把残留的结构化工具调用降级成可读文本（正常情况已被 toolshim 处理）。
			if len(m.ToolCalls) > 0 {
				text = foldToolCalls(text, m.ToolCalls)
			}
			if text == "" {
				continue // 空 assistant 轮：丢掉
			}
			out = append(out, buildChatMessage(sourceAssistant, text))
		case "tool":
			// 工具结果没有独立 role：拼成 user 文本并保留调用 id，模型才对得上号。
			label := "[tool result"
			if m.ToolCallID != "" {
				label += " for " + m.ToolCallID
			}
			if text == "" {
				text = "[空结果]"
			}
			out = append(out, buildChatMessage(sourceUser, label+"]: "+text))
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

// foldToolCalls 把 assistant 的 tool_calls 降级成文本标记（toolshim 不介入时的兜底）。
func foldToolCalls(text string, calls []channel.ToolCall) string {
	parts := make([]string, 0, len(calls)+1)
	if text != "" {
		parts = append(parts, text)
	}
	for _, tc := range calls {
		args := trimSpace(tc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		parts = append(parts, "[called tool "+tc.Function.Name+" with "+args+"]")
	}
	return joinNonEmpty(parts, "\n")
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
