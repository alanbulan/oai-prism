package account

import (
	"path/filepath"
	"testing"
	"time"
)

func TestParseDBTime(t *testing.T) {
	want := time.Date(2026, 10, 5, 11, 48, 15, 0, time.UTC)
	for _, s := range []string{"2026-10-05 11:48:15", "2026-10-05T11:48:15Z", "2026-10-05 11:48:15+00:00", " 2026-10-05T11:48:15.000Z "} {
		if got, ok := parseDBTime(s); !ok || !got.Equal(want) {
			t.Errorf("%q → %v %v", s, got, ok)
		}
	}
	if _, ok := parseDBTime("not a time"); ok {
		t.Fatal("非法时间不应解析成功")
	}
}

// 会话与消息读回来要带着真实的时间（早先全是零值，调试台的消息时间显示成 "-"）。
func TestSQLiteStore_ChatTimesRoundTrip(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "c.db"))
	defer s.Close()
	if err := s.SaveChatSession(ChatSessionRecord{ID: "s1", Title: "t", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChatMessage(ChatMessageRecord{ID: "m1", SessionID: "s1", Role: "user", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	sessions, err := s.ListChatSessions()
	if err != nil || len(sessions) != 1 {
		t.Fatal(err)
	}
	msgs, err := s.ListChatMessages("s1")
	if err != nil || len(msgs) != 1 {
		t.Fatal(err)
	}
	for name, ts := range map[string]time.Time{"会话创建": sessions[0].CreatedAt, "会话更新": sessions[0].UpdatedAt, "消息": msgs[0].CreatedAt} {
		if d := time.Since(ts); d < 0 || d > time.Minute {
			t.Errorf("%s时间应是刚才: %v", name, ts)
		}
	}
}
