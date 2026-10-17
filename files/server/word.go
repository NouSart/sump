// Word（.docx）支持：read 提取正文段落文本，edit_word 做纯文本替换。
// 实现基于 Go 标准库（archive/zip + encoding/xml），无外部依赖；旧版 .doc 不支持。
package server

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

const (
	docxDocumentXML = "word/document.xml"
	wordBr          = "<w:br/>"
	wordTab         = "<w:tab/>"
)

// isWordFile 判定 .docx：mime 为准；扩展名兜底（docx 是 zip，嗅探只得 application/zip）。
func isWordFile(mime, filename string) bool {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mime == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		return true
	}
	if strings.EqualFold(filepath.Ext(filename), ".docx") {
		switch mime {
		case "", "application/octet-stream", "application/zip":
			return true
		}
	}
	return false
}

// isLegacyDoc 判定旧版 .doc（OLE2 格式）。
func isLegacyDoc(mime, filename string) bool {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mime == "application/msword" {
		return true
	}
	return strings.EqualFold(filepath.Ext(filename), ".doc") &&
		(mime == "" || mime == "application/octet-stream")
}

// docxToText 把 .docx 正文按段落提取为纯文本，超 maxChars 在段落边界截断。
// budget 是**解压后**的字节上限（压缩炸弹防线）：先按 zip 目录里的实体大小预检，再用
// LimitReader 兜住"头部撒谎"的包。
func docxToText(data []byte, maxChars int, budget int64) (string, bool, error) {
	r := bytes.NewReader(data)
	zr, err := zip.NewReader(r, int64(len(data)))
	if err != nil {
		return "", false, fmt.Errorf("解析 docx 失败: %w", err)
	}
	if err := checkExpandBudget(zr, budget); err != nil {
		return "", false, err
	}
	f, err := openZipFile(zr, docxDocumentXML)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	dec := xml.NewDecoder(io.LimitReader(f, budget+1))
	var paragraphs []string
	var para strings.Builder
	var run strings.Builder
	inT := false

	flushRun := func() {
		if run.Len() > 0 {
			para.WriteString(run.String())
			run.Reset()
		}
	}
	flushPara := func() {
		flushRun()
		if para.Len() > 0 {
			paragraphs = append(paragraphs, para.String())
			para.Reset()
		}
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false, fmt.Errorf("解析 document.xml 失败: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inT = true
			case "br":
				run.WriteByte('\n')
			case "tab":
				run.WriteByte('\t')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "p":
				flushPara()
			}
		case xml.CharData:
			if inT {
				run.WriteString(string(t))
			}
		}
	}
	flushPara()

	var b strings.Builder
	used := 0
	for i, p := range paragraphs {
		line := p
		if i > 0 {
			line = "\n\n" + p
		}
		n := utf8.RuneCountInString(line)
		if used+n > maxChars {
			if i == 0 {
				// 第一个段落就超限，按字符截断（避免空返回）
				text, _ := truncateRunes(p, maxChars)
				return text, true, nil
			}
			return strings.TrimRight(b.String(), "\n"), true, nil
		}
		b.WriteString(line)
		used += n
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

// editDocx 对 .docx 正文执行操作列表，返回新字节和替换条数。
// v0 仅支持 op="replace"；所有 Old 预检查通过后才写入，保证原子性。
// budget 是解压后 document.xml 的字节上限（压缩炸弹防线）。
func editDocx(data []byte, ops []protocol.WordEditOperation, budget int64) ([]byte, int, error) {
	for i, op := range ops {
		if op.Op != "replace" {
			return nil, 0, fmt.Errorf("第 %d 个操作不支持 op=%q（v0 仅支持 replace）", i+1, op.Op)
		}
		if strings.TrimSpace(op.Old) == "" {
			return nil, 0, fmt.Errorf("第 %d 个 replace 的 old 不能为空", i+1)
		}
	}

	r := bytes.NewReader(data)
	zr, err := zip.NewReader(r, int64(len(data)))
	if err != nil {
		return nil, 0, fmt.Errorf("解析 docx 失败: %w", err)
	}
	if err := checkExpandBudget(zr, budget); err != nil {
		return nil, 0, err
	}
	docFile, err := openZipFile(zr, docxDocumentXML)
	if err != nil {
		return nil, 0, err
	}
	contentBytes, err := io.ReadAll(io.LimitReader(docFile, budget+1))
	docFile.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("读取 document.xml 失败: %w", err)
	}
	if int64(len(contentBytes)) > budget {
		return nil, 0, fmt.Errorf("document.xml 解压后超过 %d 字节上限（压缩炸弹防线）", budget)
	}
	content := string(contentBytes)

	// 预检查：所有 old 必须存在。
	for i, op := range ops {
		old := escapeForDocx(op.Old)
		if !strings.Contains(content, old) {
			return nil, 0, fmt.Errorf("第 %d 个 replace 未找到文本 %q", i+1, op.Old)
		}
	}

	replaced := 0
	for i, op := range ops {
		old := escapeForDocx(op.Old)
		newText := escapeForDocx(op.New)
		if !strings.Contains(content, old) {
			return nil, 0, fmt.Errorf("第 %d 个 replace 在替换前文本已消失", i+1)
		}
		count := strings.Count(content, old)
		if count == 0 {
			return nil, 0, fmt.Errorf("第 %d 个 replace 未匹配到任何内容", i+1)
		}
		content = strings.Replace(content, old, newText, -1)
		replaced += count
	}

	out, err := rebuildZip(zr, docxDocumentXML, []byte(content))
	if err != nil {
		return nil, 0, fmt.Errorf("重建 docx 失败: %w", err)
	}
	return out, replaced, nil
}

// checkExpandBudget 按 zip 目录里的实体大小预检解压总量（压缩炸弹的第一道防线：
// 目录里声明的大小不可信，所以读的时候还要有第二道 LimitReader）。
func checkExpandBudget(zr *zip.Reader, budget int64) error {
	var total uint64
	for _, f := range zr.File {
		total += f.UncompressedSize64
		if total > uint64(budget) {
			return fmt.Errorf("解压后总量超过 %d 字节上限（压缩炸弹防线）", budget)
		}
	}
	return nil
}

// openZipFile 在 zip 中打开指定文件。
func openZipFile(zr *zip.Reader, name string) (io.ReadCloser, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f.Open()
		}
	}
	return nil, fmt.Errorf("docx 中缺少 %s", name)
}

// rebuildZip 复制 zip 中所有文件，并用 newContent 替换指定文件。
func rebuildZip(zr *zip.Reader, replaceName string, newContent []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	defer w.Close()
	for _, f := range zr.File {
		header := &zip.FileHeader{
			Name:     f.Name,
			Method:   f.Method,
			Modified: f.Modified,
		}
		writer, err := w.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		if f.Name == replaceName {
			if _, err := writer.Write(newContent); err != nil {
				return nil, err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(writer, rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// escapeForDocx 把普通文本转为可安全放入 Word XML 的字符串，并把 \n/\t 转为 Word 标签。
func escapeForDocx(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	out := buf.String()
	out = strings.ReplaceAll(out, "&#xD;&#xA;", wordBr)
	out = strings.ReplaceAll(out, "&#xA;", wordBr)
	out = strings.ReplaceAll(out, "&#xD;", wordBr)
	out = strings.ReplaceAll(out, "&#x9;", wordTab)
	return out
}
