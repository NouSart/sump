package ooxml

import (
	"archive/zip"
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sampleParts 构造一份最小 docx 的 part 表（顺序即打包顺序）。
func sampleParts() ([]string, map[string][]byte) {
	order := []string{
		"[Content_Types].xml",
		"_rels/.rels",
		"word/document.xml",
		"word/styles.xml",
		"docProps/core.xml",
	}
	files := map[string][]byte{
		"[Content_Types].xml": []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`),
		"_rels/.rels":         []byte(`<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"/>`),
		"word/document.xml":   []byte(sampleDocx),
		"word/styles.xml":     []byte(`<?xml version="1.0" encoding="UTF-8"?><w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"/>`),
		"docProps/core.xml":   []byte(`<?xml version="1.0" encoding="UTF-8"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties"/>`),
	}
	return order, files
}

// zipOf 按给定顺序打包（时间戳固定，保证测试可重复）。
func zipOf(t *testing.T, order []string, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range order {
		h := &zip.FileHeader{
			Name:     name,
			Method:   zip.Deflate,
			Modified: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("创建 zip 条目 %s 失败: %v", name, err)
		}
		if _, err := w.Write(files[name]); err != nil {
			t.Fatalf("写入 zip 条目 %s 失败: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	return buf.Bytes()
}

// TestPackageKeepsUntouchedPartsIdentical 是 P0 的核心验收：改一个 part，
// 其余 part 必须逐字节一致，顺序也不能变（Word 对某些顺序敏感）。
func TestPackageKeepsUntouchedPartsIdentical(t *testing.T) {
	order, files := sampleParts()
	src := zipOf(t, order, files)

	p, err := Open(src, 0)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	doc, err := p.Part("word/document.xml")
	if err != nil {
		t.Fatalf("读取 document.xml 失败: %v", err)
	}
	patched := bytes.Replace(doc, []byte("科技有限公司"), []byte("股份有限公司"), 1)
	if err := p.SetPart("word/document.xml", patched); err != nil {
		t.Fatalf("SetPart 失败: %v", err)
	}
	if got := p.Modified(); !reflect.DeepEqual(got, []string{"word/document.xml"}) {
		t.Fatalf("Modified = %v", got)
	}
	out, err := p.Bytes()
	if err != nil {
		t.Fatalf("Bytes 失败: %v", err)
	}

	p2, err := Open(out, 0)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	if got := p2.Names(); !reflect.DeepEqual(got, order) {
		t.Fatalf("part 顺序变了：%v，期望 %v", got, order)
	}
	for _, name := range order {
		before, err := p.Part(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		after, err := p2.Part(name)
		if err != nil {
			t.Fatalf("重新读取 %s 失败: %v", name, err)
		}
		if name == "word/document.xml" {
			if !bytes.Equal(after, patched) {
				t.Fatalf("目标 part 没写进去")
			}
			continue
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("非目标 part 被改动了：%s", name)
		}
	}

	// 二次打包必须稳定（否则每次保存都会产生不同的文件哈希，防重放 / 去重都会失效）。
	again, err := p2.Bytes()
	if err != nil {
		t.Fatalf("二次 Bytes 失败: %v", err)
	}
	if !bytes.Equal(out, again) {
		t.Fatal("重新打包结果不稳定")
	}
}

// TestPackageRejectsDuplicateParts 重复 part 名会让"读到哪一份"不确定，必须拒绝。
func TestPackageRejectsDuplicateParts(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < 2; i++ {
		w, err := zw.Create("word/document.xml")
		if err != nil {
			t.Fatalf("创建条目失败: %v", err)
		}
		if _, err := w.Write([]byte("<w:document/>")); err != nil {
			t.Fatalf("写入条目失败: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	if _, err := Open(buf.Bytes(), 0); err == nil {
		t.Fatal("期望拒绝重复 part，实际通过")
	}
}

// TestPackageRejectsOversize 解压炸弹防线：目录里声明的解压总量超限即拒绝。
func TestPackageRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "word/document.xml", Method: zip.Store})
	if err != nil {
		t.Fatalf("创建条目失败: %v", err)
	}
	if _, err := w.Write(bytes.Repeat([]byte("a"), 4096)); err != nil {
		t.Fatalf("写入条目失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	if _, err := Open(buf.Bytes(), 1024); err == nil {
		t.Fatal("期望按预算拒绝，实际通过")
	}
}

// TestPackagePartGuards part 名拼错时给明确错误，避免"新增了一个幽灵 part"。
func TestPackagePartGuards(t *testing.T) {
	order, files := sampleParts()
	p, err := Open(zipOf(t, order, files), 0)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if _, err := p.Part("word/missing.xml"); err == nil {
		t.Fatal("读取不存在的 part 应报错")
	}
	if err := p.SetPart("word/missing.xml", []byte("x")); err == nil {
		t.Fatal("替换不存在的 part 应报错")
	}
	if err := p.AddPart("word/document.xml", []byte("x")); err == nil {
		t.Fatal("新增已存在的 part 应报错")
	}
	if err := p.AddPart("word/footnotes.xml", []byte("<w:footnotes/>")); err != nil {
		t.Fatalf("AddPart 失败: %v", err)
	}
	out, err := p.Bytes()
	if err != nil {
		t.Fatalf("Bytes 失败: %v", err)
	}
	p2, err := Open(out, 0)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	got, err := p2.Part("word/footnotes.xml")
	if err != nil {
		t.Fatalf("新 part 没写进去: %v", err)
	}
	if string(got) != "<w:footnotes/>" {
		t.Fatalf("新 part 内容 = %q", got)
	}
	p2.Remove("word/footnotes.xml")
	final, err := p2.Bytes()
	if err != nil {
		t.Fatalf("删除后 Bytes 失败: %v", err)
	}
	p3, err := Open(final, 0)
	if err != nil {
		t.Fatalf("删除后重新打开失败: %v", err)
	}
	if p3.Has("word/footnotes.xml") {
		t.Fatal("删除的 part 仍在容器里")
	}
}

// TestPackageMatch 前缀匹配按原顺序返回（pptx 的 slide 列表靠它）。
func TestPackageMatch(t *testing.T) {
	order, files := sampleParts()
	p, err := Open(zipOf(t, order, files), 0)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	got := p.Match("word/")
	want := []string{"word/document.xml", "word/styles.xml"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Match = %v，期望 %v", got, want)
	}
	if got := p.Match("ppt/slides/slide"); len(got) != 0 {
		t.Fatalf("不该匹配到 ppt 的 part：%v", got)
	}
	if !strings.Contains(strings.Join(p.Names(), ","), "docProps/core.xml") {
		t.Fatalf("Names 不完整：%v", p.Names())
	}
}
