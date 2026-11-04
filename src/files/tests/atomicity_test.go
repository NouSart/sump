package tests

// 失败原子性实验：一次编辑请求里先放合法操作、再放非法操作。
// 契约来自 README 的"就地修改权威副本"：请求报错时，权威副本必须一位不差。
// 只比 digest 不够——digest 只看可见文本，可能漏掉样式/结构/元数据的半改。

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/NouSart/sump/protocol"
)

func TestFailedEditsLeaveAuthorityBytesUnchanged(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)

	cases := []struct {
		name     string
		filename string
		data     []byte
		action   string
		// payload 接收文件 id，返回该动作的请求体。
		payload func(id string) any
	}{
		{
			name: "xlsx", filename: "原子.xlsx", data: makeXlsx(t), action: "write",
			payload: func(id string) any {
				return protocol.FileWritePayload{ID: id, Sheet: "Sheet1", Edits: []protocol.FileWriteEdit{
					{Cell: "A1", Value: "合法修改"},
					{Cell: "!", Value: "非法坐标"},
				}}
			},
		},
		{
			name: "docx_edit_word", filename: "原子.docx", data: makeDocx(t, "原文"), action: "edit_word",
			payload: func(id string) any {
				return protocol.FileWordEditPayload{ID: id, Operations: []protocol.WordEditOperation{
					{Op: "replace", Old: "原文", New: "合法修改"},
					{Op: "insert", Old: "x", New: "y"},
				}}
			},
		},
		{
			name: "docx_patch", filename: "原子_patch.docx", data: makeDocx(t, "原文"), action: "patch",
			payload: func(id string) any {
				return protocol.DocPatchPayload{ID: id, Operations: []protocol.DocPatchOp{
					{Op: "replace_text", Node: "body/p1", Text: "合法修改"},
					{Op: "replace_text", Node: "body/p99", Text: "非法节点"},
				}}
			},
		},
		{
			name: "pptx", filename: "原子.pptx", data: makePptx(t, []string{"原文"}), action: "edit_ppt",
			payload: func(id string) any {
				return protocol.FilePptEditPayload{ID: id, Operations: []protocol.PptEditOperation{
					{Op: "replace", Old: "原文", New: "合法修改"},
					{Op: "insert", Old: "x", New: "y"},
				}}
			},
		},
		{
			name: "pdf", filename: "原子.pdf", data: makePdf(t,
				"BT /F1 24 Tf 72 720 Td (One) Tj ET",
				"BT /F1 24 Tf 72 720 Td (Two) Tj ET"), action: "edit_pdf",
			payload: func(id string) any {
				return protocol.FilePdfEditPayload{ID: id, Operations: []protocol.PdfEditOperation{
					{Op: "delete_pages", Pages: "2"},
					{Op: "rotate", Degrees: 45},
				}}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{
				Data: base64.StdEncoding.EncodeToString(tc.data), Filename: tc.filename,
			})
			if !rp.OK {
				t.Fatalf("save 失败: %s", rp.Error)
			}
			var sr protocol.FileSaveResult
			if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
				t.Fatal(err)
			}

			before := fetchBytes(t, srv.WsURL(), sr.ID)
			sumBefore := sha256.Sum256(before)

			bad := call(t, srv.WsURL(), tc.action, tc.payload(sr.ID))
			if bad.OK {
				t.Fatalf("第二条非法操作应让整个请求失败，实际成功")
			}

			after := fetchBytes(t, srv.WsURL(), sr.ID)
			sumAfter := sha256.Sum256(after)
			if !bytes.Equal(before, after) {
				t.Fatalf("失败请求改动了权威副本: sha256 %x → %x（%d → %d 字节，错误=%s）",
					sumBefore, sumAfter, len(before), len(after), bad.Error)
			}
		})
	}

	// 对照：把每个用例的第二条也改成合法操作，证明"哈希不变"不是因为编辑根本没生效。
	t.Run("control_docx_patch_applies", func(t *testing.T) {
		id := saveDocx(t, srv.WsURL(), "对照.docx", makeDocx(t, "原文"))
		before := fetchBytes(t, srv.WsURL(), id)
		rp := call(t, srv.WsURL(), "patch", protocol.DocPatchPayload{ID: id,
			Operations: []protocol.DocPatchOp{
				{Op: "replace_text", Node: "body/p1", Text: "合法修改"},
			}})
		if !rp.OK {
			t.Fatalf("对照 patch 失败: %s", rp.Error)
		}
		after := fetchBytes(t, srv.WsURL(), id)
		if bytes.Equal(before, after) {
			t.Fatal("合法 patch 后二进制未变化（实验对照失效，无法证明原子性）")
		}
	})
}
