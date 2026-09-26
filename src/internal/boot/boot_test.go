package boot

// boot_test.go 盯两件「一旦被改回去就会静默出错」的事：
//
//  1. 内置渠道必须被登记成**保留名** —— 否则用户可以用一个同名的「接入源」把内置渠道顶掉，
//     面板上什么都看不出来，实际已经换了一个上游（最难查的一类故障）。
//  2. 每个内置渠道的能力声明必须**说实话**：原生工具调用与 toolshim 模拟是两件事，
//     声明错了客户端会拿到乱码（红线一）。这里挑几个关键位钉住。

import (
	"testing"

	"poolgate/internal/adapter/kimi"
	"poolgate/internal/adapter/openaiup"
	"poolgate/internal/channel"
	"poolgate/internal/registry"
	"poolgate/internal/store"
)

// 期望注册的内置渠道（新增渠道时这里要一起加 —— 这条断言的意义正是「别忘了」）。
var wantKinds = []channel.Kind{
	channel.QoderCN, channel.QoderCOM, channel.TraeWork,
	channel.WorkBuddyCN, channel.WorkBuddyAI, channel.QwenWork,
	channel.Qwen, channel.Gemini, channel.DeepSeek,
	channel.Kimi, channel.ChatGLM, channel.Doubao, channel.Yuanbao,
	channel.ChatGPT, channel.Anthropic, channel.CodeBuddy, channel.Copilot,
	channel.Kiro, channel.IFlow, channel.Lingma, channel.Antigravity, channel.Windsurf,
}

func TestRegisterChannelsRegistersAndReserves(t *testing.T) {
	RegisterChannels()

	for _, k := range wantKinds {
		if _, ok := registry.Get(k); !ok {
			t.Errorf("渠道 %q 没有注册实现", k)
		}
		if !store.IsReserved(string(k)) {
			t.Errorf("渠道 %q 没有被登记成保留名：同名来源能把它顶掉", k)
		}
	}

	// 保留名必须挡住「接入源」的保存：这条是用户看得见的第一道闸。
	cfg := store.ProviderConfig{Name: "kimi", BaseURL: "https://example.test/v1", APIKey: "k"}
	if err := cfg.Validate(); err == nil {
		t.Error("与内置渠道重名的来源应被 Validate 拒绝")
	}
}

// 即使磁盘上已经存在同名的旧来源（0.4.3 时代建的），挂载时也必须**跳过**而不是覆盖。
func TestMountProviderSkipsReservedName(t *testing.T) {
	RegisterChannels()

	MountProvider(store.ProviderConfig{
		Name: "kimi", DisplayName: "旧配置里的同名来源",
		BaseURL: "https://example.test/v1", APIKey: "k",
	}, nil)

	got, ok := registry.Get(channel.Kimi)
	if !ok {
		t.Fatal("内置 Kimi 渠道不应被摘掉")
	}
	if _, isProvider := got.(*openaiup.Adapter); isProvider {
		t.Fatal("同名的「接入源」把内置渠道顶掉了 —— 这正是要防的静默换渠道")
	}
	if _, isBuiltin := got.(*kimi.Adapter); !isBuiltin {
		t.Fatalf("内置 Kimi 渠道应保持原样，实际类型 %T", got)
	}

	// 摘除同理：不能因为删掉一条同名来源就把内置渠道从注册表里摘掉。
	UnmountProvider("kimi", nil)
	if _, ok := registry.Get(channel.Kimi); !ok {
		t.Fatal("删除同名来源不该影响内置渠道")
	}
}

