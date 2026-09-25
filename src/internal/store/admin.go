// Package store 负责管理员凭证、会话与状态的持久化。
//
// 范围裁定（设计方案 §11.1）：单管理员密码，不做多用户/注册/找回密码/角色权限。
// 忘记密码走命令行 poolgate admin reset-password。
// 控制台登录是本地密码，与渠道 OAuth 授权是两套独立的东西。
package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// ErrNotInitialized 表示还没有管理员账号 —— 首次运行的判据是「密码文件不存在」，
// 不是「有没有用户表」。此时应进入设置密码向导，而不是落到空面板。
var ErrNotInitialized = errors.New("poolgate: 尚未设置管理员密码")

// 密码哈希参数（Argon2id）。并存迁移期本机负载很低，取值偏保守以保证离线安全。
const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Admin 是管理员账号的落盘形态。明文密码永不落盘、永不入日志。
type Admin struct {
	Hash      []byte    `json:"hash"`
	Salt      []byte    `json:"salt"`
	CreatedAt time.Time `json:"created_at"`
}

// AdminStore 管理管理员凭证。
type AdminStore struct {
	mu   sync.Mutex
	path string
}

// NewAdminStore 建立一个管理员凭证存储。
func NewAdminStore(dir string) *AdminStore {
	return &AdminStore{path: filepath.Join(dir, "admin.json")}
}

// Exists 报告是否已设置管理员密码。这是「是否首次运行」的唯一判据。
func (s *AdminStore) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

// Path 返回凭证文件路径（供日志与提示使用，不含任何秘密）。
func (s *AdminStore) Path() string { return s.path }

// Set 首次设置密码。已存在时返回错误 —— 设置接口只能成功一次，之后永久关闭。
func (s *AdminStore) Set(password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.path); err == nil {
		return errors.New("poolgate: 管理员密码已设置，如需重置请用 admin reset-password")
	}
	a, err := hashPassword(password)
	if err != nil {
		return err
	}
	return s.write(a)
}

// Reset 强制重设密码（命令行用）。
func (s *AdminStore) Reset(password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := hashPassword(password)
	if err != nil {
		return err
	}
	return s.write(a)
}

// Verify 校验密码。使用恒定时间比较，避免时序侧信道。
func (s *AdminStore) Verify(password string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, ErrNotInitialized
		}
		return false, fmt.Errorf("读取管理员凭证失败: %w", err)
	}
	var a Admin
	if err := json.Unmarshal(raw, &a); err != nil {
		return false, fmt.Errorf("管理员凭证格式损坏: %w", err)
	}
	want := argon2.IDKey([]byte(password), a.Salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return subtle.ConstantTimeCompare(want, a.Hash) == 1, nil
}

func (s *AdminStore) write(a *Admin) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	// 0600：凭证文件只对属主可读（D5）。
	return os.WriteFile(s.path, raw, 0o600)
}

func hashPassword(password string) (*Admin, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("生成随机盐失败: %w", err)
	}
	h := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return &Admin{Hash: h, Salt: salt, CreatedAt: time.Now().UTC()}, nil
}
