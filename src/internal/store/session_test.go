package store

import (
	"testing"
	"time"
)

func TestSessionLifecycle(t *testing.T) {
	s := NewSessionStore()
	sess, err := s.Create(false)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Valid(sess.Token) {
		t.Fatal("新会话应有效")
	}
	s.Destroy(sess.Token)
	if s.Valid(sess.Token) {
		t.Fatal("注销后不应有效")
	}
}

func TestRememberExtendsTTL(t *testing.T) {
	s := NewSessionStore()
	short, _ := s.Create(false)
	long, _ := s.Create(true)
	if long.ExpiresAt.Sub(short.ExpiresAt) <= 24*time.Hour {
		t.Fatalf("记住我应有 30 天有效期，差值 %v", long.ExpiresAt.Sub(short.ExpiresAt))
	}
}

func TestEmptyOrUnknownTokenRejected(t *testing.T) {
	s := NewSessionStore()
	if s.Valid("") {
		t.Fatal("空令牌不应有效")
	}
	if s.Valid("not-a-real-token") {
		t.Fatal("未知令牌不应有效")
	}
}

func TestLockAfterMaxFailures(t *testing.T) {
	// 需求 F7.1d：连续 5 次失败锁 15 分钟。
	s := NewSessionStore()
	var lastRemain int
	var locked bool
	for i := 0; i < MaxFailures; i++ {
		lastRemain, locked = s.RecordFailure("192.0.2.7")
	}
	if !locked {
		t.Fatal("达到上限后应锁定")
	}
	if lastRemain != 0 {
		t.Fatalf("锁定后剩余次数应为 0，实际 %d", lastRemain)
	}
	remain, locked := s.CheckLock("192.0.2.7")
	if !locked {
		t.Fatal("锁定窗口内 CheckLock 应报告锁定")
	}
	if remain <= 0 || remain > LockFor {
		t.Fatalf("剩余锁定时间不合理: %v", remain)
	}
}

func TestFailuresArePerIP(t *testing.T) {
	s := NewSessionStore()
	for i := 0; i < MaxFailures; i++ {
		s.RecordFailure("10.0.0.1")
	}
	if _, locked := s.CheckLock("10.0.0.2"); locked {
		t.Fatal("一个 IP 的失败不应牵连其他 IP")
	}
}

func TestClearFailuresOnSuccess(t *testing.T) {
	s := NewSessionStore()
	s.RecordFailure("10.0.0.3")
	s.ClearFailures("10.0.0.3")
	if _, locked := s.CheckLock("10.0.0.3"); locked {
		t.Fatal("登录成功后应清空失败计数")
	}
}
