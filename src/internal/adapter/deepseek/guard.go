package deepseek

// guard.go 是「出站正文哨兵」：本渠道的 prompt 是我们**手拼的官方对话模板**
// （<｜System｜>…<｜User｜>…<｜Assistant｜>，见 packMessages），而官方 Web 客户端不是这么发的
// —— 它只发本轮那条消息，会话历史在上游服务器上，本轮由上游在 end▁of▁sentence 处收尾。
// 「整段剧本塞进 prompt」这种形态，模型偶尔会**顺着剧本往下写**：正文里出现
//   <｜end▁of▁sentence｜><｜User｜>[工具执行结果] terminal {"output": …}<｜end▁of▁sentence｜><｜User｜>…
// 也就是把前几轮的工具结果当正文再念一遍，甚至自造标记。真机现场（2026-09-26，Hermes CLI
// 走本渠道读项目）连续 3 轮都出现这种套娃正文，其中还夹着一个 </std::Assistant> —— 这个串
// 在本仓、在上游协议里都不存在，只可能是生成出来的，所以可以确认是**模型在续写剧本**，
// 不是上游把请求回显给我们。更糟的是这段脏正文会被客户端当成助手发言存进历史、
// 下一轮原样发回来，模型看到「上一轮自己就是这么写的」就更会接着演（自激回路）。
//
// 本文件只做收窄，不改写正文：
//  1. 控制标记永不下发：任何 <｜…｜> 形态的标记一旦出现，就认为模型开始自演下一轮
//     —— 本轮到此结束（等价于官方客户端在 end▁of▁sentence 收尾），该点之后一律丢弃并记日志。
//  2. 跨帧也要拦得住：标记可能被上游切在两个 delta 之间，所以可能成为标记前缀的尾部先扣住不发。
//
// 为什么不去猜「哪一段是自演的」：判不准就丢内容 = 静默失败（红线一）。只认**结构性**判据
// —— 控制标记是模板的一部分，永远不会是正常回答的内容。

import (
	"strings"
	"unicode/utf8"
)

const (
	// guardOpen / guardClose 是控制标记的定界符（全角竖线 U+FF5C）。
	guardOpen  = "<｜"
	guardClose = "｜>"
	// guardMaxToken 是单个控制标记的字节上限（实测最长 <｜begin▁of▁sentence｜> 约 30 字节）。
	// 超出这个窗口还没闭合，就认为它不是标记、按普通文本放行（正文里写 "<｜" 字面量不会被吃掉）。
	guardMaxToken = 40
	// guardUpstreamNote 是「整轮自演」时给下游的上游原话前缀（红线一：客户端要看得到）。
	guardUpstreamNote = "上游正文里出现对话模板标记（模型续写了我们拼的对话脚本）"
	// guardHeadCap 是「被丢弃内容」留样的字节上限（日志与上游原话用；留够能认出形态）。
	guardHeadCap = 256
)

// streamGuard 逐帧喂入、按需放行。
type streamGuard struct {
	buf     string // 还没决定去留的尾部（可能是被切开的标记）
	cut     bool   // 已判定「自演开始」：本轮不再放行任何内容
	emitted bool   // 是否已放行过非空白正文
	dropped int    // 丢弃的字节数（日志用）
	head    string // 丢弃区的起始预览（日志/上游原话用）
}

// feed 喂入一段增量，返回可下发的正文与「本轮是否就此结束」。
func (g *streamGuard) feed(s string) (string, bool) {
	if s == "" {
		return "", g.cut
	}
	if g.cut {
		g.drop(s)
		return "", true
	}
	g.buf += s
	if i := strings.Index(g.buf, guardOpen); i >= 0 {
		head := g.buf[:i]
		// 标记必须在这个窗口内闭合；闭合了就是真的控制标记。
		if j := strings.Index(g.buf[i+len(guardOpen):], guardClose); j >= 0 && j <= guardMaxToken {
			g.cut = true
			g.drop(g.buf[i:])
			g.buf = ""
			return g.emit(head), true
		}
		// 还没闭合（多半被切在下一帧）：只放行它之前的内容，剩下的扣住等下一帧。
		g.buf = g.buf[i:]
		return g.emit(head), false
	}
	safe, tail := guardSafeSplit(g.buf)
	g.buf = tail
	return g.emit(safe), false
}

// flush 收尾：把扣住的尾巴放行（走到这里说明它不含完整标记）。
func (g *streamGuard) flush() string {
	if g.cut {
		return ""
	}
	out := g.buf
	g.buf = ""
	return g.emit(out)
}

// junkOnly 表示「整轮都是自演」：截断过，且从头到尾没有一个字的合法正文。
// 调用方据此上报明确的失败，而不是把空回复当成功（红线二）。
func (g *streamGuard) junkOnly() bool { return g.cut && !g.emitted }

// preview 返回被丢弃内容的起始若干字节（日志与「上游原话」用）。
// 截断落在字符边界上，不产生半个汉字（日志里出现乱码等于把线索弄丢）。
func (g *streamGuard) preview(n int) string {
	s := strings.TrimSpace(g.head)
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// drop 记一笔被丢弃的内容：留样（有上限）与累计字节数。
// 留样要连着攒够 guardHeadCap —— 只留第一片的话，日志里常只有半个标记，看不出形态。
func (g *streamGuard) drop(s string) {
	if len(g.head) < guardHeadCap {
		g.head += s
		if len(g.head) > guardHeadCap {
			g.head = g.head[:guardHeadCap]
		}
	}
	g.dropped += len(s)
}

func (g *streamGuard) emit(s string) string {
	if strings.TrimSpace(s) != "" {
		g.emitted = true
	}
	return s
}

// guardSafeSplit 把缓冲切成「可立即下发」与「需继续观察的尾部」。
//
// 只需保留最后一个 '<' 起的那一小段：控制标记一定以 '<' 开头，一旦这段长过窗口上限
// 就绝不可能是标记（于是正文里普通的 '<' 最多被推迟一帧，不会被长期扣住）。
func guardSafeSplit(s string) (safe, tail string) {
	i := strings.LastIndex(s, "<")
	if i < 0 {
		return s, ""
	}
	tail = s[i:]
	if len(tail) > guardMaxToken+len(guardClose) {
		return s, ""
	}
	return s[:i], tail
}

// cutAtControlMarker 用于**入站**消息：把客户端回传的 assistant 历史在第一个控制标记处截断。
//
// 脏正文一旦被客户端存进历史，下一轮会原样发回来，模型看到「自己上一轮就是这么写的」
// 就更会接着自演（真机同一会话连续 3 轮都脏）。在拼提示词这一步掐掉这段尾巴，等于断掉自激回路。
//
// 只对 assistant 生效：用户消息与工具结果里出现这个字面量是**数据**，动了就是丢内容（红线一）。
func cutAtControlMarker(s string) string {
	if i := strings.Index(s, guardOpen); i >= 0 {
		return s[:i]
	}
	return s
}
