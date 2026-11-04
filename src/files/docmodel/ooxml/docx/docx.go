// Package docx 把 word/document.xml 映射成 docmodel 的节点树，并提供字节级保真的编辑。
//
// 两条定位原则：
//   - 节点 ID 只依赖结构顺序（body/p3、body/tbl1/r2/c1/p1），不依赖文本——文本会变，
//     地址不能跟着变，否则"改第 3 段的第 5 个字"在下一次保存后就指到别处。
//   - 编辑只重写被命中的 <w:t> 内容，其余字节一个不动（容器层保证非目标 part 逐字节
//     不变），所以样式、修订标记、批注、域代码、命名空间前缀都不会被打散。
package docx

import (
	"errors"
	"fmt"
	"strings"

	"github.com/NouSart/sump/files/docmodel"
	"github.com/NouSart/sump/files/docmodel/ooxml"
)

// partDocument 是正文 part 名（页眉/页脚/脚注是别的 part，P0 只编辑正文）。
const partDocument = "word/document.xml"

// previewRunes 是 outline 返回的文本预览长度：够模型判断"要不要读这一段"即可。
const previewRunes = 80

// Document 是一个已打开的 .docx。
// 约定：不是并发安全的（同一文件的写由服务层的版本号串行化）。
type Document struct {
	pkg *ooxml.Package
	xml []byte // document.xml 的当前字节

	paras  map[docmodel.NodeID]*para
	order  []docmodel.NodeID // 段落按文档顺序（含表格单元格里的段落）
	blocks []block           // body 级块（段落 / 表格），用于 outline

	out []byte // 当前完整文件字节（未改动时就是原始输入）

	warnings []string
}

// block 是 body 级的一个块。
type block struct {
	id   docmodel.NodeID
	typ  docmodel.NodeType
	para *para
	tbl  *table
}

// table 是一个 <w:tbl>：行 → 单元格 → 段落 ID。
type table struct {
	id   docmodel.NodeID
	rows []tableRow
	cols int
}

type tableRow struct {
	id    docmodel.NodeID
	cells []tableCell
}

type tableCell struct {
	id    docmodel.NodeID
	paras []docmodel.NodeID
}

// Open 解析一个 .docx。
func Open(data []byte) (*Document, error) {
	pkg, err := ooxml.Open(data, ooxml.DefaultBudget)
	if err != nil {
		return nil, fmt.Errorf("打开 docx 失败: %w", err)
	}
	xmlData, err := pkg.Part(partDocument)
	if err != nil {
		return nil, err
	}
	d := &Document{pkg: pkg, xml: xmlData, out: data}
	if err := d.parse(); err != nil {
		return nil, err
	}
	return d, nil
}

// Kind 返回文档类型。
func (d *Document) Kind() docmodel.Kind { return docmodel.KindDocx }

// Bytes 返回当前完整文件字节（未改动时 = 原始输入）。
func (d *Document) Bytes() []byte { return d.out }

// parse 从 document.xml 重建段落/表格索引。
func (d *Document) parse() error {
	toks, err := ooxml.Scan(d.xml)
	if err != nil {
		return fmt.Errorf("%s 解析失败: %w", partDocument, err)
	}
	body := -1
	for i, t := range toks {
		if t.Kind == ooxml.TokStart && t.Name == "body" {
			body = i
			break
		}
	}
	if body < 0 {
		return errors.New("docx 缺少 w:body（不是 WordprocessingML 正文？）")
	}
	bodyEnd := matchEnd(toks, body)
	if bodyEnd < 0 {
		return errors.New("docx 的 w:body 未闭合")
	}

	d.paras = make(map[docmodel.NodeID]*para)
	d.order = nil
	d.blocks = nil
	d.warnings = nil

	if err := d.parseBlockContainer(toks, body+1, bodyEnd, toks[body].Depth+1, "body"); err != nil {
		return err
	}
	if len(d.order) == 0 {
		d.warnings = append(d.warnings, "正文里没有段落（文档可能是空的或结构异常）")
	}
	return nil
}

