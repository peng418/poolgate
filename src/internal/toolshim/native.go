package toolshim

// native.go 认模型**自带的**工具调用语法（不是我们提示词里约定的 <tool_call>）。
//
// 为什么必须有这一层：我们的格式约定只是「写在提示词里的一句话」，模型不一定守。
// 实测（2026-09-26，deepseek 网页渠道 + 真实 agent 请求 30 个工具）：小请求它会照约定
// 输出 <tool_call>，但请求一大就改用**它训练时见过的原生语法** —— 整段标记原样留在正文里，
// 客户端拿到的是 markup 而不是 tool_calls，coding agent 直接不可用。
// 解析层宽容是这一层的存在意义（与红线一一致：认不出就把原文交出去，绝不吞内容）。
//
// 目前认两个家族，它们的语义是同一件事（invoke + parameter），所以共用一套抽取器：
//
//	DSML（DeepSeek 网页版）：
//	  <bar><bar>DSML<bar><bar> calls>
//	  <bar><bar>DSML<bar><bar> invoke name="Read">
//	  <bar><bar>DSML<bar><bar> parameter name="file_path" string="true">/tmp/a.txt</<bar><bar>DSML<bar><bar> parameter>
//	  </<bar><bar>DSML<bar><bar> invoke>
//	  </<bar><bar>DSML<bar><bar> calls>
//	  （bar 是全角竖线 U+FF5C，模型也可能混用半角 | 与尖括号 —— 一律归一后抽取）
//
//	XML（Qwen/Kimi/GLM 系与多数 agent 框架）：
//	  <function_calls><invoke name="Read"><parameter name="file_path">/tmp/a.txt</parameter></invoke></function_calls>
//
// 抽取失败时的行为：把**原始文本**还回去（连同标记），不静默丢弃。

import (
	"encoding/json"
	"regexp"
	"strings"

	"poolgate/internal/channel"
)

// 家族名（进入原生模式后据此找收尾标记）。
const (
	familyDSML = "dsml"
	familyXML  = "xml"
)

var (
	// reStartDSML 命中 DSML 家族开头：一到两个竖线 + DSML + 一到两个竖线。
	reStartDSML = regexp.MustCompile(`(?i)(?:\||｜){1,2}\s*DSML\s*(?:\||｜){1,2}`)
	// reEndDSML 命中 DSML 家族收尾：…/…calls… 那一行（斜杠可能在竖线前或后）。
	reEndDSML = regexp.MustCompile(`(?i)(?:\||｜)*\s*/?\s*(?:\||｜)*\s*DSML\s*(?:\||｜)*\s*/?\s*calls\s*>\s*`)

	// reStartXML / reEndXML 是 XML 家族的头尾（只认 function_calls，避免与正文里的
	// 普通 <invoke> 撞车；单个 invoke 而无 function_calls 头的情况由收尾/Flush 兜住）。
	reStartXML = regexp.MustCompile(`(?i)<\s*function_calls\s*>`)
	reEndXML   = regexp.MustCompile(`(?i)<\s*/\s*function_calls\s*>`)

	// reInvoke / reParam 是家族无关的抽取器。
	reInvoke = regexp.MustCompile(`(?is)\binvoke\s+name\s*=\s*["']?([\w.\-:]+)`)
	reParam  = regexp.MustCompile(`(?is)\bparameter\s+name\s*=\s*["']?([\w.\-:]+)["']?(?:\s+\w+\s*=\s*["'][^"']*["'])*\s*>`)
	// reParamString 抓 parameter 上的 string 属性（string="false" 表示值要按 JSON 解）。
	reParamString = regexp.MustCompile(`(?is)\bstring\s*=\s*["']?(true|false)["']?`)
	// reCloseAny 命中任意一个原生收尾标记（calls / invoke / parameter 的收尾）。
	reCloseAny = regexp.MustCompile(`(?i)(?:\||｜)*\s*/?\s*(?:\||｜)*\s*DSML\s*(?:\||｜)*\s*/?\s*(?:calls|invoke|parameter)\s*>`)
)

// splitNativeTrailing 把「原生标记体」与它后面的正文分开（Flush 在没等到收尾标记时用）。
//
// 切点取**最后一个**收尾标记之后：模型先写标记、再接着说人话时，
// 人话不该被当成最后一个参数的值。
func splitNativeTrailing(text string) (region, tail string) {
	locs := reCloseAny.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return text, ""
	}
	last := locs[len(locs)-1]
	return text[:last[1]], text[last[1]:]
}

// nativeStart 在 text 里找最早出现的原生标记，返回起点、家族名与标记长度。
//
// 找不到返回 (-1, "", 0)。
func nativeStart(text string) (int, string, int) {
	best, family, length := -1, "", 0
	if m := reStartDSML.FindStringIndex(text); m != nil {
		best, family, length = m[0], familyDSML, m[1]-m[0]
	}
	if m := reStartXML.FindStringIndex(text); m != nil && (best < 0 || m[0] < best) {
		best, family, length = m[0], familyXML, m[1]-m[0]
	}
	return best, family, length
}

// nativeEnd 在 text 里找家族的收尾标记，返回区间。
func nativeEnd(text, family string) []int {
	re := reEndXML
	if family == familyDSML {
		re = reEndDSML
	}
	return re.FindStringIndex(text)
}

