package kiro

// login.go 「登录」= 你在本机登录 Kiro（Kiro IDE 或 kiro-cli），把凭据文件里的
// refreshToken 粘回面板一次。
//
// 为什么不做扫码/密码自动登录：
//   - Kiro 的登录发生在你本机的 IDE / CLI 里（浏览器走的是 AWS Builder ID /
//     IAM Identity Center 的授权页），服务端复现不了；
//   - 把 AWS 账号密码存在 NAS 上，风险远大于「多一步粘贴」——参考实现那条
//     「用 SQLite 里的凭据自动续期」的路我们不抄，只抄「粘贴」这一步。
//
// 认两种粘贴形态：
//   - 桌面版（个人 / Builder ID）：只粘 refreshToken（裸串，或整段凭据 JSON）；
//   - 企业版（AWS SSO / IAM Identity Center）：粘 refreshToken + clientId + clientSecret
//     三件套（整段凭据 JSON 也行）—— 有 clientId+clientSecret 就自动走 OIDC 换令牌。
//
// 企业版为什么必须让用户把 clientId/clientSecret 也粘过来：它们在你本机的
// ~/.aws/sso/cache/<clientIdHash>.json 里，而 PoolGate 跑在 NAS 上，读不到你本机的文件。

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

const loginPage = "https://kiro.dev"

// hintText 是给面板的粘贴引导语。写清楚「去哪拿、粘什么」，因为这是本渠道唯一的用户步骤。
const hintText = "在装 Kiro 的机器上取一份凭据，粘到下面：\n" +
	"• 桌面版（个人 / Builder ID）：打开 ~/.aws/sso/cache/kiro-auth-token.json" +
	"（Kiro IDE 写入；部分版本在 ~/.kiro/ 下），把里面的 refreshToken 整串粘过来（整段 JSON 也行）。\n" +
	"• 命令行版（kiro-cli）：凭据在 ~/.local/share/kiro-cli/data.sqlite3，" +
	"取 key 为 kirocli:social:token / kirocli:odic:token 的那条 JSON 里的 refresh_token。\n" +
	"• 企业版（AWS SSO / IAM Identity Center）：把 ~/.aws/sso/cache/ 下凭据 JSON 里的 " +
	"refreshToken、clientId、clientSecret 三件套一起粘过来（必须是三件套，只有 refreshToken 会换不了令牌）。\n" +
	"可选：一并带上 region（默认 us-east-1）与 profileArn。"

// paste 是解析出来的粘贴内容。
type paste struct {
	refreshToken string
	clientID     string
	clientSecret string
	region       string
	apiRegion    string
	profileArn   string
}

type session struct {
	a  *Adapter
	mu sync.Mutex
	p  paste
	// uid 在 AcceptCallback 里算好并固定：它同时决定「缓存键」与「面板显示的短标识」。
	uid  string
	done bool
}

// StartLogin 返回一个「等粘贴」的登录会话：控制台对实现 CallbackAcceptor 的渠道
// 会自动显示粘贴入口与引导语。
func (a *Adapter) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	return &session{a: a}, nil
}

func (s *session) AuthURL() string { return loginPage }

// Hint 实现控制台的可选接口：面板据此切换成「粘贴」形态（不画二维码、不提示扫码）。
func (s *session) Hint() string { return hintText }

