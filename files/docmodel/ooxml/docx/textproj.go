package docx

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/NouSart/sump/files/docmodel"
	"github.com/NouSart/sump/files/docmodel/ooxml"
)

// pieceKind 是段落文本里一个"投影片段"的来源。
type pieceKind int

const (
	pieceText  pieceKind = iota // <w:t> 的字符数据
	pieceBreak                  // <w:br/>、<w:cr/> → "\n"
	pieceTab                    // <w:tab/> → "\t"
)

// piece 是段落文本中的一段，记录它在文本投影里的字符区间与在 XML 里的字节区间。
//
// 两个区间是分开的：文本偏移是"给模型看的"，字节偏移是"往文件里改的"；
// 二者由本结构一一对应，编辑时只需要改被命中的片段。
type piece struct {
	kind       pieceKind
	runIndex   int    // 所在 <w:r> 的序号（0 起；用于 p12/r3 这类 run 级地址）
	text       string // 本片段的投影文本（文本片段是解码后的字符数据）
	textStart  int    // 投影文本里的起始字符（rune）下标
	textEnd    int    // 结束字符下标（不含）
	rawStart   int    // 原始字节区间：text 为 <w:t> 的内容区间，break/tab 为整个元素
	rawEnd     int
	tagStart   int // 仅文本片段：<w:t ...> 起止（加 xml:space / 展开换行时用）
	tagEnd     int
	elemEnd    int  // 仅文本片段：</w:t> 结束位置（整元素区间 = [tagStart, elemEnd)）
	selfClosed bool // <w:t/>：没有内容可写，不能作为编辑目标
}

// para 是一个 <w:p> 及其文本投影。
type para struct {
	id         docmodel.NodeID
	start      int // <w:p ...> 起
	end        int // </w:p> 止（不含）
	text       string
	pieces     []piece
	style      string // pStyle/@w:val（可能为空）
	level      int    // 标题层级（0 = 正文）
	selfClosed bool   // <w:p/>：没有内容也没有结束标签
}

// buildPieces 把段落的直接内容投影成文本片段序列。
//
// 投影规则（与 read 的正文提取同口径）：
//   - <w:t> 的字符数据按出现顺序拼接（保留其中的空格）；
//   - <w:br/> / <w:cr/> → "\n"，<w:tab/> → "\t"（模型能原样看到换行与缩进）；
//   - <w:delText>（修订删除）、<w:instrText>（域代码）不计入正文；
//   - 文本框里的嵌套 <w:p> 整体跳过——那是另一个段落，会由自己的节点地址覆盖。
func (p *para) buildPieces(data []byte, toks []ooxml.Token, startIdx, endIdx int) error {
	var b strings.Builder
	runes := 0
	runIdx := -1

	add := func(kind pieceKind, text string, rawStart, rawEnd, tagStart, tagEnd, elemEnd int, selfClosed bool) {
		n := utf8.RuneCountInString(text)
		p.pieces = append(p.pieces, piece{
			kind: kind, runIndex: runIdx, text: text,
			textStart: runes, textEnd: runes + n,
			rawStart: rawStart, rawEnd: rawEnd,
			tagStart: tagStart, tagEnd: tagEnd, elemEnd: elemEnd,
			selfClosed: selfClosed,
		})
		b.WriteString(text)
		runes += n
	}

	for j := startIdx + 1; j < endIdx; j++ {
		t := toks[j]
		if t.Kind != ooxml.TokStart {
			continue
		}
		switch t.Name {
		case "p":
			// 文本框里的嵌套段落：不并入本段文本。
			if e := matchEnd(toks, j); e > j {
				j = e
			}
			continue
		case "r":
			// 已经跳过嵌套段落，剩下的 run 都属于本段（包括行内 sdt / ins 里的 run）。
			runIdx++
			continue
		case "br", "cr", "tab":
			e := matchEnd(toks, j)
			if e < 0 {
				return fmt.Errorf("w:%s 未闭合（偏移 %d）", t.Name, t.Start)
			}
			text, kind := "\n", pieceBreak
			if t.Name == "tab" {
				text, kind = "\t", pieceTab
			}
			add(kind, text, t.Start, toks[e].End, 0, 0, 0, false)
			j = e
		case "t":
			// 不要用固定深度判断：行内内容控件的路径是 p→sdt→sdtContent→r→t，
			// 比 p→r→t 深；嵌套文本框里的段落已在上面整体跳过，不会有重复风险。
			e := matchEnd(toks, j)
			if e < 0 {
				return fmt.Errorf("w:t 未闭合（偏移 %d）", t.Start)
			}
			innerStart, innerEnd := t.End, toks[e].Start
			if innerEnd < innerStart {
				return fmt.Errorf("w:t 区间异常（偏移 %d）", t.Start)
			}
			var sb strings.Builder
			for k := j + 1; k < e; k++ {
				if toks[k].Kind == ooxml.TokCharData {
					sb.WriteString(ooxml.UnescapeText(string(data[toks[k].Start:toks[k].End])))
				}
			}
			selfClosed := innerStart == innerEnd && bytes.HasSuffix(data[t.Start:t.End], []byte("/>"))
			add(pieceText, sb.String(), innerStart, innerEnd, t.Start, t.End, toks[e].End, selfClosed)
			j = e
		}
	}
	p.text = b.String()
	return nil
}

