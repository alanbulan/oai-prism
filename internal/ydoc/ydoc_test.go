package ydoc

import (
	"math"
	"testing"
)

func TestAnyRoundTrip(t *testing.T) {
	for _, v := range []any{nil, true, false, "", "中文 abc", 0, 1, -1, 63, 64, -64, 8191, 1 << 30, -(1 << 30), 1.5, math.MaxInt32 + 10.0} {
		var e encoder
		if err := e.any(v); err != nil {
			t.Fatal(err)
		}
		d := &decoder{b: e.b}
		got := d.any()
		if d.err != nil || len(d.b) != 0 {
			t.Fatalf("%v: err=%v rest=%d", v, d.err, len(d.b))
		}
		want := v
		if n, ok := v.(int); ok {
			want = float64(n)
		}
		if got != want {
			t.Fatalf("%v (%T) 读回 %v (%T)", v, v, got, got)
		}
	}
}

func TestAddFileToEmptyDoc(t *testing.T) {
	empty, err := Decode([]byte{0, 0}) // Y-Sweet 对空文档返回的正是这两个字节
	if err != nil {
		t.Fatal(err)
	}
	if empty.Root() != "" || len(empty.Nodes()) != 0 {
		t.Fatal("空文档不该有节点")
	}
	up, err := AddFile(empty, "prism-uploads", "file-1", "a.png")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Decode(up)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Roots["settings"]["init"] != true || doc.Roots["settings"]["deleted"] != false {
		t.Fatalf("settings: %v", doc.Roots["settings"])
	}
	root := doc.Root()
	folder, ok := doc.Child(root, "prism-uploads")
	if root == "" || !ok || folder.Type != "folder" {
		t.Fatalf("根目录 / 上传目录缺失: %+v", doc.Nodes())
	}
	f, ok := doc.Child(folder.ID, "a.png")
	if !ok || f.ID != "file-1" || f.Type != "url" || f.Deleted {
		t.Fatalf("文件节点不对: %+v", f)
	}

	// 第二个文件：根目录与上传目录都复用
	up2, err := AddFile(doc, "prism-uploads", "file-2", "b.png")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := Decode(up2)
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.Nodes()) != 1 || d2.Nodes()[0].InFolder != folder.ID || d2.Roots["settings"] != nil {
		t.Fatalf("第二次只该新增文件节点: %+v %v", d2.Nodes(), d2.Roots["settings"])
	}
	if _, err := AddFile(doc, "prism-uploads", "file-3", "a.png"); err == nil {
		t.Fatal("同名文件应拒绝")
	}
}

// 网页里删除文件是给 deleted 赋新值：新条目以旧条目为 origin（不再写父级与键），旧条目进删除集。
func TestDecodeOverwriteViaOrigin(t *testing.T) {
	var e encoder
	e.varUint(2) // 两个客户端

	// 客户端 7：content["n1"] = Map{filename:"x.png", deleted:false}
	e.varUint(3)
	e.varUint(7)
	e.varUint(0)
	e.u8(refType | bitParentSub)
	e.varUint(1)
	e.varString("content")
	e.varString("n1")
	e.varUint(TypeMap)
	for _, kv := range [][2]any{{"filename", "x.png"}, {"deleted", false}} {
		e.u8(refAny | bitParentSub)
		e.varUint(0)
		e.varUint(7)
		e.varUint(0)
		e.varString(kv[0].(string))
		e.varUint(1)
		_ = e.any(kv[1])
	}

	// 客户端 9：deleted = true，origin 指向 7:2
	e.varUint(1)
	e.varUint(9)
	e.varUint(0)
	e.u8(refAny | bitOrigin)
	e.varUint(7)
	e.varUint(2)
	e.varUint(1)
	_ = e.any(true)

	// 删除集：7:2 长 1
	e.varUint(1)
	e.varUint(7)
	e.varUint(1)
	e.varUint(2)
	e.varUint(1)

	doc, err := Decode(e.b)
	if err != nil {
		t.Fatal(err)
	}
	n := doc.Nodes()
	if len(n) != 1 || n[0].Filename != "x.png" || !n[0].Deleted {
		t.Fatalf("应解析出已删除的 x.png: %+v", n)
	}
	if doc.Root() != "" {
		t.Fatal("没有根目录")
	}
}

// 文本文件节点：content 是 Y.Text，正文条目没有键、时钟按 UTF-16 计。
func TestSetTextNode(t *testing.T) {
	u := NewUpdate()
	n := u.NewRootMap("content", "t1")
	n.Set("filename", "note.txt")
	n.SetText("content", "a😀b\n")
	n.Set("type", "text")
	b, err := u.Encode()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	m := doc.Roots["content"]["t1"].(*Map)
	if m.Fields["filename"] != "note.txt" || m.Fields["type"] != "text" || m.Fields["content"] != (Type{Ref: TypeText}) {
		t.Fatalf("文本节点解析不对: %+v", m.Fields)
	}
	if u.clock != 4+5 { // map + 2 个字段 + text 类型 = 4；正文 "a😀b\n" 占 5 个 UTF-16 码元
		t.Fatalf("时钟 %d", u.clock)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	for _, b := range [][]byte{{1}, {1, 1, 5, 0, 0x1f}, {1, 1, 5, 0, refString | bitParentSub, 1, 50}} {
		if _, err := Decode(b); err == nil {
			t.Fatalf("%v 应报错", b)
		}
	}
}
