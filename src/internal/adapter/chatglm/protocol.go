package chatglm

// protocol.go 消息拼装 + parts 状态机（把 SSE 里不断更新的 parts 变成「增量文本」）。
//
// 上游的响应不是「一帧一段文字」，而是**每帧带一份 parts 快照**（按 logic_id 增量更新）。
// 所以取增量的正确做法是：把 parts 攒起来 → 重新渲染成完整正文/思考 → 与已发送的部分求差。
// 直接拿某一帧的 text 当增量会重复吐字（这是这类协议最常见的写错方式）。

import (
	"regexp"
	"strconv"
	"strings"

	"poolgate/internal/channel"
)

// packMessages 把内部消息拼成上游要的那一段文本。
//
// 上游只接受一条 user 消息，所以历史必须压进一段里；角色用 `<|user|>` 这类标记区分。
// 注意 `sytstem` 的拼写：这不是笔误 —— 参考实现（对着官网实测出来的）就是这个拼写，
// 改「对」了反而与上游形态不一致。
func packMessages(msgs []channel.Message) string {
	// 只有一条消息时直接透传，不加角色标记（参考实现的分支：messages.length < 2）。
	var texts []string
	for _, m := range msgs {
		t := strings.TrimSpace(m.Content)
		if t == "" && len(m.ToolCalls) == 0 {
			continue
		}
		texts = append(texts, t)
	}
	if len(texts) < 2 {
		// 参考实现透传分支的原文是 `content + \`${message.content}\n\``（chat.ts:827-835），
		// 段末带一个换行 —— 这是真正发给上游的 text 字节，照抄。
		return strings.Join(texts, "\n") + "\n"
	}

	var b strings.Builder
	for _, m := range msgs {
		text := strings.TrimSpace(m.Content)
		// assistant 的工具调用拍平成标记块：上游没有工具概念，只能这样表达
		// （否则模型看到的是「自己上一轮什么都没调用」，会重复调用同一个工具）。
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			calls := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, "[call:"+tc.Function.Name+"]"+tc.Function.Arguments+"[/call]")
			}
			if len(calls) > 0 {
				text = "[function_calls]\n" + strings.Join(calls, "\n") + "\n[/function_calls]"
			}
		}
		// 工具结果没有独立角色，改成带 id 的 user 文本。
		if m.Role == "tool" {
			if m.ToolCallID != "" {
				text = "[TOOL_RESULT for " + m.ToolCallID + "] " + text
			}
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		b.WriteString(roleMark(m.Role))
		b.WriteString("\n")
		b.WriteString(text)
		b.WriteString("\n")
	}
	b.WriteString("<|assistant|>\n")

	out := b.String()
	// 去掉 Markdown 图片链接与临时文件路径：参考实现发现它们会诱导模型产生幻觉
	// （模型会「看到」并不存在的图片/文件）。这两条属于实测经验，照抄。
	out = mdImageRe.ReplaceAllString(out, "")
	out = tmpPathRe.ReplaceAllString(out, "")
	return out
}

var (
	// mdImageRe 匹配 Markdown 图片。参考实现用的是贪婪的 `!\[.+\]\(.+\)`：
	// 一行里有两张图时，它会从第一张的 `![` 一直吃到最后一个 `)`，把中间的文字也删掉。
	// 我们改成逐张匹配（空 alt 也认）—— 目的是「别把图片 URL 喂给模型引发幻觉」，
	// 顺带不该误伤正文。
	mdImageRe = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	tmpPathRe = regexp.MustCompile(`/mnt/data/.+`)
)

// roleMark 是上游的角色标记（拼写照参考实现，含那个 sytstem）。
func roleMark(role string) string {
	switch role {
	case "system", "developer":
		return "<|sytstem|>"
	case "assistant":
		return "<|assistant|>"
	default:
		return "<|user|>"
	}
}

// ---------------------------------------------------------------------------
// parts 状态机
// ---------------------------------------------------------------------------

// streamState 累积上游的 parts 并渲染出「已经说了什么」。
type streamState struct {
	order []string                  // logic_id 的出现顺序（渲染顺序必须稳定）
	parts map[string]map[string]any // logic_id → part
	sent  string                    // 已经发出去的正文（用于求差）
	sentR string                    // 已经发出去的思考
	// 联网检索结果：正文里的 `turnXsearchY` 标记要换成带编号的链接，
	// 否则客户端会看到一堆没有意义的内部标记。
	searchRefs map[string]searchRef
	refIDs     map[string]int
	nextRefID  int
}