// styleInfo 取段落的样式名与标题层级。
//
// 这里用 encoding/xml **只读**解析这一个段落（不回写），所以不存在保真问题：
// 需要判断"是不是标题、第几级"来给模型提纲，仅靠 token 扫描拿不到属性值。
func styleInfo(paraXML []byte) (string, int) {
	var v struct {
		PPr *struct {
			PStyle *struct {
				Val string `xml:"val,attr"`
			} `xml:"pStyle"`
			OutlineLvl *struct {
				Val string `xml:"val,attr"`
			} `xml:"outlineLvl"`
		} `xml:"pPr"`
	}
	_ = xml.Unmarshal(paraXML, &v)
	if v.PPr == nil {
		return "", 0
	}
	style := ""
	if v.PPr.PStyle != nil {
		style = strings.TrimSpace(v.PPr.PStyle.Val)
	}
	level := headingLevel(style)
	if level == 0 && v.PPr.OutlineLvl != nil {
		// outlineLvl 0 起；0..8 是 1..9 级标题，9 = 正文。
		if n, err := strconv.Atoi(strings.TrimSpace(v.PPr.OutlineLvl.Val)); err == nil && n >= 0 && n < 9 {
			level = n + 1
		}
	}
	return style, level
}

// headingLevel 从样式名推断标题层级（Heading1 / heading 2 / 标题 3 / Title）。
func headingLevel(style string) int {
	s := strings.ToLower(strings.TrimSpace(style))
	if s == "" {
		return 0
	}
	if s == "title" || s == "标题" {
		return 1
	}
	for _, prefix := range []string{"heading", "标题"} {
		if strings.HasPrefix(s, prefix) {
			rest := strings.TrimSpace(strings.TrimPrefix(s, prefix))
			if n, err := strconv.Atoi(rest); err == nil && n >= 1 && n <= 9 {
				return n
			}
		}
	}
	return 0
}

// runIDAt 返回某个字符位置所属的 run 地址（形如 body/p12/r3）。
func (p *para) runIDAt(pos int) docmodel.NodeID {
	for i := range p.pieces {
		pc := &p.pieces[i]
		if pc.textEnd <= pc.textStart {
			continue
		}
		if pos >= pc.textStart && pos < pc.textEnd {
			return docmodel.NodeID(fmt.Sprintf("%s/r%d", p.id, pc.runIndex+1))
		}
	}
	return p.id
}

