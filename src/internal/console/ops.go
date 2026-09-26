// ops.go 面板的运维接口：账号启停/删除、模型与费率、体检/测速、一键诊断、
// 日志过滤、设置读写、备份导出（需求 F1.7/F1.9/F4.x/F5.x/F6.x）。
//
// 所有写操作都返回结构化结果；失败必带原因（红线一）。这里不返回「请重试」。
package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/health"
	"poolgate/internal/registry"
	"poolgate/internal/store"
)

// ---------------------------------------------------------------------------
// 账号：启停 / 删除 / 签到 / 刷新余额
// ---------------------------------------------------------------------------

type accountActionReq struct {
	Kind string `json:"kind"`
	UID  string `json:"uid"`
}

// handleAccountEnable 手动启用/停用账号（F1.9）。停用后不再参与路由，原因可见。
func (s *Server) handleAccountEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req struct {
		Kind     string `json:"kind"`
		UID      string `json:"uid"`
		Disabled bool   `json:"disabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	if s.opts.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.NoCandidate, "账号池未装配"))
		return
	}
	if _, ok := s.opts.Pool.Get(channel.Kind(req.Kind), req.UID); !ok {
		writeErr(w, http.StatusNotFound, errs.New(errs.NoCandidate,
			fmt.Sprintf("账号 %s/%s 不存在", req.Kind, req.UID)))
		return
	}
	s.opts.Pool.SetDisabled(channel.Kind(req.Kind), req.UID, req.Disabled)
	if !req.Disabled {
		s.opts.Pool.ClearCooldown(channel.Kind(req.Kind), req.UID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAccountRemove 删除账号：内存池 + 磁盘凭证一起删（F1.9）。
func (s *Server) handleAccountRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req accountActionReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	kind := channel.Kind(req.Kind)
	removedMem := false
	if s.opts.Pool != nil {
		removedMem = s.opts.Pool.Remove(kind, req.UID)
	}
	removedFile := false
	if s.opts.Creds != nil {
		ok, err := s.opts.Creds.Delete(kind, req.UID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "删除凭证文件失败").WithCause(err))
			return
		}
		removedFile = ok
	}
	if !removedMem && !removedFile {
		writeErr(w, http.StatusNotFound, errs.New(errs.NoCandidate,
			fmt.Sprintf("账号 %s/%s 不存在", req.Kind, req.UID)))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed_memory": removedMem, "removed_file": removedFile})
}

// handleAccountCheckin 对一个账号执行签到（F1.8）。
func (s *Server) handleAccountCheckin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req accountActionReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var res channel.CheckinResult
	if err := s.withCredRetry(ctx, channel.Kind(req.Kind), req.UID,
		func(ch channel.Channel, c channel.Credential) error {
			var e error
			res, e = ch.Checkin(ctx, &c)
			return e
		}); err != nil {
		writeErrFromErr(w, err)
		return
	}
	s.noteCheckin(req.Kind, req.UID, res)
	writeJSON(w, http.StatusOK, res)
}

// handleAccountRefresh 刷新一个账号的余额（F1.7）。
func (s *Server) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req accountActionReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	kind := channel.Kind(req.Kind)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var bal channel.Balance
	if err := s.withCredRetry(ctx, kind, req.UID,
		func(ch channel.Channel, c channel.Credential) error {
			var e error
			bal, e = ch.Balance(ctx, &c)
			return e
		}); err != nil {
		writeErrFromErr(w, err)
		return
	}
	if s.opts.Pool != nil {
		s.opts.Pool.SetBalance(kind, req.UID, bal)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"credits": bal.Credits, "known": bal.Known,
		"expires_at": rfc3339OrEmpty(bal.ExpiresAt),
	})
}

// handleCheckinAll 对所有可签到账号执行签到（账号屏「全部签到」）。
func (s *Server) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	type one struct {
		Kind    string `json:"kind"`
		UID     string `json:"uid"`
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Reward  string `json:"reward,omitempty"`
		NoAct   bool   `json:"no_activity,omitempty"`
	}
	out := []one{}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	for _, e := range registry.Active() {
		if !e.Spec.CheckinCap {
			continue
		}
		for _, uid := range s.uidsOf(e.Spec.Kind) {
			cred, ch, ok := s.credFor(ctx, e.Spec.Kind, uid)
			if !ok {
				continue
			}
			res, err := ch.Checkin(ctx, &cred)
			item := one{Kind: string(e.Spec.Kind), UID: uid}
			if err != nil {
				item.OK = false
				item.Message = err.Error()
			} else {
				item.OK, item.Message, item.Reward, item.NoAct = res.OK, res.Message, res.Reward, res.NoActivity
				s.noteCheckin(string(e.Spec.Kind), uid, res)
			}
			out = append(out, item)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// handleRefreshAll 刷新全部账号余额（账号屏「刷新余额」）。
func (s *Server) handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	n := s.RefreshAllBalances(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "refreshed": n})
}

// RefreshAllBalances 拉取全部账号余额并写入池子，返回成功条数。
//
// 供两处共用：面板「刷新余额」按钮，以及进程启动时的一次预热。
// 启动预热是必需的 —— 否则刚装完/刚重启的面板余额全是「未知」，
// 用户以为没接上，其实只是没人去问过上游（F1.7：余额与到期感知）。
func (s *Server) RefreshAllBalances(ctx context.Context) int {
	if s.opts.Pool == nil {
		return 0
	}
	n := 0
	for _, e := range registry.All() {
		for _, uid := range s.uidsOf(e.Spec.Kind) {
			var bal channel.Balance
			err := s.withCredRetry(ctx, e.Spec.Kind, uid,
				func(ch channel.Channel, c channel.Credential) error {
					var e error
					bal, e = ch.Balance(ctx, &c)
					return e
				})
			if err != nil {
				// 单个账号失败不阻塞其它账号；面板上它仍显示「未知」。
				log.Printf("console: 刷新 %s/%s 余额失败: %v", e.Spec.Kind, uid, err)
				continue
			}
			s.opts.Pool.SetBalance(e.Spec.Kind, uid, bal)
			n++
		}
	}
	return n
}

// balanceTimeout 是单次余额查询的上限。上游回得慢时宁可标「未知」，
// 也不要让面板等出个转圈。
const balanceTimeout = 15 * time.Second

// refreshOneBalance 拉取一个账号的余额并写入池子，返回面板用的余额对象。
//
// 返回的第二个值是「没取到时的人类可读原因」，成功时为空串。
// 失败不回滚任何东西：授权/导入已经成功这件事不因余额取不到而改变
// （余额只是感知，不是可用性判据）—— 但必须把原因说出来，不能默默空着。
func (s *Server) refreshOneBalance(ctx context.Context, kind channel.Kind, uid string) (map[string]any, string) {
	cred, ch, ok := s.credFor(ctx, kind, uid)
	if !ok {
		return nil, "账号不在池中或该渠道无适配器"
	}
	ctx, cancel := context.WithTimeout(ctx, balanceTimeout)
	defer cancel()
	bal, err := ch.Balance(ctx, &cred)
	if err != nil {
		log.Printf("console: 刷新 %s/%s 余额失败: %v", kind, uid, err)
		return nil, err.Error()
	}
	if s.opts.Pool != nil {
		s.opts.Pool.SetBalance(kind, uid, bal)
	}
	return map[string]any{
		"credits":    bal.Credits,
		"known":      bal.Known,
		"expires_at": rfc3339OrEmpty(bal.ExpiresAt),
	}, ""
}

// ---------------------------------------------------------------------------
// 模型与费率
// ---------------------------------------------------------------------------

type modelRow struct {
	ID            string `json:"id"`
	Channel       string `json:"channel"`
	ChannelName   string `json:"channel_name"`
	DisplayName   string `json:"display_name,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	ContextSource string `json:"context_source"`
	Tools         string `json:"tools"`
	Images        string `json:"images"`
	Reasoning     string `json:"reasoning"`
	// Rate 费率倍率；上游未给时为 nil，UI 显示「未知」而不是编数字（F4.3）。
	Rate *float64 `json:"rate,omitempty"`
	// RateSource 标注倍率来源：upstream / unknown。
	RateSource string `json:"rate_source"`
	Healthy    string `json:"healthy"`
	TTFTms     int64  `json:"ttft_ms,omitempty"`
	Downstream bool   `json:"downstream"`
	// Excluded 表示人工把该模型剔除下发（F4.4，模型页的「下发」开关）。
	Excluded bool `json:"excluded"`
	// HiddenByHealth 表示该模型正因「只下发可用模型」开关被隐藏（F4.5）。
	// 面板要如实标出来：模型页不能让用户以为它凭空消失了。
	HiddenByHealth bool `json:"hidden_by_health"`
}

