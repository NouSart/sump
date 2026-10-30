package tests

// files Word 测试：.docx 读取（段落 / 截断 / 旧版 .doc 拒绝）与 edit_word 替换。

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

// makeDocx 在内存中构造一个最小 .docx，包含若干段落。
func makeDocx(t *testing.T, paragraphs ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)

	contentTypes := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>`
	writeZipEntry(t, w, "[Content_Types].xml", contentTypes)

	var body strings.Builder
	for _, p := range paragraphs {
		body.WriteString(fmt.Sprintf("<w:p><w:r><w:t>%s</w:t></w:r></w:p>", xmlEscape(p)))
	}
	doc := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>` + body.String() + `</w:body>
</w:document>`
	writeZipEntry(t, w, "word/document.xml", doc)

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeZipEntry(t *testing.T, w *zip.Writer, name, data string) {
	t.Helper()
	f, err := w.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

func saveDocx(t *testing.T, addr, filename string, data []byte) string {
	t.Helper()
	rp := call(t, addr, "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString(data),
		Filename: filename,
	})
	if !rp.OK {
		t.Fatalf("save docx 失败: %s", rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	return sr.ID
}

// TestReadDocx 读取 .docx：按段落输出，顺序正确，未截断。
func TestReadDocx(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := saveDocx(t, srv.WsURL(), "报告.docx", makeDocx(t, "第一段", "第二段"))

	rr := readFile(t, srv.WsURL(), id)
	if rr.Truncated {
		t.Fatalf("小文件不应截断: %+v", rr)
	}
	want := "第一段\n\n第二段"
	if rr.Text != want {
		t.Fatalf("读取结果不符:\n%q\n想要:\n%q", rr.Text, want)
	}
}

// TestReadDocxTruncated 小阈值下在段落边界截断。
func TestReadDocxTruncated(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 6) // 仅够“第一段”三个字符
	id := saveDocx(t, srv.WsURL(), "报告.docx", makeDocx(t, "第一段", "第二段"))

	rr := readFile(t, srv.WsURL(), id)
	if !rr.Truncated {
		t.Fatalf("应截断: %+v", rr)
	}
	if rr.Text != "第一段" {
		t.Fatalf("截断结果不符: %q", rr.Text)
	}
}

// TestReadLegacyDoc 旧版 .doc 明确拒绝并引导另存。
func TestReadLegacyDoc(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString([]byte("假装是旧格式")),
		Filename: "旧文档.doc",
		Mime:     "application/msword",
	})
	if !rp.OK {
		t.Fatalf("save 失败: %s", rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}

	rp2 := call(t, srv.WsURL(), "read", protocol.FileReadPayload{ID: sr.ID})
	if rp2.OK || !strings.Contains(rp2.Error, "另存为 .docx") {
		t.Fatalf(".doc 应报错并引导另存: %+v", rp2)
	}
}

// TestEditWord 替换生效：read 见新值、fetch 二进制变化。
func TestEditWord(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	orig := makeDocx(t, "第一段", "第二段")
	id := saveDocx(t, srv.WsURL(), "报告.docx", orig)

	rp := call(t, srv.WsURL(), "edit_word", protocol.FileWordEditPayload{
		ID: id,
		Operations: []protocol.WordEditOperation{
			{Op: "replace", Old: "第一段", New: "开头"},
			{Op: "replace", Old: "第二段", New: "结尾"},
		},
	})
	if !rp.OK {
		t.Fatalf("edit_word 失败: %s", rp.Error)
	}
	var wr protocol.FileWordEditResult
	if err := protocol.DecodeRaw(rp.Data, &wr); err != nil {
		t.Fatal(err)
	}
	if wr.Replaced != 2 || wr.ID != id {
		t.Fatalf("edit_word 结果不符: %+v", wr)
	}

	rr := readFile(t, srv.WsURL(), id)
	if rr.Text != "开头\n\n结尾" {
		t.Fatalf("替换未生效:\n%s", rr.Text)
	}

	rp2 := call(t, srv.WsURL(), "fetch", protocol.FileFetchPayload{ID: id})
	if !rp2.OK {
		t.Fatalf("fetch 失败: %s", rp2.Error)
	}
	var fr protocol.FileFetchResult
	if err := protocol.DecodeRaw(rp2.Data, &fr); err != nil {
		t.Fatal(err)
	}
	got, err := base64.StdEncoding.DecodeString(fr.Data)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, orig) {
		t.Fatal("编辑后文件二进制未变化")
	}
}

