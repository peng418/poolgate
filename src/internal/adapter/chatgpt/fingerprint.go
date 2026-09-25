package chatgpt

// fingerprint.go 设备指纹的**持久化**与请求头组装。
//
// 为什么这件事单独成一篇：网页渠道被封的头号线索不是「请求太多」，而是
// 「同一个账号的设备号一直在变」或「一堆账号共用同一个设备号」。
// 前者看起来像「同一个账号被很多台机器轮流用」（盗号/共享特征），
// 后者一眼就是把号聚在一起的同一台机器。
//
// 所以这里的规则是：
//   - 指纹存 **Credential.Extra**，随账号落盘，是账号身份的一部分；
//   - 缺失时**从账号 UID 派生**（同号恒定、异号不同），绝不使用随机值；
//   - 登录时就把派生结果写进 Extra（ensureFingerprint），让面板保存下去。
//
// 已知边界（真上游复核项）：本适配器只发 `OAI-Device-Id` 等**头**，不发对应的
// `oai-did` cookie。上游若校验「cookie 与头必须一致」，表现会是 sentinel 403 ——
// 那时要在这里补一个按账号存 cookie 的口子，而不是改指纹派生逻辑。

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"time"

	"poolgate/internal/channel"
)

// bootstrapMaxAttempts 是首页引导的重试次数（上游抖的时候重试，凭证问题不重试）。
const bootstrapMaxAttempts = 3

// defaultTimezone / 默认时区偏移（分钟）。参考实现固定写成上海（-480），
// 与浏览器时区串一起被上游当作环境一致性信号；改它要连时区串一起改。
const (
	defaultTimezone          = "Asia/Shanghai"
	defaultTimezoneOffsetMin = -480
)

// fingerprint 是一个账号在浏览器上的「长相」。
type fingerprint struct {
	DeviceID      string // oai-did
	SessionID     string
	Language      string
	ClientVersion string
	ClientBuild   string
	UserAgent     string
	SecCHUA       string
	SecCHUAMobile string
	SecCHUAPlat   string
}

// fingerprintOf 取账号指纹：Extra 里有就用存的，没有就**派生**（不随机）。
//
// 派生而不是随机，是为了「即使 Extra 因为历史原因缺字段，同一账号每次也拿到同一个值」：
// 随机值导致的失败是间歇性的，最难查。
func fingerprintOf(c *channel.Credential) fingerprint {
	seed := seedOf(c)
	get := func(key, def string) string {
		if c != nil && c.Extra != nil {
			if v := strings.TrimSpace(c.Extra[key]); v != "" {
				return v
			}
		}
		return def
	}
	return fingerprint{
		DeviceID:      get(fpDeviceID, deriveUUID(seed, "chatgpt-oai-did")),
		SessionID:     get(fpSessionID, deriveUUID(seed, "chatgpt-oai-session")),
		Language:      get(fpLanguage, defaultLanguage),
		ClientVersion: get(fpClientVersion, defaultClientVersion),
		ClientBuild:   get(fpClientBuild, defaultClientBuild),
		UserAgent:     get(fpUserAgent, defaultUserAgent),
		SecCHUA:       get(fpSecCHUA, defaultSecCHUA),
		SecCHUAMobile: get(fpSecCHUAMobile, defaultSecCHUAMobile),
		SecCHUAPlat:   get(fpSecCHUAPlat, defaultSecCHUAPlat),
	}
}

// ensureFingerprint 把派生出来的指纹写进凭证（幂等）。
//
// 只在「没有值」时写，已有值绝不覆盖 —— 覆盖等于悄悄换了设备，
// 那正是本文件开头说的头号封号线索。登录时调用，让面板把指纹随账号一起存下来。
func ensureFingerprint(c *channel.Credential) {
	if c == nil {
		return
	}
	if c.Extra == nil {
		c.Extra = map[string]string{}
	}
	fp := fingerprintOf(c)
	for k, v := range map[string]string{
		fpDeviceID:      fp.DeviceID,
		fpSessionID:     fp.SessionID,
		fpLanguage:      fp.Language,
		fpClientVersion: fp.ClientVersion,
		fpClientBuild:   fp.ClientBuild,
		fpUserAgent:     fp.UserAgent,
		fpSecCHUA:       fp.SecCHUA,
		fpSecCHUAMobile: fp.SecCHUAMobile,
		fpSecCHUAPlat:   fp.SecCHUAPlat,
	} {
		if strings.TrimSpace(c.Extra[k]) == "" {
			c.Extra[k] = v
		}
	}
}

