package chatgpt

// turnstile_test.go VM 解释器的测试。
//
// 样例分两类：
//  1. **参考实现里的那条**（ChatGPT2API-GO/internal/app/upstream_test.go:129）——
//     `[[3,"ok"]]` → "b2s="，认证态下异或密钥为空串；
//  2. 我们自己按 opcode 表构造的小程序，逐条压容易写错的语义：
//     set/xor/index/append/reenter/reject/cond-call/subprogram/window 取值。
//
// 边界必须说清楚：/tmp/refs 里的四份参考实现都**没有**留下真实的 dx 挑战体，
// 所以「解释器对真实程序是否够用」只能在真上游验（见 solveTurnstileToken 的注释）。
// 这里能保证的是：opcode 语义与参考实现一致、解不出来时明确失败。

import (
	"encoding/base64"
	"strings"
	"testing"
)

// 构造程序时只用**大于 35 的槽位**（36 起）写字。
//
// 原因不是风格：寄存器表与 opcode 分发表是同一张表，写 regs[5] 就把「op5 处理函数」
// 覆盖掉了（之后 `[5,...]` 这条指令会被当成「认不出的 opcode」跳过）。
// 真实程序靠「把 opcode 重绑到随机槽位」做混淆，也正是这个机制 —— 参考实现同样如此。
//
// blob 把一段 opcode 程序按上游的形态打包：与密钥异或后 base64。
func blob(program, xorKey string) string {
	return base64.StdEncoding.EncodeToString([]byte(xorCipher(program, xorKey)))
}

func solveBlob(t *testing.T, program, xorKey string) (string, bool) {
	t.Helper()
	return solveTurnstileToken(blob(program, xorKey), xorKey, []string{defaultPowScript}, defaultUserAgent)
}

// 参考实现自带的样例：认证态（密钥为空串）。
func TestSolveTurnstileReferenceSample(t *testing.T) {
	dx := base64.StdEncoding.EncodeToString([]byte(`[[3,"ok"]]`))
	got, ok := solveTurnstileToken(dx, "", nil, defaultUserAgent)
	if !ok {
		t.Fatal("参考实现给的样例必须能解出")
	}
	if got != "b2s=" { // base64("ok")
		t.Fatalf("样例结果应为 b2s=，得到 %q", got)
	}
}

// [[2,n,v]] 写入，[[7,3,n]] 经「求值调用」把 reg[n] 交给 op3 结束。
func TestSolveTurnstileSetAndEvaluatedResolve(t *testing.T) {
	got, ok := solveBlob(t, `[[2,36,"hello"],[7,3,36]]`, "")
	if !ok || got != "aGVsbG8=" { // base64("hello")
		t.Fatalf("期望 hello 的结果，得到 %q（ok=%v）", got, ok)
	}
}

// op1 是逐码点异或（不是按字节）。
func TestSolveTurnstileXor(t *testing.T) {
	// "aB" 与 "\u0001" 异或 → "`C"
	got, ok := solveBlob(t, `[[2,36,"aB"],[2,37,"\u0001"],[1,36,37],[7,3,36]]`, "")
	if !ok || got != "YEM=" { // base64("`C")
		t.Fatalf("异或结果不对：%q（ok=%v）", got, ok)
	}
}

// op5 追加：数组 push 后字符串化成 JSON 再 base64。
func TestSolveTurnstileAppendToArray(t *testing.T) {
	got, ok := solveBlob(t, `[[2,36,[]],[2,37,"x"],[5,36,37],[7,3,36]]`, "")
	if !ok || got != "WyJ4Il0=" { // base64(`["x"]`)
		t.Fatalf("数组 push 的结果不对：%q（ok=%v）", got, ok)
	}
}

// op4 是显式失败：必须报「没解出」，而不是把拒绝理由当结果返回。
func TestSolveTurnstileReject(t *testing.T) {
	if got, ok := solveBlob(t, `[[4,"nope"]]`, ""); ok {
		t.Fatalf("被拒绝的程序不该算解出：%q", got)
	}
}

// op20 条件调用：两个寄存器严格相等时才调用 reg[r]。
func TestSolveTurnstileConditionalCall(t *testing.T) {
	got, ok := solveBlob(t, `[[2,36,1],[2,37,1],[20,36,37,3,"eq"]]`, "")
	if !ok || got != "ZXE=" { // base64("eq")
		t.Fatalf("条件调用结果不对：%q（ok=%v）", got, ok)
	}
	// 不相等时什么都不做 → 没有结果。
	if _, ok := solveBlob(t, `[[2,36,1],[2,37,2],[20,36,37,3,"eq"]]`, ""); ok {
		t.Fatal("条件不成立时不该解出结果")
	}
}