// handleModelCatalog 返回面板用的模型矩阵（含能力位/来源/健康/下发开关）。
// 与 /v1/models 同源，但带上面板需要的元数据。
func (s *Server) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	rows := []modelRow{}
	// 与网关共用同一个过滤结论（含开关判断），面板上标「已按健康度隐藏」的模型，
	// 客户端那边一定也拿不到。
	snap := s.healthyModels()
	for _, e := range registry.All() {
		if e.Channel == nil {
			continue
		}
		models := s.modelsFor(r, e)
		for _, m := range models {
			id := string(e.Spec.Kind) + "/" + m.ID
			row := modelRow{
				ID:            id,
				Channel:       string(e.Spec.Kind),
				ChannelName:   e.Spec.DisplayName,
				DisplayName:   m.DisplayName,
				ContextWindow: m.ContextWindow,
				ContextSource: string(contextSource(m)),
				Tools:         capString(m.Tools, e.Spec.Tools),
				Images:        capString(m.Images, e.Spec.Images),
				Reasoning:     capString(m.Reasoning, e.Spec.Reasoning),
				RateSource:    "unknown", // 上游费率接口未接入前不折算、不编造
				Healthy:       "unknown",
				Downstream:    e.Spec.Downstream() && !s.isExcluded(string(e.Spec.Kind), m.ID),
				Excluded:      s.isExcluded(string(e.Spec.Kind), m.ID),
				// 注意：HiddenByHealth 与 Downstream 是两回事 —— 前者是探测结论
				// （会变），后者是人工裁定（不动）。面板上要分别显示。
				HiddenByHealth: snap.Hide(id),
			}
			if v, ok := s.modelVerdict(string(e.Spec.Kind), m.ID); ok {
				if v.OK {
					row.Healthy = "ok"
					row.TTFTms = v.TTFTms
				} else {
					row.Healthy = "error"
				}
			}
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Channel != rows[j].Channel {
			return rows[i].Channel < rows[j].Channel
		}
		return rows[i].ID < rows[j].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"models": rows})
}

