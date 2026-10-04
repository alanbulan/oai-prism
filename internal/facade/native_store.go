package facade

import (
	"encoding/binary"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
)

// 原生续接绑定落盘。
//
// 上游会话本身一直在（沙箱回收也不丢，见 native.go），丢的只是网关内存里的
// "客户端会话 → 上游会话"绑定。不落盘的话网关一重启，客户端会话只能新建上游会话、
// 把客户端手里的历史补种进去 —— 补种只补最近的 nativeSeedMaxBytes，更早的记忆就没了，
// 长会话还要花几分钟补种。落盘后重启照样接回原来的上游会话，只发增量。

// NativeStore 持久化原生续接的绑定（*account.SQLiteStore 实现了它）。
type NativeStore interface {
	SaveNativeBinding(account.NativeBindingRecord) error
	DeleteNativeBinding(key string) error
	LoadNativeBindings(since time.Time) ([]account.NativeBindingRecord, error)
}

// UseNativeStore 让绑定落盘，并载入未过期的旧绑定（网关重启后接回原来的上游会话）。
func (r *Runner) UseNativeStore(st NativeStore) {
	if st == nil {
		return
	}
	recs, err := st.LoadNativeBindings(time.Now().Add(-nativeBindingTTL))
	if err != nil {
		r.log.Warn("原生续接：载入落盘的会话绑定失败", "err", err)
	}
	nativeBindings.mu.Lock()
	for _, rec := range recs {
		if rec.CID == "" {
			continue
		}
		b := bindingFromRecord(rec)
		if _, ok := nativeBindings.m[rec.Key]; !ok {
			nativeBindings.m[rec.Key] = b
		}
		for _, a := range rec.Aliases {
			if _, ok := nativeBindings.m[a]; !ok {
				nativeBindings.m[a] = b
			}
		}
	}
	nativeBindings.mu.Unlock()
	r.nativeStore = st
	if len(recs) > 0 {
		r.log.Info("原生续接：已载入落盘的会话绑定", "count", len(recs))
	}
}

func bindingFromRecord(rec account.NativeBindingRecord) *nativeBinding {
	delivered := make([]uint64, len(rec.Delivered)/8)
	for i := range delivered {
		delivered[i] = binary.LittleEndian.Uint64(rec.Delivered[i*8:])
	}
	return &nativeBinding{
		key: rec.Key, aliases: rec.Aliases,
		account: rec.Account, project: rec.Project, cid: rec.CID,
		delivered: delivered, sysHash: rec.SysHash, sinceSys: rec.SinceSys,
		summary: rec.Summary, weak: rec.Weak, updated: rec.Updated,
	}
}

// record 是绑定的落盘形态（调用方持 b.mu）。
func (b *nativeBinding) record() account.NativeBindingRecord {
	delivered := make([]byte, 8*len(b.delivered))
	for i, fp := range b.delivered {
		binary.LittleEndian.PutUint64(delivered[i*8:], fp)
	}
	nativeBindings.mu.Lock()
	aliases := append([]string(nil), b.aliases...)
	nativeBindings.mu.Unlock()
	return account.NativeBindingRecord{
		Key: b.key, Aliases: aliases,
		Account: b.account, Project: b.project, CID: b.cid,
		Delivered: delivered, SysHash: b.sysHash, SinceSys: b.sinceSys,
		Summary: b.summary, Weak: b.weak, Updated: b.updated,
	}
}

// persist 把绑定写盘（调用方持 b.mu）；上游会话作废时删除。落盘失败不影响本轮。
func (r *Runner) persistNative(b *nativeBinding) {
	if r == nil || r.nativeStore == nil || b.key == "" {
		return
	}
	var err error
	if b.cid == "" {
		err = r.nativeStore.DeleteNativeBinding(b.key)
	} else {
		err = r.nativeStore.SaveNativeBinding(b.record())
	}
	if err != nil {
		r.log.Warn("原生续接：会话绑定落盘失败", "key", b.key, "err", err)
	}
}

// AliasNative 让 alias 指向 key 的绑定并落盘（见 nativeAlias）。绑定正被下一轮占用时
// 不等它：那一轮提交时会连同别名一起落盘。
func (r *Runner) AliasNative(alias, key string) {
	b := nativeAlias(alias, key)
	if b == nil || r.nativeStore == nil || !b.mu.TryLock() {
		return
	}
	r.persistNative(b)
	b.mu.Unlock()
}
