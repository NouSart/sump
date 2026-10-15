package tests

// files Excel 测试：xlsx 读取（TSV 文本化 / 截断 / .xls 拒绝）与 write 编辑
// （改格生效 / fetch 二进制已变 / sheet 不存在 / 非法坐标 / 非 xlsx 拒绝）。

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// makeXlsx 程序化生成双表 xlsx：Sheet1 人员表 + “数据”表。
func makeXlsx(t *testing.T) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	rows := [][]string{
		{"姓名", "年龄", "城市"},
		{"张三", "30", "北京"},
		{"李四", "25", "上海"},
	}
	for i, row := range rows {
		for j, v := range row {
			cell, err := excelize.CoordinatesToCellName(j+1, i+1)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.SetCellValue("Sheet1", cell, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := f.NewSheet("数据"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("数据", "A1", "指标"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("数据", "B1", "季度营收上涨"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// saveXlsx 把 xlsx 字节存入 files，返回 id。
func saveXlsx(t *testing.T, addr, filename string, data []byte) string {
	t.Helper()
	rp := call(t, addr, "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString(data),
		Filename: filename,
	})
	if !rp.OK {
		t.Fatalf("save xlsx 失败: %s", rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	return sr.ID
}

// TestReadXlsx 读取 xlsx：逐表 TSV 输出（表头、内容、表序），未截断。
func TestReadXlsx(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := saveXlsx(t, srv.WsURL(), "报表.xlsx", makeXlsx(t))

	rr := readFile(t, srv.WsURL(), id)
	if rr.Truncated {
		t.Fatalf("小文件不应截断: %+v", rr)
	}
	for _, want := range []string{"# 工作表: Sheet1", "姓名", "张三", "30", "# 工作表: 数据", "季度营收上涨"} {
		if !strings.Contains(rr.Text, want) {
			t.Fatalf("TSV 缺少 %q:\n%s", want, rr.Text)
		}
	}
	if strings.Index(rr.Text, "# 工作表: Sheet1") > strings.Index(rr.Text, "# 工作表: 数据") {
		t.Fatalf("表序不符:\n%s", rr.Text)
	}
	if !strings.Contains(rr.Text, "张三\t30\t北京") {
		t.Fatalf("行应为制表符分列:\n%s", rr.Text)
	}
}

// TestReadXlsxTruncated 小阈值下 xlsx 在行边界截断并置 truncated。
func TestReadXlsxTruncated(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 30) // 仅容表头 + 一行多
	id := saveXlsx(t, srv.WsURL(), "报表.xlsx", makeXlsx(t))

	rr := readFile(t, srv.WsURL(), id)
	if !rr.Truncated {
		t.Fatalf("应截断: %+v", rr)
	}
	if !strings.HasPrefix(rr.Text, "# 工作表: Sheet1") {
		t.Fatalf("截断文本应以表头开头:\n%s", rr.Text)
	}
	if strings.Contains(rr.Text, "季度营收上涨") {
		t.Fatalf("第二张表不应进入截断文本:\n%s", rr.Text)
	}
}

// TestReadLegacyXls 旧版 .xls 明确拒绝并引导另存（显式 vnd.ms-excel mime——
// 文本内容嗅探会得 text/plain，覆盖不到旧格式判定）。
func TestReadLegacyXls(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString([]byte("假装是旧格式")),
		Filename: "旧表.xls",
		Mime:     "application/vnd.ms-excel",
	})
	if !rp.OK {
		t.Fatalf("save 失败: %s", rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}

	rp2 := call(t, srv.WsURL(), "read", protocol.FileReadPayload{ID: sr.ID})
	if rp2.OK || !strings.Contains(rp2.Error, "另存为 .xlsx") {
		t.Fatalf(".xls 应报错并引导另存: %+v", rp2)
	}
}

// TestWriteXlsx 编辑单元格：改格生效（read 见新值、原值其余不动）、
// fetch 二进制已变；错误路径：sheet 不存在（附可用清单）/ 非法坐标 / 非 xlsx / 空 edits。
func TestWriteXlsx(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	orig := makeXlsx(t)
	id := saveXlsx(t, srv.WsURL(), "报表.xlsx", orig)

	// 编辑：A1 姓名→姓名改、B2 30→31。
	rp := call(t, srv.WsURL(), "write", protocol.FileWritePayload{
		ID:    id,
		Sheet: "Sheet1",
		Edits: []protocol.FileWriteEdit{{Cell: "A1", Value: "姓名改"}, {Cell: "B2", Value: "31"}},
	})
	if !rp.OK {
		t.Fatalf("write 失败: %s", rp.Error)
	}
	var wr protocol.FileWriteResult
	if err := protocol.DecodeRaw(rp.Data, &wr); err != nil {
		t.Fatal(err)
	}
	if wr.Updated != 2 || wr.ID != id || wr.Sheet != "Sheet1" {
		t.Fatalf("write 结果不符: %+v", wr)
	}

	// read 复验：新值在、未动格仍在。
	rr := readFile(t, srv.WsURL(), id)
	if !strings.Contains(rr.Text, "姓名改") || !strings.Contains(rr.Text, "31") {
		t.Fatalf("编辑未生效:\n%s", rr.Text)
	}
	if !strings.Contains(rr.Text, "张三") || !strings.Contains(rr.Text, "李四") {
		t.Fatalf("未编辑的格不应变化:\n%s", rr.Text)
	}

	// fetch：二进制已变化（权威副本就地更新）。
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

	// sheet 不存在：报错附可用清单。
	bad1 := call(t, srv.WsURL(), "write", protocol.FileWritePayload{
		ID: id, Sheet: "不存在", Edits: []protocol.FileWriteEdit{{Cell: "A1", Value: "x"}},
	})
	if bad1.OK || !strings.Contains(bad1.Error, "Sheet1") || !strings.Contains(bad1.Error, "数据") {
		t.Fatalf("sheet 不存在应报错并附可用清单: %+v", bad1)
	}

	// 非法坐标。
	bad2 := call(t, srv.WsURL(), "write", protocol.FileWritePayload{
		ID: id, Sheet: "Sheet1", Edits: []protocol.FileWriteEdit{{Cell: "!", Value: "x"}},
	})
	if bad2.OK || !strings.Contains(bad2.Error, "非法") {
		t.Fatalf("非法坐标应报错: %+v", bad2)
	}

	// 非 xlsx 文件：拒绝。
	txtID := saveText(t, srv.WsURL(), "a.txt", "文本")
	bad3 := call(t, srv.WsURL(), "write", protocol.FileWritePayload{
		ID: txtID, Sheet: "Sheet1", Edits: []protocol.FileWriteEdit{{Cell: "A1", Value: "x"}},
	})
	if bad3.OK || !strings.Contains(bad3.Error, "仅支持编辑 .xlsx") {
		t.Fatalf("非 xlsx 应拒绝: %+v", bad3)
	}

	// 空 edits。
	bad4 := call(t, srv.WsURL(), "write", protocol.FileWritePayload{ID: id, Sheet: "Sheet1"})
	if bad4.OK || !strings.Contains(bad4.Error, "edits 至少一条") {
		t.Fatalf("空 edits 应报错: %+v", bad4)
	}
}
