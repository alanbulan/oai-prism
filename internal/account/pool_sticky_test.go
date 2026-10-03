package account

import (
	"context"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

// 粘性命中必须续期：TTL 按最后一次使用计，否则活跃会话满 TTL 后照样被换号。
func TestPool_StickyRenewedOnHit(t *testing.T) {
	p := testPool(t, "round_robin",
		config.AccountConfig{ID: "a", AccessToken: "t1"},
		config.AccountConfig{ID: "b", AccessToken: "t2"},
	)
	p.sticky.ttl = 300 * time.Millisecond

	l, err := p.Acquire(context.Background(), "sess")
	if err != nil {
		t.Fatal(err)
	}
	owner := l.Account.ID
	l.Release()
	for i := 0; i < 6; i++ { // 总时长远超 TTL，但每次都在 TTL 内再次命中
		time.Sleep(100 * time.Millisecond)
		l, err := p.Acquire(context.Background(), "sess")
		if err != nil {
			t.Fatal(err)
		}
		if l.Account.ID != owner {
			t.Fatalf("第 %d 次命中换号了：%s != %s", i, l.Account.ID, owner)
		}
		l.Release()
	}
}

// 粘性账号只是满并发时应等它空出槽位，而不是立刻改绑到别的账号。
func TestPool_StickyWaitsForBusyAccount(t *testing.T) {
	p := testPool(t, "least_inflight",
		config.AccountConfig{ID: "a", AccessToken: "t1", MaxConcurrency: 1},
		config.AccountConfig{ID: "b", AccessToken: "t2", MaxConcurrency: 1},
	)
	l1, err := p.Acquire(context.Background(), "sess")
	if err != nil {
		t.Fatal(err)
	}
	owner := l1.Account.ID
	go func() {
		time.Sleep(150 * time.Millisecond)
		l1.Release()
	}()
	start := time.Now()
	l2, err := p.Acquire(context.Background(), "sess")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Release()
	if l2.Account.ID != owner {
		t.Fatalf("粘性账号繁忙时被改绑：%s != %s", l2.Account.ID, owner)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("应等待粘性账号释放")
	}
}

// 并发释放不能把在途计数减成负数。
func TestAccount_ReleaseNeverNegative(t *testing.T) {
	p := testPool(t, "least_inflight", config.AccountConfig{ID: "a", AccessToken: "t1"})
	a := p.Get("a")
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func() { a.Release(); done <- struct{}{} }()
	}
	for i := 0; i < 50; i++ {
		<-done
	}
	if a.Inflight() != 0 {
		t.Fatalf("在途计数 = %d", a.Inflight())
	}
}