// parseBlockContainer 解析一个块级容器（body / 单元格 / sdtContent …）。
//
// 直接子段落继续使用容器原有编号（body/pN、cell/pN），所以历史地址不变；
// 容器里的 sdt / customXml / 修订容器递归展开，路径追加一段（body/sdt1/p1）。
// 这样文本框、内容控件里的段落不再是“read 看得见、节点模型看不见”。
func (d *Document) parseBlockContainer(toks []ooxml.Token, start, end, depth int, path string) error {
	pn, tn, sn := 0, 0, 0
	for i := start; i < end; i++ {
		t := toks[i]
		if t.Kind != ooxml.TokStart || t.Depth != depth {
			continue
		}
		switch t.Name {
		case "p":
			pn++
			p, pEnd, err := d.newPara(toks, i, docmodel.NodeID(fmt.Sprintf("%s/p%d", path, pn)))
			if err != nil {
				return err
			}
			d.registerPara(p)
			d.blocks = append(d.blocks, block{id: p.id, typ: docmodel.NodeParagraph, para: p})
			if err := d.parseNestedIn(toks, i, pEnd, string(p.id), nil, true); err != nil {
				return err
			}
			i = pEnd
		case "tbl":
			tn++
			tbl, err := d.newTable(toks, i, fmt.Sprintf("%s/tbl%d", path, tn))
			if err != nil {
				return err
			}
			d.blocks = append(d.blocks, block{id: tbl.id, typ: docmodel.NodeTable, tbl: tbl})
			i = matchEnd(toks, i)
		case "sdt", "customXml", "ins", "moveTo", "AlternateContent":
			sn++
			next, nextEnd, ok := wrapperContent(toks, i)
			if !ok {
				i = matchEnd(toks, i)
				continue
			}
			childPath := fmt.Sprintf("%s/%s%d", path, wrapperSegment(t.Name), sn)
			if err := d.parseBlockContainer(toks, next+1, nextEnd, toks[next].Depth+1, childPath); err != nil {
				return err
			}
			i = nextEnd
		case "del", "moveFrom", "Fallback":
			// 修订删除与 AlternateContent 的兼容分支不是当前可见正文；跳过以免重复计数。
			if e := matchEnd(toks, i); e > i {
				i = e
			}
		}
	}
	return nil
}

// parseCellBlocks 解析表格单元格里的块；嵌套段落追加到 cell.paras，不进入 body 大纲。
func (d *Document) parseCellBlocks(toks []ooxml.Token, start, end, depth int, path string, cellParas *[]docmodel.NodeID) error {
	pn, tn, sn := 0, 0, 0
	for i := start; i < end; i++ {
		t := toks[i]
		if t.Kind != ooxml.TokStart || t.Depth != depth {
			continue
		}
		switch t.Name {
		case "p":
			pn++
			p, pEnd, err := d.newPara(toks, i, docmodel.NodeID(fmt.Sprintf("%s/p%d", path, pn)))
			if err != nil {
				return err
			}
			d.registerPara(p)
			*cellParas = append(*cellParas, p.id)
			if err := d.parseNestedIn(toks, i, pEnd, string(p.id), cellParas, false); err != nil {
				return err
			}
			i = pEnd
		case "tbl":
			tn++
			tbl, err := d.newTable(toks, i, fmt.Sprintf("%s/tbl%d", path, tn))
			if err != nil {
				return err
			}
			*cellParas = append(*cellParas, tbl.allParas()...)
			i = matchEnd(toks, i)
		case "sdt", "customXml", "ins", "moveTo", "AlternateContent":
			sn++
			next, nextEnd, ok := wrapperContent(toks, i)
			if !ok {
				i = matchEnd(toks, i)
				continue
			}
			childPath := fmt.Sprintf("%s/%s%d", path, wrapperSegment(t.Name), sn)
			if err := d.parseCellBlocks(toks, next+1, nextEnd, toks[next].Depth+1, childPath, cellParas); err != nil {
				return err
			}
			i = nextEnd
		case "del", "moveFrom", "Fallback":
			if e := matchEnd(toks, i); e > i {
				i = e
			}
		}
	}
	return nil
}

// parseNestedIn 找段落内部的嵌套段落/表格（文本框、内容控件、图形组等）。
//
// 只按“在父段落里出现的顺序”编号：body/p1/p1、body/p1/p2。不把 txbx/drawing
// 这类前缀写进地址——它们会随 Word 版本变化，而顺序是稳定的结构事实。
func (d *Document) parseNestedIn(toks []ooxml.Token, start, end int, path string, cellParas *[]docmodel.NodeID, addBlocks bool) error {
	pn, tn := 0, 0
	for i := start + 1; i < end; i++ {
		t := toks[i]
		if t.Kind != ooxml.TokStart {
			continue
		}
		switch t.Name {
		case "Fallback", "del", "moveFrom":
			if e := matchEnd(toks, i); e > i {
				i = e
			}
			continue
		case "p":
			pn++
			p, pEnd, err := d.newPara(toks, i, docmodel.NodeID(fmt.Sprintf("%s/p%d", path, pn)))
			if err != nil {
				return err
			}
			d.registerPara(p)
			if cellParas != nil {
				*cellParas = append(*cellParas, p.id)
			}
			if addBlocks {
				d.blocks = append(d.blocks, block{id: p.id, typ: docmodel.NodeParagraph, para: p})
			}
			if err := d.parseNestedIn(toks, i, pEnd, string(p.id), cellParas, addBlocks); err != nil {
				return err
			}
			i = pEnd
		case "tbl":
			tn++
			tbl, err := d.newTable(toks, i, fmt.Sprintf("%s/tbl%d", path, tn))
			if err != nil {
				return err
			}
			if cellParas != nil {
				*cellParas = append(*cellParas, tbl.allParas()...)
			}
			if addBlocks {
				d.blocks = append(d.blocks, block{id: tbl.id, typ: docmodel.NodeTable, tbl: tbl})
			}
			i = matchEnd(toks, i)
		}
	}
	return nil
}

