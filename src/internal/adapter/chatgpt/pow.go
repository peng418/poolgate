package chatgpt

// pow.go 工作量证明（sentinel 的 `p` 与 `openai-sentinel-proof-token`）。
//
// 两件事共用同一套算法，只是前缀与难度不同：
//
//	p（requirements token）：难度固定 0fffff，前缀 "gAAAAAC"，随 chat-requirements 一起发；
//	proof token：难度由上游下发，前缀 "gAAAAAB"，放进对话请求头。
//
// 算法（三份参考实现一致）：
//
//	1. 造一个 18 元素的「浏览器形状」配置数组（UA、脚本地址、时区串、屏幕、navigator 特征…）；
//	2. 循环 i：把数组第 4 个元素写成 i、第 10 个元素写成 i>>1，序列化成紧凑 JSON，
//	   base64 之后算 sha3_512(seed + base64)；
//	3. 取哈希的前 len(difficulty)/2 **字节**与 difficulty 解码出的字节比较，<= 就算命中。
//
// 比较必须按**字节**而不是十六进制字符串：十六进制字符串比较在长度是奇数、
// 或有前导零时会得出不同结论（yukkcat 那份实现把这点写对了，我们跟它）。
//
// 性能注意：绝不能每轮都重新 json.Marshal —— 18 元素数组 × 最多 50 万轮，
// marshal 的开销比 sha3 还大。这里照参考实现的做法，把数组**切成三段只 marshal 一次**，
// 循环里只做「拼字符串 + base64 + 哈希」（见 powAnswer）。

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"golang.org/x/crypto/sha3"
)

// buildPowConfig 造一份「浏览器形状」的配置数组（18 元素）。
//
// 元素顺序**不能改**：它是按上游 SDK 读字段的顺序抄下来的，错位等于给上游一个
// 它没见过的形状（后果不是报错，而是静默降级成更严的风控）。
// 第 4 / 第 10 个元素是循环里要被改写的槽位，这里先占位。
//
// 几个看似奇怪的常量都有来由：
//   - 4294705152 是错误码式的固定值；
//   - 时区串固定写成 EST（含 "GMT-0500 (Eastern Standard Time)"），跟服务器真实时区无关；
//   - navigator / window 特征从清单里随机取一个（参考实现如此，取值域不要缩）。
//
// **元素个数上两份参考实现不一致**，这里选了 18 元素这一版（也就是 ChatGPT2API-GO 的
// buildPowConfig 与 gpt4free 的 new.py:get_config）：gpt4free 另有一份老实现
// （proofofwork.py，13 元素、只改第 4 个元素）还在给对话的 proof 头用。
// 选 18 元素的理由：主参考（Go）与两个 chatgpt2api 项目都用它，且同一份配置同时喂给
// `p` 与 proof，行为一致。要回到 13 元素只需改本函数与 powAnswer 里的两个槽位下标
// （第 4 个元素仍是 i）。
// 上游对它的校验只有一条：sha3_512(seed + base64) 的前 len(difficulty)/2 字节 <= difficulty
// —— 所以这一处如果真上游拒绝，症状会是「proof 头被拒」，改本函数即可，不必动别处。
func buildPowConfig(ua string, scripts []string, dataBuild string) []any {
	navKeys := []string{
		"webdriver−false", "vendor−Google Inc.", "hardwareConcurrency−32",
		"language−zh-CN", "cookieEnabled−true",
	}
	winKeys := []string{"window", "document", "location", "navigator", "crypto", "fetch", "screen"}
	cores := []int{8, 16, 24, 32}

	script := defaultPowScript
	if len(scripts) > 0 {
		script = scripts[rand.IntN(len(scripts))]
	}
	now := float64(time.Now().UnixNano()) / 1e6
	return []any{
		[]int{3000, 4000, 5000}[rand.IntN(3)], // 0 屏幕相关
		legacyParseTime(),                     // 1 固定 EST 时区串
		4294705152,                            // 2
		0,                                     // 3 ← 循环里写成 i
		ua,                                    // 4
		script,                                // 5
		dataBuild,                             // 6
		"en-US",                               // 7
		"en-US,es-US,en,es",                   // 8
		0,                                     // 9 ← 循环里写成 i>>1
		navKeys[rand.IntN(len(navKeys))],      // 10
		"location",                            // 11
		winKeys[rand.IntN(len(winKeys))],      // 12
		now,                                   // 13
		randHex(16),                           // 14
		"",                                    // 15
		cores[rand.IntN(len(cores))],          // 16
		now - 1000,                            // 17
	}
}

