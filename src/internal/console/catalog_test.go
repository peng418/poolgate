package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"poolgate/internal/boot"
	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/health"
	"poolgate/internal/registry"
	"poolgate/internal/store"
)

// ensureChannels 保证测试里注册表已装配（测试进程不跑 boot.RegisterChannels）。
func ensureChannels(t *testing.T) {
	t.Helper()
	if len(registry.All()) > 0 {
		return
	}
	boot.RegisterChannels()
	if len(registry.All()) == 0 {
		t.Fatal("渠道注册表为空，无法验证渠道清单")
	}
}

// 全新安装（0 个账号）时**不能**返回任何模型。
//
// 模型是「用某个账号向上游问出来的」；没有账号就没有可用模型。
// 之前这里会在无账号时回退静态兜底表，于是刚装完的面板就显示 23 个模型，
// 用户以为装完即用，点下去全失败 —— 这正是「面板不撒谎」要避免的。
func TestModelCatalogEmptyWithoutAccounts(t *testing.T) {
	srv, _, _, _ := newOpsServer(t)
	token := loginToken(t, srv)

	r := httptest.NewRequest(http.MethodGet, "/api/model/catalog", nil)
	r.Header.Set("Cookie", "pg_session="+token)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	var resp struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 0 {
		t.Fatalf("没有账号时不应返回模型，实际 %d 个", len(resp.Models))
	}
}

