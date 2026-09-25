// models.go 动态模型获取：COSY 签名 GET /algo/api/v2/model/list?Encode=1。
// 移植自 wild-work qodercn/models.go：无静态兜底表，缓存策略为「上次成功拉取」。
package qodercn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"poolgate/internal/channel"
)

// contextConfig 形状：{"<label>": {"is_default":bool,"token_count":int}}
type contextConfig map[string]struct {
	IsDefault  bool  `json:"is_default"`
	TokenCount int64 `json:"token_count"`
}

// parseDynamicModels 解析模型列表响应：assistant→developer→chat 三级回退。
func parseDynamicModels(apiResp map[string]json.RawMessage) ([]ModelEntry, error) {
	for _, scene := range []string{"assistant", "developer", "chat"} {
		raw, ok := apiResp[scene]
		if !ok {
			continue
		}
		var models []ModelEntry
		if err := json.Unmarshal(raw, &models); err != nil {
			continue
		}
		enabled := make([]ModelEntry, 0, len(models))
		for _, m := range models {
			if m.Enable && m.Key != "" {
				enabled = append(enabled, m)
			}
		}
		if len(enabled) > 0 {
			return enabled, nil
		}
	}
	return nil, fmt.Errorf("no enabled models in any scene")
}

// fetchModels 调上游动态模型接口。
// GET 无 body，签名用空串 ""（非 "{}"，后者 403 Signature invalid）。
func (a *Adapter) fetchModels(ctx context.Context, c *channel.Credential) ([]ModelEntry, error) {
	rawURL := a.gateway + EpModels
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	ut := a.userTypeOf(c)
	sess, err := newCosySession(a.fingerprint(c), c.Nickname, c.UID, c.AccessToken, c.RefreshToken, ut)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, "", rawURL, c.UID, "application/json", false, ""); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var apiResp map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	enabled, err := parseDynamicModels(apiResp)
	if err != nil {
		return nil, err
	}
	a.setCache(enabled) // 上次成功即缓存（无静态兜底）
	return enabled, nil
}

// cachedModels 返回上次成功拉取的模型表快照；无缓存返回 nil。
func (a *Adapter) cachedModels() []ModelEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.cache) == 0 {
		return nil
	}
	out := make([]ModelEntry, len(a.cache))
	copy(out, a.cache)
	return out
}

func (a *Adapter) setCache(m []ModelEntry) {
	mm := make(map[string]string, len(m))
	for _, e := range m {
		name := NormalizeModelName(e.DisplayName)
		if name == "" {
			name = e.Key
		}
		mm[name] = e.Key
	}
	a.mu.Lock()
	a.modelMap = mm
	a.cache = m
	a.mu.Unlock()
}

// models 实现 channel.Channel.Models：动态模型 → channel.ModelInfo，
// 失败回退上次成功缓存；连缓存都无 → 返回错误（Fail Early）。
func (a *Adapter) models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	dyn, err := a.fetchModels(ctx, c)
	if err != nil {
		if cached := a.cachedModels(); len(cached) > 0 {
			return toModelInfos(cached), nil
		}
		return nil, err
	}
	return toModelInfos(dyn), nil
}

// ModelsSnapshot 返回上次成功拉取的模型目录快照（供 /v1/models 用），
// 无缓存时返回空 —— 首次使用需先有一次成功拉取（boot 预热或面板触发）。
func (a *Adapter) ModelsSnapshot() []channel.ModelInfo {
	return toModelInfos(a.cachedModels())
}

// toModelInfos 动态表 → channel.ModelInfo。
func toModelInfos(dyn []ModelEntry) []channel.ModelInfo {
	out := make([]channel.ModelInfo, 0, len(dyn))
	for _, m := range dyn {
		name := NormalizeModelName(m.DisplayName)
		if name == "" {
			name = m.Key
		}
		mi := channel.ModelInfo{
			ID:            name,
			DisplayName:   m.DisplayName,
			ContextWindow: 180000,
			Tools:         channel.CapUnknown,
			Images:        capOf(m.IsVL),
			Reasoning:     capOf(m.IsReasoning),
			Source:        channel.SourceUpstream,
		}
		if m.ContextWindow > 0 {
			mi.ContextWindow = int(m.ContextWindow)
		} else if m.MaxInputTokens > 0 {
			mi.ContextWindow = int(m.MaxInputTokens)
		}
		out = append(out, mi)
	}
	return out
}

func capOf(b bool) channel.Cap {
	if b {
		return channel.CapYes
	}
	return channel.CapNo
}

// modelKey 客户端模型名 → 上游 model key：动态映射优先；
// 无静态兜底表，未命中时原样返回。
func (a *Adapter) modelKey(clientName string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if k := a.modelMap[clientName]; k != "" {
		return k
	}
	return clientName
}

func (a *Adapter) modelEntry(key string) *ModelEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for i := range a.cache {
		if a.cache[i].Key == key {
			return &a.cache[i]
		}
	}
	return nil
}

// NormalizeModelName 把 display_name 转成 OpenAI 风格客户端名：
// 小写、空格/下划线转连字符、保留点号（版本号）、去重连字符。
func NormalizeModelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '_' || r == '-':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			b.WriteRune(r)
			prevDash = false
		}
	}
	return strings.Trim(b.String(), "-")
}

// truncate 截断字符串。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
