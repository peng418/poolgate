// checkin.go QoderCN 每日签到：双路径实现（与 wild-work internal/qodercn/checkin.go 同源）。
//
//	路径 A：GET  /sash/api/v1/me/daily-check-in/status → POST .../claim
//	路径 B：GET  /sash/api/v1/me/campaigns            → POST .../campaigns/{id}/claim
//
// 2026-09 实测：路径 A 的 legacy 系统已全局 DISABLED（status 恒 DISABLED、streak 恒 0），
// 当前生效的是路径 B（活动 key 每日变化，不可硬编码）。故 **B 优先、A 兜底**。
//
// 签到认证只认 Bearer dt- + cosy-clienttype:10（桌面端标识），不需要 COSY 签名 ——
// 与推理链路用的 clienttype 5 不可混用。
//
// 幂等：已签到（409 / CLAIMED / replayed）与「无活动」都算成功，不报错。
package qodercn

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

// 签到端点。
const (
	EpCampaigns = "/sash/api/v1/me/campaigns"
	EpCheckinSt = "/sash/api/v1/me/daily-check-in/status"
	EpCheckinCl = "/sash/api/v1/me/daily-check-in/claim"
)

// checkinResult 单次签到的归一结果。
type checkinResult struct {
	OK      bool   // 视为成功（含「已签到」「无活动」）
	Msg     string // 展示文案
	Claimed bool   // 本次是否新领取
	Amount  int64  // 领取到的积分
}

// Checkin 实现 channel.Channel：先走 campaigns，失败回退 legacy daily-check-in。
func (a *Adapter) Checkin(ctx context.Context, c *channel.Credential) (channel.CheckinResult, error) {
	// 路径 B：campaigns（实测主路径）
	res, handled, err := a.tryCampaigns(ctx, c)
	if handled {
		if err != nil {
			return channel.CheckinResult{}, err
		}
		return toChannelResult(res), nil
	}
	// 路径 A：legacy daily-check-in 兜底
	res, handled, err = a.tryDailyCheckin(ctx, c)
	if handled {
		if err != nil {
			return channel.CheckinResult{}, err
		}
		return toChannelResult(res), nil
	}
	// 两条路都不可用：如实报错，不假装「无活动」。
	return channel.CheckinResult{}, errs.New(errs.UpstreamFault, "签到失败：两条签到路径都不可用").
		WithChannel(string(channel.QoderCN)).WithUpstream(truncate(err.Error(), 200))
}

func toChannelResult(r checkinResult) channel.CheckinResult {
	out := channel.CheckinResult{OK: r.OK, Message: r.Msg}
	if r.Claimed && r.Amount > 0 {
		out.Reward = fmt.Sprintf("+%d Credits", r.Amount)
	}
	return out
}