// nativePartialLen 返回 text 末尾有多长「可能是原生标记的一半」，这些字符先别输出。
//
// 只对两种形态生效：竖线+DSML 的任意前缀、以及 <function_calls> 的任意前缀后缀。
// 限长 32，避免把正常正文长期压在缓冲区里。
func nativePartialLen(text string) int {
	const max = 32
	tail := text
	if len(tail) > max {
		tail = tail[len(tail)-max:]
	}
	// <function_calls> 的前缀（含 "<"、"<f" … 整串的一半）。
	for n := len(tail); n > 0; n-- {
		s := tail[len(tail)-n:]
		if strings.HasPrefix("<function_calls>", strings.ToLower(s)) && strings.HasPrefix(s, "<") {
			if n < len("<function_calls>") {
				return n
			}
			return 0
		}
	}
	// 竖线 + DSML（大小写不敏感）的局部形态：从最后一组竖线起到结尾，只含竖线/DSML 字母/空白。
	i := strings.LastIndexAny(tail, "|｜")
	if i < 0 {
		return 0
	}
	cand := tail[i:]
	upper := strings.ToUpper(cand)
	if strings.TrimLeft(upper, "|｜ \t\r\n") == "" {
		return len(tail) - i
	}
	stripped := strings.TrimLeft(upper, "|｜ \t\r\n")
	rest := strings.TrimRight(stripped, "|｜ \t\r\n")
	if rest == "" || strings.HasPrefix("DSML", rest) {
		return len(tail) - i
	}
	return 0
}

// parseNativeRegion 从原生标记体里抽工具调用。
//
// 抽取器是家族无关的：找 invoke name=…，再在它的范围内找 parameter name=…>值。
// 一个 invoke 都没有时，退回「区域里有没有我们的规范 JSON 体」，仍没有就返回空
// （调用方负责把原文还给客户端）。
func parseNativeRegion(region string, index int) []channel.ToolCall {
	norm := strings.NewReplacer("｜", "|").Replace(region)

	invokes := reInvoke.FindAllStringSubmatchIndex(norm, -1)
	if len(invokes) == 0 {
		if tc, ok := parseCall(norm, index); ok {
			return []channel.ToolCall{tc}
		}
		return nil
	}

	out := make([]channel.ToolCall, 0, len(invokes))
	for i, iv := range invokes {
		name := strings.TrimSpace(norm[iv[2]:iv[3]])
		if name == "" {
			continue
		}
		bodyStart := iv[1]
		bodyEnd := len(norm)
		if i+1 < len(invokes) {
			bodyEnd = invokes[i+1][0]
		}
		body := norm[bodyStart:bodyEnd]

		args := map[string]any{}
		params := reParam.FindAllStringSubmatchIndex(body, -1)
		for j, pm := range params {
			key := strings.TrimSpace(body[pm[2]:pm[3]])
			if key == "" {
				continue
			}
			valStart := pm[1]
			valEnd := len(body)
			if j+1 < len(params) {
				valEnd = params[j+1][0]
			}
			val := trimNativeTail(body[valStart:valEnd])
			// string="false" 声明值本来不是字符串 → 按 JSON 解回原类型。
			attrs := body[pm[0]:pm[1]]
			if m := reParamString.FindStringSubmatch(attrs); len(m) == 2 && strings.EqualFold(m[1], "false") {
				var typed any
				if json.Unmarshal([]byte(val), &typed) == nil {
					args[key] = typed
					continue
				}
			}
			args[key] = val
		}

		raw, err := json.Marshal(args)
		if err != nil {
			raw = []byte("{}")
		}
		out = append(out, channel.ToolCall{
			Index: index + len(out),
			ID:    "call_shim_" + itoa(index+len(out)+1),
			Type:  "function",
			Function: channel.FunctionCall{
				Name:      name,
				Arguments: string(raw),
			},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// trimNativeTail 剥掉值末尾的收尾噪声（</parameter>、|parameter>、竖线与空白）。
//
// 只在末尾**看起来像标签**时才剥：含 JSON 标点或换行的尾巴一律保留
// —— 参数值可能是命令、代码片段或路径，宁可多留一个字符也不能改内容。
func trimNativeTail(s string) string {
	s = strings.TrimRight(s, " \t\r\n")
	for {
		t := strings.TrimRight(s, " \t\r\n")
		i := strings.LastIndexAny(t, "<|")
		if i < 0 {
			return t
		}
		tail := t[i:]
		if !looksLikeTagTail(tail) {
			return t
		}
		s = t[:i]
	}
}

// looksLikeTagTail 判断一段尾巴像不像标签（而不是值的正常结尾）。
func looksLikeTagTail(s string) bool {
	if s == "" || len(s) > 64 || strings.ContainsAny(s, "\n") {
		return false
	}
	if strings.ContainsAny(s, "{}[]\"'=,$") {
		return false
	}
	core := strings.Trim(s, "<>|/ \t")
	if core == "" {
		return true
	}
	// 只允许字母（parameter / invoke / calls / DSML 这类），不允许空格分段之外的东西。
	for _, r := range core {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == ' ', r == '_':
		default:
			return false
		}
	}
	return true
}
