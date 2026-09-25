package deepseek

// login.go 「登录」= 你在自己的浏览器里登录 DeepSeek，然后把 localStorage 里的 userToken 粘回面板。
//
// 为什么不能做成扫码：登录接口的 `device_id` 必须来自**真实浏览器**里的数美 SDK 指纹
// （空串/随机值会被判 `RISK_DEVICE_DETECTED`），服务端复现不了；而参考实现那条
// 「存邮箱密码自动登录」的路我们不抄 —— 存密码的风险远大于多一点操作。
//
// 所以这里实现 channel.CallbackAcceptor（控制台已有通用的「粘贴」通道），
// 用户粘一次 userToken，我们落盘 token + 派生的设备 id；**不存任何密码**。

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const loginPage = "https://chat.deepseek.com"

type session struct {
	a  *Adapter
	mu sync.Mutex
	// token 是用户粘回来的 userToken；deviceID 由它派生（每账号一个，绝不共用）。
	token    string
	deviceID string
	done     bool
}

// StartLogin 返回「去这儿登录」的地址（面板会显示并可画成二维码）；
// 真正的凭证靠下一步粘贴回来 —— 控制台对 CallbackAcceptor 渠道会自动显示粘贴入口。
func (a *Adapter) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	return &session{a: a}, nil
}

func (s *session) AuthURL() string { return loginPage }

// Hint 是给面板的粘贴引导语：告诉用户去哪拿 token、粘到哪。
// 实现这个可选接口后，控制台在 /api/login/start 的响应里带上 paste_hint，
// 面板据此切换成「粘贴」形态（不画二维码、不提示扫码）。
func (s *session) Hint() string {
	return "在浏览器登录 DeepSeek 后，于控制台执行 JSON.parse(localStorage.getItem(\"userToken\")).value，把结果整段粘到面板的输入框里"
}

// AcceptCallback 收用户粘回来的 userToken（裸串 / 带引号 / 整段 JSON 都认）。
func (s *session) AcceptCallback(raw string) error {
	tok := extractUserToken(raw)
	if tok == "" {
		return errs.New(errs.Parse,
			"没识别出 userToken：请在浏览器登录 DeepSeek 后，于控制台执行 "+
				"`JSON.parse(localStorage.getItem(\"userToken\")).value`，把结果整段粘过来").
			WithChannel(string(channel.DeepSeek))
	}
	s.mu.Lock()
	s.token = tok
	s.deviceID = deriveDeviceID(tok)
	s.mu.Unlock()
	return nil
}

// Poll 在用户粘了 token 之后完成凭证组装（并做一次轻量的有效性检查）。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	tok, dev, done := s.token, s.deviceID, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.DeepSeek))
	}
	if tok == "" {
		return nil, channel.ErrPending // 还没粘：正常的等待态
	}
	cred := &channel.Credential{
		UID:         "deepseek-" + shortHash(tok),
		Nickname:    "DeepSeek 账号",
		AccessToken: tok,
		Extra:       map[string]string{"device_id": dev},
	}
	// 用 check_device 做一次「这串 token 还能用吗」的确认：不能就当场报错，
	// 免得把一个已失效的凭证塞进池子（那会变成「装上就报 401」）。
	if err := s.a.checkToken(ctx, cred); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	return cred, nil
}

func (s *session) Cancel() {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
}

// checkToken 用 check_device 验证 token（也是唯一可能轮换 token 的地方）。
func (a *Adapter) checkToken(ctx context.Context, c *channel.Credential) error {
	body, _ := json.Marshal(map[string]any{"device_id": c.Extra["device_id"], "device_model": deviceModel})
	resp, err := a.do(ctx, c, http.MethodPost, a.base+"/users/auth_token/check_device", body, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Data struct {
			BizData struct {
				Rotate struct {
					Token string `json:"token"`
				} `json:"rotate"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// 解析不了不等于 token 坏：保守放行，交给第一次真正调用去暴露问题。
		return nil
	}
	if tok := strings.TrimSpace(out.Data.BizData.Rotate.Token); tok != "" {
		c.AccessToken = tok // 上游轮换就换成新的
	}
	return nil
}

// Refresh 尝试续期：网页端只在 check_device 返回 rotate 时才换 token（实测几乎恒为空），
// 所以拿不到轮换时返回 (nil, nil) —— 明确表示「刷不了」，让上层按 401 处理，而不是假装成功。
func (a *Adapter) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	if c == nil {
		return nil, nil
	}
	before := c.AccessToken
	nc := *c
	if err := a.checkToken(ctx, &nc); err != nil {
		return nil, err
	}
	if nc.AccessToken == before {
		return nil, nil
	}
	return &nc, nil
}

// extractUserToken 从粘贴内容里抽 token：裸串 / "带引号" / {"value":"…"} 都认。
func extractUserToken(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "{") {
		var obj struct {
			Value string `json:"value"`
			Token string `json:"token"`
		}
		if json.Unmarshal([]byte(s), &obj) == nil {
			if v := firstNonEmpty(obj.Value, obj.Token); v != "" {
				s = v
			}
		}
	}
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	s = strings.TrimSpace(s)
	// userToken 是一串较长的 JWT（三段点分）或类似长度的不透明串；挡掉明显误粘的内容
	if len(s) < 20 || strings.ContainsAny(s, " \t\n") {
		return ""
	}
	return s
}

// deriveDeviceID 由 token 派生一个**稳定**的 RFC4122 v4 设备 UUID（FNV-1a 双哈希）。
//
// 为什么要派生而不是随机：设备 id 是上游用来区分「同一个客户端」的，每账号必须固定且互不相同
// （共用设备 id 会被判异常）；由 token 派生保证「同一个账号重登后还是同一个 id」，而不同账号天然不同。
func deriveDeviceID(seed string) string {
	h1 := fnv.New64a()
	_, _ = h1.Write([]byte(seed))
	h2 := fnv.New64a()
	_, _ = h2.Write([]byte(seed + "#deepseek-device"))
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], h1.Sum64())
	binary.BigEndian.PutUint64(b[8:16], h2.Sum64())
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// shortHash 给账号一个稳定短标识（面板显示用，不泄露 token）。
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}
