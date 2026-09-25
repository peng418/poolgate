// checkin.go TraeWork 每日签到：状态查询 + 领取（与 wild-work internal/traework 同源）。
//
//	POST /trae/api/v2/ug/checkin_credits/status  → {checked_in, credits, enable}
//	POST /trae/api/v2/ug/checkin_credits/claim   → 领取（空 body）
//
// 两个要点（实测）：
//   - 领取后**必须再查一次 status** 才算数：claim 可能返回 9095 这类业务无害码，
//     真正的结论以状态查询为准。
//   - code=9074 是限流，等一会儿重试一次即可，不是失败。
package traework

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 签到端点。
const (
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
)

// checkinRetryDelay 是 9074 限流后的重试等待（与 wild-work 一致）。
const checkinRetryDelay = 8 * time.Second

// Checkin 实现 channel.Channel：查状态 → 未签到则领取 → 复查确认。
func (a *Adapter) Checkin(ctx context.Context, c *channel.Credential) (channel.CheckinResult, error) {
	checked, credits, enable, err := a.checkinStatus(ctx, c)
	if err != nil {
		return channel.CheckinResult{}, err
	}
	// enable=false：该渠道当前没开启签到活动 → 如实标注「无活动」，不报失败。
	if !enable {
		return channel.CheckinResult{OK: true, NoActivity: true, Message: "该渠道当前未开启签到活动"}, nil
	}
	if checked {
		return channel.CheckinResult{OK: true, Message: "已签到", Reward: rewardOf(credits)}, nil
	}

	if err := a.checkinClaim(ctx, c); err != nil {
		return channel.CheckinResult{}, err
	}
	// 复查：以状态查询为权威结论（claim 的响应码可能只是业务提示）。
	checked2, credits2, _, err := a.checkinStatus(ctx, c)
	if err != nil {
		return channel.CheckinResult{}, err
	}
	if !checked2 {
		return channel.CheckinResult{}, errs.New(errs.UpstreamFault, "签到请求已发出但状态未变为已签到").
			WithChannel(string(channel.TraeWork)).WithAccount(c.UID)
	}
	return channel.CheckinResult{OK: true, Message: "签到成功", Reward: rewardOf(credits2)}, nil
}

func rewardOf(credits int64) string {
	if credits <= 0 {
		return ""
	}
	return fmt.Sprintf("+%d Credits", credits)
}

// checkinStatus 查签到状态。
func (a *Adapter) checkinStatus(ctx context.Context, c *channel.Credential) (checked bool, credits int64, enable bool, err error) {
	raw, err := a.ugPost(ctx, EpCheckinStatus, c, []byte("{}"))
	if err != nil {
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool   `json:"checked_in"`
		Credits   int64  `json:"credits"`
		Enable    bool   `json:"enable"`
		Code      int    `json:"code"`
		Message   string `json:"message"`
		Msg       string `json:"msg"`
		Success   *bool  `json:"success"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return false, 0, false, errs.New(errs.Parse, "签到状态响应无法解析").
			WithChannel(string(channel.TraeWork)).WithAccount(c.UID).
			WithUpstream(truncate(string(raw), 200))
	}
	if resp.Code != 0 {
		return false, 0, false, errs.New(classifyBizCode(resp.Code), "签到状态查询失败").
			WithChannel(string(channel.TraeWork)).WithAccount(c.UID).
			WithUpstream(checkinMsg(resp.Message, resp.Msg))
	}
	if resp.Success != nil && !*resp.Success {
		return false, 0, false, errs.New(errs.UpstreamFault, "签到状态查询被拒").
			WithChannel(string(channel.TraeWork)).WithAccount(c.UID).
			WithUpstream(checkinMsg(resp.Message, resp.Msg))
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// checkinClaim 发起领取；9074 限流等一会儿重试一次。
func (a *Adapter) checkinClaim(ctx context.Context, c *channel.Credential) error {
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := a.ugPost(ctx, EpCheckinClaim, c, []byte("{}"))
		if err != nil {
			return err
		}
		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Msg     string `json:"msg"`
			Success *bool  `json:"success"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return errs.New(errs.Parse, "签到响应无法解析").
				WithChannel(string(channel.TraeWork)).WithAccount(c.UID).
				WithUpstream(truncate(string(raw), 200))
		}
		if resp.Code == 9074 && attempt == 0 {
			// 限流：等一会儿再来一次，不当失败。
			select {
			case <-time.After(checkinRetryDelay):
			case <-ctx.Done():
				return errs.New(errs.Transport, "签到重试被取消").WithCause(ctx.Err())
			}
			continue
		}
		if resp.Code != 0 {
			return errs.New(classifyBizCode(resp.Code), "签到领取失败").
				WithChannel(string(channel.TraeWork)).WithAccount(c.UID).
				WithUpstream(checkinMsg(resp.Message, resp.Msg))
		}
		if resp.Success != nil && !*resp.Success {
			return errs.New(errs.UpstreamFault, "签到领取被拒").
				WithChannel(string(channel.TraeWork)).WithAccount(c.UID).
				WithUpstream(checkinMsg(resp.Message, resp.Msg))
		}
		return nil // 结论由复查状态决定
	}
	return errs.New(errs.SoftRate, "签到被限流（code=9074），请稍后再试").
		WithChannel(string(channel.TraeWork)).WithAccount(c.UID)
}

// ugPost 向 ug 域发一个 JSON POST。
func (a *Adapter) ugPost(ctx context.Context, path string, c *channel.Credential, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.ug+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, errs.New(errs.Transport, "构造签到请求失败").WithCause(err)
	}
	ugHeaders(req, c)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "签到请求失败").WithCause(err).
			WithChannel(string(channel.TraeWork)).WithAccount(c.UID)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, errs.New(a.Classify(resp.StatusCode, raw), "签到请求被上游拒绝").
			WithChannel(string(channel.TraeWork)).WithAccount(c.UID).
			WithUpstream(truncate(string(raw), 200))
	}
	return raw, nil
}

// classifyBizCode 把上游业务码归一到错误分类。
func classifyBizCode(code int) errs.Kind {
	switch code {
	case 9074:
		return errs.SoftRate // 限流
	case 401, 403:
		return errs.SessionDead
	default:
		return errs.UpstreamFault
	}
}

func checkinMsg(msgs ...string) string {
	for _, m := range msgs {
		if strings.TrimSpace(m) != "" {
			return m
		}
	}
	return ""
}
