package kimi

// models.go 模型目录：POST GetAvailableModels（普通 JSON，不是 Connect 信封）。
//
// 上游**确实有**模型目录接口，参考实现拿它当权威来源（kimi2api app/kimi/model_catalog.py:
// fetch_model_catalog → POST /apiv2/…/GetAvailableModels，body `{}`、结果缓存 300s），
// 而且模型 id 不是拍脑袋定死的，是按固定规则从目录响应**生成**出来的
// （model_catalog.py:80-123 _model_version_slug / _model_suffix / _model_id）：
//
//	id = "kimi-" + 版本 slug            （displayName 里的 `K<数字>`，抠不到就按 scenario 推）
//	           + 可选后缀                （"-thinking" / "-agent" / "-agent-swarm"）
//	+ 再给支持联网的档位补一个 "<id>-search" 别名（model_catalog.py:176-191）
//
// 所以我们照抄这套生成规则而不是自己编白名单：白名单会和上游漂移对不上，
// 而「面板列了、点进去报错」比「少列一个」更糟（红线一：不做会撒谎的目录）。
// 拉不到目录时退到 fallbackModels（见 constants.go），能力位如实标成 SourceLocal。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const (
	// probeTimeout 是「探一下」类接口的超时：目录与订阅校验。
	// 参考实现对这两处都设了 15s（model_catalog.py:259 的 timeout=15.0、
	// client.py:260 的 timeout=15.0），而对话走的是长超时 —— 拉不动目录不该把面板拖住十分钟。
	probeTimeout = 15 * time.Second

	// catalogTTL 是目录缓存时长，与参考实现一致（model_catalog.py:22，300s）。
	// 没有它，网关每次 /v1/models 都会真的打一次上游。
	catalogTTL = 300 * time.Second
)

// versionRe 与参考实现 _model_version_slug 的正则等价（model_catalog.py:81）：
// 从 displayName 里抠 `K<数字>(.<数字>)?`，例如 "K2.6 Instant" → "2.6"。
var versionRe = regexp.MustCompile(`(?i)\bK\s*(\d+(?:\.\d+)?)\b`)

// catalogResponse 是 GetAvailableModels 响应里我们读的字段。
// 参考实现对每个键都做了驼峰/下划线两种回退（model_catalog.py:73-78 _raw_value），照抄。
type catalogResponse struct {
	AvailableModels    []catalogEntryRaw `json:"availableModels"`
	AvailableModelsAlt []catalogEntryRaw `json:"available_models"`
}

type catalogEntryRaw struct {
	Scenario       string `json:"scenario"`
	DisplayName    string `json:"displayName"`
	DisplayNameAlt string `json:"display_name"`
	Thinking       bool   `json:"thinking"`
	KimiPlusID     string `json:"kimiPlusId"`
	KimiPlusIDAlt  string `json:"kimi_plus_id"`
	AgentMode      string `json:"agentMode"`
	AgentModeAlt   string `json:"agent_mode"`
}

