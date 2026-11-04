// Word（.docx）支持：read 提取正文段落文本，edit_word 做纯文本替换。
// 实现基于 Go 标准库（archive/zip + encoding/xml），无外部依赖；旧版 .doc 不支持。
package server

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/NouSart/sump/files/docmodel"
	"github.com/NouSart/sump/protocol"
)

const docxDocumentXML = "word/document.xml"

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

// docxToTextWindow 把 .docx 正文投影成纯文本，并返回字符窗口。
//
// 投影直接复用 docmodel 的 docx 实现：read、read_range、find、patch、verify 看到的是
// 同一份段落序列（含空段落、文本框与内容控件里的段落）。以前 read 自己扫 XML，
// 会悄悄丢掉空段落，还会把文本框正文混进外层段落——两套投影让 rune 偏移对不上。
//
// budget 是**解压后**的字节上限（压缩炸弹防线）：先按 zip 目录里的实体大小预检，
// 再用 docmodel 的容器预算兜住“头部撒谎”的包。
func docxToTextWindow(data []byte, offset, limit int, budget int64) (string, int, int, bool, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", 0, 0, false, fmt.Errorf("解析 docx 失败: %w", err)
	}
	if err := checkExpandBudget(zr, budget); err != nil {
		return "", 0, 0, false, err
	}
	doc, err := docmodel.Open(docmodel.KindDocx, data)
	if err != nil {
		return "", 0, 0, false, err
	}
	text, ok := doc.(interface{ Text() string })
	if !ok {
		return "", 0, 0, false, errors.New("docx 实现没有文本投影")
	}
	w := newReadWindow(offset, limit)
	// 按段落喂窗口：默认截断落在段落边界，与 read 的既有手感一致；空段落也保留。
	for _, line := range strings.Split(text.Text(), "\n") {
		w.line(line)
	}
	return w.text(), w.total, w.next(), w.truncated(), nil
}

// docxToText 是旧调用形状的兼容包装：从开头读 maxChars，返回是否截断。
func docxToText(data []byte, maxChars int, budget int64) (string, bool, error) {
	text, _, _, truncated, err := docxToTextWindow(data, 0, maxChars, budget)
	return text, truncated, err
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

	// 只在 <w:t> 文本节点内替换：整段 XML 字符串替换会把标签名/属性一并改掉，
	// 结果是动作返回成功、正文却再也提不出来（#15）。
	ops2 := make([]textReplace, len(ops))
	for i, op := range ops {
		ops2[i] = textReplace{Old: op.Old, New: op.New}
	}
	newContent, counts, err := replaceInTextNodes(contentBytes, docxTextSpec, ops2)
	if err != nil {
		var nf errTextNotFound
		if errors.As(err, &nf) {
			return nil, 0, fmt.Errorf("%w", nf)
		}
		return nil, 0, fmt.Errorf("处理 document.xml 失败: %w", err)
	}
	replaced := 0
	for _, n := range counts {
		replaced += n
	}

	out, err := rebuildZip(zr, docxDocumentXML, newContent)
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

// escapeForDocx 已随「只在文本节点内替换」的实现移除：
// 匹配与转义现在都由 replaceInTextNodes 在解码后的字符数据上完成，
// 不再需要先把用户文本转成 XML 片段再做整段字符串匹配。