// seedOf 取派生种子：优先账号 UID。
//
// 不用 access token 当种子：token 会轮换，用它派生会让设备号跟着换 ——
// 在风控看来就是「同一账号换了设备」。只有登录途中还没有 UID 时才退回用 token。
func seedOf(c *channel.Credential) string {
	if c == nil {
		return "chatgpt-anonymous"
	}
	if strings.TrimSpace(c.UID) != "" {
		return c.UID
	}
	if strings.TrimSpace(c.AccessToken) != "" {
		return c.AccessToken
	}
	return "chatgpt-anonymous"
}

// deriveUUID 由种子派生一个**稳定**的 RFC4122 v4 UUID 形状串。
//
// 形状必须是 v4（第 7 字节的高 4 位是 4、第 9 字节高 2 位是 10）：上游会校验格式，
// 随便一串 hex 会被判非法。
func deriveUUID(seed, salt string) string {
	h1 := fnv.New64a()
	_, _ = h1.Write([]byte(seed))
	_, _ = h1.Write([]byte("#" + salt))
	h2 := fnv.New64a()
	_, _ = h2.Write([]byte(seed + "/" + salt + "#second"))
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], h1.Sum64())
	binary.BigEndian.PutUint64(b[8:16], h2.Sum64())
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ---------------------------------------------------------------------------
// 请求头
// ---------------------------------------------------------------------------

// apiHeaders 是 XHR 类请求（sentinel / 对话 / 目录）的头。
//
// sec-ch-ua* 一套必须自洽：Sec-Ch-Ua 里的版本号、Full-Version 与 UA 里的版本号
// 不一致，比不带这些头更显眼。默认值都对齐 Edge/143；如果给账号换了 UA，
// 请连这几项一起在 Extra 里覆盖。
func apiHeaders(path string, fp fingerprint) http.Header {
	h := http.Header{}
	h.Set("User-Agent", fp.UserAgent)
	h.Set("Origin", apiBase)
	h.Set("Referer", apiBase+"/")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("Priority", "u=1, i")
	h.Set("Sec-Ch-Ua", fp.SecCHUA)
	h.Set("Sec-Ch-Ua-Arch", `"x86"`)
	h.Set("Sec-Ch-Ua-Bitness", `"64"`)
	h.Set("Sec-Ch-Ua-Full-Version", `"143.0.3650.96"`)
	h.Set("Sec-Ch-Ua-Full-Version-List",
		`"Microsoft Edge";v="143.0.3650.96", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"`)
	h.Set("Sec-Ch-Ua-Mobile", fp.SecCHUAMobile)
	h.Set("Sec-Ch-Ua-Model", `""`)
	h.Set("Sec-Ch-Ua-Platform", fp.SecCHUAPlat)
	h.Set("Sec-Ch-Ua-Platform-Version", `"19.0.0"`)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("OAI-Device-Id", fp.DeviceID)
	h.Set("OAI-Session-Id", fp.SessionID)
	h.Set("OAI-Language", fp.Language)
	h.Set("OAI-Client-Version", fp.ClientVersion)
	h.Set("OAI-Client-Build-Number", fp.ClientBuild)
	// 这两个头是上游用来做路由校验的：值必须等于真正的 API 路径（不含 query），
	// 写错或漏掉会拿到 403 / 静默降级。
	h.Set("X-OpenAI-Target-Path", path)
	h.Set("X-OpenAI-Target-Route", path)
	return h
}

// documentHeaders 是导航类请求（首页引导）的头：Accept 要 html、Sec-Fetch-* 要 navigate。
//
// 引导请求用 XHR 那一套头是明显的破绽 —— 浏览器打开首页绝不会带
// Sec-Fetch-Dest: empty。
func documentHeaders(fp fingerprint) map[string]string {
	return map[string]string{
		"User-Agent":                fp.UserAgent,
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"Accept-Language":           "zh-CN,zh;q=0.9,en;q=0.8",
		"Sec-Ch-Ua":                 fp.SecCHUA,
		"Sec-Ch-Ua-Mobile":          fp.SecCHUAMobile,
		"Sec-Ch-Ua-Platform":        fp.SecCHUAPlat,
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "none",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
	}
}

// ---------------------------------------------------------------------------
// 退避
// ---------------------------------------------------------------------------

// backoffOf 是引导重试的退避：3*n 起步（与 notes 里那条风控经验一致：被标记的
// 客户端要慢一点、稳一点，猛冲只会加深标记）。
func backoffOf(attempt int) time.Duration {
	return time.Duration(attempt) * 3 * time.Second
}

// sleepCtx 睡一会儿，被取消就返回 false（不阻塞退出）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
