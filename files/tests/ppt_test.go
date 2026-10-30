package tests

// files PowerPoint 测试：.pptx 读取（按幻灯片分节 / 截断 / 旧版 .ppt 拒绝）与 edit_ppt 替换。

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

// makePptx 在内存中构造一个最小 .pptx，包含若干幻灯片，每页若干段落。
func makePptx(t *testing.T, slides ...[]string) []byte {
	return makePptxExtra(t, nil, slides...)
}

// makePptxExtra 同 makePptx，并可额外注入 extra 条目（用于测试非标准文件名）。
func makePptxExtra(t *testing.T, extra map[string]string, slides ...[]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)

	contentTypes := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>
  <Override PartName="/ppt/slides/slide1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>
</Types>`
	writeZipEntry(t, w, "[Content_Types].xml", contentTypes)

	rels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/>
</Relationships>`
	writeZipEntry(t, w, "_rels/.rels", rels)

	var presRels strings.Builder
	presRels.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := range slides {
		presRels.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide%d.xml"/>`, i+1, i+1))
	}
	presRels.WriteString(`</Relationships>`)
	writeZipEntry(t, w, "ppt/_rels/presentation.xml.rels", presRels.String())

	presentation := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <p:sldIdLst>`
	for i := range slides {
		presentation += fmt.Sprintf(`<p:sldId id="%d" r:id="rId%d"/>`, 256+i+1, i+1)
	}
	presentation += `</p:sldIdLst>
</p:presentation>`
	writeZipEntry(t, w, "ppt/presentation.xml", presentation)

	for i, paras := range slides {
		var body strings.Builder
		for _, p := range paras {
			body.WriteString(fmt.Sprintf("<a:p><a:r><a:t>%s</a:t></a:r></a:p>", xmlEscape(p)))
		}
		slide := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
  <p:cSld><p:spTree><p:sp><p:txBody>` + body.String() + `</p:txBody></p:sp></p:spTree></p:cSld>
</p:sld>`
		writeZipEntry(t, w, fmt.Sprintf("ppt/slides/slide%d.xml", i+1), slide)
	}

	for name, data := range extra {
		writeZipEntry(t, w, name, data)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func savePptx(t *testing.T, addr, filename string, data []byte) string {
	t.Helper()
	rp := call(t, addr, "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString(data),
		Filename: filename,
	})
	if !rp.OK {
		t.Fatalf("save pptx 失败: %s", rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	return sr.ID
}

// TestReadPptx 读取 .pptx：按幻灯片输出，顺序正确，未截断。
func TestReadPptx(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePptx(t, srv.WsURL(), "汇报.pptx", makePptx(t, []string{"标题", "第一页正文"}, []string{"第二页标题"}))

	rr := readFile(t, srv.WsURL(), id)
	if rr.Truncated {
		t.Fatalf("小文件不应截断: %+v", rr)
	}
	want := "# 幻灯片 1\n标题\n第一页正文\n# 幻灯片 2\n第二页标题"
	if rr.Text != want {
		t.Fatalf("读取结果不符:\n%q\n想要:\n%q", rr.Text, want)
	}
}

// TestReadPptxTruncated 小阈值下在幻灯片边界截断。
func TestReadPptxTruncated(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 10) // 仅够“# 幻灯片 1\n标题”
	id := savePptx(t, srv.WsURL(), "汇报.pptx", makePptx(t, []string{"标题", "第一页正文"}, []string{"第二页标题"}))

	rr := readFile(t, srv.WsURL(), id)
	if !rr.Truncated {
		t.Fatalf("应截断: %+v", rr)
	}
	if rr.Text != "# 幻灯片 1\n标题" {
		t.Fatalf("截断结果不符: %q", rr.Text)
	}
}

// TestReadLegacyPpt 旧版 .ppt 明确拒绝并引导另存。
func TestReadLegacyPpt(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString([]byte("假装是旧格式")),
		Filename: "旧汇报.ppt",
		Mime:     "application/vnd.ms-powerpoint",
	})
	if !rp.OK {
		t.Fatalf("save 失败: %s", rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}

	rp2 := call(t, srv.WsURL(), "read", protocol.FileReadPayload{ID: sr.ID})
	if rp2.OK || !strings.Contains(rp2.Error, "另存为 .pptx") {
		t.Fatalf(".ppt 应报错并引导另存: %+v", rp2)
	}
}