// legacyParseTime 造上游要的那个「老式时区串」。
//
// 固定按 UTC-5 输出，且格式写死（`Mon Jan 02 2006 15:04:05 GMT-0500 (Eastern Standard Time)`）：
// 这是 JS `Date.toString()` 的形态，不是 RFC3339 —— 换了格式上游虽然不报错，
// 但配置形状变了，通过率会掉。
func legacyParseTime() string {
	est := time.FixedZone("EST", -5*3600)
	return time.Now().In(est).Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)"
}

// powAnswer 求一次 PoW。
//
// 返回 base64(配置 JSON) 与「是否命中难度」。命中不了返回 (", false) ——
// **不返回参考实现那个伪造的兜底串**：伪造的答案上游一定不收，拿它去发等于把
// 「算不出来」伪装成「算出来了」，正是本项目的红线一（禁止静默降级）要挡的那种处理。
func powAnswer(seed, difficulty string, cfg []any, limit int) (string, bool) {
	if len(cfg) < 11 {
		return "", false
	}
	target, err := hexDecode(difficulty)
	if err != nil || len(target) == 0 {
		return "", false
	}
	// 按字节比较：只比前 len(difficulty)/2 个字节。difficulty 长度为奇数时
	// hexDecode 会在前面补一个 0，所以 target 一定不短于这个长度。
	diffLen := len(difficulty) / 2
	if diffLen > len(target) {
		diffLen = len(target)
	}
	if diffLen > 64 {
		diffLen = 64 // sha3-512 只有 64 字节，再长没有意义
	}

	head, err := marshalCompact(cfg[:3])
	if err != nil {
		return "", false
	}
	mid, err := marshalCompact(cfg[4:9])
	if err != nil {
		return "", false
	}
	tail, err := marshalCompact(cfg[10:])
	if err != nil {
		return "", false
	}
	// 三段切片拼回一个完整的 JSON 数组，循环里只替换那两个槽位：
	//   head = "[a,b,c,"  mid = ",d,e,f,g,h,"  tail = ",i,…,z]"
	head = head[:len(head)-1] + ","
	mid = "," + mid[1:len(mid)-1] + ","
	tail = "," + tail[1:]

	seedBytes := []byte(seed)
	for i := 0; i < limit; i++ {
		final := head + fmt.Sprint(i) + mid + fmt.Sprint(i>>1) + tail
		enc := base64.StdEncoding.EncodeToString([]byte(final))
		h := sha3.Sum512(append(seedBytes, []byte(enc)...))
		if bytes.Compare(h[:diffLen], target[:diffLen]) <= 0 {
			return enc, true
		}
	}
	return "", false
}

// requirementsToken 造 sentinel 请求体里的 `p`。
//
// 它本身也是一次 PoW（难度 0fffff，约 1/16 中一次，很快），必须**真的解出来**：
// 上游不接受随便一段 base64。算不出来返回 (", false)，由调用方明确报错。
func requirementsToken(ua string, scripts []string, dataBuild string) (string, bool) {
	cfg := buildPowConfig(ua, scripts, dataBuild)
	ans, ok := powAnswer(fmt.Sprintf("%f", rand.Float64()), requirementsDifficulty, cfg, powMaxAttempts)
	if !ok {
		return "", false
	}
	return requirementsPrefix + ans, true
}

// proofToken 用上游下发的 seed / difficulty 求解校验和。
func proofToken(seed, difficulty, ua string, scripts []string, dataBuild string) (string, bool) {
	if strings.TrimSpace(seed) == "" || strings.TrimSpace(difficulty) == "" {
		return "", false
	}
	cfg := buildPowConfig(ua, scripts, dataBuild)
	ans, ok := powAnswer(seed, difficulty, cfg, powMaxAttempts)
	if !ok {
		return "", false
	}
	return powPrefix + ans, true
}

// marshalCompact 序列化成紧凑 JSON（不转义 HTML 字符）。
//
// 为什么要 SetEscapeHTML(false)：Python 参考实现用 ensure_ascii=False 输出，
// Go 的 json.Marshal 默认会把 < > & 转成 < 之类。虽然当前配置里没有这些字符，
// 但一旦上游往 navigator 特征里塞了 `&`，转义与否就会让同一次 PoW 算出两种字节串 ——
// 那是最难查的一类「本地测试全过、线上必然 403」。
func marshalCompact(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// randHex 生成 n 字节的十六进制串（参考实现里那个「随机 id」槽位）。
func randHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rand.IntN(256))
	}
	return hex.EncodeToString(b)
}

// hexDecode 把难度串解码成字节，容忍奇数长度（前面补 0）。
func hexDecode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s)%2 == 1 {
		s = "0" + s
	}
	out := make([]byte, len(s)/2)
	for i := range out {
		hi, err := hexVal(s[2*i])
		if err != nil {
			return nil, err
		}
		lo, err := hexVal(s[2*i+1])
		if err != nil {
			return nil, err
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexVal(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, fmt.Errorf("非法十六进制字符 %q", string(c))
}
