package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// 登录失败锁定策略（需求 F7.1d）。
const (
	MaxFailures = 5
	LockFor     = 15 * time.Minute
)

// ErrLocked 表示处于锁定窗口内。
var ErrLocked = errors.New("poolgate: 登录已锁定")

// Session 是一个已登录会话。
type Session struct {
	Token     string
	CreatedAt time.Time
	ExpiresAt time.Time
	// Remember 为真时有效期 30 天，否则为单浏览器会话（进程内保留 12 小时）。
	Remember bool
}

// SessionStore 管理会话与失败计数。
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]Session

	// 失败计数按来源 IP 聚合；锁定期间页面仍可打开，但登录被拒。
	failures map[string]failRecord
}

type failRecord struct {
	count       int
	firstAt     time.Time
	lockedUntil time.Time
}

// NewSessionStore 建立会话存储。
func NewSessionStore() *SessionStore {
	return &SessionStore{
		sessions: map[string]Session{},
		failures: map[string]failRecord{},
	}
}

// Create 签发一个新会话。
func (s *SessionStore) Create(remember bool) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	ttl := 12 * time.Hour
	if remember {
		ttl = 30 * 24 * time.Hour
	}
	sess := Session{
		Token:     hex.EncodeToString(b),
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
		Remember:  remember,
	}
	s.sessions[sess.Token] = sess
	return sess, nil
}

// Valid 校验会话令牌；过期会话顺带清理。
func (s *SessionStore) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// Destroy 注销会话（登出）。
func (s *SessionStore) Destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// CheckLock 在尝试登录前调用；返回剩余锁定时间。
func (s *SessionStore) CheckLock(ip string) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.failures[ip]
	if !ok {
		return 0, false
	}
	remain := time.Until(r.lockedUntil)
	return remain, remain > 0
}

// RecordFailure 记录一次失败，返回剩余可尝试次数与是否被锁定。
func (s *SessionStore) RecordFailure(ip string) (remain int, locked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.failures[ip]
	if r.firstAt.IsZero() || time.Since(r.firstAt) > LockFor {
		r = failRecord{firstAt: time.Now().UTC()}
	}
	r.count++
	if r.count >= MaxFailures {
		r.lockedUntil = time.Now().UTC().Add(LockFor)
		locked = true
	}
	s.failures[ip] = r
	remain = MaxFailures - r.count
	if remain < 0 {
		remain = 0
	}
	return remain, locked
}

// ClearFailures 登录成功后清空该来源的失败计数。
func (s *SessionStore) ClearFailures(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failures, ip)
}

// DestroyOthers 注销除 keep 之外的全部会话，返回清掉的条数。
//
// 改密码时必须调用：密码是唯一凭据，改完若旧会话还能用，等于「改了密码但没锁门」。
// keep 传当前会话令牌，让正在操作的这个人不用重新登录。
func (s *SessionStore) DestroyOthers(keep string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for tok := range s.sessions {
		if tok == keep {
			continue
		}
		delete(s.sessions, tok)
		n++
	}
	return n
}
