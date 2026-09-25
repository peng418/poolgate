package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ensure 必须真生成一条可用的 key —— 面板要拿它填客户端。
func TestAPIKeyEnsureThenCurrent(t *testing.T) {
	s := NewAPIKeyStore(t.TempDir())
	if _, _, ok := s.Current(); ok {
		t.Fatal("还没 Ensure 就不该有 key")
	}
	if err := s.Ensure(); err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	key, created, ok := s.Current()
	if !ok {
		t.Fatal("Ensure 之后必须有 key")
	}
	if !strings.HasPrefix(key, "pg-") || len(key) < 20 {
		t.Fatalf("key 形态不对: %q", key)
	}
	if created == "" {
		t.Fatal("创建时间应透出，面板要显示")
	}
	if !s.Verify(key) {
		t.Fatal("生成的 key 必须能通过校验")
	}
	if n := s.Count(); n != 1 {
		t.Fatalf("默认只该有一条 key，实际 %d", n)
	}
	// 幂等：再 Ensure 一次不能换掉已有 key（换了等于客户端全部掉线）。
	again, _, _ := s.Current()
	if err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	now, _, _ := s.Current()
	if now != again {
		t.Fatalf("Ensure 不应改动已存在的 key：%q → %q", again, now)
	}
}

// Rotate 的语义是「轮换」：新 key 生效，旧 key 立刻失效。
func TestAPIKeyRotateInvalidatesOld(t *testing.T) {
	s := NewAPIKeyStore(t.TempDir())
	if err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	old, _, _ := s.Current()

	key, created, err := s.Rotate()
	if err != nil {
		t.Fatalf("Rotate 失败: %v", err)
	}
	if key == old {
		t.Fatal("Rotate 必须给出不同的 key")
	}
	if created == "" {
		t.Fatal("Rotate 应返回创建时间")
	}
	if s.Verify(old) {
		t.Fatal("旧 key 必须立即失效（留着旧 key 会让人以为它还有效）")
	}
	if !s.Verify(key) {
		t.Fatal("新 key 应可校验")
	}
	if s.Count() != 1 {
		t.Fatalf("轮换后只该剩一条，实际 %d", s.Count())
	}
}

// 多条 key 时 Current 必须稳定返回同一条（map 遍历顺序随机，直接 for 会来回跳）。
func TestAPIKeyCurrentStableWithMultipleKeys(t *testing.T) {
	dir := t.TempDir()
	s := NewAPIKeyStore(dir)
	if err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	raw := `{"keys":{"pg-bbb":{"label":"b","created_at":"2026-01-01T00:00:00Z"},` +
		`"pg-aaa":{"label":"a","created_at":"2026-01-02T00:00:00Z"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	first, _, _ := s.Current()
	if first == "" {
		t.Fatal("应能读到 key")
	}
	for i := 0; i < 50; i++ {
		if k, _, _ := s.Current(); k != first {
			t.Fatalf("Current 结果不稳定：%q → %q", first, k)
		}
	}
	if first != "pg-aaa" {
		t.Fatalf("多条时取字典序第一条才有确定性，实际 %q", first)
	}
}