type searchRef struct {
	title string
	url   string
}

func newStreamState() *streamState {
	return &streamState{
		parts:      map[string]map[string]any{},
		searchRefs: map[string]searchRef{},
		refIDs:     map[string]int{},
	}
}

// feed 吃一帧的 parts，返回本次**新增**的正文与思考。
func (s *streamState) feed(ev map[string]any) (text, reasoning string) {
	if parts, ok := ev["parts"].([]any); ok {
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			id, _ := pm["logic_id"].(string)
			if id == "" {
				continue
			}
			if _, seen := s.parts[id]; !seen {
				s.order = append(s.order, id)
			}
			s.parts[id] = pm
		}
	}
	full, fullR := s.render()
	// 求差：只发新增的部分。上游的 parts 是增量更新的，但看起来是快照。
	text = diffTail(full, s.sent)
	reasoning = diffTail(fullR, s.sentR)
	s.sent, s.sentR = full, fullR
	return text, reasoning
}

// diffTail 返回 full 里相对 sent 新增的那一段。
//
// 先按前缀比较：正常情况（上游只追加）能直接切出增量。若前缀已经不一致
// （上游改了之前发出过的内容），就把差异当成**替换**整体发出去 —— 宁可让客户端
// 看到一次重复，也不要默默丢掉模型改过的内容。
func diffTail(full, sent string) string {
	if strings.HasPrefix(full, sent) {
		return full[len(sent):]
	}
	return full
}

// render 按顺序把 parts 渲染成（正文，思考）。
func (s *streamState) render() (string, string) {
	s.collectSearchRefs()

	var textParts, thinkParts []string
	for _, id := range s.order {
		p := s.parts[id]
		pmeta, _ := p["meta_data"].(map[string]any)
		status, _ := p["status"].(string)
		items, _ := p["content"].([]any)

		var tb, rb strings.Builder
		for _, it := range items {
			m, _ := it.(map[string]any)
			if m == nil {
				continue
			}
			// 参考实现一律从 **part 级** 取 meta_data（chat.ts:1118、1321 `const { content, meta_data } = part`），
			// item 级它没看过。我们在 part 级为空时额外认一次 item 级：纯属防御性放宽 ——
			// 认错级别的表现是「检索结果凭空消失」，多认一级不会把对的读错（item 级只在 part 级缺失时生效）。
			meta := mergeMeta(pmeta, itemMeta(m))
			switch typ, _ := m["type"].(string); typ {
			case "text":
				tb.WriteString(s.rewriteRefs(str(m["text"])))
			case "think":
				rb.WriteString(str(m["think"]))
			case "code":
				tb.WriteString("```python\n" + str(m["code"]))
				if status == "finish" {
					tb.WriteString("\n```\n")
				}
			case "execution_output":
				// 参考实现要求 content 是字符串、且 part 已 finish（chat.ts:1215-1220）——
				// 少了字符串判断，非字符串的 content 会被拼成 "undefined" 喂给客户端。
				if out, ok := m["content"].(string); ok && status == "finish" {
					tb.WriteString(out + "\n")
				}
			case "image":
				// 参考实现要求该 part 已经 finish 才渲染图片（chat.ts:1210 `type=="image" && isArray(image) && part.status=="finish"`）：
				// 生成中的占位图不该发给客户端。
				if status == "finish" {
					tb.WriteString(s.renderImages(m))
				}
			case "tool_result":
				rb.WriteString(searchLines(meta, "tool_result_extra"))
			case "quote_result":
				if status == "finish" {
					rb.WriteString(searchLines(meta, "metadata_list"))
				}
			}
		}
		if v := tb.String(); v != "" {
			textParts = append(textParts, v)
		}
		if v := rb.String(); v != "" {
			thinkParts = append(thinkParts, v)
		}
	}
	// parts 之间用换行分隔（与参考实现一致：块之间要断开，否则前后文会粘在一起）。
	return strings.Join(textParts, "\n"), strings.Join(thinkParts, "\n")
}

