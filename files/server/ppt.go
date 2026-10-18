// PowerPoint（.pptx）支持：read 提取每页幻灯片文本，edit_ppt 做纯文本替换。
// 实现基于 Go 标准库（archive/zip + encoding/xml），无外部依赖；旧版 .ppt 不支持。
package server

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

const pptxSlidesDir = "ppt/slides/"

// cleanMime 去掉 mime 中可选的 "; charset=..." 参数，只保留媒体类型主体。
func cleanMime(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		return strings.TrimSpace(mime[:i])
	}
	return mime
}

// isPptFile 判定 .pptx：mime 为准；扩展名兜底（pptx 是 zip，嗅探只得 application/zip）。
func isPptFile(mime, filename string) bool {
	mime = cleanMime(mime)
	if mime == "application/vnd.openxmlformats-officedocument.presentationml.presentation" {
		return true
	}
	if strings.EqualFold(filepath.Ext(filename), ".pptx") {
		switch mime {
		case "", "application/octet-stream", "application/zip":
			return true
		}
	}
	return false
}

// isLegacyPpt 判定旧版 .ppt（OLE2 格式）。
func isLegacyPpt(mime, filename string) bool {
	mime = cleanMime(mime)
	if mime == "application/vnd.ms-powerpoint" {
		return true
	}
	return strings.EqualFold(filepath.Ext(filename), ".ppt") &&
		(mime == "" || mime == "application/octet-stream")
}

// pptxToText 把 .pptx 按幻灯片提取文本，每页以 "# 幻灯片 N" 分节，超 maxChars 在幻灯片边界截断。
// budget 是解压后的字节上限（压缩炸弹防线），与 .xlsx/.docx 同口径。
func pptxToText(data []byte, maxChars int, budget int64) (string, bool, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", false, fmt.Errorf("解析 pptx 失败: %w", err)
	}
	if err := checkExpandBudget(zr, budget); err != nil {
		return "", false, err
	}

	// 按文件名顺序读取 ppt/slides/slideN.xml，保证幻灯片顺序稳定。
	var slides []string
	for _, name := range slideXMLNames(zr) {
		text, err := extractSlideText(zr, name)
		if err != nil {
			return "", false, err
		}
		slides = append(slides, text)
	}

	var b strings.Builder
	used := 0
	// writeBlock 在不超过 maxChars 时写入 s 并更新已用字符数；超限返回 false。
	writeBlock := func(s string) bool {
		n := utf8.RuneCountInString(s)
		if used+n > maxChars {
			return false
		}
		b.WriteString(s)
		used += n
		return true
	}

	for i, text := range slides {
		if text == "" {
			continue
		}
		if !writeBlock(fmt.Sprintf("# 幻灯片 %d\n", i+1)) {
			return strings.TrimRight(b.String(), "\n"), true, nil
		}
		if !writeBlock(text + "\n") {
			if remain := maxChars - used; remain > 0 {
				trunc, _ := truncateRunes(text, remain)
				b.WriteString(trunc)
			}
			return strings.TrimRight(b.String(), "\n"), true, nil
		}
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

// slideXMLNames 返回 zip 中所有幻灯片 XML 文件名（严格匹配 ppt/slides/slide<数字>.xml），按页序排序。
func slideXMLNames(zr *zip.Reader) []string {
	type slide struct {
		name string
		num  int
	}
	var slides []slide
	for _, f := range zr.File {
		if num, ok := slideNumber(f.Name); ok {
			slides = append(slides, slide{name: f.Name, num: num})
		}
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].num < slides[j].num })
	names := make([]string, len(slides))
	for i, s := range slides {
		names[i] = s.name
	}
	return names
}

// slideNumber 从文件名解析幻灯片序号；仅接受 ppt/slides/slide<纯数字>.xml，其余返回 false。
func slideNumber(name string) (int, bool) {
	if !strings.HasPrefix(name, pptxSlidesDir) {
		return 0, false
	}
	base := name[len(pptxSlidesDir):]
	if !strings.HasPrefix(base, "slide") || !strings.HasSuffix(base, ".xml") {
		return 0, false
	}
	numStr := base[len("slide") : len(base)-len(".xml")]
	if numStr == "" {
		return 0, false
	}
	for _, c := range numStr {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, false
	}
	return n, true
}

// extractSlideText 从单个幻灯片 XML 中提取可见文本（<a:t> 节点内容）。
func extractSlideText(zr *zip.Reader, name string) (string, error) {
	f, err := openZipFile(zr, name)
	if err != nil {
		return "", err
	}
	defer f.Close()

	dec := xml.NewDecoder(f)
	var runs []string
	var run strings.Builder
	inT := false

	flushRun := func() {
		if run.Len() > 0 {
			runs = append(runs, run.String())
			run.Reset()
		}
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("解析 %s 失败: %w", name, err)
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
			if t.Name.Local == "t" {
				inT = false
				flushRun()
			}
		case xml.CharData:
			if inT {
				run.WriteString(string(t))
			}
		}
	}
	flushRun()

	var paras []string
	for _, r := range runs {
		s := strings.TrimSpace(r)
		if s != "" {
			paras = append(paras, s)
		}
	}
	return strings.Join(paras, "\n"), nil
}

