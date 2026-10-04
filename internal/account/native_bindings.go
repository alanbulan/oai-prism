package account

import (
	"encoding/json"
	"fmt"
	"time"
)

// NativeBindingRecord 是原生续接绑定（客户端会话 → 上游会话）的持久化形态，见
// internal/facade/native.go。上游会话本身一直在；绑定落盘后网关重启照样接回去，
// 不必新建会话再补种。
type NativeBindingRecord struct {
	Key       string    `json:"-"`
	Aliases   []string  `json:"aliases,omitempty"`
	Account   string    `json:"account"`
	Project   string    `json:"project"`
	CID       string    `json:"cid"`
	Delivered []byte    `json:"delivered,omitempty"` // 已送达条目的指纹（小端 uint64 序列）
	SysHash   uint64    `json:"sys_hash,omitempty"`
	SinceSys  int       `json:"since_sys,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Weak      bool      `json:"weak,omitempty"`
	Updated   time.Time `json:"-"`
}

const nativeBindingsSchema = `
CREATE TABLE IF NOT EXISTS native_bindings (
	key TEXT PRIMARY KEY,
	data TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);`

// SaveNativeBinding 插入或更新一条绑定。
func (s *SQLiteStore) SaveNativeBinding(rec NativeBindingRecord) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errSQLiteUnavailable
	}
	_, err = s.db.Exec(`
	INSERT INTO native_bindings (key, data, updated_at) VALUES (?, ?, ?)
	ON CONFLICT(key) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at;`,
		rec.Key, string(data), rec.Updated.Unix())
	return err
}

// DeleteNativeBinding 删除一条绑定（上游会话已不可用时）。
func (s *SQLiteStore) DeleteNativeBinding(key string) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errSQLiteUnavailable
	}
	_, err := s.db.Exec(`DELETE FROM native_bindings WHERE key = ?`, key)
	return err
}

// LoadNativeBindings 读出 since 之后更新过的绑定，并清掉更早的。
func (s *SQLiteStore) LoadNativeBindings(since time.Time) ([]NativeBindingRecord, error) {
	if s == nil {
		return nil, errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, errSQLiteUnavailable
	}
	if _, err := s.db.Exec(`DELETE FROM native_bindings WHERE updated_at < ?`, since.Unix()); err != nil {
		return nil, fmt.Errorf("清理过期绑定失败: %w", err)
	}
	rows, err := s.db.Query(`SELECT key, data, updated_at FROM native_bindings`)
	if err != nil {
		return nil, fmt.Errorf("查询 native_bindings 失败: %w", err)
	}
	defer rows.Close()
	var out []NativeBindingRecord
	for rows.Next() {
		var (
			key, data string
			updated   int64
		)
		if err := rows.Scan(&key, &data, &updated); err != nil {
			return nil, fmt.Errorf("读取绑定行失败: %w", err)
		}
		var rec NativeBindingRecord
		if json.Unmarshal([]byte(data), &rec) != nil {
			continue
		}
		rec.Key, rec.Updated = key, time.Unix(updated, 0)
		out = append(out, rec)
	}
	return out, rows.Err()
}
