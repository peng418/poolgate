// models.go 动态模型目录：COSY 签名 `GET /algo/api/v2/model/list`。
//
// 为什么动态而不写死：灵码的可用模型随账号权益、企业策略不同（参考实现里同一个端点在不同
// 账号下返回的表就不一样），写死一张表等于给用户下发一堆它账号里根本不存在的模型 ——
// 表现是「能选、一用就报 ModelUnavailable」。所以这里只认上游给的表，拉不到就报错
// （缓存策略是「上次成功拉取」，与 qodercn 一致：宁可给旧表，也不编一张新表）。
package lingma

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// modelEntry 是模型目录里的一行（参考实现 remote/client.go 的 Model）。
//
// 只认这四个字段：上游的表里没有上下文长度、也没有能力位，所以这里也不猜
// （channel.ModelInfo 的三态能力位正是为这种「不知道」准备的）。
type modelEntry struct {
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
	Model       string `json:"model"`
	Enable      bool   `json:"enable"`
}

// fetchModels 调上游模型目录接口。
//
// 注意：GET 请求**没有 body**，签名原文里的 body 段必须是空串 ""（不是 "{}"）——
// 传 "{}" 会得到 403 签名错误（与 qodercn 同一个坑，参考实现也是传空串）。
func (a *Adapter) fetchModels(ctx context.Context, c *channel.Credential) ([]modelEntry, error) {
	lc := credOf(c)
	if err := lc.validate(); err != nil {
		return nil, errs.New(errs.SessionDead, "凭证不完整："+err.Error()).
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c))
	}
	rawURL := a.base + epModels
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errs.New(errs.Transport, "构造模型目录请求失败").
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c)).WithCause(err)
	}
	if err := lc.signer().applyHeaders(req, "", rawURL, "application/json"); err != nil {
		return nil, errs.New(errs.Parse, "设置 COSY 头失败").
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c)).WithCause(err)
	}
	resp, err := a.clientFor(c).Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "请求模型目录失败").
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c)).WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, errs.New(Classify(resp.StatusCode, raw), "上游拒绝模型目录请求").
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	entries, err := parseModelList(raw)
	if err != nil {
		return nil, errs.New(errs.Parse, "模型目录无法解析").
			WithChannel(string(channel.Lingma)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200)).WithCause(err)
	}
	return entries, nil
}

// parseModelList 解析三种可能的目录形态：`{"chat":[…],"inline":[…]}`（参考实现实测形态）、
// `{"models":[…]}`、以及直接的顶层数组。只保留 enable==true 且 key 非空的条目。
func parseModelList(raw []byte) ([]modelEntry, error) {
	var wrapper struct {
		Chat   []modelEntry `json:"chat"`
		Inline []modelEntry `json:"inline"`
		Models []modelEntry `json:"models"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil {
		all := append(append(append([]modelEntry{}, wrapper.Chat...), wrapper.Inline...), wrapper.Models...)
		if enabled := onlyEnabled(all); len(enabled) > 0 {
			return enabled, nil
		}
		// 结构对得上但没有可用条目：说明账号确实没有可用模型，如实返回空（不编表）。
		if len(wrapper.Chat)+len(wrapper.Inline)+len(wrapper.Models) > 0 {
			return nil, fmt.Errorf("模型目录里没有 enable 的条目")
		}
	}
	var list []modelEntry
	if err := json.Unmarshal(raw, &list); err == nil {
		if enabled := onlyEnabled(list); len(enabled) > 0 {
			return enabled, nil
		}
	}
	return nil, fmt.Errorf("未识别的模型目录结构")
}

func onlyEnabled(list []modelEntry) []modelEntry {
	out := make([]modelEntry, 0, len(list))
	for _, e := range list {
		if e.Enable && strings.TrimSpace(e.Key) != "" {
			out = append(out, e)
		}
	}
	return out
}

// checkCredential 用模型目录做一次「这凭证上游认不认」的轻量校验（登录 Poll 调它）。
func (a *Adapter) checkCredential(ctx context.Context, c *channel.Credential) error {
	_, err := a.fetchModels(ctx, c)
	return err
}

// Models 实现 channel.Channel：动态拉取 → channel.ModelInfo；失败回退上次成功缓存；
// 连缓存都没有就返回错误（Fail Early，不给一张编出来的表）。
func (a *Adapter) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	dyn, err := a.fetchModels(ctx, c)
	if err != nil {
		if cached := a.cachedModels(); len(cached) > 0 {
			return toModelInfos(cached), nil
		}
		return nil, err
	}
	a.setCache(dyn)
	return toModelInfos(dyn), nil
}

// ModelsSnapshot 返回上次成功拉取的目录快照（供 /v1/models 用）；无缓存返回空。
func (a *Adapter) ModelsSnapshot() []channel.ModelInfo {
	return toModelInfos(a.cachedModels())
}

// toModelInfos 动态表 → channel.ModelInfo。
//
// 能力位一律 CapUnknown：上游的表里确实没有这三项信息。**不要**因为我们自己声明了
// Spec.Tools=true 就给每个模型都标 CapYes —— 那是把「渠道整体支持工具」当成
// 「这个模型支持工具」，对模型级能力位来说就是猜（F4.2）。
func toModelInfos(dyn []modelEntry) []channel.ModelInfo {
	out := make([]channel.ModelInfo, 0, len(dyn))
	for _, m := range dyn {
		name := normalizeModelName(m.DisplayName)
		if name == "" {
			name = normalizeModelName(m.Key)
		}
		if name == "" {
			continue
		}
		out = append(out, channel.ModelInfo{
			ID:            name,
			DisplayName:   firstNonEmpty(m.DisplayName, m.Key),
			ContextWindow: 0, // 上游没给，不猜数字
			Source:        channel.SourceUpstream,
			Tools:         channel.CapUnknown,
			Images:        channel.CapUnknown,
			Reasoning:     channel.CapUnknown,
		})
	}
	return out
}

// modelKey 客户端模型名 → 上游 model key：按目录映射；未命中时原样返回
// （让上游自己去判，比我们在本地猜一个「最接近的」更诚实 —— 猜错就是静默降级）。
func (a *Adapter) modelKey(clientName string) string {
	a.modelsMu.RLock()
	defer a.modelsMu.RUnlock()
	if k := a.modelMap[clientName]; k != "" {
		return k
	}
	return clientName
}

func (a *Adapter) cachedModels() []modelEntry {
	a.modelsMu.RLock()
	defer a.modelsMu.RUnlock()
	if len(a.cache) == 0 {
		return nil
	}
	out := make([]modelEntry, len(a.cache))
	copy(out, a.cache)
	return out
}

func (a *Adapter) setCache(entries []modelEntry) {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		name := normalizeModelName(e.DisplayName)
		if name == "" {
			name = normalizeModelName(e.Key)
		}
		if name != "" {
			m[name] = e.Key
		}
	}
	a.modelsMu.Lock()
	a.modelMap = m
	a.cache = entries
	a.modelsMu.Unlock()
}

// normalizeModelName 把 display_name 转成 OpenAI 风格的客户端模型名：
// 小写、空格/下划线转连字符、保留点号（版本号）、去掉重复与首尾连字符。
// 逻辑与 qodercn 的 NormalizeModelName 保持一致 —— 同一个上游家族的表，命名规则别写成两样。
func normalizeModelName(s string) string {
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