// pptxDoc 是内存中的 .pptx 文档：保留原始 zip 与每页幻灯片的 XML 文本。
// 后续复杂能力（增删页、备注、表格、新 op 类型）都围绕此类型扩展，动作协议无需改动。
type pptxDoc struct {
	zr     *zip.Reader
	names  []string          // 幻灯片文件名，按页序
	slides map[string]string // 幻灯片 XML 文本
}

// readZipFile 读取 zip 中指定条目的全部内容。
func readZipFile(zr *zip.Reader, name string) ([]byte, error) {
	f, err := openZipFile(zr, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", name, err)
	}
	return b, nil
}

// openPptxDoc 解析 .pptx 并把所有幻灯片 XML 读入内存。
func openPptxDoc(data []byte, budget int64) (*pptxDoc, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("解析 pptx 失败: %w", err)
	}
	if err := checkExpandBudget(zr, budget); err != nil {
		return nil, err
	}
	names := slideXMLNames(zr)
	if len(names) == 0 {
		return nil, fmt.Errorf("pptx 中未找到幻灯片")
	}
	doc := &pptxDoc{zr: zr, names: names, slides: make(map[string]string, len(names))}
	for _, name := range names {
		b, err := readZipFile(zr, name)
		if err != nil {
			return nil, err
		}
		doc.slides[name] = string(b)
	}
	return doc, nil
}

// save 重建 zip 并把内存中的幻灯片变更写回，返回完整文件字节。
func (d *pptxDoc) save() ([]byte, error) {
	out, err := rebuildZipMulti(d.zr, d.slides)
	if err != nil {
		return nil, fmt.Errorf("重建 pptx 失败: %w", err)
	}
	return out, nil
}

// contains 报告已转义的 old 是否出现在任一幻灯片中。
func (d *pptxDoc) contains(old string) bool {
	for _, name := range d.names {
		if strings.Contains(d.slides[name], old) {
			return true
		}
	}
	return false
}

// replaceText 在所有幻灯片中把 old 替换为 new，返回替换条数。
func (d *pptxDoc) replaceText(old, new string) int {
	replaced := 0
	for _, name := range d.names {
		if count := strings.Count(d.slides[name], old); count > 0 {
			d.slides[name] = strings.ReplaceAll(d.slides[name], old, new)
			replaced += count
		}
	}
	return replaced
}

// pptOpHandler 是单个编辑操作的处理函数：就地修改 doc 并返回本次替换条数。
// 新增 op 类型时只需实现一个 handler 并注册进 pptOpHandlers，动作协议无需改动。
type pptOpHandler func(doc *pptxDoc, op protocol.PptEditOperation) (int, error)

var pptOpHandlers = map[string]pptOpHandler{
	"replace": pptOpReplace,
}

// pptOpReplace 实现 op="replace"：在所有幻灯片中把 old 替换为 new。
func pptOpReplace(doc *pptxDoc, op protocol.PptEditOperation) (int, error) {
	if strings.TrimSpace(op.Old) == "" {
		return 0, fmt.Errorf("old 不能为空")
	}
	old := escapeForPpt(op.Old)
	if !doc.contains(old) {
		return 0, fmt.Errorf("未找到文本 %q", op.Old)
	}
	return doc.replaceText(old, escapeForPpt(op.New)), nil
}

// editPptx 对 .pptx 的所有幻灯片执行操作列表，返回新字节和替换条数。
// 所有操作成功后才统一写回（save），任一失败即整体报错，保证原子性。
func editPptx(data []byte, ops []protocol.PptEditOperation, budget int64) ([]byte, int, error) {
	doc, err := openPptxDoc(data, budget)
	if err != nil {
		return nil, 0, err
	}
	replaced := 0
	for i, op := range ops {
		handler, ok := pptOpHandlers[op.Op]
		if !ok {
			return nil, 0, fmt.Errorf("第 %d 个操作不支持 op=%q（v0 仅支持 replace）", i+1, op.Op)
		}
		n, err := handler(doc, op)
		if err != nil {
			return nil, 0, fmt.Errorf("第 %d 个操作 %s 失败: %w", i+1, op.Op, err)
		}
		replaced += n
	}
	out, err := doc.save()
	if err != nil {
		return nil, 0, err
	}
	return out, replaced, nil
}

// pptEscaper 按 OOXML 文本节点约定转义 & < >；引号与撇号原样保留——
// PowerPoint 在 <a:t> 中不转义引号/撇号，若全量转义会因字节不一致而匹配不到。
var pptEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// escapeForPpt 把普通文本转为可安全放入 PPT XML 文本节点的字符串。
func escapeForPpt(s string) string {
	return pptEscaper.Replace(s)
}

// rebuildZipMulti 复制 zip 中所有条目，并用 contents（文件名→新文本）替换命中的条目。
func rebuildZipMulti(zr *zip.Reader, contents map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
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
		if newContent, ok := contents[f.Name]; ok {
			if _, err := writer.Write([]byte(newContent)); err != nil {
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
