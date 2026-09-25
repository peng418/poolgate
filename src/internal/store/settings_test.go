package store

import (
	"os"
	"path/filepath"
	"testing"
)

// 设置必须能跨进程存活：写盘后重新加载，值要还在。
// 这条用例是为了钉死一个真实 bug —— 早期 decodeSettings 把目标结构体序列化后
// 再解回自己，等于没读文件，表现为「面板显示已保存、重启后全丢」。
func TestSettingsPersistAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s := NewSettingsStore(dir)
	if err := s.Update(func(st *Settings) error {
		st.ListenPort = 5199
		st.StickyRequests = 77
		st.ProbeSamples = 7
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := NewSettingsStore(dir).Get()
	if got.ListenPort != 5199 {
		t.Errorf("listen_port 未持久化：%d", got.ListenPort)
	}
	if got.StickyRequests != 77 {
		t.Errorf("sticky_requests 未持久化：%d", got.StickyRequests)
	}
	if got.ProbeSamples != 7 {
		t.Errorf("probe_samples 未持久化：%d", got.ProbeSamples)
	}
}

// 文件里没写过的键必须保留默认值 —— 尤其是 bool：
// CheckinEnabled 的零值是 false，若被当成「用户关掉了」，升级一次就会静默关闭签到。
func TestSettingsMissingKeysKeepDefaults(t *testing.T) {
	dir := t.TempDir()
	// 只写一个端口，其它字段全部缺席。
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"listen_port": 5200}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := NewSettingsStore(dir).Get()
	if got.ListenPort != 5200 {
		t.Errorf("写过的键应生效：%d", got.ListenPort)
	}
	def := DefaultSettings()
	if got.CheckinEnabled != def.CheckinEnabled {
		t.Errorf("未写过的 checkin_enabled 应保持默认 %v，实际 %v", def.CheckinEnabled, got.CheckinEnabled)
	}
	if got.KeepaliveEnabled != def.KeepaliveEnabled {
		t.Errorf("未写过的 keepalive_enabled 应保持默认 %v，实际 %v", def.KeepaliveEnabled, got.KeepaliveEnabled)
	}
	if got.ProbeSamples != def.ProbeSamples {
		t.Errorf("未写过的 probe_samples 应保持默认 %d，实际 %d", def.ProbeSamples, got.ProbeSamples)
	}
	if got.StickyRequests != def.StickyRequests {
		t.Errorf("未写过的 sticky_requests 应保持默认 %d，实际 %d", def.StickyRequests, got.StickyRequests)
	}
}

// 显式写 false 必须被尊重（不能被默认值盖回去）。
func TestSettingsExplicitFalseIsHonored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"checkin_enabled": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := NewSettingsStore(dir).Get(); got.CheckinEnabled {
		t.Fatal("显式写的 false 应生效")
	}
}

// 渠道开关要能落盘并在重新加载后保留。
func TestChannelOverridePersists(t *testing.T) {
	dir := t.TempDir()
	s := NewSettingsStore(dir)
	if err := s.SetChannelStatus("qodercn", "paused", "上游闸门"); err != nil {
		t.Fatal(err)
	}
	got := NewSettingsStore(dir).Get()
	ov, ok := got.ChannelOverrides["qodercn"]
	if !ok {
		t.Fatal("渠道开关应已落盘")
	}
	if ov.Status != "paused" || ov.Note != "上游闸门" {
		t.Fatalf("开关内容不符：%+v", ov)
	}
}

// 坏文件不应让服务起不来：退回默认值即可。
func TestSettingsCorruptFileFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := NewSettingsStore(dir).Get()
	if got.ListenPort != DefaultSettings().ListenPort {
		t.Fatalf("坏文件应退回默认值，实际 %d", got.ListenPort)
	}
}

// Get 返回的是副本：调用方改动不应影响存储内部状态。
func TestSettingsGetReturnsCopy(t *testing.T) {
	dir := t.TempDir()
	s := NewSettingsStore(dir)
	snap := s.Get()
	snap.ListenPort = 9999
	snap.ChannelOverrides["x"] = ChannelOverride{Status: "paused"}
	if got := s.Get(); got.ListenPort == 9999 {
		t.Fatal("改动副本不应影响存储")
	}
	if _, ok := s.Get().ChannelOverrides["x"]; ok {
		t.Fatal("改动副本的 map 不应影响存储")
	}
}

// 「只下发可用模型」开关要能跨重启存活，且默认必须是关的
// （关着才是老行为；默认打开会让升级上来的机器突然少一批模型，属于静默改行为）。
func TestOnlyHealthyModelsSetting(t *testing.T) {
	if DefaultSettings().OnlyHealthyModels {
		t.Fatal("默认必须是关的：默认开启等于升级即静默改行为")
	}

	dir := t.TempDir()
	s := NewSettingsStore(dir)
	if err := s.Update(func(st *Settings) error {
		st.OnlyHealthyModels = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !NewSettingsStore(dir).Get().OnlyHealthyModels {
		t.Fatal("开关未持久化：重启后又会把失败的模型下发出去")
	}

	// 老配置文件里没有这个键 → 保持默认（关）。
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "settings.json"),
		[]byte(`{"listen_port": 5201}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if NewSettingsStore(dir2).Get().OnlyHealthyModels {
		t.Fatal("文件里没写过这个键时应保持默认的 false")
	}
}

// 每账号最小间隔（防封号）必须能真的落盘 + 读回：它漏过一次白名单，
// 症状是「面板保存了、重启后失效」——留个用例钉住。
func TestSettingsAccountMinIntervalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewSettingsStore(dir)
	if err := s.Update(func(cur *Settings) error {
		cur.AccountMinIntervalSec = map[string]int{"doubao": 3, "qwen": 2}
		return nil
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got := NewSettingsStore(dir).Get()
	if got.AccountMinIntervalSec["doubao"] != 3 || got.AccountMinIntervalSec["qwen"] != 2 {
		t.Fatalf("重启后配置丢了: %+v", got.AccountMinIntervalSec)
	}
}