// contextSource 标注上下文窗口来源；拿不到就不猜数字（F4.2）。
//
// 适配器显式给了 Source 就用它；只给了数字没给来源的，算「本地声明」——
// 那是适配器里写死的常量，不是上游实时告诉我们的值。当成「上游」就是在撒谎。
func contextSource(m channel.ModelInfo) channel.Source {
	if m.ContextWindow <= 0 {
		return channel.SourceUnknown
	}
	if m.Source == "" {
		return channel.SourceLocal
	}
	return m.Source
}

// capString 把三态能力位渲染成面板用的文字 + 图标（F7.3：不只靠颜色）。
func capString(c channel.Cap, specFallback bool) string {
	switch c {
	case channel.CapYes:
		return "yes"
	case channel.CapNo:
		return "no"
	}
	if specFallback {
		return "local"
	}
	return "unknown"
}

// handleModelToggle 切换模型的下发开关（F4.4：挂死模型可从下发列表剔除）。
func (s *Server) handleModelToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req struct {
		ID         string `json:"id"`
		Downstream bool   `json:"downstream"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "缺少模型 id"))
		return
	}
	if s.opts.Excluded == nil {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.Parse, "剔除表未装配"))
		return
	}
	s.opts.Excluded.Set(req.ID, !req.Downstream)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": req.ID, "downstream": req.Downstream})
}

// withCredRetry 用某账号的凭证执行一次上游调用；**凭证失效时强制续期一次再重试**。
//
// 为什么需要：本地凭证的到期时间可能还没到、上游却已经拒绝（token 被吊销、轮换、
// 或者我们之前存错了 token 种类），只靠「临期才续期」救不回来。面板上的
// 「刷新余额 / 签到」正是用户最先点、也最先看到 401 的地方 —— 一次强制续期就能救，
// 否则用户看到的是「装完新版还是 401」，然后合理地质疑修复没生效。
func (s *Server) withCredRetry(ctx context.Context, kind channel.Kind, uid string,
	fn func(ch channel.Channel, c channel.Credential) error) error {
	cred, ch, ok := s.credFor(ctx, kind, uid)
	if !ok {
		return errs.New(errs.NoCandidate, fmt.Sprintf("账号 %s/%s 不存在或渠道未实现", kind, uid))
	}
	err := fn(ch, cred)
	if err == nil {
		return nil
	}
	k, _ := errs.KindOf(err)
	if k != errs.SessionDead && k != errs.AuthFailed {
		return err // 不是凭证问题：原样交出去
	}
	if s.opts.Pool == nil {
		return err
	}
	nc, rerr := s.opts.Pool.RefreshNow(ctx, kind, cred)
	if rerr != nil || nc == nil {
		// 刷不动（没有 refresh token / refresh_token 也被拒）：把**原始**错误交出去，
		// 它带着上游原话，比「续期失败」更有诊断价值。
		return err
	}
	log.Printf("console: %s/%s 凭证已续期，重试刚才那次调用", kind, uid)
	return fn(ch, *nc)
}

// healthyModels 返回「只下发可用模型」生效时的健康结论索引（未装配 / 开关关着 → 空索引）。
//
// 索引由装配层用 health.Health 统一构造，网关拿到的是**同一个函数** ——
// 面板上标着「已按健康度隐藏」的模型，客户端那边一定也拿不到，不会两边说法不一。
func (s *Server) healthyModels() health.Snapshot {
	if s.opts.HealthyModels == nil {
		return health.Snapshot{}
	}
	return s.opts.HealthyModels()
}

func (s *Server) isExcluded(kind, model string) bool {
	if s.opts.Excluded == nil {
		return false
	}
	return s.opts.Excluded.Has(kind + "/" + model)
}

// ---------------------------------------------------------------------------
// 体检 / 测速
// ---------------------------------------------------------------------------

// handleProbe 执行抽样体检或全量测速（F5.1/F5.3/F5.4）。
func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req struct {
		Mode string `json:"mode"` // sample | full
	}
	_ = decodeJSON(r, &req)
	if s.opts.Health == nil || s.opts.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.Parse, "健康探测未装配"))
		return
	}
	entries := s.probeEntries(r)
	if len(entries) == 0 {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.NoCandidate, "没有可探测的渠道（无可用账号）"))
		return
	}
	// 探测可能跑很久（全量 57 个模型）：给足超时，但要有上限。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()

	var run health.Run
	if req.Mode == "full" {
		run = s.opts.Health.Full(ctx, entries)
	} else {
		run = s.opts.Health.Sample(ctx, entries, s.probeSamples())
	}
	if s.opts.History != nil {
		s.opts.History.Add(run)
	}
	logProbeRun(run)
	writeJSON(w, http.StatusOK, run)
}

// logProbeRun 把体检结论落进日志。
//
// 面板是给眼睛看的，日志是给「事后回看」用的。2026-09-26 的真机事故就是这样：
// 体检三个模型全红、红得有理有据（上游原话都在面板上），但日志里一个字都没有 ——
// 用户把日志发过来，我们只能看到「什么都没发生过」。失败项必须留下痕迹（红线一）。
func logProbeRun(run health.Run) {
	label := "抽样体检"
	if run.Kind == health.RunFull {
		label = "全量测速"
	}
	dash := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "-"
		}
		return s
	}
	clip := func(s string, n int) string {
		if len(s) <= n {
			return s
		}
		return s[:n] + "…"
	}
	log.Printf("console: %s完成 %d/%d 通过，耗时 %.1fs",
		label, run.Passed, run.Total, float64(run.Duration)/1000)
	for _, res := range run.Results {
		skip := ""
		if n := len(res.Skipped); n > 0 {
			skip = fmt.Sprintf("（已跳过 %d 个失效账号：%s）", n, strings.Join(res.Skipped, "、"))
		}
		// 「换号后才通过」也要留痕：只写失败的话，日志里会看不见中间换过号，
		// 而「哪个号是坏的」恰恰是最该被记住的那件事。
		if res.OK {
			if skip != "" {
				log.Printf("console: %s通过 %s/%s 账号=%s%s", label, res.Channel, res.Model, dash(res.Account), skip)
			}
			continue
		}
		log.Printf("console: %s失败 %s/%s 账号=%s 分类=%s%s · %s · 上游原话 %s",
			label, res.Channel, res.Model, dash(res.Account), dash(res.Kind),
			skip, dash(res.Reason), dash(clip(res.Upstream, 200)))
	}
}

// handleProbeHistory 返回体检历史（F5.5：能回看「上次为什么判它坏」）。
func (s *Server) handleProbeHistory(w http.ResponseWriter, r *http.Request) {
	if s.opts.History == nil {
		writeJSON(w, http.StatusOK, map[string]any{"runs": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.opts.History.List()})
}

// probeEntries 组装探测素材：Active 渠道 + 其可用模型 + 有账号。
func (s *Server) probeEntries(r *http.Request) []health.Entry {
	out := []health.Entry{}
	for _, e := range registry.Active() {
		if len(s.uidsOf(e.Spec.Kind)) == 0 {
			continue // 无账号的渠道在 plan 里会记失败，但这里直接跳过更干净
		}
		models := s.modelsFor(r, e)
		ids := make([]string, 0, len(models))
		for _, m := range models {
			if s.isExcluded(string(e.Spec.Kind), m.ID) {
				continue // 已剔除的模型不参与下发，也不参与体检
			}
			ids = append(ids, m.ID)
		}
		out = append(out, health.Entry{Kind: e.Spec.Kind, Channel: e.Channel, Models: ids})
	}
	return out
}

func (s *Server) probeSamples() int {
	if s.opts.Settings != nil {
		if n := s.opts.Settings.Get().ProbeSamples; n > 0 {
			return n
		}
	}
	return 3
}

// modelVerdict 取最近一次体检里某模型（含渠道前缀）的结论。
func (s *Server) modelVerdict(channelName, model string) (health.Result, bool) {
	if s.opts.History == nil {
		return health.Result{}, false
	}
	runs := s.opts.History.List()
	for i := len(runs) - 1; i >= 0; i-- {
		for _, res := range runs[i].Results {
			if res.Channel == channelName && res.Model == model {
				return res, true
			}
		}
	}
	return health.Result{}, false
}

// ---------------------------------------------------------------------------
// 一键诊断
// ---------------------------------------------------------------------------

// handleDiagnose 一键诊断（F6.3）：给定渠道/模型，回答「本地问题还是上游问题」。
func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req struct {
		Channel string `json:"channel"`
		Model   string `json:"model"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	kind := channel.Kind(req.Channel)
	if kind == "" {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "缺少 channel"))
		return
	}
	steps := []diagStep{}
	add := func(name, value, status string) {
		steps = append(steps, diagStep{Name: name, Value: value, Status: status})
	}

	// 1) 渠道是否注册 / 是否暂停
	spec, ok := registry.GetSpec(kind)
	if !ok {
		add("渠道注册", fmt.Sprintf("未注册渠道 %q", kind), "crit")
		writeJSON(w, http.StatusOK, diagResp{Verdict: "本地配置问题：渠道未注册", Kind: string(errs.NoCandidate), Steps: steps})
		return
	}
	add("渠道注册", fmt.Sprintf("%s（%s）", spec.DisplayName, spec.Status), statusOf(spec.Status == channel.Active))

	// 2) 是否有可用账号
	uids := s.uidsOf(kind)
	if len(uids) == 0 {
		add("账号", "该渠道没有账号凭证", "crit")
		writeJSON(w, http.StatusOK, diagResp{Verdict: "本地配置问题：没有可用账号", Kind: string(errs.NoCandidate), Steps: steps})
		return
	}
	add("账号", fmt.Sprintf("%d 个账号", len(uids)), "ok")

	// 3) 凭证能否解析 + 模型目录能否获取
	cred, ch, ok := s.credFor(r.Context(), kind, uids[0])
	if !ok {
		add("凭证", "凭证无法加载或渠道未实现", "crit")
		writeJSON(w, http.StatusOK, diagResp{Verdict: "本地配置问题：凭证不可用", Kind: string(errs.AuthFailed), Steps: steps})
		return
	}
	add("凭证状态", "有效（已解析出 accessToken）", "ok")

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()

	models, mErr := ch.Models(ctx, &cred)
	if mErr != nil {
		k, _ := errs.KindOf(mErr)
		add("模型目录", "获取失败："+mErr.Error(), "crit")
		writeJSON(w, http.StatusOK, diagResp{
			Verdict: verdictFor(k, true), Kind: string(k), Steps: steps, Upstream: upstreamOf(mErr),
		})
		return
	}
	add("模型目录", fmt.Sprintf("可获取（%d 个模型）", len(models)), "ok")

	// 4) 真实探测：发一次最小对话，看是否收到内容
	model := req.Model
	if model == "" {
		model = firstModelID(models)
	}
	if model == "" {
		add("推理端点", "没有可用模型可探测", "crit")
		writeJSON(w, http.StatusOK, diagResp{Verdict: "本地配置问题：模型目录为空", Kind: string(errs.ModelUnavailable), Steps: steps})
		return
	}
	if s.opts.Health == nil {
		add("推理端点", "健康探测未装配", "crit")
		writeJSON(w, http.StatusOK, diagResp{Verdict: "本地配置问题：探测未装配", Kind: string(errs.Parse), Steps: steps})
		return
	}
	res := s.opts.Health.ProbeOne(ctx, health.Target{Kind: kind, Channel: ch, Account: cred, Model: model})
	if res.OK {
		add("推理端点", fmt.Sprintf("%s 收到内容（首字 %dms）", model, res.TTFTms), "ok")
		writeJSON(w, http.StatusOK, diagResp{Verdict: "正常：本地配置与上游都可用", Kind: "", Steps: steps})
		return
	}
	add("推理端点", res.Reason, "crit")
	if res.Upstream != "" {
		add("上游原话", res.Upstream, "crit")
	}
	writeJSON(w, http.StatusOK, diagResp{
		Verdict: verdictFor(errs.Kind(res.Kind), true), Kind: res.Kind, Steps: steps, Upstream: res.Upstream,
	})
}

