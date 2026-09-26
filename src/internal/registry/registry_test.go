package registry

import (
	"context"
	"errors"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// fakeChannel 是测试用的最小实现：只提供 Kind/Spec，其余方法返回未实现。
// 它的存在是为了验证「有实现的启用渠道」才会进 Active()。
type fakeChannel struct{ spec channel.Spec }

func (f fakeChannel) Kind() channel.Kind { return f.spec.Kind }
func (f fakeChannel) Spec() channel.Spec { return f.spec }

func (f fakeChannel) Login(ctx context.Context) (*channel.Credential, error) {
	return nil, errors.New("未实现")
}
func (f fakeChannel) Refresh(ctx context.Context, c *channel.Credential) (*channel.Credential, error) {
	return nil, errors.New("未实现")
}
func (f fakeChannel) Models(ctx context.Context, c *channel.Credential) ([]channel.ModelInfo, error) {
	return nil, errors.New("未实现")
}
func (f fakeChannel) Balance(ctx context.Context, c *channel.Credential) (channel.Balance, error) {
	return channel.Balance{}, errors.New("未实现")
}
func (f fakeChannel) Checkin(ctx context.Context, c *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{}, errors.New("未实现")
}
func (f fakeChannel) Classify(status int, body []byte) errs.Kind { return errs.Parse }

func (f fakeChannel) Chat(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
	return nil, errors.New("未实现")
}

func TestPausedChannelNotDownstream(t *testing.T) {
	// 摘除渠道 = 改 Status；核心代码零改动（红线三）。
	Reset()
	Register(fakeChannel{spec: channel.Spec{
		Kind: channel.QoderCN, DisplayName: "QoderCN", Status: channel.Active,
	}}, channel.Spec{Kind: channel.QoderCN, DisplayName: "QoderCN", Status: channel.Active})
	RegisterSpec(channel.Spec{Kind: "qwenwork", DisplayName: "千问办公", Status: channel.Paused})

	active := Active()
	if len(active) != 1 || active[0].Spec.Kind != channel.QoderCN {
		t.Fatalf("只有启用且有实现的渠道应下发，实际 %+v", active)
	}

	// 暂停渠道仍在注册表里，面板才能显示它与恢复条件。
	spec, ok := GetSpec("qwenwork")
	if !ok {
		t.Fatal("暂停渠道应保留注册位置")
	}
	if spec.Downstream() {
		t.Fatal("暂停渠道不应下发模型")
	}
}

func TestActiveRequiresImplementation(t *testing.T) {
	// 只有声明、没有实现的启用渠道不得参与路由 —— 否则会调到 nil。
	Reset()
	RegisterSpec(channel.Spec{Kind: channel.TraeWork, Status: channel.Active})
	if got := Active(); len(got) != 0 {
		t.Fatalf("无实现的渠道不应进 Active，实际 %d 个", len(got))
	}
}

func TestSpecOnlyChannelHasNoImplementation(t *testing.T) {
	Reset()
	RegisterSpec(channel.Spec{Kind: "qodercom", Status: channel.Paused})
	if _, ok := Get("qodercom"); ok {
		t.Fatal("只有声明的渠道不应给出实现")
	}
	if _, ok := Get("never-registered"); ok {
		t.Fatal("未注册渠道不应给出实现")
	}
}

func TestAllIsSortedForStableUI(t *testing.T) {
	Reset()
	for _, k := range []channel.Kind{channel.TraeWork, channel.QoderCN, channel.WorkBuddyCN} {
		RegisterSpec(channel.Spec{Kind: k, Status: channel.Active})
	}
	all := All()
	if len(all) != 3 {
		t.Fatalf("应返回 3 条，实际 %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Spec.Kind > all[i].Spec.Kind {
			t.Fatalf("顺序不稳定: %+v", all)
		}
	}
}

func TestRegisterOverwrites(t *testing.T) {
	Reset()
	RegisterSpec(channel.Spec{Kind: channel.QoderCN, DisplayName: "旧名", Status: channel.Active})
	RegisterSpec(channel.Spec{Kind: channel.QoderCN, DisplayName: "QoderCN", Status: channel.Paused})
	spec, _ := GetSpec(channel.QoderCN)
	if spec.DisplayName != "QoderCN" || spec.Status != channel.Paused {
		t.Fatalf("重复注册应覆盖: %+v", spec)
	}
}

func TestSetStatusPreservesImplementation(t *testing.T) {
	// 暂停渠道时实现必须保留，否则面板签到/刷新走 credFor→Get 会报「渠道未实现」，
	// 且设置页「渠道开关」的 implemented=false 会让「启用」按钮变成「未实现」无法恢复。
	Reset()
	impl := fakeChannel{spec: channel.Spec{Kind: channel.QoderCN, DisplayName: "QoderCN", Status: channel.Active}}
	Register(impl, impl.Spec())

	SetStatus(channel.QoderCN, channel.Paused)

	got, ok := Get(channel.QoderCN)
	if !ok {
		t.Fatal("暂停后渠道实现不应丢失")
	}
	if got == nil {
		t.Fatal("暂停后渠道实现不应为 nil")
	}
	spec, _ := GetSpec(channel.QoderCN)
	if spec.Downstream() {
		t.Fatal("暂停后不应下发模型")
	}

	// 恢复后应重新下发。
	SetStatus(channel.QoderCN, channel.Active)
	spec, _ = GetSpec(channel.QoderCN)
	if !spec.Downstream() {
		t.Fatal("恢复后应重新下发模型")
	}
	if _, ok := Get(channel.QoderCN); !ok {
		t.Fatal("恢复后实现仍应在")
	}
}
