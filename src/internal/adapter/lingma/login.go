// login.go 「登录」= 让用户把 IDE 的登录缓存粘回面板，服务端解密成凭证。
//
// 为什么不自动读本机缓存：PoolGate 跑在 NAS/服务器上，IDE 在用户自己的电脑里，
// 两边不是同一台机器（参考实现因为跑在桌面端才能直接读目录，我们做不到）。
// 而缓存里那三样东西（cosy_key / encrypt_user_info / machineID）都是**长期有效**的，
// 粘一次即可，不需要账号密码 —— 也就不需要把用户的阿里云密码存在 NAS 上。
//
// 需要两个文件，因为缺一不可：
//   - cache/user：AES 密文，密钥是 machineID 前 16 字节 —— 没有 machineID 就解不开；
//   - cache/id  ：machineID 明文，同时也是请求头 `Cosy-Machineid` 的值。
//
// 实现 channel.CallbackAcceptor + Hint()：控制台据此把面板切成「粘贴」形态
// （与 DeepSeek/Kimi 同一条通道，前端不需要为灵码做任何特判）。
package lingma

import (
	"context"
	"strings"
	"sync"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// loginPage 是让用户去登录/安装灵码的官方页面（面板会显示它，虽不是授权页，
// 但这是用户「从哪拿到缓存」的起点）。
const loginPage = "https://lingma.aliyun.com"

// pasteSeparator 是「一次粘两段」时用的分隔行。挑一个不可能出现在 base64 与
// machineID 里的字符串，避免误切。
const pasteSeparator = "@@@id@@@"

const hintText = "通义灵码是「复用 IDE 登录态」的渠道，没有 API key，请把 IDE 的缓存粘回来。" +
	"在装了灵码的机器上找到这两个文件：① cache/user（一段较长的 base64）② cache/id（machineID 明文）。" +
	"常见位置：~/.lingma/cache/ 或编辑器全局存储里的 alibaba-cloud.tongyi-lingma/。" +
	"粘贴方式任选其一：" +
	"A) 一次粘两段 —— 先粘 cache/user 的全部内容，另起一行写 " + pasteSeparator + "，再粘 cache/id 的内容；" +
	"B) 分两次粘 —— 先粘 cache/user 提交，再粘 cache/id 提交；" +
	"C) 直接粘已导出的凭证 JSON（含 auth.cosy_key / auth.encrypt_user_info / auth.user_id / auth.machine_id）。"

// session 是一次进行中的「粘贴式」登录。
type session struct {
	a  *Adapter
	mu sync.Mutex
	// userContent 是粘回来的 cache/user 原文（base64），machineID 是 cache/id 原文。
	userContent string
	machineID   string
	// ready 是已导出凭证 JSON 直接粘回来的情况（无需解密）。
	ready lingmaCred
	done  bool
}

// StartLogin 返回一个「等粘贴」的登录会话（不碰网络 —— 组装凭证本身不需要请求）。
func (a *Adapter) StartLogin(context.Context, channel.LoginOptions) (channel.LoginSession, error) {
	return &session{a: a}, nil
}

func (s *session) AuthURL() string { return loginPage }

// Hint 实现控制台的可选接口：面板据此切换成「粘贴」形态（不画二维码、不提示扫码）。
func (s *session) Hint() string { return hintText }

// AcceptCallback 收用户粘回来的内容，逐种形态尝试识别：
//  1. 已导出的凭证 JSON（自描述，一次到位）；
//  2. 两段式 JSON `{"user":…,"id":…}`；
//  3. 带分隔行的两段文本（A 方式）；
//  4. 单段内容（B 方式）：能当作密文解的就是 cache/user，否则当 cache/id。
//
// 识别不出来就明确报错（红线一），不做「先存着看看」—— 粘错内容却静默接受，
// 用户会以为成功，直到调用时才炸。
func (s *session) AcceptCallback(raw string) error {
	text := strings.TrimSpace(raw)
	if text == "" {
		return errs.New(errs.Parse, "粘贴内容为空").WithChannel(string(channel.Lingma))
	}

	// 1) 已导出的凭证 JSON。
	if lc, ok := parseExportedCredential(text); ok {
		if err := lc.validate(); err != nil {
			return errs.New(errs.Parse, "凭证 JSON 字段不全："+err.Error()).
				WithChannel(string(channel.Lingma))
		}
		s.set(func(st *session) { st.ready = lc })
		return nil
	}
	// 2) 两段式 JSON。
	if user, id, ok := parseCachePasteJSON(text); ok {
		s.set(func(st *session) { st.userContent, st.machineID = user, id })
		return nil
	}
	// 3) 分隔行两段文本。
	if user, id, ok := splitBySeparator(text); ok {
		s.set(func(st *session) { st.userContent, st.machineID = user, id })
		return nil
	}
	// 4) 单段：按形态判断这是哪一半。
	if looksLikeCacheUser(text) {
		s.set(func(st *session) { st.userContent = text })
		return nil
	}
	if looksLikeMachineID(text) {
		s.set(func(st *session) { st.machineID = text })
		return nil
	}
	return errs.New(errs.Parse,
		"没认出这段内容：请按面板提示粘 cache/user（base64）与 cache/id（machineID），"+
			"或直接粘已导出的凭证 JSON").WithChannel(string(channel.Lingma))
}

func (s *session) set(fn func(*session)) {
	s.mu.Lock()
	fn(s)
	s.mu.Unlock()
}

// Poll 在用户粘完内容后完成凭证组装，并**当场验一次**（拉模型目录）。
//
// 为什么要当场验：凭证是本地解密拼出来的，解错/粘错都不会有网络报错，
// 只能在调用时以 403 的形式暴露；那对用户来说是「装上就坏」。
// 这里直接拿模型目录问一句「这凭证上游认不认」，认了才入池。
func (s *session) Poll(ctx context.Context) (*channel.Credential, error) {
	s.mu.Lock()
	user, id, ready, done := s.userContent, s.machineID, s.ready, s.done
	s.mu.Unlock()
	if done {
		return nil, errs.New(errs.Parse, "这次登录已经完成过了").WithChannel(string(channel.Lingma))
	}

	lc := ready
	if lc.CosyKey == "" {
		if user == "" || id == "" {
			return nil, channel.ErrPending // 还没粘全：正常的等待态
		}
		decoded, err := decodeCacheUser(user, id)
		if err != nil {
			return nil, errs.New(errs.Parse, "解 IDE 缓存失败："+err.Error()).
				WithChannel(string(channel.Lingma))
		}
		lc = decoded
	}
	if err := lc.validate(); err != nil {
		return nil, errs.New(errs.Parse, "凭证不完整："+err.Error()).
			WithChannel(string(channel.Lingma))
	}

	cred := toChannelCredential(lc, nicknameOf(lc.UserID))
	// 轻量校验：模型目录能拉通，说明签名与凭证都对。
	if err := s.a.checkCredential(ctx, cred); err != nil {
		return nil, err
	}
	s.set(func(st *session) { st.done = true })
	return cred, nil
}

func (s *session) Cancel() {
	s.set(func(st *session) { st.done = true })
}

// splitBySeparator 按分隔行切两段（用 SplitN，避免内容里意外再出现分隔串时切碎）。
func splitBySeparator(text string) (user, id string, ok bool) {
	i := strings.Index(text, pasteSeparator)
	if i < 0 {
		return "", "", false
	}
	user = strings.TrimSpace(text[:i])
	id = strings.TrimSpace(text[i+len(pasteSeparator):])
	if user == "" || id == "" {
		return "", "", false
	}
	return user, id, true
}

// looksLikeCacheUser 判断这段文本是否像 cache/user 密文。
//
// 判据是「base64 解码后长度 >= 64 字节且是 AES 块的整数倍」：
//   - 解出来的明文是带 key/encrypt_user_info/uid 的 JSON，必然远大于 64 字节，
//     所以短于 64 字节的（典型是 32 字符的 machineID，解码后仅 24 字节）一定不是它；
//   - 长度必须是 16 的倍数，否则连 CBC 解密都做不了，不该往这条路上引。
func looksLikeCacheUser(s string) bool {
	raw := strings.TrimSpace(s)
	if len(raw) < 64 {
		return false
	}
	decoded, err := decodeBase64Loose(raw)
	if err != nil || len(decoded) < 64 || len(decoded)%aesBlockSize != 0 {
		return false
	}
	return true
}

// looksLikeMachineID 判断这段文本是否像 cache/id（machineID 明文）。
//
// 反向判据而不是正向匹配：machineID 的形态随编辑器版本变（uuid / hex / 带短横线都有），
// 正向匹配一定会漏。这里只要求「是单行、可见 ASCII、长度 >= 16」——
// 刚好够挡住误粘的多行文本与 JSON，把剩下的交给用户提示。
func looksLikeMachineID(s string) bool {
	raw := strings.TrimSpace(s)
	if len(raw) < aesBlockSize || len(raw) > 256 {
		return false
	}
	if strings.ContainsAny(raw, "\n\r \t{[\"'") {
		return false
	}
	return true
}

// decodeBase64Loose 先按标准 base64 解，失败再按无填充解（用户复制时可能丢了 '='）。
func decodeBase64Loose(s string) ([]byte, error) {
	if b, err := b64Std.DecodeString(s); err == nil {
		return b, nil
	}
	return b64Raw.DecodeString(s)
}

// nicknameOf 给账号一个便于分辨的展示名：面板里同渠道多个号看起来一样最难用，
// 带上 uid 的头尾能直接对上人。
func nicknameOf(uid string) string {
	uid = strings.TrimSpace(uid)
	if len(uid) <= 8 {
		return "通义灵码账号 " + uid
	}
	return "通义灵码 " + uid[:4] + "…" + uid[len(uid)-4:]
}
