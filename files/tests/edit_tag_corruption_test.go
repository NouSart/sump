package tests

// 回归：edit_ppt / edit_word 的替换只能在文本节点内生效（#15）。
//
// 之前的实现对整段 XML 做字符串替换，old="t" 会把 <a:t> 改成 <a:Z>：
// 动作返回「成功 13 处」，读回来却是空文本，文件已经写回——静默毁文件。

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

// TestEditPptDoesNotRewriteTags 把只在标签里出现的字符当作搜索词：
// 必须报「未找到」，而不是改坏标签后报成功。
func TestEditPptDoesNotRewriteTags(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	orig := makePptx(t, []string{"abc"})
	id := savePptx(t, srv.WsURL(), "deck.pptx", orig)

	// "t" 与 "p" 只出现在标签/命名空间里，正文是 "abc" 没有它们。
	for _, old := range []string{"t", "p"} {
		rp := call(t, srv.WsURL(), "edit_ppt", protocol.FilePptEditPayload{
			ID:         id,
			Operations: []protocol.PptEditOperation{{Op: "replace", Old: old, New: "Z"}},
		})
		if rp.OK {
			t.Fatalf("old=%q 只出现在标签里，不该替换成功", old)
		}
		if !strings.Contains(rp.Error, "未找到文本") {
			t.Fatalf("old=%q 的错误信息应说明未找到文本，实际: %s", old, rp.Error)
		}
	}

	// 关键断言：文件没被写坏，正文仍读得出来。
	rr := readFile(t, srv.WsURL(), id)
	if rr.Text != "# 幻灯片 1\nabc" {
		t.Fatalf("标签被改坏，正文丢失:\n%q", rr.Text)
	}
}

// TestEditPptReplacesTextOnly 正常文本替换仍然生效，且不影响标签。
func TestEditPptReplacesTextOnly(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	orig := makePptx(t, []string{"abc"})
	id := savePptx(t, srv.WsURL(), "deck.pptx", orig)

	rp := call(t, srv.WsURL(), "edit_ppt", protocol.FilePptEditPayload{
		ID:         id,
		Operations: []protocol.PptEditOperation{{Op: "replace", Old: "abc", New: "XYZ"}},
	})
	if !rp.OK {
		t.Fatalf("edit_ppt 失败: %s", rp.Error)
	}
	var pr protocol.FilePptEditResult
	if err := protocol.DecodeRaw(rp.Data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Replaced != 1 {
		t.Fatalf("应替换 1 处，实际 %+v", pr)
	}
	if rr := readFile(t, srv.WsURL(), id); rr.Text != "# 幻灯片 1\nXYZ" {
		t.Fatalf("替换结果不符: %q", rr.Text)
	}
}

// TestEditPptEmptyNewDoesNotEmptyDocument 复现 issue 里的第二行：
// old="a" new="" 以前会返回 18 处「成功」并让文本全没。
func TestEditPptEmptyNewDoesNotEmptyDocument(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	orig := makePptx(t, []string{"abc"})
	id := savePptx(t, srv.WsURL(), "deck.pptx", orig)

	rp := call(t, srv.WsURL(), "edit_ppt", protocol.FilePptEditPayload{
		ID:         id,
		Operations: []protocol.PptEditOperation{{Op: "replace", Old: "a", New: ""}},
	})
	if !rp.OK {
		t.Fatalf("删掉正文里的 a 应该成功: %s", rp.Error)
	}
	var pr protocol.FilePptEditResult
	if err := protocol.DecodeRaw(rp.Data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Replaced != 1 {
		t.Fatalf("只应替换正文里的 1 处 a，实际 %+v", pr)
	}
	if rr := readFile(t, srv.WsURL(), id); rr.Text != "# 幻灯片 1\nbc" {
		t.Fatalf("替换结果不符（标签可能被改）: %q", rr.Text)
	}
}

// TestEditWordDoesNotRewriteTags 同一保证作用于 .docx。
func TestEditWordDoesNotRewriteTags(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	orig := makeDocx(t, "abc")
	id := saveDocx(t, srv.WsURL(), "doc.docx", orig)

	rp := call(t, srv.WsURL(), "edit_word", protocol.FileWordEditPayload{
		ID:         id,
		Operations: []protocol.WordEditOperation{{Op: "replace", Old: "w", New: "Z"}},
	})
	if rp.OK {
		t.Fatal("old=\"w\" 只出现在 xmlns 与标签里，不该替换成功")
	}
	if rr := readFile(t, srv.WsURL(), id); rr.Text != "abc" {
		t.Fatalf("标签被改坏，正文丢失: %q", rr.Text)
	}
}
