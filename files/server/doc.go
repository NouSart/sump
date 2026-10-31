// doc.go：文档模型层的服务侧入口（outline / read_range / find / patch / verify）。
//
// 与 edit_word 的分工：edit_word 是"整篇字符串替换"，命中几处改几处；这一组先给结构
// （outline），再按节点地址定位（find），只改那一处（patch），改完自查（verify）。
// 服务层只认 docmodel 的类型与哨兵错误——格式细节留在 docmodel/ooxml/docx 里。
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/NouSart/sump/files/docmodel"
	_ "github.com/NouSart/sump/files/docmodel/ooxml/docx" // 注册 docx 实现
	"github.com/NouSart/sump/files/store"
	"github.com/NouSart/sump/protocol"
)

// docKindOf 按文件名 / MIME 判断文档类型（P0 只有 docx 注册了实现）。
func docKindOf(meta store.Meta) (docmodel.Kind, bool) {
	if isWordFile(meta.Mime, meta.Filename) {
		return docmodel.KindDocx, true
	}
	return "", false
}

// loadDoc 读库里的文件并打开成文档（只读动作走这里；不改库）。
func (s *Server) loadDoc(id, action string) (docmodel.Document, store.Meta, error) {
	if strings.TrimSpace(id) == "" {
		return nil, store.Meta{}, fmt.Errorf("%s: id 必填", action)
	}
	meta, err := s.cfg.Store.Stat(id)
	if err != nil {
		return nil, store.Meta{}, err
	}
	kind, ok := docKindOf(meta)
	if !ok {
		return nil, meta, fmt.Errorf("%s: 仅支持按节点编辑 .docx（%s 的类型是 %s）；旧版 .doc 请先另存为 .docx",
			action, meta.Filename, meta.Mime)
	}
	data, _, err := s.cfg.Store.Load(id)
	if err != nil {
		return nil, meta, err
	}
	doc, err := docmodel.Open(kind, data)
	if err != nil {
		return nil, meta, fmt.Errorf("%s: 打开 %s 失败: %w", action, meta.Filename, err)
	}
	return doc, meta, nil
}

// toDocNode 把节点树转成线上形态（逐层递归；P0 的树只有 body→tbl→tr→tc→p 这几层）。
func toDocNode(n docmodel.Node) protocol.DocNode {
	out := protocol.DocNode{
		ID:        n.ID.String(),
		Type:      string(n.Type),
		Text:      n.Text,
		Level:     n.Level,
		Truncated: n.Truncated,
	}
	if len(n.Children) > 0 {
		out.Children = make([]protocol.DocNode, 0, len(n.Children))
		for _, c := range n.Children {
			out.Children = append(out.Children, toDocNode(c))
		}
	}
	return out
}

// actionDocOutline 返回文档目录（结构树 + 规模 + 整篇指纹）。
func (s *Server) actionDocOutline(ctx context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.DocOutlinePayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	doc, meta, err := s.loadDoc(p.ID, "outline")
	if err != nil {
		return nil, err
	}
	outline, err := doc.Outline(ctx, p.Depth)
	if err != nil {
		return nil, fmt.Errorf("outline: 解析 %s 失败: %w", meta.Filename, err)
	}
	nodes := make([]protocol.DocNode, 0, len(outline.Nodes))
	for _, n := range outline.Nodes {
		nodes = append(nodes, toDocNode(n))
	}
	s.logger.Printf("已列出文档结构 %s（%s, %d 个顶层节点, %d 字）", p.ID, meta.Filename, len(nodes), outline.Stats.Chars)
	return protocol.DocOutlineResult{
		ID:        p.ID,
		DocKind:   outline.Kind.String(),
		Digest:    doc.Digest(),
		Nodes:     nodes,
		NodeCount: outline.Stats.Nodes,
		Chars:     outline.Stats.Chars,
		Tables:    outline.Stats.Tables,
		Parts:     outline.Stats.Parts,
		Warnings:  outline.Warnings,
	}, nil
}

