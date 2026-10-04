// Package ydoc 是 Yjs 文档更新（v1 编码）的最小编解码，只覆盖 Prism 项目文档用到的那部分：
// 根级 Y.Map（"content" 文件树、"settings"）下挂 Y.Map 节点，节点字段是字符串 / 布尔等基本值。
//
// Prism 的项目文件树存在 Y-Sweet 托管的 Yjs 文档里，沙箱按这棵树同步工作区。
// 只把文件传到 /api/project-files/upload 而不登记进树，沙箱里就没有这个文件 ——
// 官方前端的做法是"上传 + 往 content 里加一个节点"，这里用纯 Go 复刻后一步，
// 经 Y-Sweet 的 HTTP 接口（GET as-update / POST update）读写，不需要 WebSocket 与 CRDT 依赖。
//
// 编码格式见 yjs/src/utils/UpdateEncoder.js、structs/Item.js 与 lib0/encoding.js。
package ydoc

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"unicode/utf16"
)

// 条目信息字节的标志位（Item.write）。
const (
	bitOrigin      = 0x80
	bitRightOrigin = 0x40
	bitParentSub   = 0x20
	refMask        = 0x1f
)

// 内容类型编号（readItemContent 的分派表）。
const (
	refGC      = 0
	refDeleted = 1
	refJSON    = 2
	refBinary  = 3
	refString  = 4
	refEmbed   = 5
	refFormat  = 6
	refType    = 7
	refAny     = 8
	refDoc     = 9
	refSkip    = 10
)

// Yjs 类型编号（YMapRefID / YTextRefID）。
const (
	TypeMap  = 1
	TypeText = 2
)

// ---------------------------- 编码 ----------------------------

type encoder struct{ b []byte }

func (e *encoder) u8(v byte) { e.b = append(e.b, v) }

func (e *encoder) varUint(v uint64) {
	for v > 0x7f {
		e.b = append(e.b, byte(v&0x7f)|0x80)
		v >>= 7
	}
	e.b = append(e.b, byte(v))
}

func (e *encoder) varString(s string) {
	e.varUint(uint64(len(s)))
	e.b = append(e.b, s...)
}

// varInt 是 lib0 的有符号变长整数：首字节 6 位数值 + 符号位 + 续位。
func (e *encoder) varInt(n int64) {
	neg := n < 0
	if neg {
		n = -n
	}
	first := byte(n & 0x3f)
	if neg {
		first |= 0x40
	}
	n >>= 6
	if n > 0 {
		first |= 0x80
	}
	e.u8(first)
	for n > 0 {
		b := byte(n & 0x7f)
		n >>= 7
		if n > 0 {
			b |= 0x80
		}
		e.u8(b)
	}
}

// any 是 lib0 的 writeAny（只支持本包会写出的类型）。
func (e *encoder) any(v any) error {
	switch x := v.(type) {
	case nil:
		e.u8(126)
	case bool:
		if x {
			e.u8(120)
		} else {
			e.u8(121)
		}
	case string:
		e.u8(119)
		e.varString(x)
	case int:
		if x < -(1<<31-1) || x > 1<<31-1 {
			return e.any(float64(x))
		}
		e.u8(125)
		e.varInt(int64(x))
	case float64:
		if x == math.Trunc(x) && math.Abs(x) <= 1<<31-1 {
			e.u8(125)
			e.varInt(int64(x))
			return nil
		}
		e.u8(123)
		e.b = binary.BigEndian.AppendUint64(e.b, math.Float64bits(x))
	default:
		return fmt.Errorf("ydoc: 不支持写入 %T", v)
	}
	return nil
}

// Update 构造一次更新：所有条目都属于同一个新客户端，时钟从 0 递增。
// 只做"新增键"——给已存在的键赋值需要引用旧条目做 origin，本包不需要也不支持。
type Update struct {
	client uint64
	clock  uint64
	items  encoder
	n      int
	err    error
}

// MapRef 指向本次更新里新建的一个 Y.Map。
type MapRef struct {
	u  *Update
	id ID
}