// collectSearchRefs 先把所有联网检索结果收集起来，供正文里的引用标记替换用。
// 同样要两级都看（见 mergeMeta 的说明）。
//
// 索引键用 `match_key`：参考实现 chat.ts:1301-1305 就是 `if (res.match_key) searchMap.set(res.match_key, res)`，
// 而正文里的标记形态是 `turn\d+[a-zA-Z]+\d+` —— 两边对得上才说明 match_key 就是这个标记本身。
// （`id` 作为兜底：我们先前只认 id，没有证据支持，保留它不会误伤 match_key 命中的情形。）
func (s *streamState) collectSearchRefs() {
	for _, id := range s.order {
		part := s.parts[id]
		pm := partMeta(part)
		items, _ := part["content"].([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			if m == nil {
				continue
			}
			meta := mergeMeta(pm, itemMeta(m))
			if meta == nil {
				continue
			}
			extra, _ := meta["tool_result_extra"].(map[string]any)
			results, _ := extra["search_results"].([]any)
			for _, r := range results {
				rm, _ := r.(map[string]any)
				if rm == nil {
					continue
				}
				key, _ := rm["match_key"].(string)
				if key == "" {
					key, _ = rm["id"].(string)
				}
				if key == "" {
					continue
				}
				title, _ := rm["title"].(string)
				url, _ := rm["url"].(string)
				s.searchRefs[key] = searchRef{title: title, url: url}
			}
		}
	}
}

func partMeta(part map[string]any) map[string]any {
	if part == nil {
		return nil
	}
	m, _ := part["meta_data"].(map[string]any)
	return m
}

func itemMeta(item map[string]any) map[string]any {
	if item == nil {
		return nil
	}
	m, _ := item["meta_data"].(map[string]any)
	return m
}

// mergeMeta 合并 part 级与 item 级的 meta_data（都不为空时 item 级优先覆盖）。
func mergeMeta(partMeta, itemMeta map[string]any) map[string]any {
	if len(partMeta) == 0 {
		return itemMeta
	}
	if len(itemMeta) == 0 {
		return partMeta
	}
	out := make(map[string]any, len(partMeta)+len(itemMeta))
	for k, v := range partMeta {
		out[k] = v
	}
	for k, v := range itemMeta {
		out[k] = v
	}
	return out
}

// rewriteRefs 把正文里的 `turnXsearchY` 标记换成带编号的链接（上游内部标记不该给用户看）。
func (s *streamState) rewriteRefs(text string) string {
	if text == "" || len(s.searchRefs) == 0 {
		return text
	}
	return refRe.ReplaceAllStringFunc(text, func(match string) string {
		key := strings.Trim(match, "【】")
		ref, ok := s.searchRefs[key]
		if !ok {
			return match
		}
		id, seen := s.refIDs[key]
		if !seen {
			s.nextRefID++
			id = s.nextRefID
			s.refIDs[key] = id
		}
		return " [" + strconv.Itoa(id) + "](" + ref.url + ")"
	})
}

// refRe 匹配上游的检索引用标记（可带中文方括号）。
var refRe = regexp.MustCompile(`【?turn\d+[a-zA-Z]+\d+】?`)

// renderImages 把图片项渲染成 Markdown 图片（只认 http(s) 的，其余丢掉）。
func (s *streamState) renderImages(m map[string]any) string {
	imgs, _ := m["image"].([]any)
	var b strings.Builder
	for _, img := range imgs {
		im, _ := img.(map[string]any)
		if im == nil {
			continue
		}
		u, _ := im["image_url"].(string)
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			b.WriteString("![图像](" + u + ")")
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String() + "\n"
}

// searchLines 把检索结果渲染成「> 检索 标题(链接) ...」的引用行（进思考流，不进正文）。
//
// 行尾那个 ` ...` 是参考实现的原文（chat.ts:1168、1176 模板串 `> 检索 ${v.title}(${v.url}) ...\n`），
// 照抄 —— 它是展示给用户的思考流内容，不留神会被当成我们自己的省略号而删掉。
//
// 两种 item 的嵌套层级不一样，别写成一个（写错的表现是「检索结果凭空消失」）：
//   - tool_result：meta_data.tool_result_extra.**search_results**[]（多一层对象）
//   - quote_result：meta_data.metadata_list[]（直接就是数组）
func searchLines(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	var list []any
	if key == "tool_result_extra" {
		if extra, ok := meta[key].(map[string]any); ok {
			list, _ = extra["search_results"].([]any)
		}
	} else {
		list, _ = meta[key].([]any)
	}
	var b strings.Builder
	for _, r := range list {
		rm, _ := r.(map[string]any)
		if rm == nil {
			continue
		}
		title, _ := rm["title"].(string)
		url, _ := rm["url"].(string)
		b.WriteString("> 检索 " + title + "(" + url + ") ...\n")
	}
	return b.String()
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
