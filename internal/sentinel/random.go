package sentinel

import (
	"crypto/rand"
	"encoding/binary"
	"math"
)

// v8Random 复刻 V8 的 Math.random：xorshift128+，每次批量填 64 个、倒序取用，
// 结果是 2^-52 的整数倍。goja 自带的 Math.random 是 53 位精度，转成字符串时
// 平均多一位数字 —— dx 会把 ""+Math.random() 的长度采进指纹，必须对齐。
type v8Random struct {
	s0, s1 uint64
	cache  [64]float64
	idx    int
}

func newV8Random() *v8Random {
	var b [16]byte
	_, _ = rand.Read(b[:])
	r := &v8Random{s0: binary.LittleEndian.Uint64(b[:8]), s1: binary.LittleEndian.Uint64(b[8:])}
	if r.s0 == 0 && r.s1 == 0 {
		r.s0 = 1
	}
	return r
}

func (r *v8Random) next() float64 {
	if r.idx == 0 {
		for i := range r.cache {
			s1, s0 := r.s0, r.s1
			r.s0 = s0
			s1 ^= s1 << 23
			s1 ^= s1 >> 17
			s1 ^= s0
			s1 ^= s0 >> 26
			r.s1 = s1
			r.cache[i] = math.Float64frombits(r.s0>>12|0x3FF0000000000000) - 1
		}
		r.idx = len(r.cache)
	}
	r.idx--
	return r.cache[r.idx]
}
