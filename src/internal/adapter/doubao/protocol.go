package doubao

// protocol.go 凭证解析（Cookie 串 → sessionid / 设备参数）+ 请求参数与请求体组装 + SSE 事件解析。
//
// 设备参数（device_id / web_id）来自浏览器的 localStorage，**不在 Cookie 里** ——
// 用户粘 Cookie 时拿不到它们。参考实现用的是硬编码兜底值（那是它自己抓包时的设备号）；
// 我们改成**由 sessionid 派生**：同账号恒定、不同账号天然不同，既不需要额外落盘，
// 也不会让所有用户共用一个设备号（共用设备号在风控看来就是同一个人）。
// 用户在粘贴时如果顺手带了 device_id/web_id（JSON 形态），以他给的为准。

import (
	"encoding/json"
	"hash/fnv"
	"strconv"
	"strings"
)

// cred 是一次调用需要的全部凭证材料。
type cred struct {
	cookie    string // 原始 Cookie 头，原样回传
	sessionid string
	csrf      string // x-tt-passport-csrf-token
	deviceID  string
	webID     string
	fp        string // 浏览器指纹（cookie s_v_web_id）
	msToken   string // 通常为空：空的比假的安全
}

// parseCred 解析用户粘回来的凭证。
//
// 认两种形态：
//  1. 浏览器里复制的那一整行 Cookie（最省事，推荐）；
//  2. JSON：{"cookie":"…","device_id":"…","web_id":"…","fp":"…"}（进阶用户能补全设备参数）。
//
// sessionid 是必需的：没有它上游一律 401，早失败比晚失败好（红线一）。
func parseCred(raw string) (*cred, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errNoSession
	}
	out := &cred{}
	values := map[string]string{}

	if strings.HasPrefix(s, "{") {
		var obj struct {
			Cookie   string `json:"cookie"`
			Session  string `json:"sessionid"`
			DeviceID string `json:"device_id"`
			WebID    string `json:"web_id"`
			FP       string `json:"fp"`
			MSToken  string `json:"ms_token"`
		}
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return nil, errNoSession
		}
		out.cookie = strings.TrimSpace(obj.Cookie)
		values = parseCookieHeader(out.cookie)
		if v := strings.TrimSpace(obj.Session); v != "" {
			values["sessionid"] = v
		}
		out.deviceID, out.webID, out.fp, out.msToken = strings.TrimSpace(obj.DeviceID),
			strings.TrimSpace(obj.WebID), strings.TrimSpace(obj.FP), strings.TrimSpace(obj.MSToken)
	} else {
		out.cookie = s
		values = parseCookieHeader(s)
	}

	// `Cookie: a=b; c=d` 这种带前缀的整行，把前缀去掉（原始串也顺手清一下）。
	if i := strings.Index(strings.ToLower(out.cookie), "cookie:"); i == 0 {
		out.cookie = strings.TrimSpace(out.cookie[len("cookie:"):])
	}

	out.sessionid = values["sessionid"]
	if out.sessionid == "" {
		return nil, errNoSession
	}
	if out.fp == "" {
		out.fp = values["s_v_web_id"]
	}
	out.csrf = values["passport_csrf_token"]
	if out.csrf == "" {
		out.csrf = values["passport_csrf_token_default"]
	}
	if out.msToken == "" {
		// msToken 一般不在 Cookie 里（在 JS 变量里）。**宁可不传**：
		// 参考实现实测「空/假的 msToken 会触发 710022002」，空着反而安全。
		out.msToken = ""
	}
	if out.deviceID == "" {
		out.deviceID = deriveNum(out.sessionid, "device", 15)
	}
	if out.webID == "" {
		out.webID = deriveNum(out.sessionid, "web", 19)
	}
	return out, nil
}

// parseCookieHeader 把 `a=b; c=d` 解析成 map（容忍换行、多余空格、末尾分号）。
func parseCookieHeader(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ';' || r == '\n' || r == '\r'
	}) {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		if k == "" {
			continue
		}
		out[k] = strings.TrimSpace(kv[1])
	}
	return out
}