// TestEditWordErrors 错误路径：非 docx、空 operations、未知 op、old 未找到。
func TestEditWordErrors(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := saveDocx(t, srv.WsURL(), "报告.docx", makeDocx(t, "第一段"))

	// 非 docx。
	txtID := saveText(t, srv.WsURL(), "a.txt", "文本")
	bad1 := call(t, srv.WsURL(), "edit_word", protocol.FileWordEditPayload{
		ID:         txtID,
		Operations: []protocol.WordEditOperation{{Op: "replace", Old: "x", New: "y"}},
	})
	if bad1.OK || !strings.Contains(bad1.Error, "仅支持编辑 .docx") {
		t.Fatalf("非 docx 应拒绝: %+v", bad1)
	}

	// 空 operations。
	bad2 := call(t, srv.WsURL(), "edit_word", protocol.FileWordEditPayload{ID: id})
	if bad2.OK || !strings.Contains(bad2.Error, "operations 至少一条") {
		t.Fatalf("空 operations 应报错: %+v", bad2)
	}

	// 未知 op。
	bad3 := call(t, srv.WsURL(), "edit_word", protocol.FileWordEditPayload{
		ID:         id,
		Operations: []protocol.WordEditOperation{{Op: "delete", Old: "第一段"}},
	})
	if bad3.OK || !strings.Contains(bad3.Error, "不支持 op") {
		t.Fatalf("未知 op 应报错: %+v", bad3)
	}

	// old 未找到。
	bad4 := call(t, srv.WsURL(), "edit_word", protocol.FileWordEditPayload{
		ID:         id,
		Operations: []protocol.WordEditOperation{{Op: "replace", Old: "不存在", New: "x"}},
	})
	if bad4.OK || !strings.Contains(bad4.Error, "未找到") {
		t.Fatalf("old 未找到应报错: %+v", bad4)
	}

	// old 为空。
	bad5 := call(t, srv.WsURL(), "edit_word", protocol.FileWordEditPayload{
		ID:         id,
		Operations: []protocol.WordEditOperation{{Op: "replace", Old: "   ", New: "x"}},
	})
	if bad5.OK || !strings.Contains(bad5.Error, "old 不能为空") {
		t.Fatalf("old 为空应报错: %+v", bad5)
	}
}

// TestEditWordReplacesAllOccurrences 补上一个此前没覆盖的语义：同一个 old 在正文里出现
// 多次时，edit_word 是**全替换**（Replaced 报的是实际替换处数，不是操作条数）。
// （此前两个用例的 old 各只出现一次，所以"只替第一处"这种退化抓不住。）
func TestEditWordReplacesAllOccurrences(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	orig := makeDocx(t, "重复内容", "中间", "重复内容")
	id := saveDocx(t, srv.WsURL(), "重复.docx", orig)

	rp := call(t, srv.WsURL(), "edit_word", protocol.FileWordEditPayload{
		ID:         id,
		Operations: []protocol.WordEditOperation{{Op: "replace", Old: "重复内容", New: "改过"}},
	})
	if !rp.OK {
		t.Fatalf("edit_word 失败: %s", rp.Error)
	}
	var wr protocol.FileWordEditResult
	if err := protocol.DecodeRaw(rp.Data, &wr); err != nil {
		t.Fatal(err)
	}
	if wr.Replaced != 2 {
		t.Fatalf("两处相同文本都应被替换（Replaced 应为 2），实际 %+v", wr)
	}

	rr := readFile(t, srv.WsURL(), id)
	if rr.Text != "改过\n\n中间\n\n改过" {
		t.Fatalf("全替换未生效:\n%s", rr.Text)
	}
}
