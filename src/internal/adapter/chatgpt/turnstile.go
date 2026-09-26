package chatgpt

// turnstile.go sentinel turnstile 的 VM 解释器。
//
// 上游下发 turnstile.dx（base64）后，与 requirements token（`p`）逐字符异或，
// 得到的是一段 JSON **opcode 程序**（官网 sentinel/sdk.js 里那个被压缩的 VM）。
// 它没有固定的 wasm/JS blob 要内嵌 —— 程序每次挑战都不一样，所以只能自己解释执行。
//
// 移植来源：ChatGPT2API-GO/internal/app/turnstile.go（Go，与我们的调用形态一致）
// 与 gpt4free/g4f/Provider/openai/turnstile_vm.py（opcode 语义写得更全，
// 我们按它的寄存器/队列模型实现，把 Go 那份里被特判掉的「window 对象」
// 还原成真正的嵌套字典 + 可调用值）。
//
// 语义要点（照抄，不要「优化」）：
//
//   - **寄存器表就是分发表**：执行 `[code, *args]` = 取 regs[code] 调用。
//     处理函数与数据共用一张表，所以程序会把 opcode 重绑到随机槽位（混淆），
//     也能通过调用类指令去调用 xor / set 这些处理函数。
//   - reg[9] 是**程序队列**：指令可以中途换掉它（自修改续跑，op0/op22/op30 用到）。
//   - op3 是唯一的正常结束（把参数原样记成结果），op4 是显式失败。
//   - 认不出的 opcode **跳过**（参考实现行为），不抛错。
//
// 覆盖面与已知边界写在 solveTurnstileToken 的注释里。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// vmUndef 表示 JS 的 undefined（寄存器里「没有这个值」）。
// 不能用 nil —— nil 在这里表示 JS 的 null，两者在 === 与字符串化上行为不同。
type vmUndef struct{}

var jsUndefined = vmUndef{}

// vmFunc 是注册在寄存器表里的可调用值（opcode 处理函数、以及 window 上的方法）。
type vmFunc func(args ...any) any

// turnstileVM 是一次挑战解释的执行体。
//
// regs 的键是**JS 对象属性名**（字符串），不是整数 —— 上游把 opcode 重绑到随机的
// **小数槽位**上做混淆（84.18 / 9.23 这类），而 9.23 与 9 在 JS 里是两个不同的键。
// 早期版本用 map[int]any 存（照 ChatGPT2API-GO 的 intAny 截断写法），
// 于是 9.23 被截成 9 —— 正好砸在程序队列寄存器的头上，队列被换成别的值、程序当场停摆，
// 表现成「解释器跑不出结果」。实测证据：程序里确实出现 `[84.18, 9.23, 7]`。
type turnstileVM struct {
	regs        map[string]any
	resolved    string
	hasResolved bool
	rejected    string
	start       time.Time
	scriptSrcs  []string
}

// solveTurnstileToken 解一次 turnstile 挑战，返回 token 或**说明原因**的失败。
//
// xorKey 是 requirements token（`p`）：dx 就是「与本次请求发出去的 p 逐码点异或后 base64」
// 的一段 opcode 程序，所以解码必须用**同一把 p**。这里曾经传空串（注释还写着「参考实现如此」，
// 但 gpt4free 的 process_turnstile_new 与 ChatGPT2API-GO 的 solveTurnstileToken 传的都是 p）——
// 解出来是一段乱码、程序永远跑不出结果，表现成「本地解释器覆盖不到上游的指令集」，
// 真实原因却是密钥用错。2026-09-26 用现网挑战核对：空串解出 22216 字节乱码，
// 换成 p 立刻得到合法程序（88 条指令），VM 随后正常跑出结果。
//
// 失败分三种，错误文本直接说是哪一种（红线一：不许把「我们没搞懂」讲成「上游不支持」）：
// 解不开（编码/密钥变了）、程序显式拒绝、程序跑完没产出结果。
func solveTurnstileToken(dx, xorKey string, scriptSrcs []string, userAgent string) (string, error) {
	dx = strings.TrimSpace(dx)
	if dx == "" {
		return "", fmt.Errorf("上游下发了空的 dx")
	}
	raw, err := b64decodeLoose(dx)
	if err != nil {
		return "", fmt.Errorf("dx 不是合法 base64")
	}
	plain := xorCipher(string(raw), xorKey)
	var program []any
	if err := json.Unmarshal([]byte(plain), &program); err != nil {
		return "", fmt.Errorf("dx 解出来不是合法程序（异或密钥或编码方式变了）")
	}
	if len(program) == 0 {
		return "", fmt.Errorf("dx 解出来是空程序")
	}
	vm := newTurnstileVM(program, xorKey, scriptSrcs, userAgent)
	vm.run()
	if !vm.hasResolved {
		if vm.rejected != "" {
			return "", fmt.Errorf("程序显式失败：%s", truncate(vm.rejected, 120))
		}
		return "", fmt.Errorf("程序跑完没有产出结果（%d 条指令，可能是上游换了 VM 指令集）", len(program))
	}
	return base64.StdEncoding.EncodeToString([]byte(vm.resolved)), nil
}