type diagStep struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Status string `json:"status"` // ok | warn | crit
}

type diagResp struct {
	Verdict  string     `json:"verdict"`
	Kind     string     `json:"kind,omitempty"`
	Steps    []diagStep `json:"steps"`
	Upstream string     `json:"upstream,omitempty"`
}

// verdictFor 把错误分类翻译成「本地问题 / 上游问题」的结论（F6.3 的核心判断）。
func verdictFor(k errs.Kind, hasAccount bool) string {
	switch k {
	case errs.UpstreamFault:
		return "上游故障（非本地问题）"
	case errs.Transport:
		return "上游连不通（网络或上游不可达，非本地配置问题）"
	case errs.SoftRate:
		return "上游限流（429）：稍后重试，或降低并发"
	case errs.SessionBusy:
		return "上游同一账号的会话启动冲突（瞬态，2 秒级）：网关已自动退避重试；仍失败说明该号正被另一个会话占用 —— 稍后重试或换个账号。不是账号故障，也未计入错误"
	case errs.HardCredit:
		return "账号额度不足：需要充值或换号"
	case errs.Muted:
		return "账号被上游禁言（上游风控处置，非本地配置问题、也非凭证失效）：到解禁时间自动恢复，或换个账号"
	case errs.SessionDead:
		return "凭证已失效：需要重新授权登录"
	case errs.ContentBlocked:
		return "内容被上游拦截：请求内容问题，非配置问题"
	case errs.PromptTooLong:
		return "请求超出上下文窗口：缩短输入或换更大窗口的模型"
	case errs.ModelUnavailable:
		return "该账号无此模型权限：换模型或换号"
	case errs.NoCandidate:
		if hasAccount {
			return "本地配置问题：账号均不可用（冷却或禁用）"
		}
		return "本地配置问题：没有可用账号"
	case errs.AuthFailed:
		return "本地配置问题：凭证不可用"
	default:
		return "无法判定：" + string(k)
	}
}

