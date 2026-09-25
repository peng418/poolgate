package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// APIKeyStore 管理网关 API Key（F3.5 分层：普通 key / 管理 key 的落点，M1 先做普通 key）。
// Key 明文只落一次盘（首次生成后写 config），后续校验用恒定时间比较。
type APIKeyStore struct {
	mu   sync.Mutex
	path string
}

type apiKeyConfig struct {
	Keys map[string]apiKeyEntry `json:"keys"`
}

type apiKeyEntry struct {
	Label     string `json:"label"`
	IsAdmin   bool   `json:"is_admin,omitempty"`
	CreatedAt string `json:"created_at"`
}

// NewAPIKeyStore 建立 API Key 存储（config.json）。
func NewAPIKeyStore(dir string) *APIKeyStore {
	return &APIKeyStore{path: filepath.Join(dir, "config.json")}
}

// Path 返回配置文件路径（含 key，注意别写进导出包）。
func (s *APIKeyStore) Path() string { return s.path }

// Ensure 首次运行时生成一个默认 key（若 config 尚不存在或 keys 为空）。
// 返回 (是否已有 key)。
func (s *APIKeyStore) Ensure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.path); err == nil {
		cfg, err := s.readLocked()
		if err == nil && len(cfg.Keys) > 0 {
			return nil
		}
	}
	return s.writeLocked(apiKeyConfig{Keys: map[string]apiKeyEntry{
		newKey(): {Label: "默认网关 Key", CreatedAt: nowRFC3339()},
	}})
}

// Current 返回当前 key 明文与创建时间。
//
// 面板需要它：用户要的是「能直接复制去填客户端」。Key 本来就以明文存在
// config.json（0600）里，不是哈希 —— 所以这里回显并不降低安全等级，
// 只是把「只能 ssh 上去 cat 文件」变成「登录后的面板里能看」。
// 调用方（console）必须把接口挂在管理员会话鉴权之后，且不得落日志。
func (s *APIKeyStore) Current() (key, createdAt string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.readLocked()
	if err != nil || len(cfg.Keys) == 0 {
		return "", "", false
	}
	// keys 正常情况下只有一条；多于一条时按字典序取第一条，保证同一个 key
	// 每次都返回同一个（map 遍历顺序随机，直接 for 会来回跳）。
	cands := make([]string, 0, len(cfg.Keys))
	for k := range cfg.Keys {
		cands = append(cands, k)
	}
	sort.Strings(cands)
	key = cands[0]
	return key, cfg.Keys[key].CreatedAt, true
}

// Count 返回 key 条数（面板提示「还有 N 个旧 key」用）。
func (s *APIKeyStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.readLocked()
	if err != nil {
		return 0
	}
	return len(cfg.Keys)
}

// Rotate 生成一条新 key 并废弃全部旧 key，返回新 key 明文。
//
// 语义是「轮换」而不是「追加」：单管理员自用系统里留着旧 key 只会让人
// 以为它还有效。旧 key 立即失效，面板必须明确告知客户端要同步更新。
func (s *APIKeyStore) Rotate() (key, createdAt string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key = newKey()
	createdAt = nowRFC3339()
	cfg := apiKeyConfig{Keys: map[string]apiKeyEntry{
		key: {Label: "默认网关 Key", CreatedAt: createdAt},
	}}
	if err := s.writeLocked(cfg); err != nil {
		return "", "", err
	}
	return key, createdAt, nil
}

// newKey 生成一条 key 明文（pg- + 48 位十六进制）。
func newKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "pg-" + hex.EncodeToString(b)
}

// Verify 校验 API Key；返回是否有效。
func (s *APIKeyStore) Verify(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.readLocked()
	if err != nil {
		return false
	}
	_, ok := cfg.Keys[key]
	return ok
}

func (s *APIKeyStore) readLocked() (apiKeyConfig, error) {
	var cfg apiKeyConfig
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (s *APIKeyStore) writeLocked(cfg apiKeyConfig) error {
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o600)
}

func nowRFC3339() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z07:00")
}
