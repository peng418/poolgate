package channel

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

// 凭证里绑了代理就必须走它 —— 「一账号一出口」是网页渠道防封号的第一道护栏。
func TestNewHTTPClientUsesCredentialProxy(t *testing.T) {
	c := &Credential{UID: "u1", Extra: map[string]string{"proxy": "http://user:pw@127.0.0.1:7890"}}
	cl := NewHTTPClient(c, 5*time.Second)
	tr, ok := cl.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("transport/proxy 没建起来")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	u, err := tr.Proxy(req)
	if err != nil || u == nil {
		t.Fatalf("代理没生效: %v %v", u, err)
	}
	if u.Host != "127.0.0.1:7890" || u.User.Username() != "user" {
		t.Fatalf("代理地址不对: %v", u)
	}
	if ProxyOf(c) == "" {
		t.Fatal("ProxyOf 应返回绑定的出口（面板要显示它）")
	}
}

// 没绑代理时沿用环境变量（保持「整机走代理」的部署方式不变）。
func TestNewHTTPClientWithoutProxyFallsBackToEnv(t *testing.T) {
	c := &Credential{UID: "u1"}
	cl := NewHTTPClient(c, 5*time.Second)
	tr := cl.Transport.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatal("没绑代理时应当回落环境变量，而不是完全不走代理")
	}
	if ProxyOf(c) != "" {
		t.Fatal("没绑代理时 ProxyOf 应为空")
	}
	// 手滑写错的代理地址：不该让整个渠道直接崩，回落环境变量（但这是配置错误，面板要看出来）
	bad := &Credential{UID: "u2", Extra: map[string]string{"proxy": "://nope"}}
	tr2 := NewHTTPClient(bad, time.Second).Transport.(*http.Transport)
	if tr2.Proxy == nil {
		t.Fatal("非法代理地址应回落而不是置空")
	}
	_ = url.URL{}
}