// fetchModelCatalog 拉一次上游目录，转成「面板形态」+「id → 档位」两张表。
func (a *Adapter) fetchModelCatalog(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, map[string]modelSpec, error) {
	token, err := a.accessToken(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// body 是普通 JSON 的 `{}`（不是 Connect 信封）：参考实现这里传 json={}，
	// 并且只覆盖 Accept/Content-Type 为 application/json（model_catalog.py:248-265）。
	resp, err := a.send(ctx, c, http.MethodPost, epModels, token, []byte("{}"), false,
		map[string]string{"Accept": "application/json", "Content-Type": "application/json"})
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, nil, errs.New(a.Classify(resp.StatusCode, raw), "获取模型目录失败").
			WithChannel(string(channel.Kimi)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	var parsed catalogResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, nil, errs.New(errs.Parse, "模型目录不是可解析的 JSON").
			WithChannel(string(channel.Kimi)).WithAccount(uidOf(c)).
			WithUpstream(truncate(string(raw), 200))
	}
	models := parseCatalog(parsed)
	if len(models) == 0 {
		return nil, nil, errs.New(errs.Parse, "上游模型目录为空").WithChannel(string(channel.Kimi))
	}
	specs := make(map[string]modelSpec, len(models))
	for _, m := range models {
		specs[strings.ToLower(m.ID)] = m.Spec
	}
	return toModelInfos(models, channel.SourceUpstream), specs, nil
}

// parseCatalog 按参考实现的规则把目录响应转成模型表（含 -search 别名）。
func parseCatalog(raw catalogResponse) []localModel {
	src := raw.AvailableModels
	if len(src) == 0 {
		src = raw.AvailableModelsAlt
	}
	out := make([]localModel, 0, len(src)*2)
	seen := make(map[string]bool, len(src)*2)

	// 第一轮：目录里的基础档位（_model_spec + 去重按先到先得，model_catalog.py:144-149）。
	type base struct {
		model       localModel
		displayName string
		supportsWeb bool
	}
	bases := make([]base, 0, len(src))
	for _, r := range src {
		sc := strings.TrimSpace(r.Scenario)
		if sc == "" {
			// 没有 scenario 的条目我们发不出请求（scenario 是请求体必填项），跳过。
			// 参考实现不显式挡这一条，但它的 id 生成此时会产出 "kimi-" 这种空壳，
			// 对我们没用 —— 宁可少列，也不下发一个必然失败的档位。
			continue
		}
		name := firstNonEmpty(r.DisplayName, r.DisplayNameAlt, sc)
		plusID := firstNonEmpty(r.KimiPlusID, r.KimiPlusIDAlt)
		agentMode := firstNonEmpty(r.AgentMode, r.AgentModeAlt)
		id := "kimi-" + modelVersionSlug(name, sc) + modelSuffix(sc, name, r.Thinking, plusID, agentMode)
		if seen[id] {
			continue
		}
		seen[id] = true
		m := localModel{
			ID:   id,
			Name: name,
			Spec: modelSpec{scenario: sc, thinking: r.Thinking, kimiPlusID: plusID, agentMode: agentMode, known: true},
		}
		// 「能联网」的能力参考实现只给 SCENARIO_K2D5 的档位（model_catalog.py:134：
		// supports_web_search = scenario == "SCENARIO_K2D5"）—— agent 档（OK_COMPUTER）
		// 因此**不会**有 -search 别名，这一点参考实现的测试里明确断言了
		// （test_model_catalog.py:57：by_id("kimi-k2.6-agent-search") is None）。
		bases = append(bases, base{model: m, displayName: name, supportsWeb: sc == scenario})
		out = append(out, m)
	}

	// 第二轮：给「支持联网」的档位补 "<id>-search" 别名（_with_search_aliases，model_catalog.py:176-191）。
	// 参考实现把「联网」能力绑在 SCENARIO_K2D5 上（_model_spec 里 supports_web_search=scenario==K2D5）。
	for _, b := range bases {
		if !b.supportsWeb || strings.HasSuffix(b.model.ID, "-search") {
			continue
		}
		aliasID := b.model.ID + "-search"
		if seen[aliasID] {
			continue
		}
		seen[aliasID] = true
		spec := b.model.Spec
		spec.search = true // 别名的语义就是「同一个档位 + 强制联网」
		out = append(out, localModel{ID: aliasID, Name: b.displayName + " Search", Spec: spec})
	}
	return out
}

// modelVersionSlug 对应 _model_version_slug：优先从 displayName 抠版本号，
// 抠不到再按 scenario 推（K2 → k2、K2D5 → k2.6、其它 → scenario 去掉前缀）。
// 参数名用 sc 而不是 scenario：包级还有个 scenario 常量，同名会把它遮住（这里踩过一次）。
func modelVersionSlug(displayName, sc string) string {
	if m := versionRe.FindStringSubmatch(displayName); m != nil {
		return "k" + m[1]
	}
	if sc == "SCENARIO_K2" {
		return "k2"
	}
	if sc == scenario { // SCENARIO_K2D5：本适配器唯一在用的对话场景
		return "k2.6"
	}
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(sc, "SCENARIO_")), "_", "-")
}

// modelSuffix 对应 _model_suffix：agent-swarm 优先于 agent，agent 优先于 thinking。
func modelSuffix(sc, displayName string, thinking bool, kimiPlusID, agentMode string) string {
	name := strings.ToLower(displayName)
	switch {
	case agentMode == agentModeUltra || strings.Contains(name, "swarm"):
		return "-agent-swarm"
	case sc == scenarioOKComputer || kimiPlusID != "" || strings.Contains(name, "agent"):
		return "-agent"
	case thinking:
		return "-thinking"
	}
	return ""
}

// toModelInfos 模型表 → channel.ModelInfo。能力位的判定与兜底表一致：
// 工具调用由网关模拟（Tools=CapYes）、思考能力跟着档位走、上下文长度上游没给就留 0。
func toModelInfos(models []localModel, src channel.Source) []channel.ModelInfo {
	out := make([]channel.ModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, channel.ModelInfo{
			ID:            m.ID,
			DisplayName:   m.Name,
			ContextWindow: 0, // 上游没给，不猜数字（F4.2）
			Source:        src,
			Tools:         channel.CapYes,
			Reasoning:     capOf(m.Spec.thinking),
			Images:        channel.CapNo,
		})
	}
	return out
}

// cachedCatalog 返回还在 TTL 内的目录快照。
func (a *Adapter) cachedCatalog() ([]channel.ModelInfo, bool) {
	a.catMu.RLock()
	defer a.catMu.RUnlock()
	if a.catalog == nil || time.Since(a.catalogAt) > catalogTTL {
		return nil, false
	}
	out := make([]channel.ModelInfo, len(a.catalog))
	copy(out, a.catalog)
	return out, true
}

// storeCatalog 记下这次成功拉取的目录（快照 + id→档位表）。
func (a *Adapter) storeCatalog(models []channel.ModelInfo, specs map[string]modelSpec) {
	a.catMu.Lock()
	a.catalog = models
	a.catalogAt = time.Now()
	for id, spec := range specs {
		a.specs[id] = spec
	}
	a.catMu.Unlock()
}

// specFor 按模型名查模型表（上游目录优先，兜底表在 New 里预置）。
// 查表用小写：客户端传的模型名大小写不稳，而表里的 id 全是小写。
func (a *Adapter) specFor(id string) (modelSpec, bool) {
	a.catMu.RLock()
	defer a.catMu.RUnlock()
	spec, ok := a.specs[strings.ToLower(strings.TrimSpace(id))]
	return spec, ok
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
