// credentials.go 处理「IDE 登录缓存 → 渠道凭证」的换算：
//   - 解 `cache/user`（AES-128-CBC，key = IV = machineID 前 16 字节）；
//   - 从解出的 JSON 里取出 cosy_key / encrypt_user_info / uid；
//   - 与 channel.Credential 的落盘形态互转。
//
// 本渠道没有接口签发的令牌，凭证就是 IDE 缓存里的那三样东西。所以「登录」这一步
// 不做任何网络请求就完成了凭证组装 —— 但**必须**再做一次轻量校验（Poll 里拉模型目录），
// 否则用户粘错一个字符，表现就是「装上了，一调用就 403」，没人知道错在哪。
//
// 落盘槽位说明（很重要）：store 的 parseCredential 只认固定的 5 个 Extra 键
// （machine_id / machine_token / machine_type / device_id / api_host，见 internal/store/creds.go），
// 自定义键写在磁盘上但**重启后读不回来**。所以本渠道把字段映射到既有槽位：
//
//	AccessToken      ← cosy_key            （COSY 主密钥，与签名强绑定，放主凭证槽）
//	UID              ← user_id（缓存里的 uid）
//	Extra[machine_id]     ← machineID      （cache/id 原文；同时用于解密与 Cosy-Machineid）
//	Extra[machine_token]  ← encrypt_user_info（COSY payload 的 info；沿用 store 槽位名，语义见下）
//
// 「用 machine_token 存 info」看着别扭，但这是唯一不需要改 store 就能持久化的办法：
// 灵码协议里 Cosy-Machinetoken 恒为空串（设备授权是 QoderCN 的事），这个槽位对本渠道是空的，
// 拿它装 info 不会和任何真实字段撞车。换槽位前请先确认 store 的键集合有没有放开。
package lingma

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"poolgate/internal/channel"
)

// base64 解码器：标准与无填充。用户复制时常常把尾部 '=' 弄丢，两种都认。
var (
	b64Std = base64.StdEncoding
	b64Raw = base64.RawStdEncoding
)

// lingmaCred 是本渠道真正需要的那几样东西（从 channel.Credential 或缓存解析而来）。
type lingmaCred struct {
	CosyKey   string
	Info      string
	MachineID string
	UserID    string
}

// signer 把凭证转成签名素材。
func (lc lingmaCred) signer() cosySigner {
	return cosySigner{CosyKey: lc.CosyKey, Info: lc.Info, MachineID: lc.MachineID, UserID: lc.UserID}
}

// validate 检查凭证是否齐全。缺一不可 —— 少任何一项签名都算不出来，
// 与其让上游回一个含糊的 403，不如在本地说清楚缺什么（红线一）。
func (lc lingmaCred) validate() error {
	switch {
	case strings.TrimSpace(lc.CosyKey) == "":
		return fmt.Errorf("缺少 cosy_key（IDE 缓存里的 key 字段）")
	case strings.TrimSpace(lc.Info) == "":
		return fmt.Errorf("缺少 encrypt_user_info（IDE 缓存里的 encrypt_user_info 字段）")
	case strings.TrimSpace(lc.UserID) == "":
		return fmt.Errorf("缺少 user_id（IDE 缓存里的 uid 字段）")
	case strings.TrimSpace(lc.MachineID) == "":
		return fmt.Errorf("缺少 machineID（cache/id 文件内容）")
	}
	return nil
}

// credOf 从已落盘的 channel.Credential 还原出签名素材。
func credOf(c *channel.Credential) lingmaCred {
	if c == nil {
		return lingmaCred{}
	}
	return lingmaCred{
		CosyKey:   strings.TrimSpace(c.AccessToken),
		Info:      extraOf(c, "machine_token"),
		MachineID: extraOf(c, "machine_id"),
		UserID:    strings.TrimSpace(c.UID),
	}
}

// toChannelCredential 把签名素材打包成可落盘的 channel.Credential。
func toChannelCredential(lc lingmaCred, nickname string) *channel.Credential {
	return &channel.Credential{
		UID:         lc.UserID,
		Nickname:    nickname,
		AccessToken: lc.CosyKey,
		Extra: map[string]string{
			"machine_id":    lc.MachineID,
			"machine_token": lc.Info, // 见文件头：槽位复用，存的是 COSY 的 info 密文
		},
	}
}

func extraOf(c *channel.Credential, key string) string {
	if c == nil || c.Extra == nil {
		return ""
	}
	return strings.TrimSpace(c.Extra[key])
}

// ---------------------------------------------------------------------------
// cache/user 解密
// ---------------------------------------------------------------------------

// decryptCacheUser 解密 `cache/user`：AES-128-CBC，key = IV = machineID 前 16 字节，PKCS7 填充。
//
// 与参考实现 remote/credentials.go decryptCacheUser 逐行等价。
func decryptCacheUser(machineID string, ciphertext []byte) ([]byte, error) {
	if len(machineID) < aesBlockSize {
		return nil, fmt.Errorf("machineID 长度不足 %d 字节，无法作为 AES 密钥", aesBlockSize)
	}
	if len(ciphertext) == 0 || len(ciphertext)%aesBlockSize != 0 {
		return nil, fmt.Errorf("cache/user 密文长度非法（%d 字节，须为 %d 的倍数）",
			len(ciphertext), aesBlockSize)
	}
	key := []byte(machineID[:aesBlockSize])
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("构造 AES 密码: %w", err)
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, key).CryptBlocks(plaintext, ciphertext)
	return unpadPKCS7(plaintext)
}

