package health

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// 本文件测的是「探测的账号纪律」：一条死号不该让整个渠道在面板上变红，
// 但也不能把上游故障算到账号头上。
//
// 起因是 2026-09-26 的真机事故：池里躺着两个 token 早已失效的旧账号，
// Pick 遍历 map、余额相同时顺序随机，于是三个模型全被抽到死号 ——
// 面板三条全红、上游原话都是 40003，用户完全看不出「是哪个号坏了」。

// fakePool 是 AccountSource 的测试实现：按传入顺序发号，并记录回写动作。
type fakePool struct {
	creds []channel.Credential
	// refresh 里有的 uid 才能续期成功（返回这里给的凭证）；没有的当作续期失败。
	refresh map[string]channel.Credential

	mu           sync.Mutex
	pickCalls    int
	refreshCalls []string
	notes        []poolNote
	oks          []string
}

type poolNote struct {
	uid  string
	kind errs.Kind
}

func (f *fakePool) Pick(_ context.Context, _ channel.Kind, tried map[string]bool) (channel.Credential, bool) {
	f.mu.Lock()
	f.pickCalls++
	f.mu.Unlock()
	for _, c := range f.creds {
		if tried != nil && tried[c.UID] {
			continue
		}
		return c, true
	}
	return channel.Credential{}, false
}

func (f *fakePool) RefreshNow(_ context.Context, _ channel.Kind, c channel.Credential) (*channel.Credential, error) {
	f.mu.Lock()
	f.refreshCalls = append(f.refreshCalls, c.UID)
	nc, ok := f.refresh[c.UID]
	f.mu.Unlock()
	if !ok {
		// 与真实池一致：续期失败时把凭证类错误交回去，由调用方决定换号。
		return nil, errs.New(errs.SessionDead, "续期失败")
	}
	return &nc, nil
}

func (f *fakePool) NoteError(_ channel.Kind, uid string, k errs.Kind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, poolNote{uid: uid, kind: k})
}

func (f *fakePool) NoteSuccess(_ channel.Kind, uid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oks = append(f.oks, uid)
}

func (f *fakePool) snapshot() (int, []string, []poolNote, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pickCalls,
		append([]string(nil), f.refreshCalls...),
		append([]poolNote(nil), f.notes...),
		append([]string(nil), f.oks...)
}

// fakeChannel 是 channel.Channel 的测试实现：只关心 Chat 对每个账号的反应。
type fakeChannel struct {
	kind  channel.Kind
	chat  func(c channel.Credential) (channel.Stream, error)
	calls []string
	mu    sync.Mutex
}

func (f *fakeChannel) Kind() channel.Kind                                 { return f.kind }
func (f *fakeChannel) Spec() channel.Spec                                 { return channel.Spec{Kind: f.kind} }
func (f *fakeChannel) Login(context.Context) (*channel.Credential, error) { return nil, nil }
func (f *fakeChannel) Refresh(context.Context, *channel.Credential) (*channel.Credential, error) {
	return nil, nil
}
func (f *fakeChannel) Models(context.Context, *channel.Credential) ([]channel.ModelInfo, error) {
	return nil, nil
}
func (f *fakeChannel) Balance(context.Context, *channel.Credential) (channel.Balance, error) {
	return channel.Balance{}, nil
}
func (f *fakeChannel) Checkin(context.Context, *channel.Credential) (channel.CheckinResult, error) {
	return channel.CheckinResult{}, nil
}
func (f *fakeChannel) Classify(int, []byte) errs.Kind { return errs.Parse }

func (f *fakeChannel) Chat(_ context.Context, c *channel.Credential, _ channel.ChatRequest) (channel.Stream, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c.UID)
	f.mu.Unlock()
	return f.chat(*c)
}