// deriveNum 由凭证派生一个固定位数的十进制串（设备号形态必须是数字）。
func deriveNum(seed, salt string, digits int) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	_, _ = h.Write([]byte("#doubao-" + salt))
	var span uint64 = 1
	for i := 0; i < digits; i++ {
		span *= 10
	}
	lo := span / 10 // 保证首位非 0，长度稳定
	return strconv.FormatUint(lo+h.Sum64()%(span-lo), 10)
}

// queryParams 组装 URL 上的安全参数。
//
// 这些参数看着啰嗦，但它们是**上游风控的一部分**：少一个都可能被判成非官方客户端。
// 其中 web_tab_id 每次请求新生成（模拟「又开了一个标签页」）。
func queryParams(c *cred, tabID string) string {
	pairs := [][2]string{
		{"aid", "582478"},
		{"real_aid", "582478"},
		{"device_id", c.deviceID},
		{"tea_uuid", c.deviceID},
		{"web_id", c.webID},
		{"device_platform", "web"},
		{"language", "zh"},
		{"region", "CN"},
		{"sys_region", "CN"},
		{"pkg_type", "release_version"},
		{"version_code", versionCode},
		{"pc_version", pcVersion},
		{"chromium_version", chromiumBuild},
		{"client_platform", "pc_client"},
		{"runtime", "web"},
		{"runtime_version", runtimeVersion},
		{"samantha_web", "1"},
		{"use-olympus-account", "1"},
		{"fp", c.fp},
		{"web_tab_id", tabID},
	}
	if c.msToken != "" {
		pairs = append(pairs, [2]string{"msToken", c.msToken})
	}
	var b strings.Builder
	for i, kv := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(kv[0])
		b.WriteByte('=')
		b.WriteString(urlEncode(kv[1]))
	}
	return b.String()
}

// buildBody 组装请求体（形态照参考实现，字段一个都不能少：它们是上游 SDK 的固定协议）。
func buildBody(text string, think int, c *cred) []byte {
	body := map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": "",
			"conversation_id":       "",
			"bot_id":                botID,
			"last_section_id":       "",
			"last_message_index":    0,
		},
		"messages": []any{map[string]any{
			"local_message_id": "",
			"content_block": []any{map[string]any{
				"block_type": 10000,
				"content": map[string]any{
					"text_block": map[string]any{"text": text},
				},
			}},
			"message_status": 0,
		}},
		"option": map[string]any{
			"need_deep_think":          think,
			"need_create_conversation": true,
			"answer_with_suggest":      true,
			"sse_recv_event_options":   map[string]any{"support_chunk_delta": true},
			"conversation_init_option": map[string]any{"need_ack_conversation": true},
			"disable_sse_cache":        false,
			"is_regen":                 false,
			"is_replace":               false,
			"start_seq":                0,
			"message_from":             0,
			"scene_type":               0,
			"is_audio":                 false,
			"tts_switch":               false,
			"click_clear_context":      false,
			"from_suggest":             false,
			"resend_for_regen":         false,
			"select_text_action":       "",
			"send_message_scene":       "",
			"collect_id":               "",
			"shared_app_name":          "",
			"action_bar_skill_id":      0,
			"is_ai_playground":         false,
			"no_replace_for_regen":     false,
			"regen_instruction":        "",
			"regen_query_id":           []any{},
			"edit_query_id":            []any{},
		},
		"chat_ability": map[string]any{},
		"ext": map[string]any{
			"use_deep_think":                strconv.Itoa(think),
			"fp":                            c.fp,
			"use_submit_pipeline":           "1",
			"sub_conv_firstmet_type":        "1",
			"commerce_credit_config_enable": "0",
		},
	}
	raw, _ := json.Marshal(body)
	return raw
}

// urlEncode 是最小化的 percent-encoding：设备号/fp 里可能出现 `_`、`+`、`/` 等字符。
func urlEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			b.WriteByte(ch)
			continue
		}
		b.WriteString("%")
		const hex = "0123456789ABCDEF"
		b.WriteByte(hex[ch>>4])
		b.WriteByte(hex[ch&0x0f])
	}
	return b.String()
}
