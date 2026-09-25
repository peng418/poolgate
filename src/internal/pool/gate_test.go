package pool

import (
	"context"
	"sync"
	"testing"
	"time"

	"poolgate/internal/channel"
)

func TestGateSerializesSameAccount(t *testing.T) {
	g := NewGate(nil)
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			rel, err := g.Acquire(context.Background(), channel.QoderCN, "u1")
			if err != nil {
				t.Errorf("Acquire 失败: %v", err)
				return
			}
			mu.Lock()
			order = append(order, "in-"+n)
			mu.Unlock()
			time.Sleep(40 * time.Millisecond) // 模拟一次上游调用
			mu.Lock()
			order = append(order, "out-"+n)
			mu.Unlock()
			rel()
		}(name)
	}
	wg.Wait()
	// 串行：必须成对出现（in-a,out-a,in-b,out-b 或反过来），不能交错。
	if len(order) != 4 {
		t.Fatalf("顺序不对：%v", order)
	}
	for i := 0; i < 4; i += 2 {
		if order[i][:2] != "in" || order[i+1][:3] != "out" || order[i][3:] != order[i+1][4:] {
			t.Fatalf("同一账号被并发执行了：%v", order)
		}
	}
}

func TestGateEnforcesInterval(t *testing.T) {
	g := NewGate(func(channel.Kind) time.Duration { return 150 * time.Millisecond })
	rel, err := g.Acquire(context.Background(), channel.QoderCN, "u1")
	if err != nil {
		t.Fatalf("首次 Acquire 失败: %v", err)
	}
	rel()

	start := time.Now()
	rel2, err := g.Acquire(context.Background(), channel.QoderCN, "u1")
	if err != nil {
		t.Fatalf("第二次 Acquire 失败: %v", err)
	}
	rel2()
	if d := time.Since(start); d < 140*time.Millisecond {
		t.Fatalf("间隔没生效：只等了 %v", d)
	}
}

func TestGateDifferentAccountsRunConcurrently(t *testing.T) {
	g := NewGate(func(channel.Kind) time.Duration { return time.Second })
	relA, err := g.Acquire(context.Background(), channel.QoderCN, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer relA()
	done := make(chan struct{})
	go func() {
		relB, err := g.Acquire(context.Background(), channel.QoderCN, "b")
		if err != nil {
			t.Errorf("另一个账号不该被拦: %v", err)
		} else {
			relB()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("不同账号之间不该互相等待")
	}
}

func TestGateAcquireHonoursContext(t *testing.T) {
	g := NewGate(nil)
	rel, err := g.Acquire(context.Background(), channel.QoderCN, "u1")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(ctx, channel.QoderCN, "u1"); err == nil {
		t.Fatal("账号被占住时应按 ctx 超时返回错误，而不是无限等")
	}
}