// newTurnstileVM 建表。userAgent 为空时用渠道默认 UA（navigator.userAgent 探针要用）。
func newTurnstileVM(program []any, xorKey string, scriptSrcs []string, userAgent string) *turnstileVM {
	if strings.TrimSpace(userAgent) == "" {
		userAgent = defaultUserAgent
	}
	if len(scriptSrcs) == 0 {
		scriptSrcs = []string{defaultPowScript}
	}
	v := &turnstileVM{
		regs:       map[string]any{},
		start:      time.Now(),
		scriptSrcs: scriptSrcs,
	}
	v.set(0, vmFunc(v.op0Reenter))
	v.set(1, vmFunc(v.op1Xor))
	v.set(2, vmFunc(v.op2Set))
	v.set(3, vmFunc(v.op3Resolve))
	v.set(4, vmFunc(v.op4Reject))
	v.set(5, vmFunc(v.op5Append))
	v.set(6, vmFunc(v.op6Index))
	v.set(7, vmFunc(v.op7Call))
	v.set(8, vmFunc(v.op8Copy))
	v.set(9, program)
	v.set(10, v.makeWindow(userAgent))
	v.set(11, vmFunc(v.op11FindScript))
	v.set(12, vmFunc(v.op12ExposeMap))
	v.set(13, vmFunc(v.op13CallCatch))
	v.set(14, vmFunc(v.op14JSONParse))
	v.set(15, vmFunc(v.op15JSONStringify))
	v.set(16, xorKey)
	v.set(17, vmFunc(v.op17AsyncCall))
	v.set(18, vmFunc(v.op18B64Decode))
	v.set(19, vmFunc(v.op19B64Encode))
	v.set(20, vmFunc(v.op20CondCall))
	v.set(21, vmFunc(v.op21CondDelta))
	v.set(22, vmFunc(v.op22Subprogram))
	v.set(23, vmFunc(v.op23CallIfDefined))
	v.set(24, vmFunc(v.op24Bind))
	v.set(25, vmFunc(noop))
	v.set(26, vmFunc(noop))
	v.set(27, vmFunc(v.op27Remove))
	v.set(28, vmFunc(noop))
	v.set(29, vmFunc(v.op29LessThan))
	v.set(30, vmFunc(v.op30Closure))
	v.set(33, vmFunc(v.op33Multiply))
	v.set(34, vmFunc(v.op34Await))
	v.set(35, vmFunc(v.op35Divide))
	return v
}

