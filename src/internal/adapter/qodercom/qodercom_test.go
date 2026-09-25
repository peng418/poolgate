package qodercom

import (
	"context"
	"strings"
	"testing"

	"poolgate/internal/channel"
)

// QoderCOM 与 QoderCN 同一套 COSY 框架，但域名与渠道标识必须分开 ——
// 混在一起会让两个渠道的凭证与路由互相串（同一账号体系的两个区）。
func TestSpecIsDistinctFromQoderCN(t *testing.T) {
	sp := Spec()
	if sp.Kind != channel.QoderCOM {
		t.Fatalf("Kind 应为 qodercom，实际 %v", sp.Kind)
	}
	if sp.Kind == channel.QoderCN {
		t.Fatal("不能与 QoderCN 同 Kind")
	}
	if sp.DisplayName != "QoderCOM" {
		t.Fatalf("DisplayName 应为 QoderCOM，实际 %q", sp.DisplayName)
	}
}

// COM 无 legacy daily-check-in（实测 404）→ CheckinCap 必须为 false，
// 否则面板会给出一个点了没反应的「签到」按钮。
func TestNoCheckinCapability(t *testing.T) {
	if Spec().CheckinCap {
		t.Fatal("QoderCOM 无签到活动，CheckinCap 应为 false")
	}
}

// 域名表必须是 COM 的，不能残留 CN 的（复制改域名最容易漏的地方）。
func TestDomainsAreCOM(t *testing.T) {
	cases := map[string]string{
		"OpenAPIBase": OpenAPIBase,
		"GatewayBase": GatewayBase,
		"ModelsBase":  ModelsBase,
	}
	for name, v := range cases {
		if v == "" {
			t.Fatalf("%s 不能为空", name)
		}
		if contains(v, "qoder.com.cn") {
			t.Fatalf("%s 残留了 CN 域名：%s", name, v)
		}
		if !contains(v, "qoder.sh") {
			t.Fatalf("%s 应为 COM 域名（qoder.sh），实际 %s", name, v)
		}
	}
}

// 错误分类要与 QoderCN 一致地把额度/会话类分开。
func TestClassify(t *testing.T) {
	a := New()
	if got := a.Classify(401, []byte("unauthorized")); got == "" {
		t.Fatal("401 应给出分类")
	}
	if got := a.Classify(503, []byte("gateway")); got == "" {
		t.Fatal("5xx 应给出分类")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// 适配器必须实现 channel.Authorizer（面板加号要用）。
func TestImplementsAuthorizer(t *testing.T) {
	var _ channel.Authorizer = New()
}

// 授权页必须指向 COM 域名，不能残留 CN 的。
func TestAuthorizeURLIsCOM(t *testing.T) {
	s := New()
	// StartLogin 不碰上游，只生成 PKCE 与地址。
	sess, err := s.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("发起授权失败：%v", err)
	}
	defer sess.Cancel()
	url := sess.AuthURL()
	if !strings.Contains(url, "qoder.com/device/selectAccounts") {
		t.Fatalf("授权地址应指向 COM，实际 %s", url)
	}
	if strings.Contains(url, "qoder.com.cn") {
		t.Fatalf("授权地址残留 CN 域名：%s", url)
	}
	// 设备流用 challenge=（不是 OAuth 标准的 code_challenge=），与上游实测一致。
	if !strings.Contains(url, "challenge=") || !strings.Contains(url, "challenge_method=S256") {
		t.Fatalf("必须带 PKCE challenge，实际 %s", url)
	}
}
