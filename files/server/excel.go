// Excel（.xlsx）支持：read 的文本化（逐 sheet 转 TSV）与 write 的单元格编辑。
// 实现基于 excelize（纯 Go 无 CGO）；旧版 .xls 不支持（引导另存为 .xlsx）。
package server

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/xuri/excelize/v2"

	"github.com/NouSart/sump/protocol"
)

// isExcelFile 判定 .xlsx：mime 为准；扩展名兜底（xlsx 是 zip，嗅探只得 application/zip）。
func isExcelFile(mime, filename string) bool {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mime == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		return true
	}
	if strings.EqualFold(filepath.Ext(filename), ".xlsx") {
		switch mime {
		case "", "application/octet-stream", "application/zip":
			return true
		}
	}
	return false
}

// isLegacyXls 判定旧版 .xls（OLE2 格式，excelize 不支持）。
func isLegacyXls(mime, filename string) bool {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mime == "application/vnd.ms-excel" {
		return true
	}
	return strings.EqualFold(filepath.Ext(filename), ".xls") &&
		(mime == "" || mime == "application/octet-stream")
}

// xlsxToText 把 xlsx 逐 sheet 转为 TSV 文本（“# 工作表: 名”开头，制表符分列）。
// 返回完整投影长度与续读起点；某个工作表读取失败时返回 warning，
// 由 fileReadResult 统一把“有 warning”归入 truncated——绝不能“少读一张表还说自己完整”。
func xlsxToText(f *excelize.File, offset, limit int) (string, int, int, []string, bool) {
	w := newReadWindow(offset, limit)
	var warnings []string
	for _, sheet := range f.GetSheetList() {
		w.line("# 工作表: " + sheet)
		rows, err := f.GetRows(sheet)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("工作表 %q 读取失败，结果不完整: %v", sheet, err))
			continue
		}
		for _, row := range rows {
			cells := make([]string, len(row))
			for i, cell := range row {
				cells[i] = escapeTSVCell(cell)
			}
			w.line(strings.Join(cells, "\t"))
		}
	}
	return w.text(), w.total, w.next(), warnings, w.truncated()
}

// validateWorksheetXML 逐张工作表做一次严格 XML 扫描。
//
// 为什么不能只信 GetRows 的 error：excelize 的行迭代器在底层 XML 截断 / 标签未闭合时
// 会把解析错误吞成“读到一半就结束”，GetRows 返回的行看似正常（实测坏掉的第二张表
// 直接消失且 err=nil）。这里先用严格 decoder 扫一遍，坏表至少以 warning 外显。
func validateWorksheetXML(data []byte) []string {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return []string{fmt.Sprintf("xlsx 容器无法重新读取，结果不完整: %v", err)}
	}
	var warnings []string
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "xl/worksheets/") || !strings.HasSuffix(f.Name, ".xml") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("工作表 XML %s 打开失败，结果不完整: %v", f.Name, err))
			continue
		}
		dec := xml.NewDecoder(io.LimitReader(rc, maxExpandBytes+1))
		for {
			_, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("工作表 XML %s 解析失败，结果不完整: %v", f.Name, err))
				break
			}
		}
		_ = rc.Close()
	}
	return warnings
}

// escapeTSVCell 把单元格里会破坏 TSV 结构的字符转义：制表符 → `\t`、换行 → `\n`、回车 → `\r`。
//
// 为什么必须转义：单元格允许换行（Excel 的 Alt+Enter）和制表符，直接拼进 TSV 会让
// **一列变两列、一行变两行**——模型按列读就会错位（实测：备注列「含 tab\t与空格」拆成了三列）。
// 宁可让模型看到可见的转义序列，也不能让表格结构悄悄串掉。
func escapeTSVCell(s string) string {
	if !strings.ContainsAny(s, "\t\r\n") {
		return s
	}
	return strings.NewReplacer("\t", `\t`, "\r", `\r`, "\n", `\n`).Replace(s)
}

// editXlsx 对 xlsx 应用单元格编辑，返回写回后的完整字节与实际写入格数。
// sheet 不存在时报错并附可用表清单；坐标非法逐格报错（不写入任何格）。
func editXlsx(data []byte, sheet string, edits []protocol.FileWriteEdit) ([]byte, int, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{
		UnzipSizeLimit:    maxExpandBytes,
		UnzipXMLSizeLimit: maxExpandBytes,
	})
	if err != nil {
		return nil, 0, expandError(fmt.Errorf("解析 xlsx 失败: %w", err))
	}
	defer f.Close()

	sheets := f.GetSheetList()
	found := false
	for _, s := range sheets {
		if s == sheet {
			found = true
			break
		}
	}
	if !found {
		return nil, 0, fmt.Errorf("工作表 %q 不存在（可用: %s）", sheet, strings.Join(sheets, ", "))
	}

	// 先全量校验坐标，再逐格写入——避免半途而废的半成品编辑。
	for i, e := range edits {
		if _, _, err := excelize.CellNameToCoordinates(e.Cell); err != nil {
			return nil, 0, fmt.Errorf("第 %d 个编辑的坐标 %q 非法: %w", i+1, e.Cell, err)
		}
	}
	for i, e := range edits {
		if err := f.SetCellValue(sheet, e.Cell, e.Value); err != nil {
			return nil, 0, fmt.Errorf("写入第 %d 个编辑（%s!%s）失败: %w", i+1, sheet, e.Cell, err)
		}
	}

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, 0, fmt.Errorf("写回 xlsx 失败: %w", err)
	}
	return buf.Bytes(), len(edits), nil
}