func statusOf(ok bool) string {
	if ok {
		return "ok"
	}
	return "warn"
}

func firstModelID(models []channel.ModelInfo) string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if len(ids) == 0 {
		return ""
	}
	sort.SliceStable(ids, func(i, j int) bool { return scoreModelID(ids[i]) > scoreModelID(ids[j]) })
	return ids[0]
}

func scoreModelID(id string) int {
	s := 0
	low := strings.ToLower(id)
	if low == "auto" || strings.HasSuffix(low, "/auto") {
		s += 100
	}
	if strings.Contains(low, "max") {
		s += 50
	}
	if strings.Contains(low, "flash") || strings.Contains(low, "mini") {
		s -= 20
	}
	return s
}

// ---------------------------------------------------------------------------
// 日志过滤
// ---------------------------------------------------------------------------

// handleLogsFiltered 返回可按渠道/状态/时间过滤的请求流水（F6.1）。
func (s *Server) handleLogsFiltered(w http.ResponseWriter, r *http.Request) {
	if s.opts.Log == nil {
		writeJSON(w, http.StatusOK, map[string]any{"requests": []any{}, "total": 0})
		return
	}
	q := r.URL.Query()
	chFilter := q.Get("channel")
	statusFilter := q.Get("status")
	since := parseSince(q.Get("range"))

	all := s.opts.Log.List()
	out := []store.RequestRecord{}
	for _, rec := range all {
		if chFilter != "" && chFilter != "all" && rec.Channel != chFilter {
			continue
		}
		switch statusFilter {
		case "error":
			if rec.Status != "error" {
				continue
			}
		case "rotated":
			if !strings.Contains(rec.Note, "换号") {
				continue
			}
		case "", "all":
		}
		if !since.IsZero() && rec.Time.Before(since) {
			continue
		}
		out = append(out, rec)
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out, "total": len(all)})
}

