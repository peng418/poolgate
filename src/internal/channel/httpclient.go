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

// egressDefault 是「全局默认出口」的提供者（由装配层从设置注入）。
//
// 为什么要全局默认：像通义这种**换令牌被边缘按 IP 挡**的情况，登录这一步还没有账号凭证
// （凭证是登录的结果），所以不可能靠「账号级代理」解决 —— 必须有一个全局出口兜住它。
// 优先级：凭证绑定的 > 全局默认 > 环境变量。
var egressDefault func() string

// SetEgressDefault 设置全局默认出口的提供者（读设置）。传 nil 表示清空。
func SetEgressDefault(fn func() string) { egressDefault = fn }

// EgressOf 返回该凭证实际会用的出口（面板/诊断用）。
func EgressOf(c *Credential) string {
	if p := ProxyOf(c); p != "" {
		return p
	}
	if egressDefault != nil {
		return strings.TrimSpace(egressDefault())
	}
	return ""
}

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
	if raw := EgressOf(c); raw != "" {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			tr.Proxy = http.ProxyURL(u)
		}
		// 解析失败就保持环境变量回落：宁可用原出口（可能被封），
		// 也不要因为一个手滑的代理地址让整个渠道直接不可用 —— 但请去面板看告警。
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