// op0 用当前密钥重入：外层程序只是把内层程序交给 reg[9]。
func TestSolveTurnstileReenter(t *testing.T) {
	inner := blob(`[[3,"deep"]]`, "K")
	got, ok := solveBlob(t, `[[0,"`+inner+`"]]`, "K")
	if !ok || got != "ZGVlcA==" { // base64("deep")
		t.Fatalf("重入后的结果不对：%q（ok=%v）", got, ok)
	}
}

// op22 子程序：跑完内联队列后回到外层。
func TestSolveTurnstileSubprogram(t *testing.T) {
	got, ok := solveBlob(t, `[[2,36,"sub"],[22,36,[[7,3,36]]]]`, "")
	if !ok || got != "c3Vi" { // base64("sub")
		t.Fatalf("子程序结果不对：%q（ok=%v）", got, ok)
	}
}

// op6 + window 树：两级属性取值（window.document.location）。
func TestSolveTurnstileWindowLookup(t *testing.T) {
	program := `[[2,36,"document"],[6,37,10,36],[2,38,"location"],[6,39,37,38],[7,3,39]]`
	got, ok := solveBlob(t, program, "")
	if !ok || got != "aHR0cHM6Ly9jaGF0Z3B0LmNvbS8=" { // base64("https://chatgpt.com/")
		t.Fatalf("window 取值结果不对：%q（ok=%v）", got, ok)
	}
}

// 认不出的 opcode 跳过（参考实现行为），不影响后面的指令。
func TestSolveTurnstileSkipsUnknownOpcode(t *testing.T) {
	got, ok := solveBlob(t, `[[99,"x"],[3,"ok"]]`, "")
	if !ok || got != "b2s=" {
		t.Fatalf("未知 opcode 应被跳过：%q（ok=%v）", got, ok)
	}
}

// 异或密钥非空时：打包与求解必须用同一把密钥（用错就是一段解不开的 JSON）。
func TestSolveTurnstileUsesXorKey(t *testing.T) {
	got, ok := solveBlob(t, `[[3,"ok"]]`, "requirements-token")
	if !ok || got != "b2s=" {
		t.Fatalf("带密钥的挑战解错了：%q（ok=%v）", got, ok)
	}
}

// 解不出来的几种输入都必须明确失败，而不是返回空 token 当成功。
func TestSolveTurnstileFailsExplicitly(t *testing.T) {
	cases := []struct {
		name string
		dx   string
	}{
		{"空 dx", ""},
		{"不是 base64", "!!!not-base64!!!"},
		{"base64 但内容不是 JSON", base64.StdEncoding.EncodeToString([]byte("not json at all"))},
		{"空程序", base64.StdEncoding.EncodeToString([]byte(`[]`))},
		{"程序里没有 op3", base64.StdEncoding.EncodeToString([]byte(`[[2,36,"x"]]`))},
	}
	for _, c := range cases {
		if got, ok := solveTurnstileToken(c.dx, "", nil, defaultUserAgent); ok {
			t.Fatalf("%s：不该判为解出，得到 %q", c.name, got)
		}
	}
}

// 解释器不许 panic（上游程序里含畸形指令是常态）。
func TestSolveTurnstileTolerantToMalformedOps(t *testing.T) {
	programs := []string{
		`["not-an-op"]`,
		`[[],[3,"ok"]]`,
		`[[1]]`,
		`[[6,1,2]]`,
		`[[30,36,3,[1,2],[[7,3,36]]]]`,
		`[[2,36,"x"],[35,37,36,38]]`,
	}
	for _, p := range programs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("程序 %s 让解释器 panic：%v", p, r)
				}
			}()
			_, _ = solveBlob(t, p, "")
		}()
	}
}

// 结果必须是「解出的字符串」的 base64，而不是它被二次编码过。
func TestSolveTurnstileBase64EncodesOnce(t *testing.T) {
	got, ok := solveBlob(t, `[[3,"ok"]]`, "")
	if !ok {
		t.Fatal("应能解出")
	}
	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil || string(raw) != "ok" {
		t.Fatalf("结果不是一次 base64：%q（解码后 %q，err=%v）", got, string(raw), err)
	}
	if strings.HasPrefix(got, "gAAAAA") {
		t.Fatal("turnstile token 不该带 sentinel 的 gAAAAA 前缀（那是 PoW 的前缀）")
	}
}