// parseSince 把前端的 range 选项翻成时间下界。
func parseSince(v string) time.Time {
	now := time.Now()
	switch v {
	case "1h":
		return now.Add(-time.Hour)
	case "today":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	case "7d":
		return now.AddDate(0, 0, -7)
	default:
		return time.Time{}
	}
}

// handleLogsClear 清空请求流水（设置屏危险区，需二次确认）。
func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	if s.opts.Log == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": 0})
		return
	}
	n := s.opts.Log.Clear()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": n})
}

// handleLogsExport 导出请求流水为 CSV（日志屏「导出 CSV」）。
func (s *Server) handleLogsExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="poolgate-requests.csv"`)
	// BOM：Excel 打开中文不乱码。
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	_, _ = w.Write([]byte("time,channel,account,model,status,err_kind,duration_ms,ttft_ms,note\n"))
	if s.opts.Log == nil {
		return
	}
	for _, rec := range s.opts.Log.List() {
		fmt.Fprintf(w, "%s,%s,%s,%s,%s,%s,%d,%d,%s\n",
			rec.Time.Format(time.RFC3339), csvCell(rec.Channel), csvCell(rec.Account),
			csvCell(rec.Model), rec.Status, rec.ErrKind, rec.Duration, rec.TTFT, csvCell(rec.Note))
	}
}

// csvCell 转义 CSV 单元格（账号/模型名可能含逗号或引号）。
func csvCell(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// ---------------------------------------------------------------------------
// 设置
// ---------------------------------------------------------------------------

// handleSettings 读写面板设置（F6.4）。
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.opts.Settings == nil {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.Parse, "设置存储未装配"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		st := s.opts.Settings.Get()
		writeJSON(w, http.StatusOK, map[string]any{
			"settings": st,
			"paths": map[string]string{
				"config_dir": s.opts.DataDir,
				"creds_dir":  s.opts.CredsDir,
				"settings":   s.opts.Settings.Path(),
			},
			// detected 是面板按「管理员当前是怎么访问过来的」推断出的对外基址。
			// 客户端要填的地址不该让用户自己拼字符串 —— 拼错一位就是
			// 「连不上但不报错」，这类问题最难查。
			"detected": map[string]string{"base_url": s.detectClientBase(r)},
			// listen_effective 是进程真实绑定的地址。设置里的 listen_host/port
			// 在「启动时已指定 -addr」的场合（飞牛安装包就是这样）不起作用，
			// 所以面板必须把真相摆出来，而不是只显示可编辑的那两个输入框。
			"listen_effective": s.opts.ListenAddr,
			"listen_pinned":    s.opts.ListenPinned,
			"readonly": map[string]string{
				"judge": "收到首个 content / reasoning 增量或 [DONE] 才算通过；空流、错误信封、超时一律判失败并记录上游原话。",
			},
		})
	case http.MethodPost:
		var req store.Settings
		if err := decodeJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
			return
		}
		if err := validateSettings(&req); err != nil {
			writeErrFromErr(w, err)
			return
		}
		if err := s.opts.Settings.Update(func(st *store.Settings) error {
			req.LastBackupAt = st.LastBackupAt
			*st = req
			return nil
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "保存设置失败").WithCause(err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": s.opts.Settings.Get()})
	default:
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 GET / POST"))
	}
}

// detectClientBase 推断客户端应该填的对外基址（如 http://192.0.2.10:5014/v1）。
//
// 端口取**进程实际监听的端口**（Options.ListenAddr），不是设置里的 listen_port ——
// 安装包的启动脚本会显式指定 -addr，这时候设置里的端口根本不生效，用它拼出来的
// 地址是连不上的「看起来对」的地址，比不给还糟。
//
// 主机名取管理员当前访问用的 Host（那台机器就是他眼里的「这台 NAS」）；
// 只有实际监听绑定了具体 IP/域名时才以监听地址为准（那比可被反代改写的 Host
// 头可信）。0.0.0.0 / :: 表示「所有地址」，没有信息量，继续用 Host。
//
// 面板本身可能挂在应用网关的 80 端口后面，客户端不该也走网关 —— 网关那个前缀
// 只服务于面板页面，所以这里一律给直连地址。
func (s *Server) detectClientBase(r *http.Request) string {
	return s.panelBase(r) + "/v1"
}

// panelBase 返回面板自己的对外基址（形如 http://192.0.2.10:5014，不含路径）。
//
// 用途：授权回调要让**用户浏览器**（可能来自手机或别的电脑）访问到，所以给的是
// 面板所在这台机器的地址，而不是请求里的 Host —— 面板可能挂在飞牛应用网关的
// 80 端口后面（带 /app/poolgate 前缀），那个地址只服务于面板页面，回调不能走它。
func (s *Server) panelBase(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		host = h
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "http" || p == "https" {
		scheme = p
	}

	port := 0
	listenHost := ""
	if a := strings.TrimSpace(s.opts.ListenAddr); a != "" {
		if h, pp, err := net.SplitHostPort(a); err == nil {
			listenHost = h
			port, _ = strconv.Atoi(pp)
		}
	}
	if port == 0 && s.opts.Settings != nil {
		port = s.opts.Settings.Get().ListenPort
	}
	if port == 0 {
		port = 5014 // 与 store 的默认值保持一致
	}
	switch listenHost {
	case "", "0.0.0.0", "::", "[::]":
	default:
		host = listenHost
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, host, port)
}

// validateSettings 只挡「明显会让服务起不来」的值；不做风格上的挑剔。
func validateSettings(st *store.Settings) error {
	if st.ListenPort < 1 || st.ListenPort > 65535 {
		return errs.New(errs.Parse, "端口必须在 1–65535 之间")
	}
	if st.MaxRetry < 0 || st.MaxRetry > 10 {
		return errs.New(errs.Parse, "最大换号次数必须在 0–10 之间")
	}
	if st.StickyRequests < 1 || st.StickyRequests > 10000 {
		return errs.New(errs.Parse, "粘性请求数必须在 1–10000 之间")
	}
	if st.ProbeSamples < 1 || st.ProbeSamples > 50 {
		return errs.New(errs.Parse, "每渠道抽样模型数必须在 1–50 之间")
	}
	if st.ProbeTimeoutSec < 5 || st.ProbeTimeoutSec > 600 {
		return errs.New(errs.Parse, "单模型超时必须在 5–600 秒之间")
	}
	// 连续错误门槛（0.9.9 ①）：0 表示用出厂值（5 次）；上限拦住手滑写 100000 的情况。
	if st.ErrThreshold < 0 || st.ErrThreshold > 100 {
		return errs.New(errs.Parse, "连续错误门槛必须在 0–100 之间（0 = 用出厂值 5）")
	}
	if st.ErrCooldownSeconds < 0 || st.ErrCooldownSeconds > 86400 {
		return errs.New(errs.Parse, "门槛冷却必须在 0–86400 秒之间（0 = 用出厂值 600）")
	}
	for _, t := range st.CheckinTimes {
		if _, err := time.Parse("15:04", t); err != nil {
			return errs.New(errs.Parse, fmt.Sprintf("签到时间 %q 不是 HH:MM 格式", t))
		}
	}
	return nil
}

// handleChannelToggle 渠道级启停（设置屏「渠道开关」）。
func (s *Server) handleChannelToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req struct {
		Kind   string `json:"kind"`
		Status string `json:"status"` // active | paused
		Note   string `json:"note"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	if req.Status != string(channel.Active) && req.Status != string(channel.Paused) {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "status 只能是 active 或 paused"))
		return
	}
	if _, ok := registry.GetSpec(channel.Kind(req.Kind)); !ok {
		writeErr(w, http.StatusNotFound, errs.New(errs.Parse, "未注册的渠道 "+req.Kind))
		return
	}
	if s.opts.Settings == nil {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.Parse, "设置存储未装配"))
		return
	}
	if err := s.opts.Settings.SetChannelStatus(req.Kind, req.Status, req.Note); err != nil {
		writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "保存渠道开关失败").WithCause(err))
		return
	}
	if s.opts.ApplyOverrides != nil {
		s.opts.ApplyOverrides()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "kind": req.Kind, "status": req.Status})
}

