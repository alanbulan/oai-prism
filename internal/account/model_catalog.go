package account

import (
	"database/sql"
	"errors"
	"time"
)

// 上游在售模型清单的落盘（见 internal/facade/catalog.go）。只存最近一次成功拉到的那份：
// 网关重启后立刻可用，上游一时拉不到也不至于退回"什么都不知道"。

const modelCatalogSchema = `
CREATE TABLE IF NOT EXISTS model_catalog (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	data TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);`

// SaveModelCatalog 覆盖保存清单（data 是 JSON）。
func (s *SQLiteStore) SaveModelCatalog(data []byte, at time.Time) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errSQLiteUnavailable
	}
	_, err := s.db.Exec(`
	INSERT INTO model_catalog (id, data, updated_at) VALUES (1, ?, ?)
	ON CONFLICT(id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at;`,
		string(data), at.Unix())
	return err
}

// LoadModelCatalog 读出保存的清单；从没保存过时返回 nil, zero, nil。
func (s *SQLiteStore) LoadModelCatalog() ([]byte, time.Time, error) {
	if s == nil {
		return nil, time.Time{}, errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, time.Time{}, errSQLiteUnavailable
	}
	var (
		data    string
		updated int64
	)
	err := s.db.QueryRow(`SELECT data, updated_at FROM model_catalog WHERE id = 1`).Scan(&data, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	return []byte(data), time.Unix(updated, 0), nil
}