// 能力位钉几个关键点：原生 tools / toolshim / 都不支持，三者必须分清楚。
func TestSpecsAreHonest(t *testing.T) {
	RegisterChannels()

	cases := []struct {
		kind       channel.Kind
		tools      bool
		toolsShim  bool
		sseOnly    bool
		reasoning  bool
		categoryIs string
	}{
		// 网页版：没有原生工具调用，靠网关模拟
		{channel.DeepSeek, false, true, true, true, channel.CategoryChat},
		{channel.Kimi, false, true, true, true, channel.CategoryChat},
		{channel.ChatGLM, false, true, true, true, channel.CategoryChat},
		{channel.Doubao, false, true, true, true, channel.CategoryChat},
		{channel.Yuanbao, false, true, true, true, channel.CategoryChat},
		// 原生工具调用（Gemini Code Assist / 通义 CLI）：上游自己就给 reasoning_content，
		// 且都统一向上游要流式（非流式由网关聚合）。
		{channel.Gemini, true, false, true, true, channel.CategoryChat},
		{channel.Qwen, true, false, true, true, channel.CategoryChat},
		// 0.5.0 的登录式渠道：原生 tools 的那些
		{channel.Anthropic, true, false, true, false, channel.CategoryCoding},
		{channel.CodeBuddy, true, false, true, true, channel.CategoryCoding},
		{channel.Copilot, true, false, true, false, channel.CategoryCoding},
		{channel.Kiro, true, false, true, false, channel.CategoryCoding},
		{channel.Lingma, true, false, true, false, channel.CategoryCoding},
		{channel.Antigravity, true, false, true, true, channel.CategoryChat},
		// iFlow 上游流式/非流式都支持，所以不声明 SSEOnly（非流式请求直接透传）
		{channel.IFlow, true, false, false, true, channel.CategoryChat},
		// ChatGPT 网页版：没有可靠的原生工具调用 → toolshim
		{channel.ChatGPT, false, true, true, true, channel.CategoryChat},
		// Windsurf：参考实现已用**付费实弹标定**了 ToolDef 的内部 tag
		// （WindsurfAPI/src/devin-connect.js 的 DEFAULT_DEF_TAGS：outer #10 / name=1 /
		// description=2 / parameters=3，2026-07-04 opus-4-8 实弹确认）→ 走原生工具调用
		// （请求 #10、响应 #6 ChatToolCall），不再是 toolshim 模拟。
		{channel.Windsurf, true, false, true, true, channel.CategoryCoding},
		// 千问办公是编程/办公助手（不是网页聊天），归编程类；但协议是 chat-ws 纯文本，
		// 放不下工具定义 → Tools=false、也不做 toolshim。它是纯文本入口：客户端带
		// tools 时按 ToolsIgnore 丢掉转发（不顶死整轮请求），并落日志（不静默）。
		{channel.QwenWork, false, false, true, true, channel.CategoryCoding},
	}
	// 只有千问办公声明「忽略 tools」：默认档仍是明确拒绝，别的渠道不许悄悄变成忽略。
	if sp, ok := registry.GetSpec(channel.QwenWork); !ok || !sp.ToolsIgnore {
		t.Error("千问办公应声明 ToolsIgnore=true（纯文本入口：带 tools 丢掉转发，不是 400）")
	}
	for _, c := range cases {
		got, ok := registry.GetSpec(c.kind)
		if !ok {
			t.Fatalf("渠道 %q 没有能力声明", c.kind)
		}
		if c.kind != channel.QwenWork && got.ToolsIgnore {
			t.Errorf("%q 不该声明 ToolsIgnore（默认档是明确拒绝，只有纯文本入口才允许忽略）", c.kind)
		}
		if got.Tools != c.tools || got.ToolsShim != c.toolsShim {
			t.Errorf("%q 工具能力位不对：Tools=%v ToolsShim=%v（期望 %v/%v）",
				c.kind, got.Tools, got.ToolsShim, c.tools, c.toolsShim)
		}
		if got.SSEOnly != c.sseOnly {
			t.Errorf("%q SSEOnly 不对：%v", c.kind, got.SSEOnly)
		}
		if got.Reasoning != c.reasoning {
			t.Errorf("%q Reasoning 不对：%v", c.kind, got.Reasoning)
		}
		if c.categoryIs != "" && got.Category != c.categoryIs {
			t.Errorf("%q 分类不对：%q（期望 %q）", c.kind, got.Category, c.categoryIs)
		}
		if got.DisplayName == "" {
			t.Errorf("%q 缺显示名（面板上会显示成空白）", c.kind)
		}
	}
}
