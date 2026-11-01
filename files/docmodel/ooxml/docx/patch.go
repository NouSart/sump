package docx

import (
	"context"
	"fmt"

	"github.com/NouSart/sump/files/docmodel"
	"github.com/NouSart/sump/files/docmodel/ooxml"
)

// Patch 按节点地址改文档；要么整批生效，要么原样返回（不留半成品）。
//
// 每一步只看"这一次操作用的那份投影"：同一段上的多条操作若区间重叠，
// 字节层会拒绝——宁可直接报错，也不猜用户想按什么顺序叠加。
func (d *Document) Patch(ctx context.Context, ops []docmodel.PatchOp) (docmodel.PatchResult, error) {
	if err := ctx.Err(); err != nil {
		return docmodel.PatchResult{}, err
	}
	if len(ops) == 0 {
		return docmodel.PatchResult{}, fmt.Errorf("%w: operations 至少一条", docmodel.ErrBadOp)
	}

	var edits []ooxml.Edit
	var diff []docmodel.DiffEntry
	for _, op := range ops {
		p, err := d.lookupPara(op.Node)
		if err != nil {
			return docmodel.PatchResult{}, err
		}
		if op.Anchor != "" && docmodel.Fingerprint(p.text) != op.Anchor {
			return docmodel.PatchResult{}, fmt.Errorf("%w（%s 的当前指纹是 %s）",
				docmodel.ErrStale, p.id, docmodel.Fingerprint(p.text))
		}
		rs := []rune(p.text)
		before, _ := preview(p.text, previewRunes)

		switch op.Op {
		case docmodel.OpReplaceText:
			start, end := 0, len(rs)
			if op.Range != nil {
				start, end = op.Range.Start, op.Range.End
			}
			if start < 0 || end < start || end > len(rs) {
				return docmodel.PatchResult{}, fmt.Errorf("%w: 文本区间 [%d,%d) 越界（%s 共 %d 字）",
					docmodel.ErrBadOp, start, end, p.id, len(rs))
			}
			es, err := p.replaceRange(d.xml, start, end, op.Text)
			if err != nil {
				return docmodel.PatchResult{}, err
			}
			edits = append(edits, es...)
			after, _ := preview(string(rs[:start])+op.Text+string(rs[end:]), previewRunes)
			diff = append(diff, docmodel.DiffEntry{Op: op.Op, Node: p.id, Before: before, After: after})
		case docmodel.OpDeleteNode:
			// 只清空段落文本，保留段落本身（删 <w:p> 会把样式与编号一起带走）。
			es, err := p.replaceRange(d.xml, 0, len(rs), "")
			if err != nil {
				return docmodel.PatchResult{}, err
			}
			edits = append(edits, es...)
			diff = append(diff, docmodel.DiffEntry{Op: op.Op, Node: p.id, Before: before})
		default:
			return docmodel.PatchResult{}, fmt.Errorf("%w: docx 暂不支持 %s（P0 支持 replace_text / delete_node）",
				docmodel.ErrBadOp, op.Op)
		}
	}

	out, err := ooxml.Apply(d.xml, edits)
	if err != nil {
		return docmodel.PatchResult{}, fmt.Errorf("%w: %v", docmodel.ErrBadOp, err)
	}
	if len(edits) == 0 {
		// 内容本来就一样：不重新打包（保证"零改动 = 零字节差异"）。
		return docmodel.PatchResult{Applied: len(ops), Diff: diff, Warnings: d.Warnings()}, nil
	}
	if err := d.pkg.SetPart(partDocument, out); err != nil {
		return docmodel.PatchResult{}, err
	}
	buf, err := d.pkg.Bytes()
	if err != nil {
		return docmodel.PatchResult{}, fmt.Errorf("重新打包 docx 失败: %w", err)
	}
	prev := d.out
	d.out = buf
	if err := d.reload(); err != nil {
		// 自己的输出解析不回来 = 结构性缺陷：退回改动前的状态再报错。
		if restored, rerr := Open(prev); rerr == nil {
			d.pkg, d.xml = restored.pkg, restored.xml
			d.paras, d.order, d.blocks = restored.paras, restored.order, restored.blocks
			d.warnings = restored.warnings
		}
		d.out = prev
		return docmodel.PatchResult{}, err
	}
	return docmodel.PatchResult{Applied: len(ops), Diff: diff, Warnings: d.Warnings()}, nil
}

// reload 用当前文件字节重建索引（编辑后文本变了，地址解析必须跟着更新）。
func (d *Document) reload() error {
	nd, err := Open(d.out)
	if err != nil {
		return fmt.Errorf("编辑后的 docx 无法重新解析: %w", err)
	}
	d.pkg, d.xml = nd.pkg, nd.xml
	d.paras, d.order, d.blocks = nd.paras, nd.order, nd.blocks
	d.warnings = nd.warnings
	return nil
}