// actionDocReadRange 按节点分页读（node 空 = 整篇）。
func (s *Server) actionDocReadRange(ctx context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.DocReadPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	doc, meta, err := s.loadDoc(p.ID, "read_range")
	if err != nil {
		return nil, err
	}
	chunk, err := doc.ReadRange(ctx, docmodel.Selector{
		Node:   docmodel.NodeID(p.Node),
		Offset: p.Offset,
		Limit:  p.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("read_range: 读取 %s 失败: %w", meta.Filename, err)
	}
	return protocol.DocReadResult{
		ID:      p.ID,
		DocKind: doc.Kind().String(),
		Digest:  doc.Digest(),
		Node:    chunk.Node.String(),
		Text:    chunk.Text,
		Total:   chunk.Total,
		Next:    chunk.Next,
	}, nil
}

// actionDocFind 返回命中位置（节点 + 字符区间 + 上下文），不改文档。
func (s *Server) actionDocFind(ctx context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.DocFindPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Text) == "" {
		return nil, errors.New("find: text 必填")
	}
	doc, meta, err := s.loadDoc(p.ID, "find")
	if err != nil {
		return nil, err
	}
	matches, err := doc.Find(ctx, docmodel.Query{
		Text:  p.Text,
		Node:  docmodel.NodeID(p.Node),
		Regex: p.Regex,
		Limit: p.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("find: 在 %s 中查找失败: %w", meta.Filename, err)
	}
	out := make([]protocol.DocMatch, 0, len(matches))
	for _, m := range matches {
		out = append(out, protocol.DocMatch{
			Node:    m.Node.String(),
			Run:     m.Run,
			Start:   m.Start,
			End:     m.End,
			Context: m.Context,
		})
	}
	return protocol.DocFindResult{ID: p.ID, Digest: doc.Digest(), Matches: out}, nil
}

// actionDocPatch 按节点地址改文档（LLM 工具，就地修改权威副本）。
// 校验链：id 必填 → 必须 .docx → operations 非空 → 整篇 anchor → 逐条转换 → 原子写入。
// 写锁串行化 read-modify-write，防并发改同一文件丢更新。
func (s *Server) actionDocPatch(ctx context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.DocPatchPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if len(p.Operations) == 0 {
		return nil, errors.New("patch: operations 至少一条")
	}
	ops := make([]docmodel.PatchOp, 0, len(p.Operations))
	for i, op := range p.Operations {
		converted, err := toDocPatchOp(op)
		if err != nil {
			return nil, fmt.Errorf("patch: 第 %d 条操作非法: %w", i+1, err)
		}
		ops = append(ops, converted)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	doc, meta, err := s.loadDoc(p.ID, "patch")
	if err != nil {
		return nil, err
	}
	// anchor 是整篇指纹：对不上说明读完之后文件已被改过——照着旧地图改会改错地方。
	if p.Anchor != "" && p.Anchor != doc.Digest() {
		return nil, fmt.Errorf("%w（当前 digest 是 %s）", docmodel.ErrStale, doc.Digest())
	}
	res, err := doc.Patch(ctx, ops)
	if err != nil {
		return nil, fmt.Errorf("patch: 改 %s 失败: %w", meta.Filename, err)
	}
	if err := s.cfg.Store.Overwrite(p.ID, doc.Bytes()); err != nil {
		return nil, fmt.Errorf("patch: 写回库失败: %w", err)
	}
	diff := make([]protocol.DocDiff, 0, len(res.Diff))
	for _, d := range res.Diff {
		diff = append(diff, protocol.DocDiff{
			Op:     string(d.Op),
			Node:   d.Node.String(),
			Before: d.Before,
			After:  d.After,
		})
	}
	s.logger.Printf("已按节点编辑 docx %s（%s, %d 条操作）", p.ID, meta.Filename, res.Applied)
	return protocol.DocPatchResult{
		ID:       p.ID,
		Applied:  res.Applied,
		Digest:   doc.Digest(),
		Diff:     diff,
		Warnings: res.Warnings,
	}, nil
}

// toDocPatchOp 把线上操作转成 docmodel 操作（只做形状校验，语义校验在格式层）。
func toDocPatchOp(op protocol.DocPatchOp) (docmodel.PatchOp, error) {
	if strings.TrimSpace(op.Op) == "" {
		return docmodel.PatchOp{}, errors.New("op 必填")
	}
	if strings.TrimSpace(op.Node) == "" {
		return docmodel.PatchOp{}, errors.New("node 必填")
	}
	out := docmodel.PatchOp{
		Op:     docmodel.Op(op.Op),
		Node:   docmodel.NodeID(op.Node),
		Text:   op.Text,
		Anchor: op.Anchor,
	}
	if op.Range != nil {
		out.Range = &docmodel.TextRange{Start: op.Range.Start, End: op.Range.End}
	}
	return out, nil
}

// docTextLimit 是 verify 取全文时的上限：整篇文本一次取完（不做分页）。
const docTextLimit = 1 << 24

// actionDocVerify 改完自查：整篇指纹 + 必须出现 / 必须不出现的片段。
// 结果永远是"已判定的结论"（ok=false 也只描述事实，不报错）——让模型自己决定下一步。
func (s *Server) actionDocVerify(ctx context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.DocVerifyPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if p.Digest == "" && len(p.Contains) == 0 && len(p.NotContains) == 0 {
		return nil, errors.New("verify: digest / contains / not_contains 至少要给一项（否则没有可核对的东西）")
	}
	doc, meta, err := s.loadDoc(p.ID, "verify")
	if err != nil {
		return nil, err
	}
	chunk, err := doc.ReadRange(ctx, docmodel.Selector{Limit: docTextLimit})
	if err != nil {
		return nil, fmt.Errorf("verify: 读取 %s 失败: %w", meta.Filename, err)
	}
	digest := doc.Digest()
	var failures []string
	if p.Digest != "" && p.Digest != digest {
		failures = append(failures, fmt.Sprintf("整篇指纹不匹配：期望 %s，实际 %s", p.Digest, digest))
	}
	for _, want := range p.Contains {
		if want == "" {
			failures = append(failures, "contains 里有一项是空串（无法核对）")
			continue
		}
		if !strings.Contains(chunk.Text, want) {
			failures = append(failures, fmt.Sprintf("缺少片段：%s", want))
		}
	}
	for _, bad := range p.NotContains {
		if bad == "" {
			failures = append(failures, "not_contains 里有一项是空串（无法核对）")
			continue
		}
		if strings.Contains(chunk.Text, bad) {
			failures = append(failures, fmt.Sprintf("仍有片段：%s", bad))
		}
	}
	s.logger.Printf("已校验 docx %s（%s, 通过=%v）", p.ID, meta.Filename, len(failures) == 0)
	return protocol.DocVerifyResult{
		ID:       p.ID,
		OK:       len(failures) == 0,
		Digest:   digest,
		Failures: failures,
	}, nil
}
