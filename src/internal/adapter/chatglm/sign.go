package chatglm

// sign.go 智谱的时间戳变换 + 签名。
//
// 这是本渠道**最脆**的一环：算法与密钥都可能随官网改版变化（参考实现的源码里就写着两处
// 「官网变化记得更新」）。所以把它单独成文件，并且：
//   - 密钥可由凭证 Extra["sign_secret"] 覆盖 —— 官网换密钥时不用改代码、不用等发版；
//   - 变换过程逐步写清楚，改版时只需改这一处。
//
// 签名不对的症状：上游回 4xx 且报文里多半只提「参数/权限」，看不出是签名问题 ——
// 所以请求失败时我们会把上游原话带出去（红线一），而不是只回一句「请求失败」。

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"
)

type requestSign struct {
	timestamp string
	nonce     string
	sign      string
}

// makeSign 生成一次请求的签名（时间戳、随机串、md5 三件套，每个请求都要重新算）。
func makeSign(secret string) requestSign {
	ts := transformTimestamp(time.Now().UnixMilli())
	nonce := uuidNoDash()
	sum := md5.Sum([]byte(ts + "-" + nonce + "-" + secret))
	return requestSign{timestamp: ts, nonce: nonce, sign: hex.EncodeToString(sum[:])}
}

// transformTimestamp 把 13 位毫秒时间戳的**倒数第二位**换成「各位数字之和 − 该位」的个位数。
//
// 为什么要做这种变换：上游用它挡「服务端直接拿 now() 拼请求、不改一位」的做法 ——
// 规则不对签名就不对。注意替换的是**倒数第二位**（下标 len-2），不是末位；
// 写错一位的表现是所有请求都失败，且报文看不出原因。
func transformTimestamp(ms int64) string {
	digits := strconv.FormatInt(ms, 10)
	if len(digits) < 2 {
		return digits
	}
	sum := 0
	for _, r := range digits {
		sum += int(r - '0')
	}
	idx := len(digits) - 2
	a := (sum - int(digits[idx]-'0')) % 10
	return digits[:idx] + strconv.Itoa(a) + digits[idx+1:]
}

// uuidNoDash 生成去横线的 uuid v4（上游要的就是 32 位十六进制串）。
func uuidNoDash() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return hex.EncodeToString(b[:])
}
