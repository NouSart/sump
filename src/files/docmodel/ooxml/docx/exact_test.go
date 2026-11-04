package docx

// 精确指纹的模型层回归：空白漂移后，旧 rune 坐标必须失效。

import (
	"context"
	"testing"

	"github.com/NouSart/sump/files/docmodel"
)

// TestExactAnchorRejectsWhitespaceDrift 规范化指纹会把连续空白折叠；
// 带 rune 区间的编辑必须用精确指纹，否则空白一变旧坐标就可能改错位置。
func TestExactAnchorRejectsWhitespaceDrift(t *testing.T) {
	d := mustOpen(t, docxBody(t, `<w:p><w:r><w:t>金额 100 元</w:t></w:r></w:p>`))
	old := d.paras["body/p1"].text
	exact := docmodel.ExactFingerprint(old)
	if docmodel.Fingerprint(old) != docmodel.Fingerprint("金额  100 元") {
		t.Fatal("前置假设不成立：规范化指纹应折叠连续空白")
	}

	// 先制造一次纯空白变化：文字没变，但 rune 坐标已经整体右移。
	if _, err := d.Patch(context.Background(), []docmodel.PatchOp{{
		Op: docmodel.OpReplaceText, Node: "body/p1", Text: "金额  100 元",
	}}); err != nil {
		t.Fatalf("空白变化失败: %v", err)
	}
	before := d.paras["body/p1"].text
	if _, err := d.Patch(context.Background(), []docmodel.PatchOp{{
		Op: docmodel.OpReplaceText, Node: "body/p1",
		Range: &docmodel.TextRange{Start: 3, End: 6}, Text: "200",
		ExactAnchor: exact,
	}}); err == nil {
		t.Fatal("空白漂移后旧精确锚点应被拒绝")
	}
	if got := d.paras["body/p1"].text; got != before {
		t.Fatalf("被拒绝的编辑改动了文本: %q → %q", before, got)
	}

	newExact := docmodel.ExactFingerprint(before)
	if _, err := d.Patch(context.Background(), []docmodel.PatchOp{{
		Op: docmodel.OpReplaceText, Node: "body/p1",
		Range: &docmodel.TextRange{Start: 4, End: 7}, Text: "200",
		ExactAnchor: newExact,
	}}); err != nil {
		t.Fatalf("精确锚点正确却失败: %v", err)
	}
	if got := d.paras["body/p1"].text; got != "金额  200 元" {
		t.Fatalf("改后文本 = %q，期望 金额  200 元", got)
	}
}
