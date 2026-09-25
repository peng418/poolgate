package channel

// httpclient.go 按凭证建 HTTP 客户端 —— 「一账号一出口」的落点。
//
// 为什么必须按凭证建：网页渠道的封号线索里，「多个账号走同一个出口 IP」最常见也最致命
// （平台一眼就能把同 IP 的一串账号关联起来）。所以出口要绑到**账号**上，而不是让每个
// 适配器各自想办法。
//
// 用法：凭证 Extra 里带 "proxy" 就走它，没带则回落到环境变量（保持现有行为不变）。

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewHTTPClient 按凭证建一个 HTTP 客户端。
//
//   - 凭证 Extra["proxy"] 形如 http://user:pass@host:port 或 socks5://host:port；
//   - 没设 proxy 时沿用环境变量（HTTP_PROXY/HTTPS_PROXY），这样「整机走代理」的部署方式照旧；
//   - 超时按调用方给（网页渠道通常要给足：长思考 + 慢上游）。
func NewHTTPClient(c *Credential, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	if c != nil && c.Extra != nil {
		if raw := strings.TrimSpace(c.Extra["proxy"]); raw != "" {
			if u, err := url.Parse(raw); err == nil && u.Host != "" {
				tr.Proxy = http.ProxyURL(u)
			}
			// 解析失败就保持环境变量回落：宁可用原出口（可能被封），
			// 也不要因为一个手滑的代理地址让整个渠道直接不可用 —— 但请去面板看告警。
		}
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// ProxyOf 返回凭证绑定的出口（没绑返回空串）。面板/诊断用它显示「这个账号从哪出去」。
func ProxyOf(c *Credential) string {
	if c == nil || c.Extra == nil {
		return ""
	}
	return strings.TrimSpace(c.Extra["proxy"])
}

// EgressOf 返回该凭证实际会用的出口（面板/诊断用）：绑了代理给代理地址，否则给空（= 直连/环境变量）。
func EgressOf(c *Credential) string {
	if p := ProxyOf(c); p != "" {
		return p
	}
	return ""
}