// makeWindow 造出程序会探的那一棵「window 树」。
//
// 为什么要有 localStorage.oai-did：上游 SDK 会读它拼进结果，读到 undefined
// 与读到空串在它眼里是两种客户端。这里给出与真实浏览器一致的一小组键（不含真实值），
// 与参考实现保持同样的形状。
func (v *turnstileVM) makeWindow(userAgent string) map[string]any {
	return map[string]any{
		"document": map[string]any{
			"location":        apiBase + "/",
			"visibilityState": "visible",
		},
		"navigator": map[string]any{
			"userAgent":           userAgent,
			"language":            "en-US",
			"languages":           []any{"en-US"},
			"hardwareConcurrency": float64(8),
		},
		"localStorage": map[string]any{
			"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4": "{}",
			"STATSIG_LOCAL_STORAGE_STABLE_ID":         "",
			"client-correlated-secret":                "",
			"oai/apps/capExpiresAt":                   "",
			"oai-did":                                 "",
			"STATSIG_LOCAL_STORAGE_LOGGING_REQUEST":   "",
			"UiState.isNavigationCollapsed.1":         "",
		},
		"screen": map[string]any{
			"width": float64(412), "height": float64(915),
			"availWidth": float64(412), "availHeight": float64(915),
		},
		"history": map[string]any{"length": float64(2)},
		"performance": map[string]any{
			// 性能计时器：参考实现返回「已跑的毫秒数 + 抖动」，我们照做。
			"now": vmFunc(func(...any) any {
				return float64(time.Since(v.start).Nanoseconds())/1e6 + rand.Float64()
			}),
		},
		"Math": map[string]any{
			"random": vmFunc(func(...any) any { return rand.Float64() }),
		},
		"Object": map[string]any{
			"create": vmFunc(func(...any) any { return map[string]any{} }),
			"keys": vmFunc(func(args ...any) any {
				if len(args) > 0 && isWindowLocalStorage(args[0]) {
					return []any{
						"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4",
						"STATSIG_LOCAL_STORAGE_STABLE_ID",
						"client-correlated-secret",
						"oai/apps/capExpiresAt",
						"oai-did",
						"STATSIG_LOCAL_STORAGE_LOGGING_REQUEST",
						"UiState.isNavigationCollapsed.1",
					}
				}
				return []any{}
			}),
		},
		"Reflect": map[string]any{
			"set": vmFunc(func(args ...any) any {
				if len(args) >= 3 {
					if m, ok := args[0].(map[string]any); ok {
						m[toStr(args[1])] = args[2]
					}
				}
				return jsUndefined
			}),
		},
	}
}

// isWindowLocalStorage 判断参数是不是 window.localStorage（op17 的 keys 探针用）。
func isWindowLocalStorage(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, ok = m["client-correlated-secret"]
	return ok
}

// ---------------------------------------------------------------------------
// 主循环
// ---------------------------------------------------------------------------

// run 抽干程序队列。op0/op22/op30 会在执行途中换掉 reg[9]（自修改续跑），
// 所以每轮都要重新取队列，而不是预先拷一份。
func (v *turnstileVM) run() {
	for {
		if v.hasResolved || v.rejected != "" {
			return
		}
		q, ok := v.get(9).([]any)
		if !ok || len(q) == 0 {
			return
		}
		op, ok := q[0].([]any)
		v.set(9, q[1:])
		if !ok || len(op) == 0 {
			continue
		}
		key := slotKey(op[0])
		fn, ok := v.regs[key].(vmFunc)
		if !ok {
			continue // 认不出的 opcode：跳过（参考实现行为，不抛错）
		}
		v.callOp(key, fn, op[1:])
	}
}

// callOp 执行一条指令。JS 里指令抛错会 reject 整个 VM，这里用 recover 对应。
func (v *turnstileVM) callOp(slot string, fn vmFunc, args []any) {
	defer func() {
		if r := recover(); r != nil {
			if v.rejected == "" {
				v.rejected = fmt.Sprintf("op %s failed: %v", slot, r)
			}
		}
	}()
	fn(args...)
}

// ---------------------------------------------------------------------------
// 取值 / 调用 / JS 强转工具
// ---------------------------------------------------------------------------

// slotKey 是程序里的槽位号 → JS 属性名。数字按 JS 的 String() 归一（9.23 → "9.23"，9 → "9"，
// 两者是不同的槽位；截断成 int 会把混淆用的随机小数槽位砸到整数槽位上）。
func slotKey(k any) string { return toStr(k) }

func (v *turnstileVM) get(k any) any {
	if val, ok := v.regs[slotKey(k)]; ok {
		return val
	}
	return jsUndefined
}