// AcceptCallback 收用户粘回来的凭据（裸 refreshToken / 整段 JSON / 从 JSON 里抄的一行都认）。
func (s *session) AcceptCallback(raw string) error {
	p, err := parsePaste(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.p, s.uid = p, uidFor(p)
	s.mu.Unlock()
	return nil
}

// Poll 在用户粘了凭据之后：**当场用换令牌接口验一次**，验过了才把凭证交出去。
//
// 为什么必须当场验：把一个已失效的 refreshToken 塞进池子，用户看到的是
// 「装上了，但一调用就 403」—— 分不清是自己粘错了还是渠道坏了。
// 换令牌是这个渠道唯一的「一句话探活」手段（它没有独立的余额/目录接口）。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	p, uid, done := s.p, s.uid, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.Kiro))
	}
	if p.refreshToken == "" {
		return nil, channel.ErrPending // 还没粘：正常的等待态
	}

	cred := &channel.Credential{
		UID:          uid,
		Nickname:     nicknameFor(p),
		RefreshToken: p.refreshToken,
		Extra:        map[string]string{},
	}
	if p.region != "" {
		cred.Extra["region"] = p.region
	}
	if p.apiRegion != "" {
		cred.Extra["api_region"] = p.apiRegion
	}
	if p.profileArn != "" {
		cred.Extra["profile_arn"] = p.profileArn
	}
	if p.clientID != "" {
		cred.Extra["client_id"] = p.clientID
	}
	if p.clientSecret != "" {
		cred.Extra["client_secret"] = p.clientSecret
	}

	// 当场换一次令牌：换成功说明这组凭据可用，顺便把 accessToken / 过期时间 / profileArn 拿到手。
	access, newRefresh, exp, arn, err := s.a.exchangeRefresh(ctx, cred, p.refreshToken)
	if err != nil {
		return nil, err
	}
	cred.AccessToken = access
	if newRefresh != "" {
		cred.RefreshToken = newRefresh // 上游轮换过就存新的
	}
	cred.ExpiresAt = exp
	if arn != "" {
		cred.Extra["profile_arn"] = arn
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

// parsePaste 解析粘贴内容。
//
// 容忍三种输入：裸 refreshToken、整段凭据 JSON、以及从 JSON 文件里抄出来的一行
// （`"refreshToken": "eyJ..."`）—— 用户最常做的就是打开文件复制一行。
func parsePaste(raw string) (paste, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return paste{}, errs.New(errs.Parse, "没收到内容：请把凭据粘到输入框里").
			WithChannel(string(channel.Kiro))
	}
	var p paste
	if strings.HasPrefix(s, "{") {
		// 整段 JSON：camelCase 与 snake_case 都认（Kiro IDE 用 camelCase，
		// kiro-cli 的 SQLite 里是 snake_case）。
		var obj map[string]any
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return paste{}, errs.New(errs.Parse, "这段 JSON 解析不了，请检查是否粘贴完整").
				WithChannel(string(channel.Kiro)).WithUpstream(truncate(s, 120))
		}
		p.refreshToken = firstNonEmpty(strOf(obj, "refreshToken"), strOf(obj, "refresh_token"))
		p.clientID = firstNonEmpty(strOf(obj, "clientId"), strOf(obj, "client_id"))
		p.clientSecret = firstNonEmpty(strOf(obj, "clientSecret"), strOf(obj, "client_secret"))
		p.region = firstNonEmpty(strOf(obj, "region"), strOf(obj, "ssoRegion"))
		p.apiRegion = firstNonEmpty(strOf(obj, "apiRegion"), strOf(obj, "api_region"))
		p.profileArn = firstNonEmpty(strOf(obj, "profileArn"), strOf(obj, "profile_arn"))
		if p.clientID == "" || p.clientSecret == "" {
			// 企业版凭据里常见 clientIdHash：clientId/clientSecret 在**另一份**文件里，
			// 而那份文件在你本机、我们读不到 —— 所以要把这句话说清楚。
			if h := firstNonEmpty(strOf(obj, "clientIdHash"), strOf(obj, "client_id_hash")); h != "" {
				return paste{}, errs.New(errs.Parse,
					"这份凭据只带了 clientIdHash（"+truncate(h, 24)+"），没有 clientId/clientSecret："+
						"请把你本机 ~/.aws/sso/cache/ 下那个同名文件里的 clientId 与 clientSecret 一并粘过来"+
						"（PoolGate 跑在 NAS 上，读不到你本机的文件）").
					WithChannel(string(channel.Kiro))
			}
		}
	} else {
		// 不是整段 JSON：可能是裸串，也可能是从文件里抄的一行。
		p.refreshToken = firstNonEmpty(quotedValueOf(s, "refreshToken"), quotedValueOf(s, "refresh_token"))
		if p.refreshToken == "" {
			p.refreshToken = s
		}
		p.clientID = firstNonEmpty(quotedValueOf(s, "clientId"), quotedValueOf(s, "client_id"))
		p.clientSecret = firstNonEmpty(quotedValueOf(s, "clientSecret"), quotedValueOf(s, "client_secret"))
		p.region = quotedValueOf(s, "region")
		p.profileArn = firstNonEmpty(quotedValueOf(s, "profileArn"), quotedValueOf(s, "profile_arn"))
	}

	// 裸串（或抄错时）常带引号与行尾逗号，去掉。
	p.refreshToken = strings.Trim(strings.TrimSpace(p.refreshToken), `"',`)
	if p.refreshToken == "" {
		return paste{}, errs.New(errs.Parse,
			"没识别出 refreshToken：请打开凭据文件，把 refreshToken（或整段 JSON）粘过来；"+
				"企业版要带上 clientId/clientSecret").WithChannel(string(channel.Kiro))
	}
	// refreshToken 是较长的 JWT 或不透明串；挡住明显误粘的内容（短串、含空白、文件路径）。
	if len(p.refreshToken) < 20 || strings.ContainsAny(p.refreshToken, " \t\n") {
		return paste{}, errs.New(errs.Parse,
			"这串内容不像 refreshToken（太短或含空白）：请确认粘的是凭据文件里的 refreshToken 值，不是文件路径").
			WithChannel(string(channel.Kiro))
	}
	if looksLikePath(p.refreshToken) {
		return paste{}, errs.New(errs.Parse,
			"粘进来的像是**文件路径**而不是令牌：请打开那个文件，把 refreshToken 的值（或整段 JSON）粘过来").
			WithChannel(string(channel.Kiro)).WithUpstream(truncate(p.refreshToken, 120))
	}
	// 只有一半的 clientId/clientSecret：宁可当场说清楚，也不要等到换令牌时上游回一个 400。
	if (p.clientID == "") != (p.clientSecret == "") {
		return paste{}, errs.New(errs.Parse,
			"clientId 与 clientSecret 必须成对提供：企业版请把两个一起粘过来").
			WithChannel(string(channel.Kiro))
	}
	return p, nil
}

