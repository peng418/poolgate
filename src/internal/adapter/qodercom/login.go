// login.go QoderCOM 的面板授权：OAuth 设备流（PKCE + 轮询）。
//
// 与 QoderCN 同一套 COSY 框架、同一个公共 client_id（qoder2api 双区同码已验证），
// 只有授权页域名不同（qoder.com）。流程：
//
//	授权页（nonce + S256 challenge + client_id）→ 用户点授权
//	→ 本服务轮询 openapi.qoder.sh/api/v1/deviceToken/poll
//	→ 上游用 HTTP 404/202 表示「还没授权」，200 + token 表示完成
//
// 状态随 LoginSession 存在内存里 —— 授权是 5 分钟内的一次性交互。
package qodercom

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 授权相关端点与常量。
const (
	OAuthWebsite = "https://qoder.com"
	// OAuthClientID 是 qoder2api 用的公共客户端 ID（CN/国际版共用），不是密钥。
	OAuthClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"

	EpDTPoll = "/api/v1/deviceToken/poll"
)

// AuthorizeURL 返回设备流授权页地址。
func AuthorizeURL(nonce, challenge string) string {
	return fmt.Sprintf("%s/device/selectAccounts?nonce=%s&challenge=%s&challenge_method=S256&client_id=%s",
		OAuthWebsite, nonce, challenge, OAuthClientID)
}

// makePKCE 生成 verifier + S256 challenge（64 字节采样，与上游实测一致）。
func makePKCE() (verifier, challenge string) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	buf := make([]byte, 64)
	_, _ = rand.Read(buf)
	var sb strings.Builder
	sb.Grow(64)
	for _, b := range buf {
		sb.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	verifier = sb.String()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// deviceSession 是一次进行中的设备流授权。
type deviceSession struct {
	a         *Adapter
	verifier  string
	nonce     string
	authURL   string
	cancelled bool
}

// StartLogin 实现 channel.Authorizer：生成 PKCE 并返回授权页地址。
//
// 这一步不碰上游 —— 设备流的「发起」就是让用户去点页面，上游没有 start 接口。
func (a *Adapter) StartLogin(ctx context.Context, opts channel.LoginOptions) (channel.LoginSession, error) {
	verifier, challenge := makePKCE()
	nonce := newNonce()
	return &deviceSession{
		a:        a,
		verifier: verifier,
		nonce:    nonce,
		authURL:  AuthorizeURL(nonce, challenge),
	}, nil
}

func (s *deviceSession) AuthURL() string { return s.authURL }

func (s *deviceSession) Cancel() { s.cancelled = true }

// Poll 实现 channel.LoginSession：问一次上游「授权好了没」。
//
// 上游语义（实测）：404/202 = 还没授权；200 + token = 好了；其它 4xx/5xx = 真失败。
func (s *deviceSession) Poll(ctx context.Context) (*channel.Credential, error) {
	if s.cancelled {
		return nil, errs.New(errs.AuthFailed, "授权已取消")
	}
	url := fmt.Sprintf("%s%s?nonce=%s&verifier=%s&challenge_method=S256",
		s.a.base, EpDTPoll, s.nonce, s.verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errs.New(errs.Transport, "构造轮询请求失败").WithCause(err)
	}
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Accept", "application/json")

	resp, err := s.a.http.Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "轮询授权结果失败").WithCause(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// 未授权：上游用 404/202 表达，这是正常态，不是错误。
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return nil, channel.ErrPending
	}
	if resp.StatusCode >= 400 {
		return nil, errs.New(Classify(resp.StatusCode, string(raw)), "上游拒绝授权轮询").
			WithUpstream(truncate(string(raw), 200))
	}

	var tok map[string]any
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, errs.New(errs.Parse, "授权响应无法解析").WithUpstream(truncate(string(raw), 200)).WithCause(err)
	}
	token := getString(tok, "token")
	if token == "" {
		token = getString(tok, "device_token")
	}
	if token == "" {
		// 200 但没 token：上游还在处理，继续等（与 wild-work 一致）。
		return nil, channel.ErrPending
	}
	uid := getString(tok, "user_id")
	if uid == "" {
		uid = getString(tok, "uid")
	}
	if uid == "" {
		return nil, errs.New(errs.Parse, "授权成功但上游未返回 user_id").WithUpstream(truncate(string(raw), 200))
	}

	cred := channel.Credential{
		UID:          uid,
		AccessToken:  token,
		RefreshToken: getString(tok, "refresh_token"),
		Extra:        map[string]string{},
	}
	if secs := int64(getFloat(tok, "expires_in")); secs > 0 {
		cred.ExpiresAt = time.Now().Add(time.Duration(secs) * time.Second)
	}
	// 机器指纹：设备流响应里没有，必须本地生成，否则后续 COSY 签名过不了。
	s.a.EnsureFingerprint(&cred)
	// 昵称：设备流不给，靠 userinfo 补（失败不影响授权本身）。
	if nickname, userType := s.a.fetchUserInfo(ctx, &cred); nickname != "" {
		cred.Nickname = nickname
		if userType != "" {
			s.a.setUserType(uid, userType)
		}
	}
	return &cred, nil
}

// fetchUserInfo 取显示名与 userType；任何失败都返回空值（不阻塞授权）。
func (a *Adapter) fetchUserInfo(ctx context.Context, c *channel.Credential) (nickname, userType string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+EpUserInfo, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	resp, err := a.http.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return "", ""
	}
	name := getString(out, "name")
	if name == "" {
		name = getString(out, "nickname")
	}
	if name == "" {
		name = getString(out, "email")
	}
	return name, getString(out, "userType")
}

// newNonce 生成一次性的 nonce（时间戳 + 随机 hex，与上游实测形态一致）。
func newNonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d%x", time.Now().UnixNano()/1e6, b)
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func getFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	switch n := m[key].(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case int64:
		return float64(n)
	}
	return 0
}
