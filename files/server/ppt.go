// PowerPoint（.pptx）支持：read 提取每页幻灯片文本，edit_ppt 做纯文本替换。
// 实现基于 Go 标准库（archive/zip + encoding/xml），无外部依赖；旧版 .ppt 不支持。
package server

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/NouSart/sump/protocol"
)

const (
	pptxSlidesDir = "ppt/slides/"
	// presentation.xml 的 <p:sldIdLst> 才是放映顺序；slideN.xml 的编号只是部件名。
	pptxPresentationPart = "ppt/presentation.xml"
	pptxPresentationRels = "ppt/_rels/presentation.xml.rels"
)

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

// pptxToTextWindow 把 .pptx 按幻灯片提取文本，每页以 "# 幻灯片 N" 分节。
// budget 是解压后的字节上限（压缩炸弹防线），与 .xlsx/.docx 同口径。
func pptxToTextWindow(data []byte, offset, limit int, budget int64) (string, int, int, bool, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", 0, 0, false, fmt.Errorf("解析 pptx 失败: %w", err)
	}
	if err := checkExpandBudget(zr, budget); err != nil {
		return "", 0, 0, false, err
	}

	// 按放映顺序读取幻灯片：顺序以 presentation.xml 的 <p:sldIdLst> 为准，
	// 不按 slideN.xml 的文件名编号（PowerPoint 里拖过页序的 deck 两者不同）。
	w := newReadWindow(offset, limit)
	for i, name := range slideXMLNames(zr) {
		text, err := extractSlideText(zr, name)
		if err != nil {
			return "", 0, 0, false, err
		}
		if text == "" {
			continue
		}
		w.line(fmt.Sprintf("# 幻灯片 %d", i+1))
		for _, para := range strings.Split(text, "\n") {
			w.line(para)
		}
	}
	return w.text(), w.total, w.next(), w.truncated(), nil
}

// pptxToText 是旧调用形状的兼容包装：从开头读 maxChars，返回是否截断。
func pptxToText(data []byte, maxChars int, budget int64) (string, bool, error) {
	text, _, _, truncated, err := pptxToTextWindow(data, 0, maxChars, budget)
	return text, truncated, err
}

// slideXMLNames 返回幻灯片 XML 文件名，顺序与 PowerPoint 中的放映顺序一致。
// 顺序取自 presentation.xml 的 <p:sldIdLst>，经 presentation.xml.rels 的 Id→Target 映射到具体部件。
// 缺 presentation.xml / rels 或解析失败时退回按文件名编号排序；sldIdLst 未引用但确实存在的幻灯片追加在末尾。
func slideXMLNames(zr *zip.Reader) []string {
	byFile := slideNamesByFilename(zr)
	ordered := slideNamesByPresentationOrder(zr)
	if len(ordered) == 0 {
		return byFile
	}
	seen := make(map[string]bool, len(ordered))
	for _, name := range ordered {
		seen[name] = true
	}
	for _, name := range byFile {
		if !seen[name] {
			ordered = append(ordered, name)
		}
	}
	return ordered
}

// slideNamesByPresentationOrder 按 <p:sldIdLst> 的顺序返回幻灯片部件名；无法确定顺序时返回 nil（调用方降级）。
func slideNamesByPresentationOrder(zr *zip.Reader) []string {
	ids := slideRelIDs(zr)
	if len(ids) == 0 {
		return nil
	}
	rels := presentationSlideRels(zr)
	if len(rels) == 0 {
		return nil
	}
	exists := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		exists[f.Name] = true
	}
	names := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		name, ok := rels[id]
		if !ok || !exists[name] || seen[name] {
			continue // 关系缺失、部件不在包里、重复登记：跳过而不是出错
		}
		names = append(names, name)
		seen[name] = true
	}
	return names
}

