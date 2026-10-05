package account

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

// 优先级：先用最高的一档；这一档满并发 / 冷却时才轮到下一档。
func TestPool_PriorityTiers(t *testing.T) {
	for _, strategy := range []string{"least_inflight", "round_robin", "random", "weighted"} {
		p := testPool(t, strategy,
			config.AccountConfig{ID: "low", AccessToken: "t1", MaxConcurrency: 4},
			config.AccountConfig{ID: "high", AccessToken: "t2", MaxConcurrency: 1, Priority: 10},
			config.AccountConfig{ID: "mid", AccessToken: "t3", MaxConcurrency: 1, Priority: 5},
		)
		order := []string{}
		var leases []*Lease
		for i := 0; i < 3; i++ {
			l, err := p.Acquire(context.Background(), "")
			if err != nil {
				t.Fatalf("%s: %v", strategy, err)
			}
			order = append(order, l.Account.ID)
			leases = append(leases, l)
		}
		if order[0] != "high" || order[1] != "mid" || order[2] != "low" {
			t.Fatalf("%s: 应按优先级逐档使用，得到 %v", strategy, order)
		}
		for _, l := range leases {
			l.Release()
		}

		// 高优先级的冷却了：用下一档
		for _, a := range p.Accounts() {
			if a.ID == "high" {
				a.Cooldown(time.Now(), time.Minute)
			}
		}
		l, err := p.Acquire(context.Background(), "")
		if err != nil || l.Account.ID != "mid" {
			t.Fatalf("%s: 高优先级冷却时应用下一档: %v %v", strategy, l, err)
		}
		l.Release()
	}
}

// 停用的账号不进调度池；DisabledStats 给列表用。
func TestPool_DisabledAccountsExcluded(t *testing.T) {
	off := false
	p := testPool(t, "least_inflight",
		config.AccountConfig{ID: "on", AccessToken: "t1"},
		config.AccountConfig{ID: "off", AccessToken: "t2", Enabled: &off, Priority: 9},
	)
	if p.Size() != 1 || p.Accounts()[0].ID != "on" {
		t.Fatalf("停用的账号不应进池: %d", p.Size())
	}
	s := DisabledStats(config.AccountConfig{ID: "off", AccessToken: "t2", Enabled: &off, Priority: 9, MaxConcurrency: 2})
	if !s.Disabled || s.State != "disabled" || s.Enabled || s.Priority != 9 || !s.HasToken || s.Name != "off" {
		t.Fatalf("停用账号的快照不对: %+v", s)
	}
}

// 启用状态与优先级存进 SQLite；重新导入（没给 enabled）不会把停用的账号悄悄启用。
func TestSQLiteStore_EnabledAndPriority(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "a.db"))
	defer s.Close()
	off := false
	if err := s.SaveAccount(config.AccountConfig{ID: "a", AccessToken: "t", Enabled: &off, Priority: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAccount(config.AccountConfig{ID: "b", AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	get := func(id string) config.AccountConfig {
		list, err := s.Load()
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range list {
			if a.ID == id {
				return a
			}
		}
		t.Fatalf("没有账号 %s", id)
		return config.AccountConfig{}
	}
	if a := get("a"); a.IsEnabled() || a.Priority != 7 {
		t.Fatalf("应读回停用与优先级 7: %+v", a)
	}
	if b := get("b"); !b.IsEnabled() || b.Enabled != nil || b.Priority != 0 {
		t.Fatalf("没设置时默认启用、优先级 0: %+v", b)
	}

	// 不带 enabled 再存一次：保留停用
	if err := s.SaveAccount(config.AccountConfig{ID: "a", AccessToken: "t2", Priority: 7}); err != nil {
		t.Fatal(err)
	}
	if a := get("a"); a.IsEnabled() {
		t.Fatal("没给 enabled 时应保留原来的停用状态")
	}

	// Dashboard 的修改：启用、改优先级
	a := get("a")
	patched, err := MergeAccountPatch(a, []byte(`{"enabled":true,"priority":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAccount(patched); err != nil {
		t.Fatal(err)
	}
	if a := get("a"); !a.IsEnabled() || a.Priority != 3 {
		t.Fatalf("应改成启用、优先级 3: %+v", a)
	}
	// 只改名字不动其它
	patched, _ = MergeAccountPatch(get("a"), []byte(`{"name":"x"}`))
	if !patched.IsEnabled() || patched.Priority != 3 {
		t.Fatalf("没给的字段不应变: %+v", patched)
	}
}
