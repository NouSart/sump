// Excel（.xlsx）支持：read 的文本化（逐 sheet 转 TSV）与 write 的单元格编辑。
// 实现基于 excelize（纯 Go 无 CGO）；旧版 .xls 不支持（引导另存为 .xlsx）。
package server

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"

	"github.com/QAQ-awa-QAQ/sump/protocol"
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
// 超 maxChars 在行边界截断（不切断整行），返回 truncated=true。
func xlsxToText(f *excelize.File, maxChars int) (string, bool) {
	var b strings.Builder
	used := 0
	for _, sheet := range f.GetSheetList() {
		header := "# 工作表: " + sheet + "\n"
		n := utf8.RuneCountInString(header)
		if used+n > maxChars {
			return strings.TrimRight(b.String(), "\n"), true
		}
		b.WriteString(header)
		used += n

		rows, err := f.GetRows(sheet)
		if err != nil {
			continue // 单表读取失败跳过，其余表照常
		}
		for _, row := range rows {
			cells := make([]string, len(row))
			for i, cell := range row {
				cells[i] = escapeTSVCell(cell)
			}
			line := strings.Join(cells, "\t") + "\n"
			n = utf8.RuneCountInString(line)
			if used+n > maxChars {
				return strings.TrimRight(b.String(), "\n"), true
			}
			b.WriteString(line)
			used += n
		}
	}
	return strings.TrimRight(b.String(), "\n"), false
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