// ---------------------------------------------------------------------------
// 备份 / 导出
// ---------------------------------------------------------------------------

// handleBackup 导出配置与账号清单（F6.6）。默认**不含凭证**（安全约束 D5）。
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req struct {
		IncludeCreds bool `json:"include_creds"`
	}
	_ = decodeJSON(r, &req)
	// 含凭证的导出在 M4 只做「明确拒绝 + 说明」，不做假承诺：
	// 凭证加密落盘由 CredsStore 负责，导出包格式还没定，先不给出不安全的东西。
	if req.IncludeCreds {
		writeErr(w, http.StatusNotImplemented, errs.New(errs.Parse,
			"含凭证的加密导出尚未实现；账号请用「从 wild-work 导入」或重新授权。当前只支持导出配置。"))
		return
	}
	bundle := map[string]any{
		"version":     s.opts.Version,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"note":        "不含任何凭证（token 永不进入导出）",
	}
	if s.opts.Settings != nil {
		bundle["settings"] = s.opts.Settings.Get()
	}
	if s.opts.Pool != nil {
		accts := []map[string]any{}
		for _, e := range registry.All() {
			for _, st := range s.opts.Pool.List(e.Spec.Kind) {
				accts = append(accts, map[string]any{
					"kind": string(st.Kind), "uid": st.Cred.UID, "nickname": st.Cred.Nickname,
					"credits": st.Credits, "disabled": st.Disabled,
				})
			}
		}
		bundle["accounts"] = accts
	}
	if s.opts.Settings != nil {
		_ = s.opts.Settings.MarkBackup()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="poolgate-config.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(bundle)
}

