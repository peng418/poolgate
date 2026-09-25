package antigravity

// project.go 第三段授权：**开通**（loadCodeAssist → 必要时 onboardUser）。
//
// 这一段最容易漏，也最容易出事：只换到令牌不开通，调对话会 412 / 403。
// Google 要求先把这个「项目 + 档位」定下来，v1internal 的对话接口才认。
//
// 流程（照参考实现与官方 CLI 的实测结论）：
//  1. POST /v1internal:loadCodeAssist 拿 cloudaicompanionProject；
//     已经有现成项目就直接用（老账号、企业号多半走这条）；
//  2. 没有就 POST /v1internal:onboardUser 开一个，这是个**长任务**（done 不为真就轮询）；
//  3. **free tier 不能带 project**（带了会 412/403 Precondition Failed）——
//     本实现里 onboardUser 的请求体只带 {tierId, metadata}，从不带 project，
//     从构造上就不会踩这个坑。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// onboardPollMax / onboardPollEvery 是长任务的轮询上限与间隔。
// 上限 ~30 秒：足够绝大多数账号开完，又不会把面板的「等待授权」卡死。
const (
	onboardPollMax   = 6
	onboardPollEvery = 5 * time.Second
)

// loadResp 是 loadCodeAssist 的响应。
//
// cloudaicompanionProject 上游给过两种形态（裸字符串 / {"id":…}），
// 用 RawMessage 接住两种，别只认一种 —— 认错了表现是「明明有项目却当没有，又去开一个」。
type loadResp struct {
	CurrentTier             map[string]any   `json:"currentTier"`
	AllowedTiers            []tier           `json:"allowedTiers"`
	CloudAICompanionProject json.RawMessage  `json:"cloudaicompanionProject"`
	IneligibleTiers         []ineligibleTier `json:"ineligibleTiers"`
}

type tier struct {
	ID        string `json:"id"`
	IsDefault bool   `json:"isDefault"`
}

type ineligibleTier struct {
	ReasonCode    string `json:"reasonCode"`
	ReasonMessage string `json:"reasonMessage"`
}

func (l loadResp) projectID() string { return projectIDFromRaw(l.CloudAICompanionProject) }

// defaultTierID 选默认档位：优先 isDefault，其次第一个，都没有才用兜底常量。
func (l loadResp) defaultTierID() string {
	for _, t := range l.AllowedTiers {
		if t.IsDefault && strings.TrimSpace(t.ID) != "" {
			return strings.TrimSpace(t.ID)
		}
	}
	for _, t := range l.AllowedTiers {
		if strings.TrimSpace(t.ID) != "" {
			return strings.TrimSpace(t.ID)
		}
	}
	return defaultTier
}

// projectIDFromRaw 从 string 或 {"id":…} 里取项目 id。
func projectIDFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.ID)
	}
	return ""
}

// codeAssistMetadata 是开通接口要的元数据。
//
// platform 用该账号指纹里的那个（与请求头保持一致）：同账号在开通阶段说 MACOS、
// 对话阶段又说 WINDOWS，是明显的不一致。
func codeAssistMetadata(seed string) map[string]any {
	return map[string]any{
		"ideType":    ideType,
		"platform":   fingerprintOf(seed).metaPlatform,
		"pluginType": pluginType,
	}
}

// ensureProject 跑开通流程，返回可用的 project id。
//
// token 由调用方传入而不是从 c 里取：调用方可能刚刷过令牌（usableToken），
// 而凭证对象本身拿不到写权限 —— 用 c.AccessToken 会拿到过期的那个。
func (a *Adapter) ensureProject(ctx context.Context, c *channel.Credential, token string) (string, error) {
	if c == nil {
		return "", errs.New(errs.SessionDead, "凭证为空：无法开通").WithChannel(string(channel.Antigravity))
	}
	if p := projectOf(c); p != "" {
		return p, nil
	}

	loaded, base, err := a.loadCodeAssist(ctx, c, token)
	if err != nil {
		return "", err
	}
	if p := loaded.projectID(); p != "" {
		return p, nil
	}
	// 不满足条件时上游会给出枚举原因（地区 / 年龄 / 需验证）——原样透给用户，别吞。
	if len(loaded.IneligibleTiers) > 0 {
		t := loaded.IneligibleTiers[0]
		reason := strings.TrimSpace(t.ReasonCode)
		msg := strings.TrimSpace(t.ReasonMessage)
		if reason != "" || msg != "" {
			if msg == "" {
				msg = "上游没有给出更多说明"
			}
			return "", errs.New(errs.AuthFailed,
				"这个 Google 账号暂时不可用（"+firstNonEmpty(reason, "INELIGIBLE")+"："+msg+"）").
				WithChannel(string(channel.Antigravity)).WithAccount(c.UID)
		}
	}

	tier := loaded.defaultTierID()
	// 开通优先用刚才 loadCodeAssist 成功的那台，其次才是候选列表。
	bases := preferBase(a.loadBases, base)
	project, err := a.onboard(ctx, c, token, bases, tier)
	if err != nil {
		return "", err
	}
	return project, nil
}

// preferBase 把 base 提到列表最前（去重）。base 为空时原样返回。
func preferBase(bases []string, base string) []string {
	if strings.TrimSpace(base) == "" {
		return bases
	}
	out := []string{base}
	for _, b := range bases {
		if b != base {
			out = append(out, b)
		}
	}
	return out
}

