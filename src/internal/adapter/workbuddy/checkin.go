// checkin.go WorkBuddyCN 每日签到（与 wild-work internal/upstream 同源）。
//
//	POST /v2/billing/meter/checkin-activity-status  → {active, today_checked_in, daily_credit}
//	POST /v2/billing/meter/daily-checkin            → 领取
//
// 先探活动状态再决定要不要领：`active=false` 表示该渠道当前没有签到活动，
// 这时如实标注「无活动」，而不是报一个假的签到成功（红线一）。
// 已签到由 `today_checked_in` 判定，不靠错误码猜。
package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 签到端点。
const (
	EpCheckinActivity = "/v2/billing/meter/checkin-activity-status"
	EpDailyCheckin    = "/v2/billing/meter/daily-checkin"
)

// Checkin 实现 channel.Channel：探活动 → 未签到则领取。
func (a *Adapter) Checkin(ctx context.Context, c *channel.Credential) (channel.CheckinResult, error) {
	active, checkedIn, daily, err := a.checkinActivity(ctx, c)
	if err != nil {
		return channel.CheckinResult{}, err
	}
	if !active {
		return channel.CheckinResult{OK: true, NoActivity: true, Message: "该渠道当前未开启签到活动"}, nil
	}
	if checkedIn {
		return channel.CheckinResult{OK: true, Message: "已签到", Reward: rewardOf(daily)}, nil
	}

	raw, err := a.billingPost(ctx, EpDailyCheckin, c, []byte("{}"))
	if err != nil {
		return channel.CheckinResult{}, err
	}
	// 领取成功与否以响应码为准；上游对「今天已签到」也可能返回业务码，
	// 这里复查一次状态，避免把「已签到」误报成失败。
	if len(bytes.TrimSpace(raw)) > 0 {
		var resp struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(raw, &resp) == nil && resp.Code != 0 {
			// 业务码非 0：可能是「今天已签到」，复查确认。
			_, checked2, daily2, err2 := a.checkinActivity(ctx, c)
			if err2 == nil && checked2 {
				return channel.CheckinResult{OK: true, Message: "已签到", Reward: rewardOf(daily2)}, nil
			}
			return channel.CheckinResult{}, errs.New(errs.UpstreamFault, "签到领取失败").
				WithChannel(string(channel.WorkBuddyCN)).WithAccount(c.UID).
				WithUpstream(resp.Msg)
		}
	}
	return channel.CheckinResult{OK: true, Message: "签到成功", Reward: rewardOf(daily)}, nil
}

func rewardOf(credits int) string {
	if credits <= 0 {
		return ""
	}
	return "+" + itoa(credits) + " Credits"
}

// checkinActivity 探签到活动状态。
func (a *Adapter) checkinActivity(ctx context.Context, c *channel.Credential) (active, checkedIn bool, daily int, err error) {
	raw, err := a.billingPost(ctx, EpCheckinActivity, c, []byte("{}"))
	if err != nil {
		return false, false, 0, err
	}
	var st struct {
		Active         bool `json:"active"`
		TodayCheckedIn bool `json:"today_checked_in"`
		DailyCredit    int  `json:"daily_credit"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return false, false, 0, errs.New(errs.Parse, "签到活动状态无法解析").
			WithChannel(string(channel.WorkBuddyCN)).WithAccount(c.UID).
			WithUpstream(truncate(string(raw), 200))
	}
	return st.Active, st.TodayCheckedIn, st.DailyCredit, nil
}

// billingPost 向 billing 端点发一个 JSON POST。
func (a *Adapter) billingPost(ctx context.Context, path string, c *channel.Credential, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造签到请求失败").WithCause(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", c.UID)
	req.Header.Set("X-Domain", DomainCN)
	req.Header.Set("User-Agent", clientUA)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "签到请求失败").WithCause(err).
			WithChannel(string(channel.WorkBuddyCN)).WithAccount(c.UID)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, errs.New(Classify(resp.StatusCode, string(raw)), "签到请求被上游拒绝").
			WithChannel(string(channel.WorkBuddyCN)).WithAccount(c.UID).
			WithUpstream(truncate(string(raw), 200))
	}
	return raw, nil
}

// itoa 避免为一个小转换引入 strconv 依赖到本文件顶部（保持 import 精简）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
