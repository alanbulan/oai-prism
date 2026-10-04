package ydoc

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
)

// Prism 项目文档的形状（取自官方前端 bundle）：
//
//	settings: Y.Map { init: true, deleted: false, title? }
//	content:  Y.Map { <节点 id>: Y.Map { id, filename, type, deleted, inFolder? , content? } }
//
// type 为 folder（目录）、text（正文是 Y.Text 的文本文件）或 url（二进制文件，正文按节点 id
// 存在对象存储里，即上传时的 x-prism-file-id）。根目录是唯一没有 inFolder 的未删除目录节点。

// Node 是文件树里的一个节点。
type Node struct {
	ID       string
	Filename string
	Type     string
	InFolder string
	Deleted  bool
}

// Nodes 列出 content 里的全部节点（含已删除的），按文件名排序。
func (d *Doc) Nodes() []Node {
	var out []Node
	for key, v := range d.Roots["content"] {
		m, ok := v.(*Map)
		if !ok {
			continue
		}
		n := Node{ID: key}
		if s, ok := m.Fields["id"].(string); ok && s != "" {
			n.ID = s
		}
		n.Filename, _ = m.Fields["filename"].(string)
		n.Type, _ = m.Fields["type"].(string)
		n.InFolder, _ = m.Fields["inFolder"].(string)
		n.Deleted, _ = m.Fields["deleted"].(bool)
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}

// Root 返回根目录节点 id；文档未初始化或没有根目录时返回空串。
func (d *Doc) Root() string {
	for _, n := range d.Nodes() {
		if !n.Deleted && n.InFolder == "" && n.Type == "folder" {
			return n.ID
		}
	}
	return ""
}

// Child 在目录 parent 下找名为 name 的未删除节点。
func (d *Doc) Child(parent, name string) (Node, bool) {
	for _, n := range d.Nodes() {
		if !n.Deleted && n.InFolder == parent && n.Filename == name {
			return n, true
		}
	}
	return Node{}, false
}

// AddFile 生成一次更新：把已上传（x-prism-file-id = fileID）的二进制文件登记为 dir/filename。
// 文档还没初始化时（网关建的项目没被网页打开过，文档是空的）补上 settings 与根目录；
// dir 不存在时在根目录下新建。只支持一级目录。
func AddFile(d *Doc, dir, fileID, filename string) ([]byte, error) {
	if fileID == "" || filename == "" || dir == "" {
		return nil, errors.New("ydoc: 缺少文件 id / 文件名 / 目录")
	}
	if _, ok := d.Roots["content"][fileID]; ok {
		return nil, fmt.Errorf("ydoc: 节点 %s 已存在", fileID)
	}
	u := NewUpdate()
	settings := d.Roots["settings"]
	if init, _ := settings["init"].(bool); !init {
		if _, ok := settings["init"]; ok {
			return nil, errors.New("ydoc: settings.init 已存在但不为 true，不改")
		}
		u.SetRoot("settings", "init", true)
		if _, ok := settings["deleted"]; !ok {
			u.SetRoot("settings", "deleted", false)
		}
	}
	root := d.Root()
	if root == "" {
		root = newUUID()
		n := u.NewRootMap("content", root)
		n.Set("id", root)
		n.Set("filename", "root")
		n.Set("type", "folder")
		n.Set("deleted", false)
	}
	folder := ""
	if f, ok := d.Child(root, dir); ok {
		if f.Type != "folder" {
			return nil, fmt.Errorf("ydoc: %s 不是目录", dir)
		}
		folder = f.ID
	} else {
		folder = newUUID()
		n := u.NewRootMap("content", folder)
		n.Set("id", folder)
		n.Set("filename", dir)
		n.Set("type", "folder")
		n.Set("deleted", false)
		n.Set("inFolder", root)
	}
	if _, ok := d.Child(folder, filename); ok {
		return nil, fmt.Errorf("ydoc: %s/%s 已存在", dir, filename)
	}
	n := u.NewRootMap("content", fileID)
	n.Set("id", fileID)
	n.Set("filename", filename)
	n.Set("type", "url")
	n.Set("deleted", false)
	n.Set("inFolder", folder)
	return u.Encode()
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