// ID 是条目标识（客户端 + 时钟）。
type ID struct{ Client, Clock uint64 }

// NewUpdate 开始一次更新，客户端号随机（与 Yjs 一样取 32 位无符号整数）。
func NewUpdate() *Update {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<32-1))
	return &Update{client: n.Uint64() + 1}
}

// SetRoot 在根级 Y.Map root 里新增 key = v（v 为基本值）。
func (u *Update) SetRoot(root, key string, v any) {
	u.item(refAny, func(e *encoder) { e.varUint(1); u.setErr(e.any(v)) }, root, nil, key)
}

// NewRootMap 在根级 Y.Map root 里新增 key = 一个新的 Y.Map。
func (u *Update) NewRootMap(root, key string) MapRef {
	id := ID{u.client, u.clock}
	u.item(refType, func(e *encoder) { e.varUint(TypeMap) }, root, nil, key)
	return MapRef{u: u, id: id}
}

// Set 在这个新 Y.Map 里设 key = v（v 为基本值）。
func (m MapRef) Set(key string, v any) {
	m.u.item(refAny, func(e *encoder) { e.varUint(1); m.u.setErr(e.any(v)) }, "", &m.id, key)
}

// SetText 在这个新 Y.Map 里设 key = 一个新的 Y.Text，内容为 text（文本文件节点的正文）。
func (m MapRef) SetText(key, text string) {
	u := m.u
	textID := ID{u.client, u.clock}
	u.item(refType, func(e *encoder) { e.varUint(TypeText) }, "", &m.id, key)
	if text == "" {
		return
	}
	// 正文是挂在这个 Y.Text 下、没有键的字符串条目；时钟按 UTF-16 码元前进
	e := &u.items
	e.u8(refString)
	e.varUint(0)
	e.varUint(textID.Client)
	e.varUint(textID.Clock)
	e.varString(text)
	u.clock += uint64(len(utf16.Encode([]rune(text))))
	u.n++
}

func (u *Update) setErr(err error) {
	if err != nil && u.err == nil {
		u.err = err
	}
}

// item 写一个没有左右邻居的条目：父级是根类型名或某个条目，父级下的键为 parentSub。
func (u *Update) item(ref byte, content func(*encoder), root string, parent *ID, parentSub string) {
	e := &u.items
	e.u8(ref | bitParentSub)
	if parent == nil {
		e.varUint(1)
		e.varString(root)
	} else {
		e.varUint(0)
		e.varUint(parent.Client)
		e.varUint(parent.Clock)
	}
	e.varString(parentSub)
	content(e)
	u.clock++ // 本包写出的内容长度都是 1
	u.n++
}

// Encode 输出 v1 编码的更新。
func (u *Update) Encode() ([]byte, error) {
	if u.err != nil {
		return nil, u.err
	}
	var e encoder
	if u.n == 0 {
		e.varUint(0)
	} else {
		e.varUint(1)           // 客户端数
		e.varUint(uint64(u.n)) // 条目数
		e.varUint(u.client)    // 客户端
		e.varUint(0)           // 起始时钟
		e.b = append(e.b, u.items.b...)
	}
	e.varUint(0) // 删除集为空
	return e.b, nil
}

// ---------------------------- 解码 ----------------------------

type decoder struct {
	b   []byte
	err error
}

var errShort = errors.New("ydoc: 更新数据截断")

func (d *decoder) u8() byte {
	if d.err != nil || len(d.b) == 0 {
		d.fail(errShort)
		return 0
	}
	v := d.b[0]
	d.b = d.b[1:]
	return v
}

func (d *decoder) fail(err error) {
	if d.err == nil {
		d.err = err
	}
}

func (d *decoder) varUint() uint64 {
	var v uint64
	for shift := 0; shift < 64; shift += 7 {
		c := d.u8()
		if d.err != nil {
			return 0
		}
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return v
		}
	}
	d.fail(errors.New("ydoc: 变长整数过长"))
	return 0
}