// doCheckin 发一个签到请求（带桌面端头族）。reqBody 为 nil 时发空 body。
func (a *Adapter) doCheckin(ctx context.Context, method, path string, c *channel.Credential, reqBody any) (int, []byte, error) {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return 0, nil, err
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return 0, nil, err
	}
	// ★ cosy-clienttype 必须为 10（桌面端）；推理链路用的是 5。
	req.Header.Set("authorization", "Bearer "+c.AccessToken)
	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("user-agent", "Qoder")
	req.Header.Set("cosy-clienttype", "10")
	if method == http.MethodPost {
		req.Header.Set("origin", a.base)
		if reqBody == nil {
			req.ContentLength = 0
		}
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, nil, errs.New(errs.Transport, "签到请求失败").WithCause(err).WithAccount(c.UID)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// tryCampaigns 路径 B：查活动 → 领取 CLAIMABLE 的 CLAIM_BENEFIT。
// handled=false 表示该路径不可用（可回退到 A）。
func (a *Adapter) tryCampaigns(ctx context.Context, c *channel.Credential) (checkinResult, bool, error) {
	status, raw, err := a.doCheckin(ctx, http.MethodGet, EpCampaigns, c, nil)
	if err != nil {
		return checkinResult{}, false, err
	}
	if status == http.StatusUnauthorized {
		return checkinResult{}, true, errs.New(errs.SessionDead, "签到鉴权失败（凭证已失效）").
			WithAccount(c.UID).WithUpstream(truncate(string(raw), 120))
	}
	if status != http.StatusOK {
		return checkinResult{}, false, nil
	}
	var list struct {
		Campaigns []struct {
			CampaignID  string `json:"campaignId"`
			ActionType  string `json:"actionType"`
			ClaimStatus string `json:"claimStatus"`
		} `json:"campaigns"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return checkinResult{}, false, nil
	}
	var targetID string
	already := false
	for _, cp := range list.Campaigns {
		if cp.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		switch cp.ClaimStatus {
		case "CLAIMABLE":
			targetID = cp.CampaignID
		case "CLAIMED":
			already = true
		}
	}
	if targetID == "" {
		if already {
			return checkinResult{OK: true, Msg: "已签到"}, true, nil
		}
		return checkinResult{OK: true, Msg: "无可用签到活动"}, true, nil
	}

	status, raw, err = a.doCheckin(ctx, http.MethodPost, EpCampaigns+"/"+targetID+"/claim", c, nil)
	if err != nil {
		return checkinResult{}, true, err
	}
	if status == http.StatusConflict { // 409 幂等：今日已领
		return checkinResult{OK: true, Msg: "已签到"}, true, nil
	}
	if status == http.StatusUnauthorized {
		return checkinResult{}, true, errs.New(errs.SessionDead, "签到鉴权失败（凭证已失效）").
			WithAccount(c.UID).WithUpstream(truncate(string(raw), 120))
	}
	if status != http.StatusOK {
		return checkinResult{}, true, errs.New(errs.UpstreamFault, fmt.Sprintf("签到领取失败 HTTP %d", status)).
			WithAccount(c.UID).WithUpstream(truncate(string(raw), 150))
	}
	var claim struct {
		Status   string `json:"status"`
		Replayed bool   `json:"replayed"`
		Benefit  *struct {
			Amount int64 `json:"amount"`
		} `json:"benefit"`
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		return checkinResult{}, true, errs.New(errs.Parse, "签到响应无法解析").
			WithAccount(c.UID).WithUpstream(truncate(string(raw), 150))
	}
	if claim.Status == "CLAIMED" {
		if claim.Replayed {
			return checkinResult{OK: true, Msg: "已签到"}, true, nil
		}
		var amount int64
		if claim.Benefit != nil {
			amount = claim.Benefit.Amount
		}
		return checkinResult{OK: true, Claimed: true, Amount: amount,
			Msg: fmt.Sprintf("签到成功 +%d", amount)}, true, nil
	}
	return checkinResult{}, true, errs.New(errs.UpstreamFault, "签到返回未知状态 "+claim.Status).
		WithAccount(c.UID).WithUpstream(truncate(string(raw), 150))
}

// tryDailyCheckin 路径 A：legacy daily-check-in（实测已 DISABLED，作兜底）。
func (a *Adapter) tryDailyCheckin(ctx context.Context, c *channel.Credential) (checkinResult, bool, error) {
	status, raw, err := a.doCheckin(ctx, http.MethodGet, EpCheckinSt, c, nil)
	if err != nil {
		return checkinResult{}, false, err
	}
	// 端点不存在 → 回退
	if status == http.StatusNotFound || status == http.StatusNotImplemented {
		return checkinResult{}, false, nil
	}
	if status == http.StatusUnauthorized {
		return checkinResult{}, true, errs.New(errs.SessionDead, "签到鉴权失败（凭证已失效）").
			WithAccount(c.UID).WithUpstream(truncate(string(raw), 120))
	}
	if status != http.StatusOK {
		return checkinResult{}, false, nil
	}
	var st struct {
		Status        string `json:"status"` // CLAIMABLE | CLAIMED | DISABLED
		RewardCredits int64  `json:"rewardCredits"`
	}
	if err := json.Unmarshal(raw, &st); err != nil || st.Status == "" {
		return checkinResult{}, false, nil
	}
	if st.Status == "DISABLED" {
		// legacy 全局停用：按「无活动」成功处理（对用户透明）。
		return checkinResult{OK: true, Msg: "无可用签到活动"}, true, nil
	}

	status, raw, err = a.doCheckin(ctx, http.MethodPost, EpCheckinCl, c, map[string]any{})
	if err != nil {
		return checkinResult{}, true, err
	}
	if status == http.StatusConflict {
		return checkinResult{OK: true, Msg: "已签到"}, true, nil
	}
	if status == http.StatusUnauthorized {
		return checkinResult{}, true, errs.New(errs.SessionDead, "签到鉴权失败（凭证已失效）").
			WithAccount(c.UID).WithUpstream(truncate(string(raw), 120))
	}
	if status == http.StatusOK {
		var claim struct {
			Success       bool  `json:"success"`
			RewardCredits int64 `json:"rewardCredits"`
		}
		_ = json.Unmarshal(raw, &claim)
		amount := claim.RewardCredits
		if amount == 0 {
			amount = st.RewardCredits
		}
		return checkinResult{OK: true, Claimed: true, Amount: amount,
			Msg: fmt.Sprintf("签到成功 +%d", amount)}, true, nil
	}
	return checkinResult{}, true, errs.New(errs.UpstreamFault, fmt.Sprintf("签到领取失败 HTTP %d", status)).
		WithAccount(c.UID).WithUpstream(truncate(string(raw), 150))
}
