package chatgpt

// pow_test.go PoW 的测试。
//
// 测试**自己按同一套算法独立复算一遍**（用标准库的 hex.DecodeString + 手写的
// 前缀比较，不复用 pow.go 里的任何辅助函数），再拿实现返回的答案去对。
// 只断言「前缀对不对」是没有意义的 —— 那种测试在算法整体写错时照样全绿。

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"golang.org/x/crypto/sha3"
)

// verifyAgainstDifficulty 独立复算：sha3_512(seed + base64) 的前 len(difficulty)/2 字节
// 必须 <= difficulty 解码出的字节。
func verifyAgainstDifficulty(t *testing.T, seed, difficulty, answer string) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(answer)
	if err != nil {
		t.Fatalf("答案不是合法 base64：%v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("答案不是合法 JSON：%s", truncate(string(raw), 120))
	}
	sum := sha3.Sum512(append([]byte(seed), []byte(answer)...))
	if len(difficulty)%2 == 1 {
		difficulty = "0" + difficulty // 与实现同一约定：奇数长度前面补 0
	}
	target, err := hex.DecodeString(difficulty)
	if err != nil {
		t.Fatalf("难度串不合法：%v", err)
	}
	n := len(difficulty) / 2
	if bytes.Compare(sum[:n], target[:n]) > 0 {
		t.Fatalf("答案没有满足难度：难度 %s，实际前缀 %x", difficulty, sum[:n])
	}
}

// decodePowConfig 把答案解回数组，并检查它真的是那个 18 元素的形状。
func decodePowConfig(t *testing.T, answer string) []any {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(answer)
	if err != nil {
		t.Fatalf("答案不是合法 base64：%v", err)
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("答案不是 JSON 数组：%v", err)
	}
	if len(arr) != 18 {
		t.Fatalf("配置数组应为 18 元素，得到 %d：%s", len(arr), truncate(string(raw), 200))
	}
	return arr
}

func TestPowAnswerSatisfiesDifficulty(t *testing.T) {
	cfg := buildPowConfig(defaultUserAgent, []string{defaultPowScript}, "c/test/_")
	seed := "0.123456"
	answer, ok := powAnswer(seed, requirementsDifficulty, cfg, powMaxAttempts)
	if !ok {
		t.Fatal("0fffff 这种难度应当能在上限内解出")
	}
	verifyAgainstDifficulty(t, seed, requirementsDifficulty, answer)

	// 两个被循环改写的槽位必须满足 i 与 i>>1 的关系（i>>1 == floor(i/2)）。
	arr := decodePowConfig(t, answer)
	i, iok := arr[3].(float64)
	half, hok := arr[9].(float64)
	if !iok || !hok {
		t.Fatalf("第 4/第 10 个元素必须是数字：%v / %v", arr[3], arr[9])
	}
	if half != math.Floor(i/2) {
		t.Fatalf("第 10 个元素应为第 4 个元素的一半（i>>1）：i=%v half=%v", i, half)
	}

	// 固定形状的抽查：UA 在第 5 个、脚本在第 6 个、data-build 在第 7 个。
	if got := arr[4]; got != defaultUserAgent {
		t.Fatalf("第 5 个元素应是 UA，得到 %v", got)
	}
	if got := arr[5]; got != defaultPowScript {
		t.Fatalf("第 6 个元素应是脚本地址，得到 %v", got)
	}
	if got := arr[6]; got != "c/test/_" {
		t.Fatalf("第 7 个元素应是 data-build，得到 %v", got)
	}
}

// 最容易出分歧的一处：比较必须按字节，长度是奇数时不能按十六进制字符串比。
func TestPowAnswerComparesByBytes(t *testing.T) {
	cfg := buildPowConfig(defaultUserAgent, nil, "")
	// "ffffff" 是三字节的全 ff，任何哈希都满足 —— 用来锁定「比较方向」没写反。
	answer, ok := powAnswer("seed", "ffffff", cfg, 1)
	if !ok {
		t.Fatal("全 ff 的难度第一轮就该命中")
	}
	verifyAgainstDifficulty(t, "seed", "ffffff", answer)
	arr := decodePowConfig(t, answer)
	if arr[3].(float64) != 0 {
		t.Fatalf("第一轮命中时 i 应为 0，得到 %v", arr[3])
	}

	// 奇数长度：实现必须在前面补 0 之后按 len/2 字节比较。
	// 注意补 0 是补在**前面**，所以奇数长度的难度天然更难（首字节变成 0x0X），
	// 这里给足次数（1/16 的命中率，上限很宽裕）。
	answer, ok = powAnswer("seed", "ffffff0", cfg, powMaxAttempts)
	if !ok {
		t.Fatal("奇数长度的难度也应能在上限内命中")
	}
	verifyAgainstDifficulty(t, "seed", "ffffff0", answer)
}