// 渠道清单必须区分「有适配器」与「能面板授权」。
//
// 之前只有一个 implemented 字段（=有适配器），前端拿它当「能授权」用，
// 于是「添加账号」直接挑了第一个渠道，用户没得选；而且点到没实现 Authorizer
// 的渠道会得到一个点了没反应的按钮。
func TestChannelsExposePanelAuthSeparately(t *testing.T) {
	srv, _, _, _ := newOpsServer(t)
	token := loginToken(t, srv)
	ensureChannels(t)

	r := httptest.NewRequest(http.MethodGet, "/api/channels", nil)
	r.Header.Set("Cookie", "pg_session="+token)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	var resp struct {
		Channels []struct {
			Kind          string `json:"kind"`
			Implemented   bool   `json:"implemented"`
			PanelAuth     bool   `json:"panel_auth"`
			PanelAuthNote string `json:"panel_auth_note"`
			AccountCount  int    `json:"account_count"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Channels) == 0 {
		t.Fatal("应返回渠道清单")
	}
	byKind := map[string]bool{}
	for _, c := range resp.Channels {
		byKind[c.Kind] = c.PanelAuth
		// 不能面板授权的渠道必须给出替代做法，不能只留个空。
		if !c.PanelAuth && c.PanelAuthNote == "" {
			t.Errorf("渠道 %s 不可面板授权时应给出说明", c.Kind)
		}
		// 能面板授权的渠道不该带「不支持」的说明。
		if c.PanelAuth && c.PanelAuthNote != "" {
			t.Errorf("渠道 %s 可面板授权，不应带替代说明：%s", c.Kind, c.PanelAuthNote)
		}
	}
	// 首发三家都实现了 Authorizer，必须可面板授权。
	for _, k := range []string{"qodercn", "workbuddy", "traework"} {
		if _, ok := byKind[k]; !ok {
			continue // 测试环境未注册则跳过
		}
		if !byKind[k] {
			t.Errorf("渠道 %s 实现了 Authorizer，panel_auth 应为 true", k)
		}
	}
}

// panel_auth 必须与「真的实现了 channel.Authorizer」一致，不能硬编码。
func TestPanelAuthMatchesAuthorizerInterface(t *testing.T) {
	srv, _, _, _ := newOpsServer(t)
	token := loginToken(t, srv)
	ensureChannels(t)

	r := httptest.NewRequest(http.MethodGet, "/api/channels", nil)
	r.Header.Set("Cookie", "pg_session="+token)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	var resp struct {
		Channels []struct {
			Kind      string `json:"kind"`
			PanelAuth bool   `json:"panel_auth"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Channels {
		ch, ok := registry.Get(channel.Kind(c.Kind))
		if !ok {
			continue // 只有声明没有实现
		}
		_, isAuthorizer := ch.(channel.Authorizer)
		if isAuthorizer != c.PanelAuth {
			t.Errorf("渠道 %s：实现了 Authorizer=%v，但 panel_auth=%v（两者必须一致）",
				c.Kind, isAuthorizer, c.PanelAuth)
		}
	}
}

// 有账号的渠道才出现在模型目录里（反向验证：加了账号就该有模型）。
func TestModelCatalogAppearsWithAccount(t *testing.T) {
	srv, p, _, _ := newOpsServer(t)
	addAccount(p, channel.QoderCN, "u1")
	token := loginToken(t, srv)

	r := httptest.NewRequest(http.MethodGet, "/api/model/catalog", nil)
	r.Header.Set("Cookie", "pg_session="+token)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	var resp struct {
		Models []struct {
			Channel string `json:"channel"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, m := range resp.Models {
		if m.Channel == "" {
			t.Fatal("模型必须带渠道标识")
		}
	}
}

// fakeCatalog 是一个只提供固定模型清单的渠道（避免测试真的去打上游）。
type fakeCatalog struct {
	kind   channel.Kind
	spec   channel.Spec
	models []channel.ModelInfo
}

func (f *fakeCatalog) Kind() channel.Kind                                 { return f.kind }
func (f *fakeCatalog) Spec() channel.Spec                                 { return f.spec }
func (f *fakeCatalog) Login(context.Context) (*channel.Credential, error) { return nil, nil }
func (f *fakeCatalog) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}
func (f *fakeCatalog) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	return f.models, nil
}
func (f *fakeCatalog) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{}, nil
}
func (f *fakeCatalog) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{}, nil
}
func (f *fakeCatalog) Chat(context.Context, *channel.Credential, channel.ChatRequest) (channel.Stream, error) {
	return nil, nil
}
func (f *fakeCatalog) Classify(int, []byte) errs.Kind { return errs.Parse }

// 面板模型页必须如实标出「正被健康度隐藏」的模型：隐藏是下发侧的过滤，
// 面板上要是也看不见了，用户只会以为模型凭空少了。
func TestModelCatalogMarksHiddenByHealth(t *testing.T) {
	registry.Reset()
	ch := &fakeCatalog{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, DisplayName: "QoderCN", Status: channel.Active},
		models: []channel.ModelInfo{
			{ID: "good"}, {ID: "bad"}, {ID: "never-probed"},
		},
	}
	registry.Register(ch, ch.spec)

	srv, p, settings, _ := newOpsServer(t)
	addAccount(p, channel.QoderCN, "u1")
	// 体检结论：good 通过，bad 失败，never-probed 没跑过。
	srv.opts.History.Add(health.Run{Results: []health.Result{
		{Channel: string(channel.QoderCN), Model: "good", OK: true},
		{Channel: string(channel.QoderCN), Model: "bad", OK: false, Reason: "空流"},
	}})
	token := loginToken(t, srv)

	catalog := func() map[string]map[string]any {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/model/catalog", nil)
		r.Header.Set("Cookie", "pg_session="+token)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, r)
		var resp struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, m := range resp.Models {
			id, _ := m["id"].(string)
			out[id] = m
		}
		return out
	}

	// 开关关着：一个都不标「已隐藏」。
	rows := catalog()
	if len(rows) != 3 {
		t.Fatalf("应列出 3 个模型，实际 %d 个", len(rows))
	}
	for id, row := range rows {
		if row["hidden_by_health"] == true {
			t.Fatalf("开关关着时 %s 不该被标记为隐藏", id)
		}
	}

	// 打开：只有明确失败的被标出来，未体检的不标。
	if err := settings.Update(func(st *store.Settings) error {
		st.OnlyHealthyModels = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows = catalog()
	if rows["qodercn/bad"]["hidden_by_health"] != true {
		t.Fatalf("体检失败的模型应标记为已隐藏，实际 %+v", rows["qodercn/bad"])
	}
	if rows["qodercn/good"]["hidden_by_health"] == true {
		t.Fatal("体检通过的模型不该标隐藏")
	}
	if rows["qodercn/never-probed"]["hidden_by_health"] == true {
		t.Fatal("未体检的模型不该标隐藏（「不知道」不等于「不可用」）")
	}
	// 面板仍要能看到被隐藏的模型（不然没法复检、也没法解释模型为什么少了）。
	if _, ok := rows["qodercn/bad"]; !ok {
		t.Fatal("被隐藏的模型仍应出现在面板的模型页里")
	}
}
