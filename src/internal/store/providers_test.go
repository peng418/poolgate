package store

// providers_test.go 覆盖「来源名与内置渠道重名」的处理。
//
// 这条规则不是洁癖：来源名同时就是 channel.Kind，挂载走 registry.Register（同 Kind 覆盖），
// 所以一个叫 deepseek 的来源会把内置的 DeepSeek 渠道静默顶掉 —— 面板上看不出任何异常，
// 实际已经换了一个上游。宁可保存时就拒绝，并告诉用户改个名字。

import "testing"

func TestProviderValidateRejectsReservedKind(t *testing.T) {
	cfg := ProviderConfig{
		Name: "deepseek", DisplayName: "DeepSeek 官方",
		BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-test",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatalf("%q 与内置渠道重名，Validate 应当拒绝", cfg.Name)
	}

	cfg.Name = "deepseek-api"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("改名后应当通过，却被拒：%v", err)
	}
}

func TestProviderPutRejectsReservedKindAndDoesNotPersist(t *testing.T) {
	s := NewProviderStore(t.TempDir())
	err := s.Put(ProviderConfig{Name: "qwen", BaseURL: "https://example.test/v1", APIKey: "k"})
	if err == nil {
		t.Fatal("Put 应拒绝与内置渠道重名的来源")
	}
	if _, ok := s.Get("qwen"); ok {
		t.Fatal("被拒的配置不应落盘，否则每次启动都会去挂载它")
	}
}

func TestReserveKindsAdditiveAndCaseInsensitive(t *testing.T) {
	ReserveKinds("SomeNewChannel", "  ")
	if !IsReserved("somenewchannel") {
		t.Fatal("ReserveKinds 之后应大小写无关地命中")
	}
	if IsReserved("") || IsReserved("   ") {
		t.Fatal("空名字不算保留名")
	}
	if !IsReserved("qodercn") {
		t.Fatal("种子集合里的内置渠道名必须仍然算保留名")
	}
}
