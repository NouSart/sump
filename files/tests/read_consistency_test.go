package tests

// read 的一致性回归：read 与 read_range 必须是同一份投影（空段落、文本框都算），
// 且 text / docx / xlsx / pptx / pdf 都能用 offset/limit 续读到 complete。

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

// readChunk 调一次 read 并解码结果。
func readChunk(t *testing.T, addr, id string, offset, limit int) protocol.FileReadResult {
	t.Helper()
	rp := call(t, addr, "read", protocol.FileReadPayload{ID: id, Offset: offset, Limit: limit})
	if !rp.OK {
		t.Fatalf("read(offset=%d, limit=%d) 失败: %s", offset, limit, rp.Error)
	}
	var rr protocol.FileReadResult
	if err := protocol.DecodeRaw(rp.Data, &rr); err != nil {
		t.Fatal(err)
	}
	return rr
}

// readAllChunks 按 next 续读到末尾，返回拼接文本与实际块数。
func readAllChunks(t *testing.T, addr, id string, limit int) (string, int) {
	t.Helper()
	var b strings.Builder
	offset, chunks := 0, 0
	for {
		rr := readChunk(t, addr, id, offset, limit)
		b.WriteString(rr.Text)
		chunks++
		if rr.Next == 0 {
			if got := len([]rune(b.String())); rr.Total > 0 && got != rr.Total {
				t.Fatalf("续读拼接 %d 字，但 total=%d（offset=%d, limit=%d）", got, rr.Total, offset, limit)
			}
			return b.String(), chunks
		}
		if rr.Next <= offset {
			t.Fatalf("next 没有前进: offset=%d next=%d", offset, rr.Next)
		}
		offset = rr.Next
		if chunks > 10000 {
			t.Fatal("续读没有终止")
		}
	}
}

// TestDocxReadMatchesReadRange 空段落与文本框段落必须在两条读取路径里完全一致。
func TestDocxReadMatchesReadRange(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	body := `<w:p><w:r><w:t>外层</w:t></w:r>` +
		`<w:r><w:drawing xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" ` +
		`xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" ` +
		`xmlns:wps="http://schemas.microsoft.com/office/word/2010/wordprocessingShape">` +
		`<wp:inline><a:graphic><a:graphicData><wps:wsp><wps:txbx><w:txbxContent>` +
		`<w:p><w:r><w:t>框内</w:t></w:r></w:p>` +
		`</w:txbxContent></wps:txbx></wps:wsp></a:graphicData></a:graphic></wp:inline>` +
		`</w:drawing></w:r></w:p>` +
		`<w:p/>`
	id := saveDocx(t, srv.WsURL(), "同口径.docx", makeDocxRaw(t, body))

	rr := readFile(t, srv.WsURL(), id)
	rp := call(t, srv.WsURL(), "read_range", protocol.DocReadPayload{ID: id, Limit: 4000})
	if !rp.OK {
		t.Fatalf("read_range 失败: %s", rp.Error)
	}
	var dr protocol.DocReadResult
	if err := protocol.DecodeRaw(rp.Data, &dr); err != nil {
		t.Fatal(err)
	}
	if rr.Text != dr.Text {
		t.Fatalf("read 与 read_range 投影不一致:\nread=%q\nrange=%q", rr.Text, dr.Text)
	}
	if want := "外层\n框内\n"; rr.Text != want {
		t.Fatalf("文本 = %q，期望 %q（空段落必须保留）", rr.Text, want)
	}

	ol := outline(t, srv.WsURL(), id, 1)
	ids := map[string]bool{}
	for _, n := range ol.Nodes {
		ids[n.ID] = true
	}
	if !ids["body/p1"] || !ids["body/p1/p1"] || !ids["body/p2"] {
		t.Fatalf("outline 缺少文本框/空段落节点: %+v", ol.Nodes)
	}
}

// TestReadContinuationAllFormats 每种格式都用同一组 offset/next 拼回全文。
func TestReadContinuationAllFormats(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	type fixture struct {
		name string
		id   string
	}
	cases := []fixture{
		{"text", saveBytes(t, srv.WsURL(), "续读.txt", []byte("0123456789abcdef"))},
		{"docx", saveDocx(t, srv.WsURL(), "续读.docx", makeDocx(t, "第一段", "第二段", "第三段"))},
		{"xlsx", saveXlsx(t, srv.WsURL(), "续读.xlsx", makeXlsx(t))},
		{"pptx", savePptx(t, srv.WsURL(), "续读.pptx", makePptx(t, []string{"标题", "正文"}, []string{"第二页"}))},
		{"pdf", savePdf(t, srv.WsURL(), "续读.pdf", makePdf(t,
			"BT /F1 24 Tf 72 720 Td (Hello) Tj ET",
			"BT /F1 24 Tf 72 720 Td (World) Tj ET"))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 小窗口连续读：重点验证块边界拼接不丢字、不多字。
			chunked, chunks := readAllChunks(t, srv.WsURL(), c.id, 5)
			full := readChunk(t, srv.WsURL(), c.id, 0, 4000)
			if full.Truncated {
				t.Fatalf("测试夹具不该超过完整读取上限: total=%d", full.Total)
			}
			if chunked != full.Text {
				t.Fatalf("续读拼接不一致（%d 块）:\nchunked=%q\nfull=%q", chunks, chunked, full.Text)
			}
			if chunks < 2 {
				t.Fatalf("窗口 5 字应至少分两块，实际 %d 块（text=%q）", chunks, full.Text)
			}
			if got := len([]rune(chunked)); got != full.Total {
				t.Fatalf("拼接长度 %d != total %d", got, full.Total)
			}
		})
	}
}

// TestReadRejectsNegativeWindow 续读参数非法时明确拒绝，不能悄悄当成 0。
func TestReadRejectsNegativeWindow(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := saveBytes(t, srv.WsURL(), "窗口.txt", []byte("0123456789"))

	for _, c := range []struct {
		name string
		in   protocol.FileReadPayload
	}{
		{"negative_offset", protocol.FileReadPayload{ID: id, Offset: -1}},
		{"negative_limit", protocol.FileReadPayload{ID: id, Limit: -1}},
	} {
		rp := call(t, srv.WsURL(), "read", c.in)
		if rp.OK || !strings.Contains(rp.Error, "不能为负") {
			t.Fatalf("%s 应被拒绝: %+v", c.name, rp)
		}
	}
}

// saveBytes 存任意文本文件（新测试里避免重复 base64 样板）。
func saveBytes(t *testing.T, addr, filename string, data []byte) string {
	t.Helper()
	rp := call(t, addr, "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString(data),
		Filename: filename,
	})
	if !rp.OK {
		t.Fatalf("save %s 失败: %s", filename, rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	return sr.ID
}
