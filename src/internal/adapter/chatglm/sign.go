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

// uuidNoDash 生成去横线的 uuid，用作 nonce / X-Device-Id / X-Request-Id（32 位十六进制串）。
//
// 是 **v1（时间版）**，不是 v4 —— 依据参考实现 **GLM-Free-API**（TS 那份，chatglm.cn 上游）
// 的 `src/lib/util.ts:10` `import { v1 as uuid } from "uuid"`
// 与 util.ts:29 `uuid: (separator = true) => (separator ? uuid() : uuid().replace(/\-/g,""))`：
// 它三处（sign.nonce、X-Device-Id、X-Request-Id）用的都是 `util.uuid(false)`。
// 形态差异在于**第 13 位十六进制（版本位）是 '1'**，且高 32 位是时间低段 ——
// 上游若校验 nonce 的 UUID 版本（或从里面取时间做新鲜度判断），v4 会被判成非法客户端。
// 反过来，只要签名的 md5 输入完全一致，v1 与 v4 都能通过「不校验版本」的上游，所以照抄 v1 更稳。
func uuidNoDash() string {
	// 1582-10-15 00:00:00 UTC（UUID v1 纪元）到 1970-01-01 的 100ns 数。
	const gregorianOffset = 122192928000000000

	var b [16]byte
	// time_low(32) / time_mid(16) / time_hi_and_version(12)：从 v1 纪元起的 100ns 计数，大端。
	// 布局与 npm uuid 的 v1 输出逐字节一致（UUID 字符串去掉横线后的 32 位 hex）。
	t := uint64(time.Now().UnixNano()/100) + gregorianOffset
	b[0], b[1], b[2], b[3] = byte(t>>24), byte(t>>16), byte(t>>8), byte(t)
	b[4], b[5] = byte(t>>40), byte(t>>32)
	b[6] = byte(t>>56)&0x0f | 0x10 // 低 4 位是 time_hi 的高 4 位，高 4 位固定版本号 1
	b[7] = byte(t >> 48)
	// clock_seq(14 位随机) + 变体位（10）；node(48 位随机) 的最高字节多播位置 1。
	// 这三段参考实现也是随机的（uuid 包在取不到网卡时用随机 node），值与签名无关，只需形态合法。
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	b[8] = rnd[0]&0x3f | 0x80
	b[9] = rnd[1]
	copy(b[10:], rnd[2:8])
	b[10] |= 0x01
	return hex.EncodeToString(b[:])
}