func TestPowAnswerGivesUpOnImpossibleDifficulty(t *testing.T) {
	cfg := buildPowConfig(defaultUserAgent, nil, "")
	// 64 字节全 0：要求 sha3-512 的结果全为 0，事实上不可能。
	hard := strings.Repeat("00", 64)
	answer, ok := powAnswer("seed", hard, cfg, 3)
	if ok {
		t.Fatal("不可能的难度不该被判为解出")
	}
	if answer != "" {
		t.Fatalf("解不出时必须返回空串（而不是伪造一个答案）：%q", answer)
	}
}

func TestRequirementsTokenShape(t *testing.T) {
	tok, ok := requirementsToken(defaultUserAgent, []string{defaultPowScript}, "c/abc/_")
	if !ok {
		t.Fatal("requirements token 应当能造出来")
	}
	if !strings.HasPrefix(tok, requirementsPrefix) {
		t.Fatalf("前缀应为 %q，得到 %q", requirementsPrefix, truncate(tok, 20))
	}
	// p 的种子是本地随机、不发给上游，所以没法复算哈希；能查的是形状。
	arr := decodePowConfig(t, strings.TrimPrefix(tok, requirementsPrefix))
	if len(arr) != 18 {
		t.Fatalf("配置应是 18 元素，得到 %d", len(arr))
	}
}

func TestProofTokenPrefix(t *testing.T) {
	tok, ok := proofToken("0.5", requirementsDifficulty, defaultUserAgent, nil, "")
	if !ok {
		t.Fatal("proof token 应当能算出来")
	}
	if !strings.HasPrefix(tok, powPrefix) {
		t.Fatalf("前缀应为 %q，得到 %q", powPrefix, truncate(tok, 20))
	}
	verifyAgainstDifficulty(t, "0.5", requirementsDifficulty, strings.TrimPrefix(tok, powPrefix))

	// seed / difficulty 缺失时明确失败（而不是发一个空 token 上去）。
	if _, ok := proofToken("", requirementsDifficulty, defaultUserAgent, nil, ""); ok {
		t.Fatal("seed 为空时必须判为失败")
	}
	if _, ok := proofToken("0.5", "", defaultUserAgent, nil, ""); ok {
		t.Fatal("difficulty 为空时必须判为失败")
	}
}

func TestHexDecodeTolerantToOddLength(t *testing.T) {
	got, err := hexDecode("abc")
	if err != nil {
		t.Fatalf("奇数长度应当容忍：%v", err)
	}
	if len(got) != 2 || got[0] != 0x0a || got[1] != 0xbc {
		t.Fatalf("补 0 后的结果不对：%x", got)
	}
	if _, err := hexDecode("zz"); err == nil {
		t.Fatal("非法字符应当报错")
	}
}

func TestBuildPowConfigKeepsShape(t *testing.T) {
	arr := buildPowConfig("ua", nil, "")
	if len(arr) != 18 {
		t.Fatalf("配置数组形状变了：%d 个元素", len(arr))
	}
	if arr[2] != 4294705152 {
		t.Fatalf("第 3 个元素是上游认的固定值，不能改：%v", arr[2])
	}
	if _, ok := arr[1].(string); !ok {
		t.Fatalf("第 2 个元素（时区串）必须是字符串：%T", arr[1])
	}
	if !strings.Contains(arr[1].(string), "GMT-0500 (Eastern Standard Time)") {
		t.Fatalf("时区串格式变了：%v", arr[1])
	}
	// 没有脚本时回落默认地址（不能是空串：空串会进配置数组，形状就变了）。
	if arr[5] != defaultPowScript {
		t.Fatalf("脚本地址回落不对：%v", arr[5])
	}
}
