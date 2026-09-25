package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"poolgate/internal/channel"
)

// CredsStore 加载渠道账号凭证。凭证文件格式沿用 wild-work（嵌套形 / 扁平形双形态），
// 迁移期可直接读旧 auth 目录；文件名约定 <channel>-<uid>.json。
type CredsStore struct {
	dir string // 凭证目录（/vol6/@appconf/poolgate/creds）
}

// NewCredsStore 建立凭证存储。
func NewCredsStore(dir string) *CredsStore { return &CredsStore{dir: dir} }

// Load 扫描某渠道的凭证（<prefix>-*.json），解析并补齐机器指纹占位（由适配器填）。
func (s *CredsStore) Load(kind channel.Kind) ([]channel.Credential, error) {
	prefix := string(kind)
	glob := filepath.Join(s.dir, prefix+"-*.json")
	files, err := filepath.Glob(glob)
	if err != nil {
		return nil, err
	}
	out := []channel.Credential{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue // 单个凭证损坏不阻塞其它账号
		}
		c, err := parseCredential(raw)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// Save 落盘一个账号凭证：<dir>/<kind>-<uid>.json，嵌套形（与 wild-work 一致），
// 权限 0600、先写临时文件再 rename（半截文件会让下次启动丢账号）。
//
// 授权成功后由控制台调用 —— 这是凭证唯一的写入入口。
func (s *CredsStore) Save(kind channel.Kind, c channel.Credential) (string, error) {
	if strings.TrimSpace(c.UID) == "" {
		return "", fmt.Errorf("凭证缺少 uid，无法落盘")
	}
	if strings.TrimSpace(c.AccessToken) == "" {
		return "", fmt.Errorf("凭证缺少 accessToken，拒绝落盘空凭证")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", err
	}
	auth := map[string]any{
		"accessToken": c.AccessToken,
	}
	if c.RefreshToken != "" {
		auth["refreshToken"] = c.RefreshToken
	}
	if !c.ExpiresAt.IsZero() {
		auth["expiresAt"] = c.ExpiresAt.Unix()
	}
	// 机器指纹等渠道附加字段原样保留（QoderCN/TraeWork 靠它过上游校验）。
	for k, v := range c.Extra {
		if v == "" {
			continue
		}
		auth[extraKey(k)] = v
	}
	obj := map[string]any{
		"auth": auth,
		"account": map[string]any{
			"uid":      c.UID,
			"nickname": c.Nickname,
		},
	}
	raw, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.dir, string(kind)+"-"+sanitizeUID(c.UID)+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// Delete 删除一个账号的凭证文件。返回是否确实删掉了文件。
//
// 只删 <kind>-<uid>.json 这一个文件；用 sanitizeUID 归一后再拼路径，
// 防止 uid 里的路径分隔符把删除引到目录之外。
func (s *CredsStore) Delete(kind channel.Kind, uid string) (bool, error) {
	path := filepath.Join(s.dir, string(kind)+"-"+sanitizeUID(uid)+".json")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Dir 返回凭证目录（面板展示用）。
func (s *CredsStore) Dir() string { return s.dir }

// extraKey 把内部 Extra 的蛇形键还原成 wild-work 的驼峰键。
func extraKey(k string) string {
	switch k {
	case "machine_id":
		return "machineId"
	case "machine_token":
		return "machineToken"
	case "machine_type":
		return "machineType"
	case "device_id":
		return "deviceId"
	case "api_host":
		return "apiHost"
	default:
		return k
	}
}

// sanitizeUID 防住 uid 里的路径分隔符（上游 uid 由我们解析，但不值得信任）。
func sanitizeUID(uid string) string {
	return strings.NewReplacer("/", "_", "\\", "_", "..", "_", string(filepath.Separator), "_").Replace(uid)
}

// parseCredential 兼容 wild-work 的嵌套形/扁平形两种磁盘形态。
func parseCredential(raw []byte) (channel.Credential, error) {
	if len(raw) == 0 {
		return channel.Credential{}, fmt.Errorf("empty credential")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return channel.Credential{}, fmt.Errorf("credential parse: %w", err)
	}
	var (
		accessToken, refreshToken string
		expiresAt                 time.Time
		uid, nickname             string
		machineID, machineToken   string
		machineType               string
		deviceID                  string
		apiHost                   string
	)
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				MachineID    string `json:"machineId"`
				MachineToken string `json:"machineToken"`
				MachineType  string `json:"machineType"`
				DeviceID     string `json:"deviceId"`
				ApiHost      string `json:"apiHost"`
			} `json:"auth"`
			Account struct {
				UID      string `json:"uid"`
				Nickname string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return channel.Credential{}, err
		}
		accessToken, refreshToken = n.Auth.AccessToken, n.Auth.RefreshToken
		uid, nickname = n.Account.UID, n.Account.Nickname
		machineID, machineToken, machineType = n.Auth.MachineID, n.Auth.MachineToken, n.Auth.MachineType
		deviceID, apiHost = n.Auth.DeviceID, n.Auth.ApiHost
		if n.Auth.ExpiresAt > 0 {
			expiresAt = time.Unix(n.Auth.ExpiresAt, 0)
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			UID          string `json:"uid"`
			Nickname     string `json:"nickname"`
			MachineID    string `json:"machineId"`
			MachineToken string `json:"machineToken"`
			MachineType  string `json:"machineType"`
			DeviceID     string `json:"deviceId"`
			ApiHost      string `json:"apiHost"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return channel.Credential{}, err
		}
		accessToken, refreshToken = f.AccessToken, f.RefreshToken
		uid, nickname = f.UID, f.Nickname
		machineID, machineToken, machineType = f.MachineID, f.MachineToken, f.MachineType
		deviceID, apiHost = f.DeviceID, f.ApiHost
		if f.ExpiresAt > 0 {
			expiresAt = time.Unix(f.ExpiresAt, 0)
		}
	}
	if strings.TrimSpace(accessToken) == "" {
		return channel.Credential{}, fmt.Errorf("missing accessToken")
	}
	return channel.Credential{
		UID:          uid,
		Nickname:     nickname,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Extra: map[string]string{
			"machine_id":    machineID,
			"machine_token": machineToken,
			"machine_type":  machineType,
			"device_id":     deviceID,
			"api_host":      apiHost,
		},
	}, nil
}
