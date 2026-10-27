package docx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/NouSart/sump/files/docmodel"
)

// 默认值：查找一次最多回 20 处（够定位，不至于把上下文塞满）；分页读默认 4000 字
// （与 files.read 的 read_max_chars 同量级，模型已经习惯这个手感）。
const (
	defaultMatches = 20
	maxMatches     = 200
	defaultChunk   = 4000
	contextRunes   = 16
)

func init() {
	docmodel.Register(docmodel.KindDocx, func(data []byte) (docmodel.Document, error) {
		return Open(data)
	})
}

// Warnings 返回解析时发现的"读得懂但需要提醒"的事。
func (d *Document) Warnings() []string { return append([]string(nil), d.warnings...) }

// Digest 是整篇可见文本的规范化指纹（规范化空白后取 sha256，十六进制全串）。
// 用于 verify：改完重算一遍，和预期值比对就知道"文档内容确实变成了这个样子"。
func (d *Document) Digest() string {
	var b strings.Builder
	for _, id := range d.order {
		b.WriteString(d.paras[id].text)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(b.String()), " ")))
	return hex.EncodeToString(sum[:])
}

// Text 返回整篇可见文本（段落以换行分隔，与 read 的正文提取同口径）。
func (d *Document) Text() string {
	parts := make([]string, 0, len(d.order))
	for _, id := range d.order {
		parts = append(parts, d.paras[id].text)
	}
	return strings.Join(parts, "\n")
}

// Outline 返回文档目录：depth<=1 只列 body 级块，depth>=2 展开表格到单元格段落。
func (d *Document) Outline(ctx context.Context, depth int) (docmodel.Outline, error) {
	if err := ctx.Err(); err != nil {
		return docmodel.Outline{}, err
	}
	if depth < 1 {
		depth = 1
	}
	out := docmodel.Outline{Kind: docmodel.KindDocx, Warnings: d.Warnings()}
	sum := docmodel.Stats{Tables: countTables(d.blocks), Parts: len(d.pkg.Names())}
	for _, blk := range d.blocks {
		switch blk.typ {
		case docmodel.NodeParagraph:
			n, c := d.paraNode(blk.para)
			out.Nodes = append(out.Nodes, n)
			sum.Nodes += c
		case docmodel.NodeTable:
			node, c := d.tableNode(blk.tbl, depth)
			out.Nodes = append(out.Nodes, node)
			sum.Nodes += c
		}
	}
	for _, p := range d.paras {
		sum.Chars += len([]rune(p.text))
	}
	out.Stats = sum
	return out, nil
}

// paraNode 把一个段落转成 outline 节点（文本预览截断到 previewRunes）。
func (d *Document) paraNode(p *para) (docmodel.Node, int) {
	text, truncated := preview(p.text, previewRunes)
	return docmodel.Node{
		ID:        p.id,
		Type:      docmodel.NodeParagraph,
		Text:      text,
		Level:     p.level,
		Truncated: truncated,
	}, 1
}

// tableNode 把一个表格转成 outline 节点；depth>=2 时展开行/单元格/段落。
// 返回值第二项是本次新增的节点数（表格自身 + 展开出来的行/单元格/段落）。
func (d *Document) tableNode(tbl *table, depth int) (docmodel.Node, int) {
	node := docmodel.Node{
		ID:   tbl.id,
		Type: docmodel.NodeTable,
		Text: fmt.Sprintf("%d 行 × %d 列", len(tbl.rows), tbl.cols),
	}
	count := 1
	if depth < 2 {
		return node, count
	}
	for _, row := range tbl.rows {
		rn := docmodel.Node{ID: row.id, Type: docmodel.NodeRow}
		count++
		for _, cell := range row.cells {
			cn := docmodel.Node{ID: cell.id, Type: docmodel.NodeCell}
			count++
			for _, pid := range cell.paras {
				pn, _ := d.paraNode(d.paras[pid])
				cn.Children = append(cn.Children, pn)
				count++
			}
			rn.Children = append(rn.Children, cn)
		}
		node.Children = append(node.Children, rn)
	}
	return node, count
}

func countTables(blocks []block) int {
	n := 0
	for _, b := range blocks {
		if b.typ == docmodel.NodeTable {
			n++
		}
	}
	return n
}

// Find 在段落文本投影里找命中：只回候选（节点 ID + 字符区间 + 上下文），不改文档。
// 同一段命中多处会各回一条——"改哪一处"必须由 Start/End 说清，不能靠运气。
func (d *Document) Find(ctx context.Context, q docmodel.Query) ([]docmodel.Match, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q.Text == "" {
		return nil, fmt.Errorf("%w: query.text 必填", docmodel.ErrBadOp)
	}
	paras, err := d.scopeParas(q.Node)
	if err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultMatches
	}
	if limit > maxMatches {
		limit = maxMatches
	}
	var re *regexp.Regexp
	if q.Regex {
		re, err = regexp.Compile(q.Text)
		if err != nil {
			return nil, fmt.Errorf("%w: 正则表达式非法: %v", docmodel.ErrBadOp, err)
		}
	}
	var out []docmodel.Match
	for _, p := range paras {
		rs := []rune(p.text)
		if re != nil {
			for _, loc := range re.FindAllStringIndex(p.text, -1) {
				start := len([]rune(p.text[:loc[0]]))
				end := start + len([]rune(p.text[loc[0]:loc[1]]))
				if end == start {
					continue // 零宽匹配没有可编辑区间
				}
				out = append(out, d.matchAt(p, rs, start, end))
				if len(out) >= limit {
					return out, nil
				}
			}
			continue
		}
		needle := []rune(q.Text)
		for i := 0; i+len(needle) <= len(rs); i++ {
			if string(rs[i:i+len(needle)]) != string(needle) {
				continue
			}
			out = append(out, d.matchAt(p, rs, i, i+len(needle)))
			if len(out) >= limit {
				return out, nil
			}
			i += len(needle) - 1
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: 没有找到 %q", docmodel.ErrNoMatch, q.Text)
	}
	return out, nil
}