func (d *decoder) bytes(n uint64) []byte {
	if d.err != nil || n > uint64(len(d.b)) {
		d.fail(errShort)
		return nil
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v
}

func (d *decoder) varString() string { return string(d.bytes(d.varUint())) }

func (d *decoder) varInt() int64 {
	c := d.u8()
	n := int64(c & 0x3f)
	neg := c&0x40 != 0
	mult := int64(64)
	for c&0x80 != 0 && d.err == nil {
		c = d.u8()
		n += int64(c&0x7f) * mult
		mult *= 128
	}
	if neg {
		return -n
	}
	return n
}

func (d *decoder) any() any {
	switch t := d.u8(); t {
	case 127, 126:
		return nil
	case 125:
		return float64(d.varInt())
	case 124:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(d.bytes(4))))
	case 123:
		return math.Float64frombits(binary.BigEndian.Uint64(d.bytes(8)))
	case 122:
		return float64(int64(binary.BigEndian.Uint64(d.bytes(8))))
	case 121:
		return false
	case 120:
		return true
	case 119:
		return d.varString()
	case 118:
		n := d.varUint()
		m := make(map[string]any, min(n, 64))
		for i := uint64(0); i < n && d.err == nil; i++ {
			k := d.varString()
			m[k] = d.any()
		}
		return m
	case 117:
		n := d.varUint()
		a := make([]any, 0, min(n, 64))
		for i := uint64(0); i < n && d.err == nil; i++ {
			a = append(a, d.any())
		}
		return a
	case 116:
		return d.bytes(d.varUint())
	default:
		d.fail(fmt.Errorf("ydoc: 未知的值类型 %d", t))
		return nil
	}
}

// item 是解码出的一个条目（只保留建树需要的信息）。
type item struct {
	id          ID
	length      uint64
	origin      *ID
	rightOrigin *ID
	parentRoot  string
	parentID    *ID
	parentSub   string
	ref         byte
	typeRef     uint64
	value       any // refAny 的最后一个值
	deleted     bool
}

// Doc 是解码后的文档：只建根级 Y.Map 与其中的 Y.Map 节点。
type Doc struct {
	// Roots[根名][键] 是根级 Y.Map 的当前值；值为 Y.Map 时是 *Map。
	Roots map[string]map[string]any
}

// Map 是文档里的一个 Y.Map（字段为基本值；嵌套类型记为 Type{编号}）。
type Map struct {
	ID     ID
	Fields map[string]any
}

// Type 表示字段值是一个嵌套的 Yjs 类型（Y.Text、Y.Array……），本包不展开。
type Type struct{ Ref uint64 }

// Decode 解析 v1 编码的完整文档更新（Y-Sweet GET as-update 的响应）。
func Decode(update []byte) (*Doc, error) {
	d := &decoder{b: update}
	var items []*item
	clients := d.varUint()
	for c := uint64(0); c < clients && d.err == nil; c++ {
		n := d.varUint()
		client := d.varUint()
		clock := d.varUint()
		for i := uint64(0); i < n && d.err == nil; i++ {
			info := d.u8()
			switch ref := info & refMask; ref {
			case refGC:
				clock += d.varUint()
			case refSkip:
				clock += d.varUint()
			default:
				it := &item{id: ID{client, clock}, ref: ref}
				if info&bitOrigin != 0 {
					it.origin = &ID{d.varUint(), d.varUint()}
				}
				if info&bitRightOrigin != 0 {
					it.rightOrigin = &ID{d.varUint(), d.varUint()}
				}
				if info&(bitOrigin|bitRightOrigin) == 0 {
					if d.varUint() == 1 {
						it.parentRoot = d.varString()
					} else {
						it.parentID = &ID{d.varUint(), d.varUint()}
					}
					if info&bitParentSub != 0 {
						it.parentSub = d.varString()
					}
				}
				it.length = d.content(it)
				clock += it.length
				items = append(items, it)
			}
		}
	}
	// 删除集
	deleted := map[uint64][][2]uint64{}
	n := d.varUint()
	for i := uint64(0); i < n && d.err == nil; i++ {
		client := d.varUint()
		ranges := d.varUint()
		for j := uint64(0); j < ranges && d.err == nil; j++ {
			deleted[client] = append(deleted[client], [2]uint64{d.varUint(), d.varUint()})
		}
	}
	if d.err != nil {
		return nil, d.err
	}
	for _, it := range items {
		for _, r := range deleted[it.id.Client] {
			if it.id.Clock < r[0]+r[1] && r[0] < it.id.Clock+it.length {
				it.deleted = true
			}
		}
	}
	return build(items), nil
}