// handleMigrateFromWildWork 从 wild-work 的 auth 目录导入账号（F6.6）。
func (s *Server) handleMigrateFromWildWork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	var req struct {
		From string `json:"from"`
	}
	_ = decodeJSON(r, &req)
	from := req.From
	if from == "" {
		from = "/vol6/@appconf/wildwork/auth"
	}
	if s.opts.ImportCreds == nil {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.Parse, "导入功能未装配"))
		return
	}
	n, err := s.opts.ImportCreds(from)
	if err != nil {
		// 把底层原因带进 message：面板只显示 message，只写日志的话用户看到的
		// 就是一句没有信息量的「导入失败」。
		writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "导入失败："+err.Error()).WithCause(err))
		return
	}
	// 导入和面板加号一样，顺手把余额问出来：否则刚导进来的一屏「未知」
	// 看着像导入没成功（F1.7）。
	refreshed := 0
	if n > 0 {
		refreshed = s.RefreshAllBalances(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": n, "from": from, "balances": refreshed})
}

// handleDangerReset 危险操作：重置全部配置（设置屏危险区，需二次确认）。
func (s *Server) handleDangerReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	if s.opts.Settings == nil {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.Parse, "设置存储未装配"))
		return
	}
	if err := s.opts.Settings.Update(func(st *store.Settings) error {
		backup := st.LastBackupAt
		*st = store.DefaultSettings()
		st.LastBackupAt = backup
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "重置失败").WithCause(err))
		return
	}
	if s.opts.ApplyOverrides != nil {
		s.opts.ApplyOverrides()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDangerRemoveAccounts 危险操作：移除全部账号（需二次确认）。
func (s *Server) handleDangerRemoveAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	if s.opts.Creds == nil || s.opts.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, errs.New(errs.Parse, "账号存储未装配"))
		return
	}
	n := 0
	for _, e := range registry.All() {
		for _, uid := range s.uidsOf(e.Spec.Kind) {
			ok, err := s.opts.Creds.Delete(e.Spec.Kind, uid)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "删除凭证失败").WithCause(err))
				return
			}
			if ok {
				n++
			}
			s.opts.Pool.Remove(e.Spec.Kind, uid)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n})
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// credFor 取「账号凭证 + 渠道实现」。渠道未实现或账号不存在时返回 false。
//
// 凭证临期会先续期（pool.Fresh）：面板上的「刷新余额」「签到」和启动预热都走这里，
// 不续期的话，token 一过期这几个按钮就永远报 401 —— 用户以为面板坏了。
func (s *Server) credFor(ctx context.Context, kind channel.Kind, uid string) (channel.Credential, channel.Channel, bool) {
	if s.opts.Pool == nil {
		return channel.Credential{}, nil, false
	}
	st, ok := s.opts.Pool.Get(kind, uid)
	if !ok {
		return channel.Credential{}, nil, false
	}
	ch, ok := registry.Get(kind)
	if !ok {
		return channel.Credential{}, nil, false
	}
	return s.opts.Pool.Fresh(ctx, kind, st.Cred), ch, true
}

// uidsOf 返回某渠道的全部账号 UID。
func (s *Server) uidsOf(kind channel.Kind) []string {
	if s.opts.Pool == nil {
		return nil
	}
	return s.opts.Pool.UIDs(kind)
}

// noteCheckin 记录签到结果，供账号屏与设置屏显示（F1.8）。
func (s *Server) noteCheckin(kind, uid string, res channel.CheckinResult) {
	if s.opts.Checkins == nil {
		return
	}
	s.opts.Checkins.Set(kind, uid, res)
}

func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func upstreamOf(err error) string {
	var e *errs.Error
	if errors.As(err, &e) && e.Upstream != "" {
		return e.Upstream
	}
	return err.Error()
}