// TestEditPpt 替换生效：read 见新值、fetch 二进制变化。
func TestEditPpt(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	orig := makePptx(t, []string{"标题", "第一页正文"}, []string{"第二页"})
	id := savePptx(t, srv.WsURL(), "汇报.pptx", orig)

	rp := call(t, srv.WsURL(), "edit_ppt", protocol.FilePptEditPayload{
		ID: id,
		Operations: []protocol.PptEditOperation{
			{Op: "replace", Old: "标题", New: "新标题"},
			{Op: "replace", Old: "第二页", New: "尾页"},
		},
	})
	if !rp.OK {
		t.Fatalf("edit_ppt 失败: %s", rp.Error)
	}
	var pr protocol.FilePptEditResult
	if err := protocol.DecodeRaw(rp.Data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Replaced != 2 || pr.ID != id {
		t.Fatalf("edit_ppt 结果不符: %+v", pr)
	}

	rr := readFile(t, srv.WsURL(), id)
	want := "# 幻灯片 1\n新标题\n第一页正文\n# 幻灯片 2\n尾页"
	if rr.Text != want {
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

// TestEditPptErrors 错误路径：非 pptx、空 operations、未知 op、old 未找到。
func TestEditPptErrors(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePptx(t, srv.WsURL(), "汇报.pptx", makePptx(t, []string{"标题"}))

	// 非 pptx。
	txtID := saveText(t, srv.WsURL(), "a.txt", "文本")
	bad1 := call(t, srv.WsURL(), "edit_ppt", protocol.FilePptEditPayload{
		ID:         txtID,
		Operations: []protocol.PptEditOperation{{Op: "replace", Old: "x", New: "y"}},
	})
	if bad1.OK || !strings.Contains(bad1.Error, "仅支持编辑 .pptx") {
		t.Fatalf("非 pptx 应拒绝: %+v", bad1)
	}

	// 空 operations。
	bad2 := call(t, srv.WsURL(), "edit_ppt", protocol.FilePptEditPayload{ID: id})
	if bad2.OK || !strings.Contains(bad2.Error, "operations 至少一条") {
		t.Fatalf("空 operations 应报错: %+v", bad2)
	}

	// 未知 op。
	bad3 := call(t, srv.WsURL(), "edit_ppt", protocol.FilePptEditPayload{
		ID:         id,
		Operations: []protocol.PptEditOperation{{Op: "delete", Old: "标题"}},
	})
	if bad3.OK || !strings.Contains(bad3.Error, "不支持 op") {
		t.Fatalf("未知 op 应报错: %+v", bad3)
	}

	// old 未找到。
	bad4 := call(t, srv.WsURL(), "edit_ppt", protocol.FilePptEditPayload{
		ID:         id,
		Operations: []protocol.PptEditOperation{{Op: "replace", Old: "不存在", New: "x"}},
	})
	if bad4.OK || !strings.Contains(bad4.Error, "未找到") {
		t.Fatalf("old 未找到应报错: %+v", bad4)
	}

	// old 为空。
	bad5 := call(t, srv.WsURL(), "edit_ppt", protocol.FilePptEditPayload{
		ID:         id,
		Operations: []protocol.PptEditOperation{{Op: "replace", Old: "   ", New: "x"}},
	})
	if bad5.OK || !strings.Contains(bad5.Error, "old 不能为空") {
		t.Fatalf("old 为空应报错: %+v", bad5)
	}
}

// TestEditPptSpecialChars 含撇号与 > 的文本可被正确匹配并替换（转义仅针对 & < >）。
func TestEditPptSpecialChars(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePptx(t, srv.WsURL(), "汇报.pptx", makePptx(t, []string{"it's 和 a>b"}))

	rp := call(t, srv.WsURL(), "edit_ppt", protocol.FilePptEditPayload{
		ID: id,
		Operations: []protocol.PptEditOperation{
			{Op: "replace", Old: "it's", New: "it is"},
			{Op: "replace", Old: "a>b", New: "a大于b"},
		},
	})
	if !rp.OK {
		t.Fatalf("edit_ppt 失败: %s", rp.Error)
	}
	var pr protocol.FilePptEditResult
	if err := protocol.DecodeRaw(rp.Data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Replaced != 2 {
		t.Fatalf("特殊字符替换条数不符: %+v", pr)
	}

	rr := readFile(t, srv.WsURL(), id)
	want := "# 幻灯片 1\nit is 和 a大于b"
	if rr.Text != want {
		t.Fatalf("特殊字符替换未生效:\n%q\n想要:\n%q", rr.Text, want)
	}
}

// TestReadPptxIgnoresNonSlideEntries 非标准命名的幻灯片条目不影响读取（严格匹配 slideN.xml）。
func TestReadPptxIgnoresNonSlideEntries(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	data := makePptxExtra(t, map[string]string{
		"ppt/slides/slideX.xml": `<xml>不是幻灯片</xml>`,
		"ppt/slides/readme.txt": `说明`,
	}, []string{"正常页"})
	id := savePptx(t, srv.WsURL(), "汇报.pptx", data)

	rr := readFile(t, srv.WsURL(), id)
	want := "# 幻灯片 1\n正常页"
	if rr.Text != want {
		t.Fatalf("非标准条目应被忽略:\n%q\n想要:\n%q", rr.Text, want)
	}
}
