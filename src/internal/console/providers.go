package console

// providers.go 面板「接入源」页的后端：API Key 式来源的增删改查 + 连通性测试。
//
// 与「账号」接口刻意分开（前端把两类并在一张表，但语义完全不同）：
//   - 账号：登录授权换来的会话 —— 有余额、有签到、能「重新登录」；
//   - 来源：用户填的一串 key —— 属于**配置**，改它就是改配置，没有授权流程。
//
// 控制台不直接构造适配器：挂载/摘除/探测都由装配层实现 ProviderAdmin。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"poolgate/internal/boot"
	"poolgate/internal/channel"
	"poolgate/internal/registry"
	"poolgate/internal/store"
)

// ProviderAdmin 由装配层提供（见 boot/providers.go）。
//
// Put 要做两件事：落盘 + 挂载（热更新，不重启）；Remove 同理，落盘 + 摘除。
type ProviderAdmin interface {
	List() []store.ProviderConfig
	Put(cfg store.ProviderConfig) error
	Remove(name string) error
	Test(ctx context.Context, cfg store.ProviderConfig) boot.ProbeResult
}

// providerOut 是返回给前端的来源视图（**API Key 永远只回掩码**）。
type providerOut struct {
	Name          string   `json:"name"`
	DisplayName   string   `json:"display_name"`
	BaseURL       string   `json:"base_url"`
	APIKeyMasked  string   `json:"api_key_masked"`
	Models        []string `json:"models,omitempty"`
	ModelCount    int      `json:"model_count"`
	SupportsTools bool     `json:"supports_tools"`
	Notes         string   `json:"notes,omitempty"`
	Preset        string   `json:"preset,omitempty"`
	AddedAt       string   `json:"added_at,omitempty"`
	Status        string   `json:"status"`        // active | paused（来自注册表，渠道开关可改）
	AccountCount  int      `json:"account_count"` // 合成账号数（正常是 1）
}

// maskKey 把 key 变成可辨识但不可用的形式：前 6 后 4，中间省略。
// 短 key 直接全掩，避免「掩了等于没掩」。
func maskKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if len(k) <= 12 {
		return strings.Repeat("•", len(k))
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// handleProviders 列出全部 API Key 式来源（含预设，供「添加」向导直接用）。
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "仅支持 GET"})
		return
	}
	out := []providerOut{}
	if s.opts.Providers != nil {
		for _, c := range s.opts.Providers.List() {
			status := "active"
			if e, ok := registry.GetSpec(channel.Kind(c.Name)); ok {
				status = string(e.Status)
			}
			n := 0
			if s.opts.Pool != nil {
				n = len(s.opts.Pool.List(channel.Kind(c.Name)))
			}
			out = append(out, providerOut{
				Name: c.Name, DisplayName: c.DisplayName, BaseURL: c.BaseURL,
				APIKeyMasked: maskKey(c.APIKey), Models: c.Models, ModelCount: len(c.Models),
				SupportsTools: c.SupportsTools, Notes: c.Notes, Preset: c.Preset, AddedAt: c.AddedAt,
				Status: status, AccountCount: n,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": out,
		"presets":   boot.ProviderPresets(),
	})
}

// handleProviderSave 新增或更新一个来源（同名覆盖 = 改配置）。
func (s *Server) handleProviderSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "仅支持 POST"})
		return
	}
	if s.opts.Providers == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "本实例未启用接入源管理"})
		return
	}
	var cfg store.ProviderConfig
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(raw, &cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	// 更新已有来源时前端只会回掩码（明文不下发），此时沿用磁盘上的旧 key。
	keyMissing := strings.TrimSpace(cfg.APIKey) == "" ||
		strings.Contains(cfg.APIKey, "•") || strings.Contains(cfg.APIKey, "…")
	if keyMissing {
		name := strings.ToLower(strings.TrimSpace(cfg.Name))
		for _, o := range s.opts.Providers.List() {
			if o.Name == name && strings.TrimSpace(o.APIKey) != "" {
				cfg.APIKey = o.APIKey
				break
			}
		}
	}
	if err := s.opts.Providers.Put(cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": strings.ToLower(cfg.Name)})
}

// handleProviderDelete 删除一个来源（落盘 + 从注册表与池子里摘掉）。
func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "仅支持 POST"})
		return
	}
	if s.opts.Providers == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "本实例未启用接入源管理"})
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	json.Unmarshal(raw, &in)
	if strings.TrimSpace(in.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 name"})
		return
	}
	if err := s.opts.Providers.Remove(in.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleProviderTest 对一个**还没保存**的配置做连通性测试。
//
// 四项分开报：鉴权 / 模型目录 / 工具调用 / 首字延迟。「工具调用」那项决定这个来源
// 能不能给 coding agent 当后端 —— 这是添加来源时最该先知道的事。
func (s *Server) handleProviderTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "仅支持 POST"})
		return
	}
	if s.opts.Providers == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "本实例未启用接入源管理"})
		return
	}
	var cfg store.ProviderConfig
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(raw, &cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	// 测试用的配置可能还没填名字（向导第 2 步），给个临时名，别让校验挡住探测。
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = "probe"
	}
	// 对「已保存的来源」点测一遍时，前端只能传出掩码（明文不下发）：
	// 这时按名字回填磁盘上的 key，否则每次测都报 401，像是 key 坏了。
	if cfg.APIKey == "" || strings.Contains(cfg.APIKey, "•") || strings.Contains(cfg.APIKey, "…") {
		name := strings.ToLower(strings.TrimSpace(cfg.Name))
		for _, o := range s.opts.Providers.List() {
			if o.Name == name && strings.TrimSpace(o.APIKey) != "" {
				cfg.APIKey = o.APIKey
				break
			}
		}
	}
	res := s.opts.Providers.Test(r.Context(), cfg)
	writeJSON(w, http.StatusOK, res)
}
