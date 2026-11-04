package tests

// 文档模型链（outline / read_range / find / patch / verify）的服务级测试：
// 真 WS + 真 SQLite/文件，走"看结构 → 定位 → 只改那一处 → 自查"的完整来回。

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/NouSart/sump/files/docmodel"
	"github.com/NouSart/sump/protocol"
)

// makeDocxRaw 用给定 body 片段拼出最小 .docx（多 run / 换行等真实形状用得到）。
func makeDocxRaw(t *testing.T, body string) []byte {
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
	doc := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>` + body + `</w:body>
</w:document>`
	writeZipEntry(t, w, "word/document.xml", doc)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// outline 调 files.outline 并断言成功。
func outline(t *testing.T, addr, id string, depth int) protocol.DocOutlineResult {
	t.Helper()
	rp := call(t, addr, "outline", protocol.DocOutlinePayload{ID: id, Depth: depth})
	if !rp.OK {
		t.Fatalf("outline 失败: %s", rp.Error)
	}
	var out protocol.DocOutlineResult
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestDocOutlineFindPatchVerify 是本组动作的主干：先看结构，再定位，只改一处，改完自查。
func TestDocOutlineFindPatchVerify(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	// Word 常把一句话拆成多个 run：跨 run 的段落是这里的重点。
	src := makeDocxRaw(t, `<w:p><w:r><w:t>上海某某</w:t></w:r><w:r><w:t>科技有限公司</w:t></w:r></w:p>`+
		`<w:p><w:r><w:t>合计 100 元</w:t></w:r></w:p>`)
	id := saveDocx(t, srv.WsURL(), "合同.docx", src)

	// 1) outline：结构 + 指纹
	ol := outline(t, srv.WsURL(), id, 1)
	if ol.DocKind != "docx" || len(ol.Nodes) != 2 || ol.Digest == "" {
		t.Fatalf("outline 结果不符: %+v", ol)
	}
	if ol.Nodes[0].ID != "body/p1" || !strings.Contains(ol.Nodes[0].Text, "上海某某") {
		t.Fatalf("首节点 = %+v", ol.Nodes[0])
	}
	if ol.NodeCount < 2 || ol.Chars == 0 || ol.Parts == 0 {
		t.Fatalf("规模统计不符: %+v", ol)
	}

	// 2) find：只回位置，不改文档
	rp := call(t, srv.WsURL(), "find", protocol.DocFindPayload{ID: id, Text: "科技有限公司"})
	if !rp.OK {
		t.Fatalf("find 失败: %s", rp.Error)
	}
	var fr protocol.DocFindResult
	if err := protocol.DecodeRaw(rp.Data, &fr); err != nil {
		t.Fatal(err)
	}
	if len(fr.Matches) != 1 {
		t.Fatalf("命中数 = %d，期望 1", len(fr.Matches))
	}
	m := fr.Matches[0]
	if m.Node != "body/p1" || m.Run != "body/p1/r2" || m.Start != 4 || m.End != 10 {
		t.Fatalf("命中 = %+v", m)
	}
	if fr.Digest != ol.Digest {
		t.Fatalf("同一份文件两次读到的 digest 不同: %s vs %s", fr.Digest, ol.Digest)
	}

	// 3) patch：带整篇 anchor，只改 body/p2 里的金额
	pr := call(t, srv.WsURL(), "patch", protocol.DocPatchPayload{
		ID:     id,
		Anchor: ol.Digest,
		Operations: []protocol.DocPatchOp{{
			Op: string(docmodel.OpReplaceText), Node: "body/p2",
			Range:  &protocol.DocRange{Start: 3, End: 6}, // 100 → 200
			Text:   "200",
			Anchor: docmodel.Fingerprint("合计 100 元"),
		}},
	})
	if !pr.OK {
		t.Fatalf("patch 失败: %s", pr.Error)
	}
	var pres protocol.DocPatchResult
	if err := protocol.DecodeRaw(pr.Data, &pres); err != nil {
		t.Fatal(err)
	}
	if pres.Applied != 1 || len(pres.Diff) != 1 || pres.Digest == ol.Digest {
		t.Fatalf("patch 结果不符: %+v", pres)
	}
	if pres.Diff[0].Node != "body/p2" || !strings.Contains(pres.Diff[0].After, "200") {
		t.Fatalf("diff 不符: %+v", pres.Diff[0])
	}

	// 4) read_range：落库后再读，改的是权威副本
	rrp := call(t, srv.WsURL(), "read_range", protocol.DocReadPayload{ID: id, Node: "body/p2"})
	if !rrp.OK {
		t.Fatalf("read_range 失败: %s", rrp.Error)
	}
	var rr protocol.DocReadResult
	if err := protocol.DecodeRaw(rrp.Data, &rr); err != nil {
		t.Fatal(err)
	}
	if rr.DocKind != "docx" || rr.Text != "合计 200 元" || rr.Total != len([]rune("合计 200 元")) || rr.Next != 0 {
		t.Fatalf("read_range 结果不符: %+v", rr)
	}
	if rr.Digest != pres.Digest {
		t.Fatalf("read_range 的 digest 应与 patch 返回的一致: %s vs %s", rr.Digest, pres.Digest)
	}

	// 5) verify：指纹 + 必出现 / 必不出现
	vp := call(t, srv.WsURL(), "verify", protocol.DocVerifyPayload{
		ID: id, Digest: pres.Digest,
		Contains:    []string{"合计 200 元"},
		NotContains: []string{"100 元"},
	})
	if !vp.OK {
		t.Fatalf("verify 失败: %s", vp.Error)
	}
	var vr protocol.DocVerifyResult
	if err := protocol.DecodeRaw(vp.Data, &vr); err != nil {
		t.Fatal(err)
	}
	if !vr.OK || len(vr.Failures) != 0 || vr.Digest != pres.Digest {
		t.Fatalf("verify 结果不符: %+v", vr)
	}

	// 6) verify 说真话：给错的期望必须报失败而不是报错
	bad := call(t, srv.WsURL(), "verify", protocol.DocVerifyPayload{
		ID: id, Contains: []string{"并不存在的句子"}, NotContains: []string{"合计 200 元"},
	})
	if !bad.OK {
		t.Fatalf("verify 调用本身不该失败: %s", bad.Error)
	}
	var br protocol.DocVerifyResult
	if err := protocol.DecodeRaw(bad.Data, &br); err != nil {
		t.Fatal(err)
	}
	if br.OK || len(br.Failures) != 2 {
		t.Fatalf("应有 2 条失败项: %+v", br)
	}
}

// TestDocPatchRejectsStaleAndBadInput anchor 过期 / 非 docx / 未知 op 都要明确回绝，
// 且失败后文件内容不变（不能被半改）。
func TestDocPatchRejectsStaleAndBadInput(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	src := makeDocxRaw(t, `<w:p><w:r><w:t>原文内容</w:t></w:r></w:p>`)
	id := saveDocx(t, srv.WsURL(), "协议.docx", src)
	digest := outline(t, srv.WsURL(), id, 1).Digest

	// anchor 对不上：拒绝，并提示重新读取
	rp := call(t, srv.WsURL(), "patch", protocol.DocPatchPayload{
		ID: id, Anchor: "0123456789abcdef",
		Operations: []protocol.DocPatchOp{{Op: "replace_text", Node: "body/p1", Text: "改坏了"}},
	})
	if rp.OK || !strings.Contains(rp.Error, "已变化") {
		t.Fatalf("过期 anchor 应被拒: %+v", rp)
	}

	// 未知 op：拒绝
	rp = call(t, srv.WsURL(), "patch", protocol.DocPatchPayload{
		ID:         id,
		Operations: []protocol.DocPatchOp{{Op: "insert_paragraph", Node: "body/p1", Text: "新段"}},
	})
	if rp.OK || !strings.Contains(rp.Error, "patch") {
		t.Fatalf("未知 op 应被拒: %+v", rp)
	}

	// 节点不存在：拒绝
	rp = call(t, srv.WsURL(), "patch", protocol.DocPatchPayload{
		ID:         id,
		Operations: []protocol.DocPatchOp{{Op: "replace_text", Node: "body/p99", Text: "x"}},
	})
	if rp.OK || !strings.Contains(rp.Error, "节点不存在") {
		t.Fatalf("未知节点应被拒: %+v", rp)
	}

	// 三次都失败后，内容与指纹必须原封不动
	if got := outline(t, srv.WsURL(), id, 1).Digest; got != digest {
		t.Fatalf("被拒绝的操作改动了文件: %s → %s", digest, got)
	}

	// 非 docx：给明确指引
	mdRP := call(t, srv.WsURL(), "save", protocol.FileSavePayload{
		Data: base64.StdEncoding.EncodeToString([]byte("# 笔记")), Filename: "笔记.md",
	})
	if !mdRP.OK {
		t.Fatalf("save 失败: %s", mdRP.Error)
	}
	var mdSave protocol.FileSaveResult
	if err := protocol.DecodeRaw(mdRP.Data, &mdSave); err != nil {
		t.Fatal(err)
	}
	if rp := call(t, srv.WsURL(), "outline", protocol.DocOutlinePayload{ID: mdSave.ID}); rp.OK || !strings.Contains(rp.Error, ".docx") {
		t.Fatalf("非 docx 应被拒: %+v", rp)
	}

	// 缺 id：拒绝
	if rp := call(t, srv.WsURL(), "outline", protocol.DocOutlinePayload{}); rp.OK || !strings.Contains(rp.Error, "id 必填") {
		t.Fatalf("缺 id 应被拒: %+v", rp)
	}
}

// TestDocReadRangeWhole 不指定 node 时读整篇，分页 next 能走完。
func TestDocReadRangeWhole(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := saveDocx(t, srv.WsURL(), "长文.docx", makeDocx(t, "第一段内容", "第二段内容"))

	rp := call(t, srv.WsURL(), "read_range", protocol.DocReadPayload{ID: id, Limit: 5})
	if !rp.OK {
		t.Fatalf("read_range 失败: %s", rp.Error)
	}
	var first protocol.DocReadResult
	if err := protocol.DecodeRaw(rp.Data, &first); err != nil {
		t.Fatal(err)
	}
	if first.Node != "body" || first.Next != 5 || len([]rune(first.Text)) != 5 {
		t.Fatalf("首块不符: %+v", first)
	}
	rp = call(t, srv.WsURL(), "read_range", protocol.DocReadPayload{ID: id, Offset: first.Next, Limit: 100})
	if !rp.OK {
		t.Fatalf("续读失败: %s", rp.Error)
	}
	var rest protocol.DocReadResult
	if err := protocol.DecodeRaw(rp.Data, &rest); err != nil {
		t.Fatal(err)
	}
	if rest.Next != 0 || first.Text+rest.Text != "第一段内容\n第二段内容" {
		t.Fatalf("分页拼接不符: %q + %q", first.Text, rest.Text)
	}
}