func (f *fakeChannel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// textStream 立刻给一段内容（「收到首个内容」= 通过）。
type textStream struct{ done bool }

func (s *textStream) Next() (channel.ChatCompletionChunk, error) {
	if s.done {
		return channel.ChatCompletionChunk{}, io.EOF
	}
	s.done = true
	c := channel.ChunkChoice{}
	c.Delta.Content = "hi"
	return channel.ChatCompletionChunk{Choices: []channel.ChunkChoice{c}}, nil
}
func (s *textStream) Close() error { return nil }

// 真机上真实出现的那句上游原话（用户从面板「失败明细」里读出来的）。
const realInvalidToken = `{"code":40003,"msg":"Authorization Failed (invalid token)","data":null}`

func invalidTokenErr() error {
	return errs.New(errs.SessionDead, "建会话失败：Authorization Failed (invalid token)").
		WithChannel("deepseek").
		WithUpstream(realInvalidToken)
}

func okStream() (channel.Stream, error) { return &textStream{}, nil }

func targetWith(kind channel.Kind, ch channel.Channel, cred channel.Credential) Target {
	return Target{Kind: kind, Channel: ch, Account: cred, Model: "deepseek-default"}
}

// 死号在前、活号在后：探测必须跳过死号、最终判「渠道可用」，
// 并把死号按 SessionDead 回写池子（面板上能看见是哪个号坏了）。
func TestProbeSkipsDeadAccountAndReportsSuccess(t *testing.T) {
	kind := channel.Kind("deepseek")
	dead := channel.Credential{UID: "A-dead", AccessToken: "t-dead"}
	live := channel.Credential{UID: "B-live", AccessToken: "t-live"}

	ch := &fakeChannel{kind: kind, chat: func(c channel.Credential) (channel.Stream, error) {
		if c.UID == "A-dead" {
			return nil, invalidTokenErr()
		}
		return okStream()
	}}
	p := &fakePool{creds: []channel.Credential{dead, live}}
	r := NewRunner(p, time.Second)

	res := r.ProbeOne(context.Background(), targetWith(kind, ch, dead))

	if !res.OK {
		t.Fatalf("换到活号后应判通过，实际失败：%s / %s", res.Kind, res.Upstream)
	}
	if res.Account != "B-live" {
		t.Fatalf("结论里的账号应是真正跑通的 B-live，实际 %q", res.Account)
	}
	// 「跳过过谁」必须出现在结论里 —— 否则用户只看到一条红/绿，不知道中间发生过什么。
	if len(res.Skipped) != 1 || res.Skipped[0] != "A-dead" {
		t.Fatalf("应记录跳过的死号 [A-dead]，实际 %v", res.Skipped)
	}
	if res.TTFTms < 0 {
		t.Fatalf("通过时应带上首字延迟，实际 %d", res.TTFTms)
	}

	_, refreshCalls, notes, oks := p.snapshot()
	if len(notes) != 1 || notes[0].uid != "A-dead" || notes[0].kind != errs.SessionDead {
		t.Fatalf("死号必须按 SessionDead 回写池子，实际 %+v", notes)
	}
	if len(oks) != 1 || oks[0] != "B-live" {
		t.Fatalf("活号应记一次成功，实际 %v", oks)
	}
	// 凭证类失败要先试续期（与 router.Route 一致），救不回来才换号。
	if len(refreshCalls) != 1 || refreshCalls[0] != "A-dead" {
		t.Fatalf("凭证类失败应先尝试续期一次，实际 %v", refreshCalls)
	}
}

// 续期能救回来的账号不该被换掉：token 到期是账号的常态，不是「号坏了」。
func TestProbeRefreshesBeforeRotating(t *testing.T) {
	kind := channel.Kind("deepseek")
	old := channel.Credential{UID: "A", AccessToken: "t-old"}
	fresh := channel.Credential{UID: "A", AccessToken: "t-new"}

	ch := &fakeChannel{kind: kind, chat: func(c channel.Credential) (channel.Stream, error) {
		if c.AccessToken != "t-new" {
			return nil, invalidTokenErr()
		}
		return okStream()
	}}
	p := &fakePool{
		creds:   []channel.Credential{old},
		refresh: map[string]channel.Credential{"A": fresh},
	}
	r := NewRunner(p, time.Second)

	res := r.ProbeOne(context.Background(), targetWith(kind, ch, old))

	if !res.OK {
		t.Fatalf("续期后应判通过，实际失败：%s / %s", res.Kind, res.Upstream)
	}
	if res.Account != "A" {
		t.Fatalf("续期救回来的还是同一个账号，实际 %q", res.Account)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("续期成功不该记「跳过」，实际 %v", res.Skipped)
	}
	if ch.count() != 2 {
		t.Fatalf("应「失败一次 → 续期 → 再试一次」，实际调用 %d 次", ch.count())
	}
	_, refreshCalls, _, oks := p.snapshot()
	if len(refreshCalls) != 1 {
		t.Fatalf("应尝试续期一次，实际 %v", refreshCalls)
	}
	if len(oks) != 1 || oks[0] != "A" {
		t.Fatalf("续期后成功应记一次成功，实际 %v", oks)
	}
}

// 上游故障（不是账号的问题）不换号：换号只会白折腾，还会把好号冷却掉（红线 D4）。
func TestProbeDoesNotRotateOnUpstreamFault(t *testing.T) {
	kind := channel.Kind("deepseek")
	a := channel.Credential{UID: "A", AccessToken: "ta"}
	b := channel.Credential{UID: "B", AccessToken: "tb"}

	ch := &fakeChannel{kind: kind, chat: func(c channel.Credential) (channel.Stream, error) {
		return nil, errs.New(errs.UpstreamFault, "上游 503").WithUpstream(`{"code":503,"msg":"service unavailable"}`)
	}}
	p := &fakePool{creds: []channel.Credential{a, b}}
	r := NewRunner(p, time.Second)

	res := r.ProbeOne(context.Background(), targetWith(kind, ch, a))

	if res.OK {
		t.Fatal("上游故障不能判通过")
	}
	if ch.count() != 1 {
		t.Fatalf("上游故障不该换号，实际调用 %d 次", ch.count())
	}
	picks, refreshCalls, notes, _ := p.snapshot()
	if picks != 0 {
		// 首个账号由 Target 带进来，只有「要换号」时才会再取号。
		t.Fatalf("上游故障不该再取号，实际取号 %d 次", picks)
	}
	if len(refreshCalls) != 0 {
		t.Fatalf("非凭证类失败不该续期，实际 %v", refreshCalls)
	}
	if len(notes) != 1 || notes[0].kind != errs.UpstreamFault {
		t.Fatalf("应把 UpstreamFault 交给池子（池子内部会忽略），实际 %+v", notes)
	}
	if !strings.Contains(res.Upstream, "503") {
		t.Fatalf("结论里必须带上游原话，实际 %q", res.Upstream)
	}
}

// 池里的号全坏了：结论必须是「真红」，并且带上最后一个账号的上游原话。
func TestProbeAllAccountsDeadKeepsLastUpstream(t *testing.T) {
	kind := channel.Kind("deepseek")
	a := channel.Credential{UID: "A"}
	b := channel.Credential{UID: "B"}

	ch := &fakeChannel{kind: kind, chat: func(c channel.Credential) (channel.Stream, error) {
		return nil, errs.New(errs.SessionDead, "建会话失败："+c.UID).
			WithUpstream(`{"code":40003,"msg":"Authorization Failed (invalid token)","data":null,"uid":"` + c.UID + `"}`)
	}}
	p := &fakePool{creds: []channel.Credential{a, b}}
	r := NewRunner(p, time.Second)

	res := r.ProbeOne(context.Background(), targetWith(kind, ch, a))

	if res.OK {
		t.Fatal("全坏时必须判失败")
	}
	if res.Account != "B" {
		t.Fatalf("结论应落在最后一个试过的账号 B，实际 %q", res.Account)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "A" {
		t.Fatalf("应记录跳过的 A，实际 %v", res.Skipped)
	}
	if !strings.Contains(res.Upstream, `"uid":"B"`) {
		t.Fatalf("必须带最后一个账号的上游原话，实际 %q", res.Upstream)
	}
	picks, _, notes, oks := p.snapshot()
	if picks != 2 {
		t.Fatalf("两个号都该试过（取号 2 次），实际 %d", picks)
	}
	if len(notes) != 2 || len(oks) != 0 {
		t.Fatalf("两个号都该回写失败、且无成功，实际 notes=%+v oks=%v", notes, oks)
	}
}

// 换号有上限：池子很大时不能把一次探测拖成「每个号都敲一遍」。
func TestProbeStopsAtMaxAccountAttempts(t *testing.T) {
	kind := channel.Kind("deepseek")
	var creds []channel.Credential
	for _, uid := range []string{"A", "B", "C", "D"} {
		creds = append(creds, channel.Credential{UID: uid})
	}
	ch := &fakeChannel{kind: kind, chat: func(c channel.Credential) (channel.Stream, error) {
		return nil, invalidTokenErr()
	}}
	p := &fakePool{creds: creds}
	r := NewRunner(p, time.Second)

	res := r.ProbeOne(context.Background(), targetWith(kind, ch, creds[0]))

	if res.OK {
		t.Fatal("全坏时必须判失败")
	}
	if ch.count() != maxAccountAttempts {
		t.Fatalf("最多试 %d 个账号，实际调用 %d 次", maxAccountAttempts, ch.count())
	}
	if len(res.Skipped) != maxAccountAttempts-1 {
		t.Fatalf("应跳过 %d 个，实际 %v", maxAccountAttempts-1, res.Skipped)
	}
	if res.Account != "C" {
		t.Fatalf("结论应落在第 %d 个账号 C，实际 %q", maxAccountAttempts, res.Account)
	}
}

// 「有帧但没内容」不是账号的问题（判据给 UpstreamFault），不该触发换号。
func TestProbeEmptyStreamDoesNotRotate(t *testing.T) {
	kind := channel.Kind("deepseek")
	a := channel.Credential{UID: "A"}
	b := channel.Credential{UID: "B"}
	ch := &fakeChannel{kind: kind, chat: func(channel.Credential) (channel.Stream, error) {
		return &emptyStream{}, nil
	}}
	p := &fakePool{creds: []channel.Credential{a, b}}
	r := NewRunner(p, time.Second)

	res := r.ProbeOne(context.Background(), targetWith(kind, ch, a))

	if res.OK {
		t.Fatal("空流不能判通过")
	}
	if ch.count() != 1 {
		t.Fatalf("空流不该换号，实际调用 %d 次", ch.count())
	}
	if res.Kind != string(errs.UpstreamFault) {
		t.Fatalf("空流的分类应是 UpstreamFault，实际 %q", res.Kind)
	}
}

// emptyStream 是「HTTP 200 但没有内容」的最小形态。
type emptyStream struct{}

func (s *emptyStream) Next() (channel.ChatCompletionChunk, error) {
	return channel.ChatCompletionChunk{}, io.EOF
}
func (s *emptyStream) Close() error { return nil }
