package store

import (
	"os"
	"path/filepath"
	"testing"
)

func newAdmin(t *testing.T) *AdminStore {
	t.Helper()
	return NewAdminStore(t.TempDir())
}

func TestFirstRunDetectedByMissingFile(t *testing.T) {
	// 首次运行的判据是「密码文件不存在」，不是「有没有用户表」。
	a := newAdmin(t)
	if a.Exists() {
		t.Fatal("新目录不应已初始化")
	}
	if _, err := a.Verify("whatever"); err != ErrNotInitialized {
		t.Fatalf("应返回 ErrNotInitialized，实际 %v", err)
	}
}

func TestSetThenVerify(t *testing.T) {
	a := newAdmin(t)
	if err := a.Set("PooLGate-2026!"); err != nil {
		t.Fatalf("首次设置失败: %v", err)
	}
	ok, err := a.Verify("PooLGate-2026!")
	if err != nil || !ok {
		t.Fatalf("正确密码应通过: ok=%v err=%v", ok, err)
	}
	if ok, _ := a.Verify("wrong"); ok {
		t.Fatal("错误密码不应通过")
	}
}

func TestSetOnlyOnce(t *testing.T) {
	// 设置接口只能成功一次，之后永久关闭（§11.1）。
	a := newAdmin(t)
	if err := a.Set("first-pw-123456"); err != nil {
		t.Fatal(err)
	}
	if err := a.Set("second-pw-123456"); err == nil {
		t.Fatal("已初始化后 Set 必须失败")
	}
}

func TestSaltIsUnique(t *testing.T) {
	// 相同密码两次哈希必须不同（盐不同），否则彩虹表攻击面扩大。
	a := newAdmin(t)
	if err := a.Set("same-pw-123456"); err != nil {
		t.Fatal(err)
	}
	raw1, _ := os.ReadFile(a.Path())
	if err := a.Reset("same-pw-123456"); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(a.Path())
	if string(raw1) == string(raw2) {
		t.Fatal("相同密码的两次哈希不应相同")
	}
}

func TestCredFileIsPrivate(t *testing.T) {
	// D5：凭证文件 0600。
	a := newAdmin(t)
	if err := a.Set("pw-1234567890"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(a.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("凭证文件权限应为 0600，实际 %o", fi.Mode().Perm())
	}
}

func TestNestedDirCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conf", "poolgate")
	a := NewAdminStore(dir)
	if err := a.Set("pw-1234567890"); err != nil {
		t.Fatalf("应自动创建目录: %v", err)
	}
	if !a.Exists() {
		t.Fatal("凭证应存在")
	}
}