// slideRelIDs 按文档顺序返回 <p:sldIdLst> 中每个 <p:sldId> 的关系 ID（r:id）。
// 只认「带命名空间的 id」：<p:sldId> 上另有一个纯数字的 id 属性（放映编号），不能混用。
func slideRelIDs(zr *zip.Reader) []string {
	data, err := readZipFile(zr, pptxPresentationPart)
	if err != nil {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var ids []string
	inList := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// presentation.xml 损坏时不要交出「半截顺序」：整份降级到文件名顺序，
			// 免得出现前几页按 sldIdLst、后面按文件名的混合顺序。
			return nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "sldIdLst":
				inList = true
			case "sldId":
				if inList {
					if id := relAttrID(t); id != "" {
						ids = append(ids, id)
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "sldIdLst" {
				inList = false
			}
		}
	}
	return ids
}

// relAttrID 取元素上关系命名空间里的 id 属性（r:id）。
// 过渡模式（schemas.openxmlformats.org）与严格模式（purl.oclc.org）命名空间不同，
// 所以只判断「有命名空间的 id」，不写死 URL。
func relAttrID(se xml.StartElement) string {
	for _, a := range se.Attr {
		if a.Name.Local == "id" && a.Name.Space != "" {
			return a.Value
		}
	}
	return ""
}

// presentationSlideRels 解析 ppt/_rels/presentation.xml.rels，返回 关系ID → zip 内部件名（仅幻灯片关系）。
func presentationSlideRels(zr *zip.Reader) map[string]string {
	data, err := readZipFile(zr, pptxPresentationRels)
	if err != nil {
		return nil
	}
	var doc struct {
		Relationships []struct {
			ID     string `xml:"Id,attr"`
			Type   string `xml:"Type,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	out := make(map[string]string, len(doc.Relationships))
	for _, r := range doc.Relationships {
		if !strings.HasSuffix(r.Type, "/slide") {
			continue
		}
		if name := pptPartName(r.Target); name != "" {
			out[r.ID] = name
		}
	}
	return out
}

// pptPartName 把 rels 里的 Target 解析成 zip 内部件名。
// Target 相对 presentation.xml 所在目录（如 slides/slide2.xml），或以 "/" 开头表示包内绝对路径。
func pptPartName(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if strings.HasPrefix(target, "/") {
		return strings.TrimPrefix(target, "/")
	}
	return path.Join("ppt", target)
}

// slideNamesByFilename 返回所有幻灯片 XML 文件名（严格匹配 ppt/slides/slide<数字>.xml），按编号排序。
func slideNamesByFilename(zr *zip.Reader) []string {
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

// pptOpHandler 是单个编辑操作的处理函数：就地修改 doc 并返回本次替换条数。
// 新增 op 类型时只需实现一个 handler 并注册进 pptOpHandlers，动作协议无需改动。
type pptOpHandler func(doc *pptxDoc, op protocol.PptEditOperation) (int, error)

var pptOpHandlers = map[string]pptOpHandler{
	"replace": pptOpReplace,
}

// pptOpReplace 实现 op="replace"：在所有幻灯片的 <a:t> 文本节点内把 old 替换为 new。
// 不能对整段 XML 做字符串替换——那会打到 <a:t> 这类标签名上（#15）。
func pptOpReplace(doc *pptxDoc, op protocol.PptEditOperation) (int, error) {
	if strings.TrimSpace(op.Old) == "" {
		return 0, fmt.Errorf("old 不能为空")
	}
	if strings.ContainsAny(op.New, "\n\t") {
		return 0, fmt.Errorf("new 不能含换行或制表符：pptx 的换行是段落级 <a:br/>，v0 不改 run 结构，写不出合法文件")
	}
	total := 0
	for _, name := range doc.names {
		out, counts, err := replaceInTextNodes([]byte(doc.slides[name]), pptxTextSpec,
			[]textReplace{{Old: op.Old, New: op.New}})
		if err != nil {
			var nf errTextNotFound
			if errors.As(err, &nf) {
				continue // 这一页没有这段文本，继续看下一页
			}
			return 0, fmt.Errorf("处理 %s 失败: %w", name, err)
		}
		doc.slides[name] = string(out)
		total += counts[0]
	}
	if total == 0 {
		return 0, fmt.Errorf("未找到文本 %q", op.Old)
	}
	return total, nil
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
