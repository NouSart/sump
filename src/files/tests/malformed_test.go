package tests

// 异常文件实验：垃圾内容 + 截断的合法文件，逐格式验证：
//   - read 明确失败（不 panic、不把坏字节当文本吞掉）
//   - edit_* / write 明确失败
//   - 权威副本的二进制在失败前后完全一致

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

func TestMalformedFilesFailWithoutChangingAuthority(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)

	type format struct {
		name     string
		filename string
		mime     string
		valid    []byte
		action   string
		payload  func(id string) any
	}
	formats := []format{
		{
			name: "docx", filename: "坏.docx", mime: "application/octet-stream",
			valid: makeDocx(t, "原文"), action: "edit_word",
			payload: func(id string) any {
				return protocol.FileWordEditPayload{ID: id, Operations: []protocol.WordEditOperation{
					{Op: "replace", Old: "原文", New: "改"},
				}}
			},
		},
		{
			name: "xlsx", filename: "坏.xlsx", mime: "application/octet-stream",
			valid: makeXlsx(t), action: "write",
			payload: func(id string) any {
				return protocol.FileWritePayload{ID: id, Sheet: "Sheet1",
					Edits: []protocol.FileWriteEdit{{Cell: "A1", Value: "改"}}}
			},
		},
		{
			name: "pptx", filename: "坏.pptx", mime: "application/octet-stream",
			valid: makePptx(t, []string{"原文"}), action: "edit_ppt",
			payload: func(id string) any {
				return protocol.FilePptEditPayload{ID: id, Operations: []protocol.PptEditOperation{
					{Op: "replace", Old: "原文", New: "改"},
				}}
			},
		},
		{
			name: "pdf", filename: "坏.pdf", mime: "application/pdf",
			// 内容流足够长，3/5 处会切断流对象本身；只缺尾部 xref 的 PDF 是 pdfcpu 可修复的，不能作为"坏文件"。
			valid:  makePdf(t, "BT /F1 24 Tf 72 720 Td ("+strings.Repeat("A", 4096)+") Tj ET"),
			action: "edit_pdf",
			payload: func(id string) any {
				return protocol.FilePdfEditPayload{ID: id, Operations: []protocol.PdfEditOperation{
					{Op: "rotate", Degrees: 90},
				}}
			},
		},
	}

	for _, f := range formats {
		for _, variant := range []struct {
			name string
			data []byte
		}{
			{"garbage", []byte("这不是一个真的 " + f.name + " 文件，只是伪装扩展名。")},
			{"truncated", f.valid[:len(f.valid)*3/5]},
		} {
			t.Run(f.name+"_"+variant.name, func(t *testing.T) {
				rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{
					Data:     base64.StdEncoding.EncodeToString(variant.data),
					Filename: f.filename, Mime: f.mime,
				})
				if !rp.OK {
					t.Fatalf("save 失败: %s", rp.Error)
				}
				var sr protocol.FileSaveResult
				if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
					t.Fatal(err)
				}
				before := fetchBytes(t, srv.WsURL(), sr.ID)

				if rp := call(t, srv.WsURL(), "read", protocol.FileReadPayload{ID: sr.ID}); rp.OK {
					t.Fatalf("坏文件 read 不应成功，实际文本=%+v", rp)
				}
				if rp := call(t, srv.WsURL(), f.action, f.payload(sr.ID)); rp.OK {
					t.Fatalf("坏文件 %s 不应成功", f.action)
				}

				after := fetchBytes(t, srv.WsURL(), sr.ID)
				if !bytes.Equal(before, after) {
					t.Fatalf("失败的编辑改动了坏文件: %d → %d 字节", len(before), len(after))
				}
			})
		}
	}

	// 跨格式伪装：字节是合法 xlsx，名字却是 .docx。不能因为 zip 能打开就当 docx。
	t.Run("xlsx_bytes_named_docx", func(t *testing.T) {
		rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{
			Data:     base64.StdEncoding.EncodeToString(makeXlsx(t)),
			Filename: "其实是表格.docx", Mime: "application/octet-stream",
		})
		if !rp.OK {
			t.Fatalf("save 失败: %s", rp.Error)
		}
		var sr protocol.FileSaveResult
		if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
			t.Fatal(err)
		}
		before := fetchBytes(t, srv.WsURL(), sr.ID)
		if rp := call(t, srv.WsURL(), "read", protocol.FileReadPayload{ID: sr.ID}); rp.OK {
			t.Fatal("扩展名伪装成 docx 的 xlsx 不应按 docx 读成功")
		}
		if rp := call(t, srv.WsURL(), "edit_word", protocol.FileWordEditPayload{ID: sr.ID,
			Operations: []protocol.WordEditOperation{{Op: "replace", Old: "张三", New: "改"}}}); rp.OK {
			t.Fatal("扩展名伪装成 docx 的 xlsx 不应按 docx 编辑成功")
		}
		if !bytes.Equal(before, fetchBytes(t, srv.WsURL(), sr.ID)) {
			t.Fatal("失败的跨格式编辑改动了文件")
		}
	})
}