// content 读条目内容，返回它占的时钟长度。
func (d *decoder) content(it *item) uint64 {
	switch it.ref {
	case refDeleted:
		it.deleted = true
		return d.varUint()
	case refJSON:
		n := d.varUint()
		for i := uint64(0); i < n && d.err == nil; i++ {
			d.varString()
		}
		return n
	case refBinary:
		d.bytes(d.varUint())
		return 1
	case refString:
		// 长度按 UTF-16 码元计（JS 字符串长度）
		return uint64(len(utf16.Encode([]rune(d.varString()))))
	case refEmbed:
		d.varString()
		return 1
	case refFormat:
		d.varString()
		d.varString()
		return 1
	case refType:
		it.typeRef = d.varUint()
		if it.typeRef == 3 || it.typeRef == 5 { // XmlElement 的标签名 / XmlHook 的键
			d.varString()
		}
		return 1
	case refAny:
		n := d.varUint()
		for i := uint64(0); i < n && d.err == nil; i++ {
			it.value = d.any()
		}
		return n
	case refDoc:
		d.varString()
		d.any()
		return 1
	default:
		d.fail(fmt.Errorf("ydoc: 未知的内容类型 %d", it.ref))
		return 0
	}
}

// build 补全父级（有左右邻居的条目沿 origin 找到父级与键），再组装根级 Y.Map。
func build(items []*item) *Doc {
	byClient := map[uint64][]*item{}
	for _, it := range items {
		byClient[it.id.Client] = append(byClient[it.id.Client], it)
	}
	find := func(id ID) *item {
		for _, it := range byClient[id.Client] {
			if it.id.Clock <= id.Clock && id.Clock < it.id.Clock+it.length {
				return it
			}
		}
		return nil
	}
	var resolve func(it *item, depth int) bool
	resolve = func(it *item, depth int) bool {
		if it.parentRoot != "" || it.parentID != nil {
			return true
		}
		if depth > 10000 {
			return false
		}
		for _, ref := range []*ID{it.origin, it.rightOrigin} {
			if ref == nil {
				continue
			}
			if o := find(*ref); o != nil && resolve(o, depth+1) {
				it.parentRoot, it.parentID, it.parentSub = o.parentRoot, o.parentID, o.parentSub
				return true
			}
		}
		return false
	}

	doc := &Doc{Roots: map[string]map[string]any{}}
	maps := map[ID]*Map{}
	for _, it := range items {
		if it.ref == refType && it.typeRef == TypeMap {
			maps[it.id] = &Map{ID: it.id, Fields: map[string]any{}}
		}
	}
	value := func(it *item) any {
		if it.ref == refType {
			if m := maps[it.id]; m != nil {
				return m
			}
			return Type{Ref: it.typeRef}
		}
		return it.value
	}
	for _, it := range items {
		if !resolve(it, 0) || it.deleted || it.parentSub == "" {
			continue
		}
		if it.ref != refAny && it.ref != refType {
			continue
		}
		if it.parentRoot != "" {
			root := doc.Roots[it.parentRoot]
			if root == nil {
				root = map[string]any{}
				doc.Roots[it.parentRoot] = root
			}
			root[it.parentSub] = value(it)
		} else if m := maps[*it.parentID]; m != nil {
			m.Fields[it.parentSub] = value(it)
		}
	}
	return doc
}
