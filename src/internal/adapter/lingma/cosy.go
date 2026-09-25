// cosy.go 实现灵码的 COSY 请求签名：`Authorization: Bearer COSY.<b64(payload)>.<md5>` +
// 配套的 Cosy-* 请求头。
//
// 为什么在本包内自成一份（而不是 import qodercn 的实现）：
//   - 仓库里没有**导出**可复用的 COSY 实现 —— qodercn 与 qodercom 各自持有 package-private 的
//     cosy.go（两份逐字一致），这是既有惯例；本轮硬性要求又限定只能改 lingma/ 目录，
//     没法把 qodercn 的实现提出来共享。
//   - 两家的签名**原材料不同**，硬复用会互相污染：QoderCN 的 cosy_key 是拿 RSA 公钥现场包裹
//     临时密钥算出来的（每次会话新生成），cosyVersion 是 1.0.10、clienttype 5、要带
//     Machinetype/Modeltoken；灵码的 cosy_key 直接来自 IDE 缓存（不能重算），
//     cosyVersion 2.11.2、clienttype 2、deptype 头一律为空。
//   - 真正相同的只有「md5 原文的拼接顺序」这一段，已逐字对齐 qodercn 与参考实现，并在测试里
//     用独立复算的 md5 比对（见 cosy_test 部分）。
//
// 签名算法（参考实现 remote/client.go headers()）：
//
//	payload      = {"cosyVersion","ideVersion":"","info","requestId","version":"v1"}（key 排序、紧凑）
//	payloadB64   = base64(payload)
//	preimage     = payloadB64 \n cosy_key \n date \n body \n pathWithoutAlgo
//	signature    = md5hex(preimage)
//	Authorization = "Bearer COSY." + payloadB64 + "." + signature
//
// 其中 `info` 就是凭据里的 encrypt_user_info（IDE 登录时已经算好的身份密文，我们不解它）。
package lingma

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"
)

// cosySigner 是一次请求需要的签名素材。它由凭据派生（见 credentials.go 的 credOf）。
type cosySigner struct {
	CosyKey   string // 来自缓存解的 key
	Info      string // encrypt_user_info 原文
	MachineID string // 来自 cache/id
	UserID    string // 来自缓存解的 uid
}

// authorization 计算本次请求的 Authorization 头值。
//
// date 由调用方传入而不是在这里 time.Now()：同一个 date 既要进签名、又要进 `Cosy-Date` 头，
// 两处取到不同的秒会让上游判定签名过期（跨秒时偶发，最难查）。
func (s cosySigner) authorization(body, rawURL, date string) (string, error) {
	payload := map[string]string{
		"cosyVersion": cosyVersion,
		"ideVersion":  "",
		"info":        s.Info,
		"requestId":   uuid4(),
		"version":     "v1",
	}
	payloadB64 := base64.StdEncoding.EncodeToString(jsonSortedCompact(payload))

	path, err := signPath(rawURL)
	if err != nil {
		return "", err
	}
	preimage := payloadB64 + "\n" + s.CosyKey + "\n" + date + "\n" + body + "\n" + path
	sum := md5.Sum([]byte(preimage))
	return "Bearer COSY." + payloadB64 + "." + hex.EncodeToString(sum[:]), nil
}

// applyHeaders 把灵码形态的 COSY 头全部设置到 req。
//
// accept 由调用方按接口给（SSE 流给 text/event-stream，模型目录给 application/json）；
// body 必须与真正发送的字节**逐字一致**（含空格），否则签名对不上。
func (s cosySigner) applyHeaders(req *http.Request, body, rawURL, accept string) error {
	date := fmt.Sprintf("%d", time.Now().Unix())
	auth, err := s.authorization(body, rawURL, date)
	if err != nil {
		return err
	}
	h := req.Header
	h.Set("Authorization", auth)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", accept)
	h.Set("Cache-Control", "no-cache")
	h.Set("User-Agent", userAgent)
	// 这两条是灵码协议自带的固定头，值分别是 appcode / COSY 客户端标识。
	h.Set("Appcode", appcode)
	h.Set("Login-Version", loginVersion)
	// COSY 签名素材（date 与 Authorization 内的 date 必须同源）。
	h.Set("Cosy-Date", date)
	h.Set("Cosy-Key", s.CosyKey)
	h.Set("Cosy-Version", cosyVersion)
	h.Set("Cosy-User", s.UserID)
	h.Set("Cosy-Machineid", s.MachineID)
	h.Set("Cosy-Clientip", cosyClientIP)
	h.Set("Cosy-Clienttype", cosyClientType)
	h.Set("Cosy-Machineos", machineOSHeader())
	// 灵码不走设备授权，这两条在参考实现里是**空串**：显式设空（而不是省略），
	// 让抓包时能看清「我们确实带了这两个头、只是值为空」，排查时少一个疑点。
	h.Set("Cosy-Machinetoken", "")
	h.Set("Cosy-Machinetype", "")
	return nil
}

// signPath 取出参与签名的路径：URL 的 Path 部分，去掉 `/algo` 前缀。
func signPath(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("解析签名 URL 失败: %w", err)
	}
	return strings.TrimPrefix(u.Path, algoPrefix), nil
}

// jsonSortedCompact 按 key 排序、无空白序列化。
//
// 为什么不直接用 json.Marshal：Go 的 map 序列化**恰好**也是按 key 排序的，
// 但这是实现细节而非语言保证；签名对字节级差异敏感，这里显式排序 + 手写拼接，
// 与 qodercn 的 jsonSortedCompact 保持同一份实现（同一段算法，别写成两种样子）。
func jsonSortedCompact(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return []byte(sb.String())
}

// machineOSHeader 返回 `Cosy-Machineos`：形如 x86_64_linux / arm64_darwin。
// 这个值要与我们解缓存时用的机器标识**自洽**（都来自同一台真实机器才自然）；
// 换平台不影响签名，只影响上游看到的客户端画像 —— 所以如实报本机平台。
func machineOSHeader() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "386":
		arch = "x86_32"
	}
	return arch + "_" + runtime.GOOS
}

// uuid4 生成一个 UUIDv4 字符串（签名 payload 的 requestId、请求体里的会话 id 用它）。
func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// hexID 生成 32 字符的随机 hex（上游请求体里的 request_id/chat_record_id 就是这个形态，
// 与签名 payload 里的 UUID 形态 requestId 是两码事，别混用）。
func hexID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
