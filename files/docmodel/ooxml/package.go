// Package ooxml 的容器层：OPC（docx / pptx / xlsx 都是 zip）。
// 设计要点：按需读取 part；写回时未改动的 part 逐字节原样搬运（连压缩方式都保留），
// 只重新打包被改过的条目——这是"非目标内容零差异"的第一层保证。
package ooxml

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
)

// DefaultBudget 是单份文件解压后的字节上限（压缩炸弹防线；与服务层 maxExpandBytes 同量级）。
const DefaultBudget int64 = 256 << 20

// Package 是一个已打开的 OPC 容器。
// 约定：不是并发安全的；一个 Package 对应一次读-改-写。
type Package struct {
	files  map[string]*zip.File
	order  []string
	budget int64

	changed map[string][]byte // 内容被替换的 part
	removed map[string]bool   // 被删除的 part
	added   []string          // 新增的 part（按加入顺序，输出时排在末尾）
	addData map[string][]byte
}

// Open 解析 OPC 容器并预检解压总量。
func Open(data []byte, budget int64) (*Package, error) {
	if budget <= 0 {
		budget = DefaultBudget
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("解析 OOXML 容器失败: %w", err)
	}
	p := &Package{
		files:   make(map[string]*zip.File, len(zr.File)),
		order:   make([]string, 0, len(zr.File)),
		budget:  budget,
		changed: map[string][]byte{},
		removed: map[string]bool{},
		addData: map[string][]byte{},
	}
	var total uint64
	for _, f := range zr.File {
		if _, dup := p.files[f.Name]; dup {
			// OPC 要求 part 名唯一：重复名会让"读到的是哪一份"变成不确定行为。
			return nil, fmt.Errorf("OOXML 容器里有重复 part：%s", f.Name)
		}
		total += f.UncompressedSize64
		if total > uint64(budget) {
			return nil, fmt.Errorf("解压后总量超过 %d 字节上限（压缩炸弹防线）", budget)
		}
		p.files[f.Name] = f
		p.order = append(p.order, f.Name)
	}
	return p, nil
}

// Names 返回 part 名（原始顺序）。
func (p *Package) Names() []string { return append([]string(nil), p.order...) }

// Has 判断 part 是否存在。
func (p *Package) Has(name string) bool {
	_, ok := p.files[name]
	return ok
}

// Match 返回名字以 prefix 开头的 part（按原顺序）：用于 "ppt/slides/slide" 这类批量扫描。
func (p *Package) Match(prefix string) []string {
	var out []string
	for _, n := range p.order {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out
}

// Part 读取一个 part 的内容（已替换过的返回新内容）。
// 读取带字节上限：目录里声明的大小不可信，靠 LimitReader 兜住"头部撒谎"的包。
func (p *Package) Part(name string) ([]byte, error) {
	if d, ok := p.changed[name]; ok {
		return d, nil
	}
	f, ok := p.files[name]
	if !ok {
		return nil, fmt.Errorf("OOXML 容器里缺少 part：%s", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("打开 part %s 失败: %w", name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, p.budget+1))
	if err != nil {
		return nil, fmt.Errorf("读取 part %s 失败: %w", name, err)
	}
	if int64(len(data)) > p.budget {
		return nil, fmt.Errorf("part %s 解压后超过 %d 字节上限", name, p.budget)
	}
	return data, nil
}

// SetPart 替换一个已存在 part 的内容。
// 不存在时报错而不是新建——新增要走 AddPart，避免名字拼错时"悄悄多出一个 part"。
func (p *Package) SetPart(name string, data []byte) error {
	if _, ok := p.files[name]; !ok {
		return fmt.Errorf("OOXML 容器里缺少 part：%s（新增 part 请用 AddPart）", name)
	}
	if int64(len(data)) > p.budget {
		return fmt.Errorf("part %s 写入内容超过 %d 字节上限", name, p.budget)
	}
	p.changed[name] = data
	delete(p.removed, name)
	return nil
}

// AddPart 新增一个 part（例如 pptx 新增一页要加 slide + rels）。
func (p *Package) AddPart(name string, data []byte) error {
	if name == "" {
		return fmt.Errorf("part 名不能为空")
	}
	if _, ok := p.files[name]; ok {
		return fmt.Errorf("part 已存在：%s（替换请用 SetPart）", name)
	}
	if _, ok := p.addData[name]; ok {
		return fmt.Errorf("part 已存在：%s（替换请用 SetPart）", name)
	}
	if int64(len(data)) > p.budget {
		return fmt.Errorf("part %s 写入内容超过 %d 字节上限", name, p.budget)
	}
	p.addData[name] = data
	p.added = append(p.added, name)
	return nil
}

// Remove 删除一个 part（不存在的忽略）。
func (p *Package) Remove(name string) {
	if _, ok := p.files[name]; !ok {
		return
	}
	p.removed[name] = true
	delete(p.changed, name)
}

// Modified 返回被改动的 part 名（含新增与删除），用于诊断与"只重写受影响 part"。
func (p *Package) Modified() []string {
	out := make([]string, 0, len(p.changed)+len(p.added)+len(p.removed))
	out = append(out, p.added...)
	for n := range p.changed {
		out = append(out, n)
	}
	for n := range p.removed {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Bytes 重新打包成完整文件：未改动的 part 流式原样搬运。
func (p *Package) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(p.order) * 512)
	zw := zip.NewWriter(&buf)
	for _, name := range p.order {
		if p.removed[name] {
			continue
		}
		f := p.files[name]
		if data, ok := p.changed[name]; ok {
			if err := writePart(zw, headerOf(f), data); err != nil {
				_ = zw.Close()
				return nil, fmt.Errorf("重写 part %s 失败: %w", name, err)
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			_ = zw.Close()
			return nil, fmt.Errorf("打开 part %s 失败: %w", name, err)
		}
		w, err := zw.CreateHeader(headerOf(f))
		if err == nil {
			_, err = io.Copy(w, rc)
		}
		rc.Close()
		if err != nil {
			_ = zw.Close()
			return nil, fmt.Errorf("复制 part %s 失败: %w", name, err)
		}
	}
	for _, name := range p.added {
		if err := writePart(zw, &zip.FileHeader{Name: name, Method: zip.Deflate}, p.addData[name]); err != nil {
			_ = zw.Close()
			return nil, fmt.Errorf("写入新 part %s 失败: %w", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("写完 OOXML 容器失败: %w", err)
	}
	return buf.Bytes(), nil
}

// headerOf 复制原有条目的元数据（名字 / 压缩方式 / 时间戳 / 外部属性）。
// 刻意不复制 Extra：里面可能带 Zip64 尺寸，写新内容时按旧值会产出坏头。
func headerOf(f *zip.File) *zip.FileHeader {
	return &zip.FileHeader{
		Name:          f.Name,
		Method:        f.Method,
		Modified:      f.Modified,
		Comment:       f.Comment,
		NonUTF8:       f.NonUTF8,
		ExternalAttrs: f.ExternalAttrs,
	}
}

// writePart 写入一个 part 的全部内容。
func writePart(zw *zip.Writer, h *zip.FileHeader, data []byte) error {
	w, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