// uidFor 由 refreshToken 派生出账号 uid。
//
// 为什么要带 kiro-sso- 前缀：凭据落盘的 schema 只保留少数几个附加字段
// （见 client.go 里 ssoUIDPrefix 的说明），clientId/clientSecret 重启后会丢；
// 把「当初是哪种鉴权」记在一定会被保留的 uid 上，重启后能给出可操作的错误。
func uidFor(p paste) string {
	if p.clientID != "" && p.clientSecret != "" {
		return ssoUIDPrefix + shortHash(p.refreshToken)
	}
	return "kiro-" + shortHash(p.refreshToken)
}

func nicknameFor(p paste) string {
	if p.clientID != "" {
		return "Kiro 企业账号（AWS SSO）"
	}
	return "Kiro 账号"
}

// looksLikePath 挡掉「粘了文件路径」这种最常见的误粘。
//
// 只认明确的路径特征，不对令牌本身的形状做更多假设（令牌偶尔会出现 '/'）——
// 万一挡错了，用户还可以改粘整段 JSON。
func looksLikePath(s string) bool {
	if strings.Contains(s, "://") || strings.Contains(s, ".json") {
		return true
	}
	return strings.HasPrefix(s, "~") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "/")
}

// strOf 取 map 里的字符串字段。
func strOf(obj map[string]any, key string) string {
	if v, ok := obj[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// quotedValueOf 从一行文本里取出 `"key": "value"` 的 value。
// 用来接住「用户从 JSON 文件里抄了一行」这种最常见的粘贴方式。
func quotedValueOf(s, key string) string {
	i := strings.Index(s, `"`+key+`"`)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key)+2:]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[colon+1:])
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	if end := strings.Index(rest, `"`); end >= 0 {
		return strings.TrimSpace(rest[:end])
	}
	return ""
}

// shortHash 给账号一个稳定短标识（面板显示用，不泄露令牌本身）。
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}