// preview 截取一段文本预览（按字符，不切坏 UTF-8）。
func preview(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s, false
	}
	return string([]rune(s)[:max]) + "…", true
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// replaceRange 生成"用 repl 替换段落文本 [start, end)（字符下标）"所需的字节级编辑。
//
// 跨 run 的本质：同一句话可能被 Word 拆成任意多个 <w:t>（改过字体、拼写检查、
// 修订都会切）。这里把新文本整体放进**第一个**受影响的片段，其余片段只删不加，
// 因此不会凭空造出新的 run / rPr，样式仍由原有第一个 run 承载。
//
// 区间为空（start == end）即插入；返回空编辑表示"文本本来就是这样"。
func (p *para) replaceRange(data []byte, start, end int, repl string) ([]ooxml.Edit, error) {
	runes := []rune(p.text)
	total := len(runes)
	if start < 0 || end < start || end > total {
		return nil, fmt.Errorf("%w: 文本区间 [%d,%d) 越界（本段共 %d 字）", docmodel.ErrBadOp, start, end, total)
	}
	if start == end && repl == "" {
		return nil, nil // 空区间插空串：什么都不用做
	}
	first, last := -1, -1
	for i := range p.pieces {
		pc := &p.pieces[i]
		if pc.textEnd <= pc.textStart || pc.selfClosed {
			continue
		}
		if pc.textEnd > start {
			if first < 0 {
				first = i
			}
			if start < end && pc.textStart < end {
				last = i
			}
		}
	}
	if first < 0 {
		// 插入点在段尾（或整段没有可写的文本元素）。
		if start != total {
			return nil, fmt.Errorf("%w: 插入点 %d 不在段尾（本段共 %d 字）", docmodel.ErrBadOp, start, total)
		}
		return p.appendText(data, repl)
	}
	if last < 0 {
		last = first
	}

	var edits []ooxml.Edit
	wroteRepl := false
	for i := first; i <= last; i++ {
		pc := &p.pieces[i]
		if pc.kind != pieceText {
			// 换行/制表是独立元素：被区间覆盖就整个删掉（不能只删字符）。
			edits = append(edits, ooxml.Edit{Start: pc.rawStart, End: pc.rawEnd})
			continue
		}
		pr := []rune(pc.text)
		from := clamp(start-pc.textStart, 0, len(pr))
		to := clamp(end-pc.textStart, from, len(pr))
		var content string
		if i == first {
			content = string(pr[:from]) + repl
			wroteRepl = true
		}
		if i == last {
			content += string(pr[to:])
		}
		if content == pc.text {
			continue // 字节没变就不产生编辑（"零差异"靠这句保证）
		}
		edits = append(edits, textEdit(data, pc, content)...)
	}
	if !wroteRepl {
		// 被替换的区间里只有换行/制表（没有可写的文本元素）：把新文本接到前一个
		// 文本片段末尾；前面没有文本片段时就在被覆盖元素之前零宽插入。
		edits = append(edits, p.insertAt(data, first, start, repl)...)
	}
	return edits, nil
}

// insertAt 在位置 start 插入 repl：优先并入紧邻的前一个 <w:t>（否则新文本会落在
// <w:r> 里成为裸字符数据，Word 不认）。
func (p *para) insertAt(data []byte, first, start int, repl string) []ooxml.Edit {
	for i := first - 1; i >= 0; i-- {
		pc := &p.pieces[i]
		if pc.kind != pieceText || pc.selfClosed {
			continue
		}
		if pc.textEnd != start {
			break
		}
		content := pc.text + repl
		return textEdit(data, pc, content)
	}
	pc := &p.pieces[first]
	name := elementName(data[pc.tagStart:pc.tagEnd])
	xml := textXML("<"+name+` xml:space="preserve">`, "</"+name+">", name, repl)
	return []ooxml.Edit{{
		Start: pc.rawStart, End: pc.rawStart,
		Replace: []byte(xml),
	}}
}

// appendText 在段尾追加文本：优先写进最后一个可写的 <w:t>；整段没有文本元素时
// 新建一个 run（空段落首次写入文本的唯一场景）。
func (p *para) appendText(data []byte, repl string) ([]ooxml.Edit, error) {
	for i := len(p.pieces) - 1; i >= 0; i-- {
		pc := &p.pieces[i]
		if pc.kind != pieceText || pc.selfClosed {
			continue
		}
		content := pc.text + repl
		edits := textEdit(data, pc, content)
		return edits, nil
	}
	// 一个文本元素都没有：在 </w:p> 之前补一个 run。
	if p.selfClosed {
		// <w:p/> 没有内容也没有结束标签：展开成 <w:p>…</w:p>。
		run := `<w:r><w:t xml:space="preserve">` + ooxml.EscapeText(repl) + `</w:t></w:r>`
		prefix := elementName(data[p.start:p.end])
		return []ooxml.Edit{{
			Start: p.end - 2, End: p.end,
			Replace: []byte(">" + run + "</" + prefix + ">"),
		}}, nil
	}
	closePos := p.end - 1
	if idx := bytes.LastIndexByte(data[p.start:p.end], '<'); idx >= 0 {
		closePos = p.start + idx
	}
	run := `<w:r><w:t xml:space="preserve">` + ooxml.EscapeText(repl) + `</w:t></w:r>`
	return []ooxml.Edit{{Start: closePos, End: closePos, Replace: []byte(run)}}, nil
}

