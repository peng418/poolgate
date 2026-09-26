package yuanbao

// protocol.go 凭证解析（浏览器头 → x-uskey + cookie）+ 请求体组装 + 消息拼装。
//
// 元宝的凭证形态在几个参考实现里都是「把浏览器那一次请求的头原样重放」——
// 因为它的鉴权就是这一份头（含 x-uskey）。我们从里面只需要两样东西：x-uskey 与 cookie。
// 其余头（UA / Origin / Referer / X-Agentid / Accept）由我们按浏览器形态补上。

import (
	"encoding/json"
	"strings"

	"poolgate/internal/channel"
)

// cred 是一次调用需要的凭证材料。
type cred struct {
	uskey  string // x-uskey：唯一必需的鉴权材料
	cookie string // 同一请求的 Cookie（一起带上更接近真实客户端）
	ua     string // 用户那次请求的 UA（有就用他的，没有就用我们的默认）
}

// parseCred 解析用户粘回来的内容。
//
// 认三种形态：
//  1. 整段请求头（从 DevTools 的 Request Headers 直接复制，含 `x-uskey: …` 行）；
//  2. JSON：{"uskey":"…","cookie":"…"}；
//  3. 只有 uskey 值的裸串。
//
// 只要求 x-uskey：free-api 是整段头重放（其中就含它），chat2api 走 Cookie 的
// hy_user/hy_token 并不发 x-uskey —— 两份不一致，我们选 x-uskey 作主凭证，
// cookie 有就带上（见 constants.go 顶部说明）。
func parseCred(raw string) (*cred, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errNoUskey
	}
	out := &cred{}
	if strings.HasPrefix(s, "{") {
		var obj struct {
			Uskey  string `json:"uskey"`
			Cookie string `json:"cookie"`
			UA     string `json:"user_agent"`
		}
		if err := json.Unmarshal([]byte(s), &obj); err == nil {
			out.uskey, out.cookie, out.ua = strings.TrimSpace(obj.Uskey),
				strings.TrimSpace(obj.Cookie), strings.TrimSpace(obj.UA)
		}
	}
	if out.uskey == "" {
		// 按「整段头」解析：逐行找 `名字: 值`（大小写不敏感）。
		lines := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' })
		for _, line := range lines {
			name, value, ok := splitHeader(line)
			if !ok {
				continue
			}
			switch strings.ToLower(name) {
			case "x-uskey":
				out.uskey = value
			case "cookie":
				out.cookie = value
			case "user-agent":
				out.ua = value
			}
		}
	}
	// 裸串形态：整段就是 uskey
	if out.uskey == "" && !strings.ContainsAny(s, ":\n\t ") {
		out.uskey = s
	}
	if strings.TrimSpace(out.uskey) == "" {
		return nil, errNoUskey
	}
	return out, nil
}

// splitHeader 把一行 `Name: value` 拆开（值里允许出现冒号，所以只切第一个）。
func splitHeader(line string) (string, string, bool) {
	i := strings.Index(line, ":")
	if i <= 0 {
		return "", "", false
	}
	name := strings.TrimSpace(line[:i])
	value := strings.TrimSpace(line[i+1:])
	if name == "" || value == "" {
		return "", "", false
	}
	return name, value, true
}

// buildBody 组装对话请求体（形态照两份参考实现的共识）。
func buildBody(prompt, upward string, search bool) []byte {
	body := map[string]any{
		"model":             "gpt_175B_0404",
		"prompt":            prompt,
		"plugin":            "Adaptive",
		"displayPrompt":     prompt,
		"displayPromptType": 1,
		"options": map[string]any{
			"imageIntention": map[string]any{
				"needIntentionModel": true,
				"backendUpdateFlag":  2,
				"intentionStatus":    true,
			},
		},
		"multimedia":  []any{},
		"agentId":     agentID,
		"supportHint": 1,
		"version":     "v2",
		"chatModelId": upward,
	}
	if search {
		body["supportFunctions"] = []string{"supportInternetSearch"}
	}
	raw, _ := json.Marshal(body)
	return raw
}

// packMessages 把内部消息拼成上游要的一个 prompt 字符串（上游没有多轮概念）。
//
// 标记形态沿用仓库里其它网页渠道的约定，便于 toolshim 的两侧保持一致。
func packMessages(msgs []channel.Message) string {
	var lines []string
	for _, m := range msgs {
		text := strings.TrimSpace(m.Content)
		role := m.Role
		if role == "assistant" && len(m.ToolCalls) > 0 {
			calls := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, "[call:"+tc.Function.Name+"]"+tc.Function.Arguments+"[/call]")
			}
			if len(calls) > 0 {
				text = "[function_calls]\n" + strings.Join(calls, "\n") + "\n[/function_calls]"
			}
		}
		if role == "tool" {
			role = "user"
			if m.ToolCallID != "" {
				text = "[TOOL_RESULT for " + m.ToolCallID + "] " + text
			}
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if role == "system" || role == "developer" {
			role = "system"
		}
		lines = append(lines, role+":"+text)
	}
	return strings.Join(lines, "\n")
}
