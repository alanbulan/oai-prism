package account

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteNativeBindings_RoundTripAndPrune(t *testing.T) {
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "a.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Truncate(time.Second)
	fresh := NativeBindingRecord{Key: "h:s1", Aliases: []string{"cid:cdx1_a"}, Account: "acct", Project: "proj", CID: "cdx1_a",
		Delivered: []byte{1, 2, 3, 4, 5, 6, 7, 8}, SysHash: 1<<63 + 5, SinceSys: 9, Summary: "摘要", Weak: true, Updated: now}
	old := NativeBindingRecord{Key: "h:old", CID: "cdx1_b", Updated: now.Add(-10 * 24 * time.Hour)}
	for _, r := range []NativeBindingRecord{fresh, old} {
		if err := s.SaveNativeBinding(r); err != nil {
			t.Fatal(err)
		}
	}
	fresh.SinceSys = 10 // 再存一次是更新
	if err := s.SaveNativeBinding(fresh); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadNativeBindings(now.Add(-7 * 24 * time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("应只载入未过期的一条: %v %+v", err, got)
	}
	g := got[0]
	if g.Key != "h:s1" || g.CID != "cdx1_a" || g.SysHash != fresh.SysHash || g.SinceSys != 10 || !g.Weak ||
		string(g.Delivered) != string(fresh.Delivered) || len(g.Aliases) != 1 || g.Summary != "摘要" || !g.Updated.Equal(now) {
		t.Fatalf("字段往返不一致: %+v", g)
	}
	if all, _ := s.LoadNativeBindings(time.Time{}); len(all) != 1 {
		t.Fatalf("过期记录应在载入时被清掉: %d", len(all))
	}
	if err := s.DeleteNativeBinding("h:s1"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.LoadNativeBindings(time.Time{}); len(all) != 0 {
		t.Fatal("删除后不应再载入")
	}
}