func (v *turnstileVM) set(k any, val any) { v.regs[slotKey(k)] = val }

func (v *turnstileVM) call(fn any, args []any) any {
	if f, ok := fn.(vmFunc); ok {
		return f(args...)
	}
	return nil
}

// toInt 把寄存器里的「槽位号」转成 int（JS 里这些位置是浮点数）。
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	default:
		return 0
	}
}

// toStr 对应 JS 的 `"" + v`。
//
// nil 是 JS null（→ "null"），vmUndef 是 JS undefined（→ "undefined"）——
// 这两者在 XOR 链里会变成完全不同的字节，不能合并处理。
func toStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case vmUndef:
		return "undefined"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case []any, map[string]any:
		if b, err := marshalCompact(x); err == nil {
			return b
		}
		return ""
	case vmFunc:
		// JS 里函数字符串化成源码文本。这里给一个近似值：它只会在「把函数当字符串
		// 拼进结果」这种边缘程序里出现，而那类程序本来就依赖具体 SDK 的实现细节。
		return "function () { [native code] }"
	default:
		return fmt.Sprint(v)
	}
}

// toNum 对应 JS 的 Number()（转不动的按 0，与参考实现一致）。
func toNum(v any) float64 {
	switch x := v.(type) {
	case nil, vmUndef:
		return 0
	case float64:
		return x
	case int:
		return float64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}

func isNum(v any) bool {
	switch v.(type) {
	case float64, int:
		return true
	}
	return false
}

// strictEq 对应 JS 的 ===。
func strictEq(a, b any) bool {
	_, au := a.(vmUndef)
	_, bu := b.(vmUndef)
	if au || bu {
		return au && bu
	}
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if isNum(a) && isNum(b) {
		return toNum(a) == toNum(b)
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok || bok {
		return aok && bok && as == bs
	}
	ab, aok := a.(bool)
	bb, bok := b.(bool)
	if aok || bok {
		return aok && bok && ab == bb
	}
	return false
}

// jsAdd 对应 JS 的 `+`：两边都是数字才做加法，否则字符串拼接。
func jsAdd(a, b any) any {
	if isNum(a) && isNum(b) {
		return toNum(a) + toNum(b)
	}
	return toStr(a) + toStr(b)
}

// jsLt 对应 JS 的 `<`：两边都是字符串按字典序，否则按数字比。
func jsLt(a, b any) bool {
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return as < bs
	}
	return toNum(a) < toNum(b)
}

// jsGet 对应 JS 的属性/下标读取（对象、数组、字符串都吃）。
func jsGet(obj any, key any) (any, bool) {
	switch o := obj.(type) {
	case map[string]any:
		s, ok := key.(string)
		if !ok {
			s = toStr(key)
		}
		val, exists := o[s]
		return val, exists
	case []any:
		i := toInt(key)
		if i < 0 || i >= len(o) {
			return jsUndefined, false
		}
		return o[i], true
	case string:
		runes := []rune(o)
		i := toInt(key)
		if i < 0 || i >= len(runes) {
			return jsUndefined, false
		}
		return string(runes[i]), true
	default:
		return jsUndefined, false
	}
}

// ---------------------------------------------------------------------------
// opcode 处理函数（编号与语义一一对应上游 VM）
// ---------------------------------------------------------------------------

// op0Reenter：用当前 reg[16] 解一段新的挑战 blob，并换掉程序队列。
func (v *turnstileVM) op0Reenter(args ...any) any {
	if len(args) < 1 {
		return nil
	}
	raw, err := b64decodeLoose(toStr(args[0]))
	if err != nil {
		return nil
	}
	key := toStr(v.get(16))
	var program []any
	if err := json.Unmarshal([]byte(xorCipher(string(raw), key)), &program); err != nil {
		return nil
	}
	if len(program) > 0 {
		v.set(9, program)
	}
	return nil
}

// op1Xor：reg[n] = xor(str(reg[n]), str(reg[e]))。
func (v *turnstileVM) op1Xor(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	n, e := args[0], args[1]
	v.set(n, xorCipher(toStr(v.get(n)), toStr(v.get(e))))
	return nil
}

// op2Set：reg[n] = 原样值（注意是 raw，不做取值/求值）。
func (v *turnstileVM) op2Set(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	v.set(args[0], args[1])
	return nil
}

// op3Resolve：正常结束，参数就是最终结果（外层的 base64 由调用方做）。
func (v *turnstileVM) op3Resolve(args ...any) any {
	if v.hasResolved || v.rejected != "" {
		return nil
	}
	if len(args) < 1 {
		v.rejected = "op 3 without result"
		return nil
	}
	v.resolved = toStr(args[0])
	v.hasResolved = true
	return nil
}

// op4Reject：显式失败。
func (v *turnstileVM) op4Reject(args ...any) any {
	if v.hasResolved || v.rejected != "" {
		return nil
	}
	if len(args) > 0 {
		v.rejected = toStr(args[0])
	} else {
		v.rejected = "rejected"
	}
	return nil
}

// op5Append：数组 push / 字符串拼接。
func (v *turnstileVM) op5Append(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	n, e := args[0], args[1]
	cur := v.get(n)
	if list, ok := cur.([]any); ok {
		// Go 的 append 可能换底层数组，所以要把新切片写回寄存器；
		// Python 那边是原地 append（对别的引用可见），这里按「写回」处理，
		// 本轮所有参考程序里两者结果一致。
		v.set(n, append(list, v.get(e)))
		return nil
	}
	v.set(n, jsAdd(cur, v.get(e)))
	return nil
}

// op6Index：reg[n] = reg[e][reg[r]]。
func (v *turnstileVM) op6Index(args ...any) any {
	if len(args) < 3 {
		return nil
	}
	n, e, r := args[0], args[1], args[2]
	if val, ok := jsGet(v.get(e), v.get(r)); ok {
		v.set(n, val)
		return nil
	}
	v.set(n, jsUndefined)
	return nil
}

// op7Call：reg[n](*[reg[a] for a in args]) —— 参数**求值后**传。
func (v *turnstileVM) op7Call(args ...any) any {
	if len(args) < 1 {
		return nil
	}
	fn := v.get(args[0])
	callArgs := make([]any, 0, len(args)-1)
	for _, a := range args[1:] {
		callArgs = append(callArgs, v.get(a))
	}
	return v.call(fn, callArgs)
}

// op8Copy：reg[n] = reg[e]。
func (v *turnstileVM) op8Copy(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	v.set(args[0], v.get(args[1]))
	return nil
}

// op11FindScript：reg[n] = document 里第一个匹配正则 reg[e] 的 script src，否则 null。
func (v *turnstileVM) op11FindScript(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	n, e := args[0], args[1]
	pattern, ok := v.get(e).(string)
	if !ok {
		v.set(n, nil)
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		v.set(n, nil)
		return nil
	}
	for _, src := range v.scriptSrcs {
		if m := re.FindString(src); m != "" {
			v.set(n, m)
			return nil
		}
	}
	v.set(n, nil)
	return nil
}

// op12ExposeMap：reg[n] = 寄存器表本身。
func (v *turnstileVM) op12ExposeMap(args ...any) any {
	if len(args) < 1 {
		return nil
	}
	v.set(args[0], v.regs)
	return nil
}

// op13CallCatch：reg[n] = reg[e](*原始参数)，抛错时把错误文本存进去。
func (v *turnstileVM) op13CallCatch(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	n, e := args[0], args[1]
	v.set(n, v.callCatching(v.get(e), args[2:]))
	return nil
}

// op14JSONParse：reg[n] = JSON.parse(str(reg[e]))。
func (v *turnstileVM) op14JSONParse(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	n, e := args[0], args[1]
	var parsed any
	if err := json.Unmarshal([]byte(toStr(v.get(e))), &parsed); err != nil {
		v.set(n, jsUndefined)
		return nil
	}
	v.set(n, normaliseJSON(parsed))
	return nil
}

// op15JSONStringify：reg[n] = JSON.stringify(reg[e])。
func (v *turnstileVM) op15JSONStringify(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	n, e := args[0], args[1]
	if s, err := marshalCompact(jsonSafe(v.get(e))); err == nil {
		v.set(n, s)
	}
	return nil
}

// op17AsyncCall：reg[n] = reg[e](*[reg[a] for a in args])，语义同 op7 但会吞异常。
func (v *turnstileVM) op17AsyncCall(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	n, e := args[0], args[1]
	callArgs := make([]any, 0, len(args)-2)
	for _, a := range args[2:] {
		callArgs = append(callArgs, v.get(a))
	}
	v.set(n, v.callCatching(v.get(e), callArgs))
	return nil
}

// op18B64Decode：reg[n] = atob(str(reg[n]))。
func (v *turnstileVM) op18B64Decode(args ...any) any {
	if len(args) < 1 {
		return nil
	}
	n := args[0]
	raw, err := b64decodeLoose(toStr(v.get(n)))
	if err != nil {
		v.set(n, "")
		return nil
	}
	v.set(n, string(raw))
	return nil
}

// op19B64Encode：reg[n] = btoa(str(reg[n]))。
func (v *turnstileVM) op19B64Encode(args ...any) any {
	if len(args) < 1 {
		return nil
	}
	n := args[0]
	v.set(n, base64.StdEncoding.EncodeToString([]byte(toStr(v.get(n)))))
	return nil
}

// op20CondCall：严格相等时调用 reg[r]（原始参数）。
func (v *turnstileVM) op20CondCall(args ...any) any {
	if len(args) < 3 {
		return nil
	}
	n, e, r := args[0], args[1], args[2]
	if strictEq(v.get(n), v.get(e)) {
		return v.call(v.get(r), args[3:])
	}
	return nil
}

// op21CondDelta：数值差超过阈值时调用 reg[o]（原始参数）。
func (v *turnstileVM) op21CondDelta(args ...any) any {
	if len(args) < 4 {
		return nil
	}
	n, e, r, o := args[0], args[1], args[2], args[3]
	if math.Abs(toNum(v.get(n))-toNum(v.get(e))) > toNum(v.get(r)) {
		return v.call(v.get(o), args[4:])
	}
	return nil
}

// op22Subprogram：跑一段内联子程序（临时换掉程序队列，跑完恢复）。
func (v *turnstileVM) op22Subprogram(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	n := args[0]
	saved := v.get(9)
	sub, ok := args[1].([]any)
	if !ok {
		v.set(n, "undefined")
		return nil
	}
	v.set(9, sub)
	v.run()
	v.set(n, "undefined")
	v.set(9, saved)
	return nil
}

// op23CallIfDefined：reg[n] 不是 undefined 时调用 reg[e]（原始参数）。
func (v *turnstileVM) op23CallIfDefined(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	n, e := args[0], args[1]
	if _, isUndef := v.get(n).(vmUndef); !isUndef {
		return v.call(v.get(e), args[2:])
	}
	return nil
}

// op24Bind：reg[n] = reg[e][reg[r]]（绑定形态，取不到就拼成 "a.b" 字符串）。
func (v *turnstileVM) op24Bind(args ...any) any {
	if len(args) < 3 {
		return nil
	}
	n, e, r := args[0], args[1], args[2]
	obj, key := v.get(e), v.get(r)
	if val, ok := jsGet(obj, key); ok {
		v.set(n, val)
		return nil
	}
	v.set(n, toStr(obj)+"."+toStr(key))
	return nil
}

// op27Remove：数组删元素 / 数值相减。
func (v *turnstileVM) op27Remove(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	n, e := args[0], args[1]
	cur, val := v.get(n), v.get(e)
	if list, ok := cur.([]any); ok {
		out := make([]any, 0, len(list))
		removed := false
		for _, it := range list {
			if !removed && strictEq(it, val) {
				removed = true
				continue
			}
			out = append(out, it)
		}
		v.set(n, out)
		return nil
	}
	v.set(n, toNum(cur)-toNum(val))
	return nil
}

// op29LessThan：reg[n] = reg[e] < reg[r]。
func (v *turnstileVM) op29LessThan(args ...any) any {
	if len(args) < 3 {
		return nil
	}
	n, e, r := args[0], args[1], args[2]
	v.set(n, jsLt(v.get(e), v.get(r)))
	return nil
}

// op30Closure：在 reg[n] 上定义一个闭包。
//
// 闭包被调用时：把实参绑到声明的位置槽、把程序队列换成函数体、跑干、取 reg[e] 作为返回值，
// 再恢复原队列。这是上游 SDK 用来「延迟执行 + 逐段续跑」的手法。
func (v *turnstileVM) op30Closure(args ...any) any {
	if len(args) < 3 {
		return nil
	}
	n, e := args[0], args[1]
	// args[2] 是 Python 参考实现签名里的 r —— 它接收了但不使用，
	// 这里保留同样的「跳过」语义（改签名会与上游程序的参数个数对不上）。
	rest := args[3:]
	bindSlots := []any{}
	body := rest
	if len(rest) > 0 {
		if slots, ok := rest[0].([]any); ok {
			bindSlots = slots
			body = rest[1:]
		}
	}
	v.set(n, vmFunc(func(callArgs ...any) any {
		saved := v.get(9)
		for i, slot := range bindSlots {
			if i >= len(callArgs) {
				break
			}
			v.set(slot, callArgs[i])
		}
		v.set(9, body)
		v.run()
		result := v.get(e)
		if prev, ok := saved.([]any); ok {
			v.set(9, prev)
		} else {
			v.set(9, []any{})
		}
		return result
	}))
	return nil
}

// op33Multiply / op35Divide / op34Await：纯算术与 await。
func (v *turnstileVM) op33Multiply(args ...any) any {
	if len(args) < 3 {
		return nil
	}
	v.set(args[0], toNum(v.get(args[1]))*toNum(v.get(args[2])))
	return nil
}

func (v *turnstileVM) op34Await(args ...any) any {
	if len(args) < 2 {
		return nil
	}
	v.set(args[0], v.get(args[1]))
	return nil
}

func (v *turnstileVM) op35Divide(args ...any) any {
	if len(args) < 3 {
		return nil
	}
	divisor := toNum(v.get(args[2]))
	if divisor == 0 {
		v.set(args[0], float64(0))
		return nil
	}
	v.set(args[0], toNum(v.get(args[1]))/divisor)
	return nil
}

func noop(...any) any { return nil }

// callCatching 调用并吞掉 panic（对应 op13/op17 的 try/catch）。
func (v *turnstileVM) callCatching(fn any, args []any) (out any) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprint(r)
		}
	}()
	return v.call(fn, args)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// xorCipher 逐**码点**异或（与参考实现一致：JS 的 charCode 语义）。