// unpadPKCS7 去掉 PKCS7 填充，并逐字节校验填充值。
//
// 校验填充不是洁癖：密钥不对时 CBC 解密**不会报错**，只会得到一段乱码填充，
// 填充校验是唯一能当场把「密钥弄错了」和「密文坏了」区分开的地方。
func unpadPKCS7(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("解密结果为空")
	}
	padLen := int(data[len(data)-1])
	if padLen <= 0 || padLen > aesBlockSize || padLen > len(data) {
		return nil, fmt.Errorf("填充长度非法（%d）：通常是 machineID 与 cache/user 不匹配", padLen)
	}
	for _, b := range data[len(data)-padLen:] {
		if int(b) != padLen {
			return nil, fmt.Errorf("填充字节不一致：通常是 machineID 与 cache/user 不匹配")
		}
	}
	return data[:len(data)-padLen], nil
}

// cacheUserPayload 是 cache/user 解密后的 JSON 形态。
// 只取我们需要的四个字段，其余原样丢弃（不认识的字段不进凭证，避免污染落盘内容）。
type cacheUserPayload struct {
	Key             string `json:"key"`
	EncryptUserInfo string `json:"encrypt_user_info"`
	UID             string `json:"uid"`
	// ExpireTime 上游形态不稳定（字符串毫秒 / 数字 / 都没有），这里只做参考，不参与校验。
	ExpireTime any `json:"expire_time"`
}

// decodeCacheUser 把粘回来的 cache/user 内容（base64 文本）+ machineID 解成签名素材。
func decodeCacheUser(userContent, machineID string) (lingmaCred, error) {
	raw := strings.TrimSpace(userContent)
	if raw == "" {
		return lingmaCred{}, fmt.Errorf("cache/user 内容为空")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return lingmaCred{}, fmt.Errorf("cache/user 不是合法 base64: %w", err)
	}
	plaintext, err := decryptCacheUser(strings.TrimSpace(machineID), ciphertext)
	if err != nil {
		return lingmaCred{}, err
	}
	var payload cacheUserPayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return lingmaCred{}, fmt.Errorf("cache/user 解密后不是合法 JSON: %w", err)
	}
	lc := lingmaCred{
		CosyKey:   strings.TrimSpace(payload.Key),
		Info:      strings.TrimSpace(payload.EncryptUserInfo),
		MachineID: strings.TrimSpace(machineID),
		UserID:    strings.TrimSpace(payload.UID),
	}
	if err := lc.validate(); err != nil {
		return lingmaCred{}, fmt.Errorf("cache/user 解密成功但字段不全: %w", err)
	}
	return lc, nil
}

// ---------------------------------------------------------------------------
// 已导出的凭证 JSON
// ---------------------------------------------------------------------------

// exportedCredential 是我们**自己**导出过的凭证 JSON 形态（参考实现 SaveCredentialFile 的格式）。
//
// 支持它有两个用处：跨机器搬迁时不用再翻 IDE 缓存；以及用户从别的工具（如 lingma-proxy）
// 手里已经有这份文件时可以直粘。
type exportedCredential struct {
	Source string `json:"source"`
	Auth   struct {
		CosyKey         string `json:"cosy_key"`
		EncryptUserInfo string `json:"encrypt_user_info"`
		UserID          string `json:"user_id"`
		MachineID       string `json:"machine_id"`
	} `json:"auth"`
	// 兼容扁平形态（字段直接放在顶层）。
	CosyKey         string `json:"cosy_key"`
	EncryptUserInfo string `json:"encrypt_user_info"`
	UserID          string `json:"user_id"`
	MachineID       string `json:"machine_id"`
}

// parseExportedCredential 尝试把一段文本当成「已导出的凭证 JSON」解析。
// 第二个返回值为 false 表示「这不是导出格式」，调用方应回落到缓存两段式解析。
func parseExportedCredential(raw string) (lingmaCred, bool) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "{") {
		return lingmaCred{}, false
	}
	var out exportedCredential
	if json.Unmarshal([]byte(s), &out) != nil {
		return lingmaCred{}, false
	}
	lc := lingmaCred{
		CosyKey:   firstNonEmpty(out.Auth.CosyKey, out.CosyKey),
		Info:      firstNonEmpty(out.Auth.EncryptUserInfo, out.EncryptUserInfo),
		UserID:    firstNonEmpty(out.Auth.UserID, out.UserID),
		MachineID: firstNonEmpty(out.Auth.MachineID, out.MachineID),
	}
	if lc.CosyKey == "" || lc.Info == "" {
		return lingmaCred{}, false // 不是导出格式（或字段不全），交给别的解析路径
	}
	return lc, true
}

// cachePaste 是「两段式」粘贴的容器：`{"user":"…","id":"…"}`。
// 字段名多认几种写法，用户照着任意一份文档填都能过。
type cachePaste struct {
	User      string `json:"user"`
	CacheUser string `json:"cache_user"`
	ID        string `json:"id"`
	CacheID   string `json:"cache_id"`
	MachineID string `json:"machine_id"`
}

// parseCachePasteJSON 尝试把一段 JSON 当成两段式粘贴解析。
func parseCachePasteJSON(raw string) (user, machineID string, ok bool) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "{") {
		return "", "", false
	}
	var p cachePaste
	if json.Unmarshal([]byte(s), &p) != nil {
		return "", "", false
	}
	user = firstNonEmpty(p.User, p.CacheUser)
	machineID = firstNonEmpty(p.ID, p.CacheID, p.MachineID)
	if user == "" || machineID == "" {
		return "", "", false
	}
	return user, machineID, true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