// matchAt 组装一处命中（含所属 run 地址与上下文）。
func (d *Document) matchAt(p *para, rs []rune, start, end int) docmodel.Match {
	return docmodel.Match{
		Node:    p.id,
		Run:     p.runIDAt(start).String(),
		Start:   start,
		End:     end,
		Context: contextWindow(rs, start, end),
	}
}

// contextWindow 取命中前后各 contextRunes 个字符（越界就贴边）。
func contextWindow(rs []rune, start, end int) string {
	from := start - contextRunes
	if from < 0 {
		from = 0
	}
	to := end + contextRunes
	if to > len(rs) {
		to = len(rs)
	}
	var b strings.Builder
	if from > 0 {
		b.WriteString("…")
	}
	b.WriteString(string(rs[from:to]))
	if to < len(rs) {
		b.WriteString("…")
	}
	return b.String()
}

// ReadRange 按节点分页读：sel.Node 为空读整篇（段落以换行分隔）。
func (d *Document) ReadRange(ctx context.Context, sel docmodel.Selector) (docmodel.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return docmodel.Chunk{}, err
	}
	node := sel.Node
	var text string
	if node == "" {
		node = "body"
		text = d.Text()
	} else {
		p, err := d.lookupPara(node)
		if err != nil {
			return docmodel.Chunk{}, err
		}
		text = p.text
	}
	rs := []rune(text)
	total := len(rs)
	offset := clamp(sel.Offset, 0, total)
	limit := sel.Limit
	if limit <= 0 {
		limit = defaultChunk
	}
	end := total
	if offset+limit < total {
		end = offset + limit
	}
	chunk := docmodel.Chunk{Node: node, Text: string(rs[offset:end]), Total: total}
	if end < total {
		chunk.Next = end
	}
	return chunk, nil
}

// scopeParas 把限定节点解析成段落列表（空 = 全部段落，按文档顺序）。
func (d *Document) scopeParas(node docmodel.NodeID) ([]*para, error) {
	if node == "" {
		out := make([]*para, 0, len(d.order))
		for _, id := range d.order {
			out = append(out, d.paras[id])
		}
		return out, nil
	}
	if p, ok := d.paras[node]; ok {
		return []*para{p}, nil
	}
	// 允许用 run 地址限定范围：body/p12/r3 → 段落 body/p12。
	if p, err := d.lookupPara(node); err == nil {
		return []*para{p}, nil
	}
	// 表格 / 行 / 单元格：取其下所有段落。
	for _, blk := range d.blocks {
		if blk.typ != docmodel.NodeTable || blk.tbl == nil {
			continue
		}
		if ids := blk.tbl.parasUnder(node); len(ids) > 0 {
			out := make([]*para, 0, len(ids))
			for _, id := range ids {
				out = append(out, d.paras[id])
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", docmodel.ErrNodeNotFound, node)
}

// parasUnder 返回表格/行/单元格下的段落 ID（不是其地址则返回空）。
func (t *table) parasUnder(node docmodel.NodeID) []docmodel.NodeID {
	key := string(node)
	collect := func(cells []tableCell) []docmodel.NodeID {
		var out []docmodel.NodeID
		for _, cell := range cells {
			out = append(out, cell.paras...)
		}
		return out
	}
	if key == string(t.id) {
		var out []docmodel.NodeID
		for _, row := range t.rows {
			out = append(out, collect(row.cells)...)
		}
		return out
	}
	for _, row := range t.rows {
		if key == string(row.id) {
			return collect(row.cells)
		}
		for _, cell := range row.cells {
			if key == string(cell.id) {
				return append([]docmodel.NodeID(nil), cell.paras...)
			}
		}
	}
	return nil
}

// lookupPara 解析段落地址（允许带 /rN 后缀：run 级地址落在所在段落上）。
func (d *Document) lookupPara(node docmodel.NodeID) (*para, error) {
	if !node.Valid() {
		return nil, fmt.Errorf("%w: %q 不是合法节点地址", docmodel.ErrNodeNotFound, node)
	}
	if p, ok := d.paras[node]; ok {
		return p, nil
	}
	if i := strings.LastIndex(string(node), "/r"); i > 0 {
		if p, ok := d.paras[docmodel.NodeID(node[:i])]; ok {
			return p, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", docmodel.ErrNodeNotFound, node)
}
