// 解压封顶的防线测试（内部测试：能把 maxExpandBytes 临时调小，不必造 256MiB 夹具）。
package server

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"github.com/NouSart/sump/protocol"
)

// tinyDocx 造一个最小可用的 .docx（正文两段）。
func tinyDocx(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	write := func(name, data string) {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	write("[Content_Types].xml", `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`)
	write("word/document.xml", `<?xml version="1.0"?><w:document xmlns:w="x"><w:body><w:p><w:r><w:t>你好世界</w:t></w:r></w:p></w:body></w:document>`)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestDocxExpandBudget 验：预算调小后 docx 的 read/edit 都被拒（读到的是超限错误），
// 预算正常时同一份文件照常可读可改（证明不是无差别拒绝）。
func TestDocxExpandBudget(t *testing.T) {
	data := tinyDocx(t)

	// 预算充足：正常路径不受影响
	text, truncated, err := docxToText(data, 4000, maxExpandBytes)
	if err != nil {
		t.Fatalf("预算充足时不该失败: %v", err)
	}
	if text != "你好世界" || truncated {
		t.Fatalf("正常路径读取不符: %q truncated=%v", text, truncated)
	}
	if _, n, err := editDocx(data, []protocol.WordEditOperation{{Op: "replace", Old: "你好", New: "哈罗"}}, maxExpandBytes); err != nil || n != 1 {
		t.Fatalf("预算充足时编辑应成功: n=%d err=%v", n, err)
	}

	// 预算极小：read 与 edit 都必须明确拒绝，且理由里带"压缩炸弹防线"
	old := maxExpandBytes
	maxExpandBytes = 16
	defer func() { maxExpandBytes = old }()

	if _, _, err := docxToText(data, 4000, maxExpandBytes); err == nil || !strings.Contains(err.Error(), "压缩炸弹防线") {
		t.Fatalf("超预算的 docx 应被拒绝: %v", err)
	}
	if _, _, err := editDocx(data, []protocol.WordEditOperation{{Op: "replace", Old: "你好", New: "哈罗"}}, maxExpandBytes); err == nil {
		t.Fatalf("超预算的 docx 编辑应被拒绝: %v", err)
	}
}

// TestDocxDeclaredHugeEntry 验第一道防线（zip 目录预检）：一个实体在目录里**声明**了
// 超大解压尺寸的包，即使盘上文件很小（连内容都没有），也必须被挡在读取之前。
func TestDocxDeclaredHugeEntry(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	// CreateRaw 原样写入给定 Header：声明 UncompressedSize64 为 1TiB，实际不写数据
	hdr := &zip.FileHeader{Name: docxDocumentXML, Method: zip.Store}
	hdr.UncompressedSize64 = 1 << 40
	if _, err := w.CreateRaw(hdr); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if _, _, err := docxToText(buf.Bytes(), 4000, maxExpandBytes); err == nil || !strings.Contains(err.Error(), "压缩炸弹防线") {
		t.Fatalf("目录里声明超大解压尺寸的包应被拒绝: %v", err)
	}
}

// TestXlsxExpandBudget 验 xlsx 这条链路（excelize 的 UnzipSizeLimit 传得进去）：
// 预算调小后 editXlsx 报错且理由带防线字样；预算正常时同一份文件可编辑。
func TestXlsxExpandBudget(t *testing.T) {
	f := excelize.NewFile()
	defer f.Close()
	if err := f.SetCellValue("Sheet1", "A1", "姓名"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()

	out, n, err := editXlsx(data, "Sheet1", []protocol.FileWriteEdit{{Cell: "B1", Value: "值"}})
	if err != nil || n != 1 || len(out) == 0 {
		t.Fatalf("预算充足时应能编辑: n=%d err=%v", n, err)
	}

	old := maxExpandBytes
	maxExpandBytes = 64
	defer func() { maxExpandBytes = old }()

	if _, _, err := editXlsx(data, "Sheet1", []protocol.FileWriteEdit{{Cell: "B1", Value: "值"}}); err == nil || !strings.Contains(err.Error(), "压缩炸弹防线") {
		t.Fatalf("超预算的 xlsx 应被拒绝并说明防线: %v", err)
	}
}

// 让 "io" 的导入在将来裁剪代码时立刻显形（本文件用到 io.LimitReader 的语义验证在 word.go）。
var _ = io.Discard
var _ = fmt.Sprintf
