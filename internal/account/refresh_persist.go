package account

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/creds"
)

// RefreshPersister 把刷新后的凭据回写 SQLite，挂在 Pool.SetOnRefreshed 上。
//
// 为什么必须回写：auth.openai.com/oauth/token 每次刷新都会轮换 refresh_token，
// 旧的立即作废。只存内存的话，重启后读回"过期 access_token + 已用过的
// refresh_token"，下一次刷新必然 invalid_grant / refresh_token_reused，账号报废。
// 同理，Dashboard 编辑账号会从 SQLite 整池重建，不回写也会丢。
type RefreshPersister struct {
	store *SQLiteStore
	min   time.Duration // 同一账号两次回写的最小间隔；0 = 不节流
	log   *slog.Logger
	now   func() time.Time

	mu   sync.Mutex
	last map[string]persistMark
}

// persistMark 记录某账号上一次成功回写的时间与当时写入的 refresh_token。
type persistMark struct {
	at           time.Time
	refreshToken string
}

// persistRetries 回写失败的重试次数（外部进程持有库锁时会 SQLITE_BUSY）。
const persistRetries = 3

// NewRefreshPersister 构造回写器。minInterval 对应 creds.persist_refresh_min。
func NewRefreshPersister(store *SQLiteStore, minInterval time.Duration, log *slog.Logger) *RefreshPersister {
	if log == nil {
		log = slog.Default()
	}
	return &RefreshPersister{
		store: store,
		min:   minInterval,
		log:   log,
		now:   time.Now,
		last:  make(map[string]persistMark),
	}
}

// Persist 实现 RefreshHook：把 c 的 token 列（及 plan / email / oauth_client_id）写回 id 对应的行。
//
// 节流规则：距上次成功回写不足 min 且 refresh_token 没变时跳过 —— 库里的
// refresh_token 仍然有效，重启后最多多刷新一次。refresh_token 一旦轮换就必须
// 立刻落盘，不受节流限制，否则节流窗口内重启照样报废。
func (r *RefreshPersister) Persist(id string, c *creds.Credential) {
	if r == nil || c == nil {
		return
	}
	// 整段持锁：不同账号的回调可能并发，而 SQLiteStore 本身也是单连接串行，
	// 这里多一把锁不增加实际等待。
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	if m, ok := r.last[id]; ok && r.min > 0 &&
		c.RefreshToken == m.refreshToken && now.Sub(m.at) < r.min {
		r.log.Debug("凭据回写节流，跳过（refresh_token 未轮换）",
			"account", id, "since_last", now.Sub(m.at).Round(time.Second), "min", r.min)
		return
	}

	upd := TokenUpdate{
		AccessToken:   c.AccessToken,
		RefreshToken:  c.RefreshToken,
		Plan:          c.Plan,
		Email:         c.Email,
		OAuthClientID: c.OAuthClientID,
	}
	var err error
	for i := 0; i < persistRetries; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * 200 * time.Millisecond)
		}
		err = r.store.UpdateTokens(id, upd)
		if err == nil || errors.Is(err, ErrAccountNotInStore) || errors.Is(err, errSQLiteUnavailable) {
			break
		}
	}

	switch {
	case err == nil:
		r.last[id] = persistMark{at: now, refreshToken: c.RefreshToken}
		r.log.Info("刷新后的凭据已回写 SQLite", "account", id)
	case errors.Is(err, ErrAccountNotInStore):
		// 来自 config.yaml 的静态账号不在库里，无处可写。
		lvl := slog.LevelDebug
		if c.RefreshToken != "" {
			lvl = slog.LevelWarn
		}
		r.log.Log(context.Background(), lvl, "账号不在 SQLite 中，刷新结果仅保存在内存，重启后丢失", "account", id)
	default:
		// 内存里的新凭据仍然正确，进程不退出就不受影响；但此刻重启 = 账号报废。
		r.log.Error("刷新后的凭据回写 SQLite 失败：重启后该账号将无法续期，需要重新授权",
			"account", id, "err", err)
	}
}
