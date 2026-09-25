package workbuddyai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
)

// 国际版与国内版是两家独立渠道：Kind、域名、显示名都不能串。
func TestSpecIsDistinctFromCN(t *testing.T) {
	sp := Spec()
	if sp.Kind != channel.WorkBuddyAI {
		t.Fatalf("Kind 应为 workbuddyai，实际 %v", sp.Kind)
	}
	if sp.Kind == channel.WorkBuddyCN {
		t.Fatal("不能与国内版同 Kind（两家独立渠道）")
	}
	if sp.DisplayName != "WorkBuddyAI" {
		t.Fatalf("DisplayName 应为 WorkBuddyAI，实际 %q", sp.DisplayName)
	}
}

// 国际版 daily-checkin 实测返回 code=10001（未开启）→ 不应声明签到能力。
func TestNoCheckinCapability(t *testing.T) {
	if Spec().CheckinCap {
		t.Fatal("国际版签到未开启，CheckinCap 应为 false")
	}
}

// 域名必须是国际版，不能残留国内版的（复制改域名最容易漏）。
func TestDomainsAreInternational(t *testing.T) {
	for name, v := range map[string]string{"BaseAI": BaseAI, "OriginAI": OriginAI} {
		if v == "" {
			t.Fatalf("%s 不能为空", name)
		}
		if v != "https://www.workbuddy.ai" {
			t.Fatalf("%s 应为国际版域名，实际 %s", name, v)
		}
	}
	if DomainAI != "www.workbuddy.ai" {
		t.Fatalf("DomainAI 不对：%s", DomainAI)
	}
}

// 目录解析：国际版返回 {data:{agents:[{models:[...]}]}}。
func TestExtractCatalogModels(t *testing.T) {
	raw := []byte(`{"agents":[{"name":"cli","models":["default-model","fast-model","default-model"]}]}`)
	ids := extractCatalogModels(raw)
	if len(ids) != 2 {
		t.Fatalf("应去重后得 2 个，实际 %v", ids)
	}
	if ids[0] != "default-model" || ids[1] != "fast-model" {
		t.Fatalf("应保序，实际 %v", ids)
	}
}

// 目录形态可能是 {data:{models:[{id}]}}，两种都要能解析。
func TestExtractCatalogModelsAlternateShape(t *testing.T) {
	raw := []byte(`{"models":[{"id":"a"},{"id":"b"}]}`)
	ids := extractCatalogModels(raw)
	if len(ids) != 2 {
		t.Fatalf("应解析出 2 个，实际 %v", ids)
	}
}

// 坏响应不应 panic，返回空即可（调用方会回退静态表）。
func TestExtractCatalogModelsGarbage(t *testing.T) {
	if ids := extractCatalogModels([]byte(`not json`)); ids != nil {
		t.Fatalf("坏响应应返回 nil，实际 %v", ids)
	}
}

// Models 拉取失败时回退静态表，且回退项来源标 local（不冒充上游值）。
func TestModelsFallsBackToStatic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := channel.Credential{UID: "u1", AccessToken: "t"}
	models, err := a.Models(context.Background(), &cred)
	if err != nil {
		t.Fatalf("目录失败应回退而非报错：%v", err)
	}
	if len(models) == 0 {
		t.Fatal("应回退静态表")
	}
	for _, m := range models {
		if m.Source != channel.SourceLocal {
			t.Fatalf("静态表来源应为 local，实际 %v", m.Source)
		}
	}
}

// 动态目录成功时，窗口未知要标 unknown（上游不回该字段，不猜数字）。
func TestModelsDynamicMarksUnknownContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "OK",
			"data": map[string]any{"agents": []any{
				map[string]any{"models": []string{"default-model"}},
			}},
		})
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	cred := channel.Credential{UID: "u1", AccessToken: "t"}
	models, err := a.Models(context.Background(), &cred)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("应有 1 个模型，实际 %d", len(models))
	}
	if models[0].ContextWindow != 0 || models[0].Source != channel.SourceUnknown {
		t.Fatalf("上游不回窗口时应标 unknown，实际 window=%d source=%v",
			models[0].ContextWindow, models[0].Source)
	}
}

// ModelsSnapshot 优先返回缓存（面板刷新目录后应能立刻反映）。
func TestModelsSnapshotPrefersCache(t *testing.T) {
	a := New()
	a.cache = []channel.ModelInfo{{ID: "cached"}}
	snap := a.ModelsSnapshot()
	if len(snap) != 1 || snap[0].ID != "cached" {
		t.Fatalf("应返回缓存，实际 %+v", snap)
	}
}

// 适配器必须实现 channel.Authorizer，否则面板上「添加账号」对它没有授权按钮。
func TestImplementsAuthorizer(t *testing.T) {
	var _ channel.Authorizer = New()
}

// StartLogin 必须返回可直接打开的授权页地址（上游给的是 authUrl）。
func TestStartLoginReturnsAuthURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "OK",
			"data": map[string]any{"state": "st-1", "authUrl": "https://www.workbuddy.ai/login?platform=CLI&state=st-1"},
		})
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	s, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err != nil {
		t.Fatalf("发起授权失败：%v", err)
	}
	defer s.Cancel()
	if !strings.Contains(s.AuthURL(), "workbuddy.ai") {
		t.Fatalf("授权地址不对：%s", s.AuthURL())
	}
}

// 上游不给 state/authUrl 时必须明确报错并带上游原话，不能给个空地址让用户点。
func TestStartLoginFailsWithoutAuthURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
	}))
	defer srv.Close()

	a := New()
	a.base = srv.URL
	_, err := a.StartLogin(context.Background(), channel.LoginOptions{})
	if err == nil {
		t.Fatal("缺少 authUrl 应报错")
	}
}