// registerPara 把段落放进全局索引与文档顺序。
func (d *Document) registerPara(p *para) {
	d.paras[p.id] = p
	d.order = append(d.order, p.id)
}

// wrapperContent 返回块级包装元素里承载内容的直接子元素（sdtContent / Choice）。
// customXml / ins 本身没有中间层，直接以自身内容为容器。
func wrapperContent(toks []ooxml.Token, i int) (start, end int, ok bool) {
	end = matchEnd(toks, i)
	if end < 0 {
		return 0, 0, false
	}
	want := "sdtContent"
	if toks[i].Name == "AlternateContent" {
		want = "Choice"
	}
	if toks[i].Name == "customXml" || toks[i].Name == "ins" || toks[i].Name == "moveTo" {
		return i, end, true
	}
	depth := toks[i].Depth + 1
	for j := i + 1; j < end; j++ {
		if toks[j].Kind == ooxml.TokStart && toks[j].Depth == depth && toks[j].Name == want {
			jEnd := matchEnd(toks, j)
			if jEnd < 0 {
				return 0, 0, false
			}
			return j, jEnd, true
		}
	}
	return 0, 0, false
}

// wrapperSegment 把包装元素名转成地址段（统一小写，避免大小写差异影响地址）。
func wrapperSegment(name string) string {
	switch name {
	case "AlternateContent":
		return "alt"
	}
	return strings.ToLower(name)
}

// matchEnd 返回与 toks[i] 配对的结束 token 下标；自闭合元素返回紧随其后的合成 End。
func matchEnd(toks []ooxml.Token, i int) int {
	if i < 0 || i >= len(toks) || toks[i].Kind != ooxml.TokStart {
		return -1
	}
	name, depth := toks[i].Name, toks[i].Depth
	for j := i + 1; j < len(toks); j++ {
		if toks[j].Kind == ooxml.TokEnd && toks[j].Name == name && toks[j].Depth == depth {
			return j
		}
	}
	return -1
}

// newPara 构造一个段落（含文本投影）。
func (d *Document) newPara(toks []ooxml.Token, i int, id docmodel.NodeID) (*para, int, error) {
	endIdx := matchEnd(toks, i)
	if endIdx < 0 {
		return nil, 0, fmt.Errorf("w:p 未闭合（偏移 %d）", toks[i].Start)
	}
	p := &para{id: id, start: toks[i].Start, end: toks[endIdx].End}
	// 自闭合 <w:p/> 的合成 End 是零宽 token：文本插入时要把它展开成 <w:p>…</w:p>。
	p.selfClosed = toks[endIdx].Start == toks[endIdx].End
	if err := p.buildPieces(d.xml, toks, i, endIdx); err != nil {
		return nil, 0, err
	}
	p.style, p.level = styleInfo(d.xml[p.start:p.end])
	return p, endIdx, nil
}

// newTable 构造表格索引（行/列从 0 开始，单元格里的段落各自有 ID）。
func (d *Document) newTable(toks []ooxml.Token, i int, path string) (*table, error) {
	endIdx := matchEnd(toks, i)
	if endIdx < 0 {
		return nil, fmt.Errorf("w:tbl 未闭合（偏移 %d）", toks[i].Start)
	}
	tbl := &table{id: docmodel.NodeID(path)}
	depth := toks[i].Depth + 1
	for j := i + 1; j < endIdx; j++ {
		t := toks[j]
		if t.Kind != ooxml.TokStart || t.Depth != depth || t.Name != "tr" {
			continue
		}
		r := tableRow{id: docmodel.NodeID(fmt.Sprintf("%s/r%d", path, len(tbl.rows)+1))}
		trEnd := matchEnd(toks, j)
		if trEnd < 0 {
			return nil, fmt.Errorf("w:tr 未闭合（偏移 %d）", t.Start)
		}
		cellDepth := t.Depth + 1
		for k := j + 1; k < trEnd; k++ {
			tc := toks[k]
			if tc.Kind != ooxml.TokStart || tc.Depth != cellDepth || tc.Name != "tc" {
				continue
			}
			cell := tableCell{id: docmodel.NodeID(fmt.Sprintf("%s/c%d", r.id, len(r.cells)+1))}
			tcEnd := matchEnd(toks, k)
			if tcEnd < 0 {
				return nil, fmt.Errorf("w:tc 未闭合（偏移 %d）", tc.Start)
			}
			if err := d.parseCellBlocks(toks, k+1, tcEnd, tc.Depth+1, string(cell.id), &cell.paras); err != nil {
				return nil, err
			}
			if len(r.cells)+1 > tbl.cols {
				tbl.cols = len(r.cells) + 1
			}
			r.cells = append(r.cells, cell)
			k = tcEnd
		}
		tbl.rows = append(tbl.rows, r)
		j = trEnd
	}
	return tbl, nil
}
