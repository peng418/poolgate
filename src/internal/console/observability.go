// observability.go 面板可观测接口：总览 / 账号清单 / 请求流水。
// 所有数据来自真实运行态（registry + pool + request log），不返回硬编码假值。
package console

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/health"
	"poolgate/internal/registry"
	"poolgate/internal/store"
)

// handleOverview 返回总览：各渠道状态 + 健康账号数 + 错误分类聚合（F6.2）
// + 今日请求/失败率/TTFT 中位（原型 01-overview.html 的四张指标卡）。
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	type channelStatus struct {
		Kind        string `json:"kind"`
		DisplayName string `json:"display_name"`
		Status      string `json:"status"`
		Downstream  bool   `json:"downstream"`
		Healthy     int    `json:"healthy_accounts"`
		Total       int    `json:"total_accounts"`
		CheckinCap  bool   `json:"checkin_cap"`
		// HealthyModels/TotalModels 来自最近一次体检，没有体检时为 0（UI 显示「未体检」）。
		HealthyModels int `json:"healthy_models"`
		TotalModels   int `json:"total_models"`
	}
	var channels []channelStatus
	for _, e := range registry.All() {
		healthy, total := 0, 0
		if s.opts.Pool != nil {
			healthy = s.opts.Pool.CountHealthy(e.Spec.Kind)
			total = len(s.opts.Pool.List(e.Spec.Kind))
		}
		hm, tm := s.channelModelHealth(string(e.Spec.Kind))
		channels = append(channels, channelStatus{
			Kind:          string(e.Spec.Kind),
			DisplayName:   e.Spec.DisplayName,
			Status:        string(e.Spec.Status),
			Downstream:    e.Spec.Downstream(),
			Healthy:       healthy,
			Total:         total,
			CheckinCap:    e.Spec.CheckinCap,
			HealthyModels: hm,
			TotalModels:   tm,
		})
	}
	errStats := map[string]int{}
	var summary store.Summary
	var daily []store.DailyPoint
	chStats := map[string]struct {
		MedianTTFTms int64 `json:"median_ttft_ms"`
		Samples      int   `json:"samples"`
	}{}
	if s.opts.Log != nil {
		errStats = s.opts.Log.Stats()
		summary = s.opts.Log.Summary()
		daily = s.opts.Log.Daily(7)
		chStats = s.opts.Log.ChannelStats()
	}
	resp := map[string]any{
		"channels":    channels,
		"error_stats": errStats,
		"today": map[string]any{
			"requests":        summary.TodayRequests,
			"failed":          summary.TodayFailed,
			"ttft_ms":         summary.TodayTTFTms,
			"p95_ms":          summary.TodayP95TTFTms,
			"yesterday":       summary.YesterdayFail,
			"upstream_failed": summary.UpstreamFail,
			"account_failed":  summary.AccountFail,
			"rotations":       summary.Rotations,
			"rotated_ok":      summary.RotatedOK,
		},
		"daily":        daily,
		"channel_ttft": chStats,
		"probe":        s.probeSummary(),
		"pending":      s.pendingItems(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// pendingItem 是总览「待处理」列表的一行（原型 01-overview.html 末卡）。
type pendingItem struct {
	Severity string `json:"severity"` // crit | warn | pause
	Title    string `json:"title"`
	Meta     string `json:"meta"`
}

// pendingItems 汇总当前需要人处理的事：渠道故障/降级、余额为零、模型挂死。
// 排序按严重度：故障 → 降级 → 额度。
func (s *Server) pendingItems() []pendingItem {
	var crit, warn, pause []pendingItem
	for _, e := range registry.All() {
		total, healthy := 0, 0
		if s.opts.Pool != nil {
			total = len(s.opts.Pool.List(e.Spec.Kind))
			healthy = s.opts.Pool.CountHealthy(e.Spec.Kind)
		}
		switch e.Spec.Status {
		case channel.Paused:
			if total > 0 {
				pause = append(pause, pendingItem{
					Severity: "pause",
					Title:    fmt.Sprintf("%s 渠道暂停中（%d 个账号保留）", e.Spec.DisplayName, total),
					Meta:     "账号与适配器保留，恢复只需改状态",
				})
			}
			continue
		}
		if total > 0 && healthy == 0 {
			crit = append(crit, pendingItem{
				Severity: "crit",
				Title:    fmt.Sprintf("%s 无健康账号（%d 个账号全部冷却或禁用）", e.Spec.DisplayName, total),
				Meta:     "渠道级告警 · 到「账号」页查看原因",
			})
		}
		// 模型级：最近一次体检里失败的模型。
		for _, r := range s.failedModelsOf(string(e.Spec.Kind)) {
			warn = append(warn, pendingItem{
				Severity: "warn",
				Title:    fmt.Sprintf("%s 的 %s 体检失败", e.Spec.DisplayName, r.Model),
				Meta:     fmt.Sprintf("%s · %s", r.Kind, r.Reason),
			})
		}
	}
	out := append(crit, warn...)
	out = append(out, pause...)
	return out
}

// failedModelsOf 取某渠道最近一次体检里失败的模型（去重）。
func (s *Server) failedModelsOf(kind string) []health.Result {
	if s.opts.History == nil {
		return nil
	}
	runs := s.opts.History.List()
	for i := len(runs) - 1; i >= 0; i-- {
		var out []health.Result
		for _, r := range runs[i].Results {
			if r.Channel == kind && !r.OK {
				out = append(out, r)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// channelModelHealth 汇总某渠道最近一次体检的模型通过情况。
func (s *Server) channelModelHealth(kind string) (healthy, total int) {
	if s.opts.History == nil {
		return 0, 0
	}
	runs := s.opts.History.List()
	for i := len(runs) - 1; i >= 0; i-- {
		found := false
		for _, res := range runs[i].Results {
			if res.Channel != kind || res.Model == "-" {
				continue
			}
			found = true
			total++
			if res.OK {
				healthy++
			}
		}
		if found {
			return healthy, total
		}
	}
	return 0, 0
}

// probeSummary 返回最近一次体检的汇总，供总览「上次体检」显示。
func (s *Server) probeSummary() map[string]any {
	if s.opts.History == nil {
		return map[string]any{"has_run": false}
	}
	run, ok := s.opts.History.Latest()
	if !ok {
		return map[string]any{"has_run": false}
	}
	return map[string]any{
		"has_run":     true,
		"kind":        string(run.Kind),
		"started_at":  run.StartedAt,
		"duration_ms": run.Duration,
		"total":       run.Total,
		"passed":      run.Passed,
		"failed":      run.Failed,
	}
}

// median 取中位数（TTFT 用）。空集返回 0。
func median(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]int64(nil), xs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[len(cp)/2]
}

// checkinOut 是账号屏要显示的最近一次签到结果。
type checkinOut struct {
	OK         bool   `json:"ok"`
	Message    string `json:"message"`
	Reward     string `json:"reward,omitempty"`
	NoActivity bool   `json:"no_activity,omitempty"`
	At         string `json:"at"`
}

// handleAccounts 返回账号清单（按渠道分组，脱敏：不含 token）。
// 含余额/到期/冷却/签到/启停原因 —— 账号屏需要的都在这里（F1.7/F1.8/F1.9）。
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	type acct struct {
		Kind     string `json:"kind"`
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		Credits  int64  `json:"credits"`
		// CreditsKnown 为 false 时 UI 显示「未知」而不是 0（F4.2：不猜数字）。
		CreditsKnown bool   `json:"credits_known"`
		ExpiresAt    string `json:"expires_at,omitempty"`
		Disabled     bool   `json:"disabled"`
		Cooling      bool   `json:"cooling"`
		Reason       string `json:"reason,omitempty"`
		// CooldownUntil 是冷却截止时间，UI 显示「还要等多久」。
		CooldownUntil string `json:"cooldown_until,omitempty"`
		ErrCount      int    `json:"err_count"`
		// Checkin 是最近一次签到结果（可能为空）。
		Checkin *checkinOut `json:"checkin,omitempty"`
		// HasCredential 表示磁盘上是否有凭证文件（面板「重新登录」按钮的判据）。
		HasCredential bool `json:"has_credential"`
	}
	var out []acct
	if s.opts.Pool != nil {
		for _, e := range registry.All() {
			for _, st := range s.opts.Pool.List(e.Spec.Kind) {
				item := acct{
					Kind:          string(st.Kind),
					UID:           st.Cred.UID,
					Nickname:      st.Cred.Nickname,
					Credits:       st.Credits,
					CreditsKnown:  st.CreditsKnown,
					Disabled:      st.Disabled,
					Cooling:       !st.Until.IsZero() && time.Now().Before(st.Until),
					Reason:        st.Reason,
					ErrCount:      st.ErrCount,
					HasCredential: true,
				}
				// 到期时间优先用余额快照，其次用凭证自带的。
				exp := st.ExpiresAt
				if exp.IsZero() {
					exp = st.Cred.ExpiresAt
				}
				item.ExpiresAt = rfc3339OrEmpty(exp)
				if !st.Until.IsZero() && time.Now().Before(st.Until) {
					item.CooldownUntil = st.Until.Format(time.RFC3339)
				}
				if s.opts.Checkins != nil {
					if c, ok := s.opts.Checkins.Get(string(st.Kind), st.Cred.UID); ok {
						item.Checkin = &checkinOut{
							OK: c.OK, Message: c.Message, Reward: c.Reward,
							NoActivity: c.NoActivity, At: c.At.Format(time.RFC3339),
						}
					}
				}
				out = append(out, item)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// handleLogs 返回最近请求流水。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if s.opts.Log == nil {
		writeJSON(w, http.StatusOK, map[string]any{"requests": []any{}, "total": 0})
		return
	}
	list := s.opts.Log.List()
	writeJSON(w, http.StatusOK, map[string]any{"requests": list, "total": len(list)})
}

// handleModels 返回模型目录（面板用，会话鉴权）。与网关 /v1/models 同源：
// 优先用真实账号向上游拉取，失败回退适配器缓存快照。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	type modelOut struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	}
	out := []modelOut{}
	if s.opts.Pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": out})
		return
	}
	for _, e := range registry.Active() {
		for _, m := range s.modelsFor(r, e) {
			out = append(out, modelOut{ID: string(e.Spec.Kind) + "/" + m.ID, OwnedBy: string(e.Spec.Kind)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

// modelsFor 与 gateway.modelsFor 同逻辑：真实拉取优先，快照兜底。
//
// 硬性前提：该渠道**必须至少有一个账号**。模型是「用某个账号向上游问出来的」，
// 没有账号就不存在可用模型 —— 否则全新安装（0 个账号）也会显示一堆模型，
// 用户会以为「装完就能用」，点了却全部失败。
func (s *Server) modelsFor(r *http.Request, e registry.Entry) []channel.ModelInfo {
	if s.opts.Pool == nil {
		return nil
	}
	cred, ok := s.opts.Pool.Pick(r.Context(), e.Spec.Kind, nil)
	if !ok {
		return nil // 没有账号：不给模型（静态兜底表也不行）
	}
	if models, err := e.Channel.Models(r.Context(), &cred); err == nil && len(models) > 0 {
		return models
	}
	if snap, ok := e.Channel.(interface{ ModelsSnapshot() []channel.ModelInfo }); ok {
		return snap.ModelsSnapshot()
	}
	return nil
}