// elementName 从开始标签里取带前缀的元素名（"<w:p ...>" → "w:p"）。
func elementName(tag []byte) string {
	i := 1
	for i < len(tag) {
		c := tag[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '>' || c == '/' {
			break
		}
		i++
	}
	if i <= 1 {
		return "w:p"
	}
	return string(tag[1:i])
}

// textXML 把一个 </w:t> 的内容渲染成一个或多个 <w:t>：换行 → <w:br/>，制表 → <w:tab/>。
// docx 里字符数据中的裸换行只会被当成空白，换行必须由元素表达——这一层负责翻译。
func textXML(startTag, endTag, name, content string) string {
	prefix := ""
	if i := strings.LastIndex(name, ":"); i >= 0 {
		prefix = name[:i+1]
	}
	var b strings.Builder
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		b.WriteString(startTag)
		b.WriteString(ooxml.EscapeText(cur.String()))
		b.WriteString(endTag)
		cur.Reset()
	}
	for _, r := range strings.ReplaceAll(content, "\r\n", "\n") {
		switch r {
		case '\n', '\r':
			flush()
			b.WriteString("<" + prefix + "br/>")
		case '\t':
			flush()
			b.WriteString("<" + prefix + "tab/>")
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return b.String()
}

// textEdit 返回"把片段内容改成 content"的编辑；换行/制表会展开成元素。
// content 与原文一致时返回空（零字节差异）。
func textEdit(data []byte, pc *piece, content string) []ooxml.Edit {
	if pc.selfClosed || content == pc.text {
		return nil
	}
	tag := data[pc.tagStart:pc.tagEnd]
	if strings.ContainsAny(content, "\r\n\t") {
		name := elementName(tag)
		startTag := string(tag)
		if !hasXMLSpace(tag) {
			startTag = string(tag[:len(tag)-1]) + ` xml:space="preserve">`
		}
		xml := textXML(startTag, "</"+name+">", name, content)
		return []ooxml.Edit{{Start: pc.tagStart, End: pc.elemEnd, Replace: []byte(xml)}}
	}
	edits := []ooxml.Edit{{
		Start: pc.rawStart, End: pc.rawEnd,
		Replace: []byte(ooxml.EscapeText(content)),
	}}
	if needsPreserve(content) && !hasXMLSpace(tag) {
		edits = append(edits, preserveAttrEdit(data, pc))
	}
	return edits
}

// preserveAttrEdit 在 <w:t ...> 上补 xml:space="preserve"（返回零宽插入）。
// 不补的话，Word 打开会把新写入的首尾空格吃掉——"看起来改了却少一个空格"。
func preserveAttrEdit(data []byte, pc *piece) ooxml.Edit {
	pos := pc.tagEnd - 1 // 指向 '>'
	if pc.tagEnd-pc.tagStart >= 2 && data[pc.tagEnd-2] == '/' {
		pos = pc.tagEnd - 2 // <w:t/> → <w:t .../>
	}
	return ooxml.Edit{Start: pos, End: pos, Replace: []byte(` xml:space="preserve"`)}
}

// needsPreserve 判断内容是否需要 xml:space="preserve"。
func needsPreserve(s string) bool {
	return s != "" && strings.TrimSpace(s) != s
}

// hasXMLSpace 判断开始标签里是否已经声明了 xml:space。
func hasXMLSpace(tag []byte) bool {
	return bytes.Contains(tag, []byte("xml:space"))
}
