package health

import (
	"sync"
	"testing"
	"time"
)

func res(kind, model string, ok bool) Result {
	return Result{Channel: kind, Model: model, OK: ok, CheckedAt: time.Now().UTC()}
}

// 索引取每个模型「各自最近」的结论：抽样体检只覆盖少数模型，
// 不能把上一轮全量体检里其它模型的结论一起丢掉。
func TestSnapshotKeepsNewestVerdictPerModel(t *testing.T) {
	h := NewHistory(10)
	h.Add(Run{Results: []Result{
		res("qodercn", "a", false),
		res("qodercn", "b", true),
	}})
	// 第二轮只抽了 a（抽样体检的形态）
	h.Add(Run{Results: []Result{res("qodercn", "a", true)}})

	s := h.Snapshot()
	if s.Hide(ID("qodercn", "a")) {
		t.Fatal("a 最新结论是通过，不该被隐藏")
	}
	if s.Hide(ID("qodercn", "b")) {
		t.Fatal("b 只有上一轮的通过结论，抽样体检不该抹掉它")
	}
	if !s.Known(ID("qodercn", "b")) {
		t.Fatal("b 应有结论")
	}
	// 反向：最新一轮把 a 判失败
	h.Add(Run{Results: []Result{res("qodercn", "a", false)}})
	if !h.Snapshot().Hide(ID("qodercn", "a")) {
		t.Fatal("a 最新结论是失败，应被隐藏")
	}
}

// 未体检过的模型不能被当成「不可用」藏起来：
// 刚装完还没跑体检时，藏掉未体检的会让 /v1/models 变成空列表。
func TestSnapshotDoesNotHideUnknownModels(t *testing.T) {
	h := NewHistory(10)
	h.Add(Run{Results: []Result{res("qodercn", "known-bad", false)}})
	s := h.Snapshot()

	if s.Hide(ID("qodercn", "never-probed")) {
		t.Fatal("未体检的模型不该被隐藏（「不知道」不等于「不可用」）")
	}
	if s.Known(ID("qodercn", "never-probed")) {
		t.Fatal("未体检的模型不该被算作有结论")
	}
	if !s.Empty() && !s.Hide(ID("qodercn", "known-bad")) {
		t.Fatal("明确失败的模型应被隐藏")
	}
}

// 空历史 → 空索引，且过滤逻辑对任何模型都返回「不隐藏」。
func TestSnapshotEmptyHistory(t *testing.T) {
	s := NewHistory(10).Snapshot()
	if !s.Empty() {
		t.Fatal("没有体检历史时索引应为空")
	}
	if s.Hide(ID("qodercn", "any")) {
		t.Fatal("空索引不能隐藏任何模型")
	}
}

// 残缺记录（缺渠道或缺模型）不进索引：宁可不隐藏，也不要误伤别的模型。
func TestSnapshotSkipsIncompleteResults(t *testing.T) {
	h := NewHistory(10)
	h.Add(Run{Results: []Result{{Model: "no-channel", OK: false}, {Channel: "qodercn", OK: false}}})
	if !h.Snapshot().Empty() {
		t.Fatal("缺渠道/缺模型的记录不该进索引")
	}
}

// Add 之后缓存必须失效，否则新体检结论永远看不到。
func TestSnapshotCacheInvalidatedByAdd(t *testing.T) {
	h := NewHistory(10)
	h.Add(Run{Results: []Result{res("qodercn", "a", true)}})
	if h.Snapshot().Hide(ID("qodercn", "a")) {
		t.Fatal("初始应是通过")
	}
	h.Add(Run{Results: []Result{res("qodercn", "a", false)}})
	if !h.Snapshot().Hide(ID("qodercn", "a")) {
		t.Fatal("Add 之后快照应失效并反映新结论")
	}
}

// 开关关着 → 空索引（装配层把「开关」与「结论」合成一个查询器，
// 调用方不必各判一次开关，少一处能忘的地方）。
func TestHealthGateFollowsSwitch(t *testing.T) {
	h := NewHistory(10)
	h.Add(Run{Results: []Result{res("qodercn", "a", false)}})
	on := false
	q := Health(h, func() bool { return on })

	if q().Hide(ID("qodercn", "a")) {
		t.Fatal("开关关着时不该隐藏任何模型")
	}
	on = true
	if !q().Hide(ID("qodercn", "a")) {
		t.Fatal("开关打开后应按体检结论隐藏")
	}

	// 没装配历史（nil）时也不能崩，且返回空索引。
	if Health(nil, func() bool { return true })().Hide(ID("qodercn", "a")) {
		t.Fatal("没有体检历史时不该隐藏")
	}
}

// 并发读写：网关在请求线程里读，体检在后台写。
func TestSnapshotConcurrentAccess(t *testing.T) {
	h := NewHistory(50)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				h.Add(Run{Results: []Result{res("qodercn", "m", j%2 == 0)}})
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = h.Snapshot()
			}
		}()
	}
	wg.Wait()
}