// loadCodeAssist 问上游「这个账号有没有现成项目」。
func (a *Adapter) loadCodeAssist(ctx context.Context, c *channel.Credential, token string) (loadResp, string, error) {
	body, _ := json.Marshal(map[string]any{"metadata": codeAssistMetadata(seedOf(c))})
	var out loadResp
	status, raw, base, err := a.callJSON(ctx, c, token, a.loadBases, "/v1internal:loadCodeAssist", body, &out)
	if err != nil {
		return out, "", err
	}
	if status >= 400 {
		return out, "", errs.New(a.Classify(status, raw), "开通查询失败（loadCodeAssist）").
			WithChannel(string(channel.Antigravity)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	return out, base, nil
}

// onboard 调 onboardUser 开项目，长任务没做完就轮询。
func (a *Adapter) onboard(ctx context.Context, c *channel.Credential, token string, bases []string, tier string) (string, error) {
	payload := map[string]any{
		"tierId":   tier,
		"metadata": codeAssistMetadata(seedOf(c)),
	}
	// 关键约束：请求体里**没有** cloudaicompanionProject。free tier 带了会被上游拒绝。
	body, _ := json.Marshal(payload)

	var lro struct {
		Name     string `json:"name"`
		Done     bool   `json:"done"`
		Response struct {
			CloudAICompanionProject struct {
				ID string `json:"id"`
			} `json:"cloudaicompanionProject"`
		} `json:"response"`
	}
	status, raw, base, err := a.callJSON(ctx, c, token, bases, "/v1internal:onboardUser", body, &lro)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", errs.New(a.Classify(status, raw), "开通失败（onboardUser）").
			WithChannel(string(channel.Antigravity)).WithAccount(c.UID).WithUpstream(truncate(string(raw), 200))
	}
	// 长任务：没 done 就轮询（用同一台基址，别在轮询中途换基址导致查不到任务）。
	if basesForPoll := preferBase(bases, base); len(basesForPoll) > 0 {
		base = basesForPoll[0]
	}
	for i := 0; !lro.Done && i < onboardPollMax && strings.TrimSpace(lro.Name) != ""; i++ {
		select {
		case <-ctx.Done():
			return "", errs.New(errs.Transport, "等待开通超时").WithChannel(string(channel.Antigravity)).WithCause(ctx.Err())
		case <-time.After(onboardPollEvery):
		}
		var next struct {
			Done     bool `json:"done"`
			Response struct {
				CloudAICompanionProject struct {
					ID string `json:"id"`
				} `json:"cloudaicompanionProject"`
			} `json:"response"`
		}
		st, raw2, _, err := a.callJSON(ctx, c, token, []string{base}, "/v1internal/"+lro.Name, nil, &next)
		if err != nil {
			return "", err
		}
		if st >= 400 {
			return "", errs.New(a.Classify(st, raw2), "开通轮询失败").
				WithChannel(string(channel.Antigravity)).WithAccount(c.UID).WithUpstream(truncate(string(raw2), 200))
		}
		lro.Done = next.Done
		lro.Response = next.Response
	}
	if id := strings.TrimSpace(lro.Response.CloudAICompanionProject.ID); id != "" {
		return id, nil
	}
	return "", errs.New(errs.AuthFailed,
		"开通没拿到项目 id（可能这个账号需要先在浏览器里完成验证，或地区不支持）").
		WithChannel(string(channel.Antigravity)).WithAccount(c.UID)
}

// callJSON 发一个 JSON 请求并按需解码。body 为 nil 时发 GET。
//
// 回落判据与对话一致：只在**连不上**或 **5xx** 时换下一台；4xx 是上游的明确答案，直接返回。
// 返回实际使用的基址，便于后续调用钉在同一台上（长任务轮询必须）。
func (a *Adapter) callJSON(ctx context.Context, c *channel.Credential, token string, bases []string, path string, body []byte, out any) (int, []byte, string, error) {
	if len(bases) == 0 {
		bases = []string{epProd}
	}
	fp := fingerprintOf(seedOf(c))
	// 开通类请求带齐指纹（参考实现 loadManagedProject / onboardManagedProject 就是这么发的）。
	hdrs := map[string]string{
		"Authorization":     "Bearer " + strings.TrimSpace(token),
		"Content-Type":      "application/json",
		"User-Agent":        "google-api-nodejs-client/9.15.1",
		"X-Goog-Api-Client": fp.apiClient,
		"Client-Metadata":   `{"ideType":"` + ideType + `","platform":"` + fp.metaPlatform + `","pluginType":"` + pluginType + `"}`,
	}

	method := http.MethodPost
	if body == nil {
		method = http.MethodGet
	}

	var lastErr error
	for i, base := range bases {
		resp, err := a.doOnce(ctx, c, method, base+path, body, hdrs)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		status := resp.StatusCode
		resp.Body.Close()
		if status >= 500 && i < len(bases)-1 {
			lastErr = errors.New("上游 HTTP " + itoa(status))
			continue
		}
		if out != nil && status < 400 && len(raw) > 0 {
			if jerr := json.Unmarshal(raw, out); jerr != nil {
				return status, raw, base, errs.New(errs.Parse, "上游响应无法解析").
					WithChannel(string(channel.Antigravity)).WithUpstream(truncate(string(raw), 200))
			}
		}
		return status, raw, base, nil
	}
	return 0, nil, "", errs.New(errs.Transport, "上游不可达").
		WithChannel(string(channel.Antigravity)).WithAccount(uidOf(c)).WithCause(lastErr)
}