//
// 注意不是按字节：异或结果可能超出 255，转回 UTF-8 时一个字符会变成多个字节，
// 而 base64/JSON 处理的就是那串字节。按字节异或会把结果整段写错。
func xorCipher(text, key string) string {
	if key == "" {
		return text
	}
	textRunes := []rune(text)
	keyRunes := []rune(key)
	if len(keyRunes) == 0 {
		return text
	}
	out := make([]rune, len(textRunes))
	for i, ch := range textRunes {
		out[i] = ch ^ keyRunes[i%len(keyRunes)]
	}
	return string(out)
}

// b64decodeLoose 容忍标准/URL 两种字母表与缺失的填充。
func b64decodeLoose(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("空 base64")
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if raw, err := enc.DecodeString(s); err == nil {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("无法解码 base64")
}

// normaliseJSON 把 encoding/json 解出来的 map[string]any 原样返回。
//
// 留着这个函数是为了标注一件事：JSON 里的 null 解出来是 nil，在 VM 里正好对应
// JS 的 null（而 undefined 是另一个哨兵值）。不要「顺手」把它换成别的。
func normaliseJSON(v any) any { return v }

// jsonSafe 把 VM 内部值转成可序列化的形态（去掉函数与 undefined）。
func jsonSafe(v any) any {
	switch x := v.(type) {
	case vmFunc, vmUndef:
		return nil
	default:
		return x
	}
}
