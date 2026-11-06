// native.go：files 服务的原生 SPv2.1 method 入口。
//
// 每个 provides 动作都在这里注册一个原生 handler：直接读 call.Frame.Body，调用
// 共享业务函数（save/fetch/...），再 call.Reply / call.ReplyError 回包。
// 这里不合成 v1 信封；/spv2 是唯一入口。
package server

import (
	"context"
	"errors"

	"github.com/NouSart/sump/files/docmodel"
	"github.com/NouSart/sump/files/store"
	"github.com/NouSart/sump/spv2"
)

// registerNativeMethods 为 provides 里的每个动作注册原生 method。
// Tool 与 provides 的 tool 声明一致；这些动作都同步回包，所以 LongRunning 均为 false。
func (s *Server) registerNativeMethods() {
	s.HandleNative(spv2.Method{
		Name: "save", Tool: true, LongRunning: false,
		Description: "保存文件（{url} 或 {data(base64), filename, mime}）→ {id, filename, mime, size}",
		Input: objectSchema(map[string]any{
			"url":      stringSchema("文件下载地址（与 data 二选一）"),
			"data":     stringSchema("文件内容的 base64，不含 data: 前缀（与 url 二选一）"),
			"filename": stringSchema("文件名；只有 URL 时可由 URL 末段推断"),
			"mime":     stringSchema("MIME 类型，缺省时按内容探测"),
		}),
		Output: objectSchema(map[string]any{
			"id":       stringSchema("文件 id（凭它 fetch / read）"),
			"filename": stringSchema("保存时使用的文件名"),
			"mime":     stringSchema("最终 MIME 类型"),
			"size":     integerSchema("字节数"),
		}, "id", "filename", "mime", "size"),
	}, s.nativeSave)
	s.HandleNative(spv2.Method{
		Name: "fetch", Tool: false, LongRunning: false,
		Description: "取回文件本体（base64）→ {id, filename, mime, data(base64), size}",
		Input: objectSchema(map[string]any{
			"id": stringSchema("文件 id"),
		}, "id"),
		Output: objectSchema(map[string]any{
			"id":       stringSchema("文件 id"),
			"filename": stringSchema("文件名"),
			"mime":     stringSchema("MIME 类型"),
			"data":     stringSchema("文件内容的 base64，不含 data: 前缀"),
			"size":     integerSchema("字节数"),
		}, "id", "filename", "mime", "data", "size"),
	}, s.nativeFetch)
	s.HandleNative(spv2.Method{
		Name: "read", Tool: true, LongRunning: false,
		Description: "读取文件的可文本化内容（文本类 / .xlsx / .docx / .pptx / .pdf）→ {id, filename, text, truncated, total, next, warnings}",
		Input: objectSchema(map[string]any{
			"id":     stringSchema("文件 id"),
			"offset": integerSchema("起始字符（rune），默认 0"),
			"limit":  integerSchema("本次最多返回多少字符；0 = read_max_chars"),
		}, "id"),
		Output: objectSchema(map[string]any{
			"id":        stringSchema("文件 id"),
			"filename":  stringSchema("文件名"),
			"text":      stringSchema("文本内容（可能被截断）"),
			"truncated": booleanSchema("true = 内容未给全"),
			"total":     integerSchema("完整可见文本的字符数"),
			"next":      integerSchema("下一块起点；0 = 已读完"),
			"warnings":  arraySchema(stringSchema("警告"), "读到了但结果不完整/需注意的事"),
		}, "id", "filename", "text", "truncated"),
	}, s.nativeRead)
	s.HandleNative(spv2.Method{
		Name: "write", Tool: true, LongRunning: false,
		Description: "就地修改 .xlsx 单元格 → {id, sheet, updated}（先 read 看清坐标再改）",
		Input: objectSchema(map[string]any{
			"id":    stringSchema("文件 id（必须是 .xlsx）"),
			"sheet": stringSchema("工作表名"),
			"edits": arraySchema(objectSchema(map[string]any{
				"cell":  stringSchema("A1 风格坐标"),
				"value": stringSchema("写入的字符串值"),
			}, "cell", "value"), "单元格写入列表"),
		}, "id", "sheet", "edits"),
		Output: objectSchema(map[string]any{
			"id":      stringSchema("文件 id"),
			"sheet":   stringSchema("工作表名"),
			"updated": integerSchema("实际写入的单元格数"),
		}, "id", "sheet", "updated"),
	}, s.nativeWrite)
	s.HandleNative(spv2.Method{
		Name: "edit_word", Tool: true, LongRunning: false,
		Description: "就地修改 .docx 正文 → {id, replaced}（v0 仅 op=replace）",
		Input: objectSchema(map[string]any{
			"id": stringSchema("文件 id（必须是 .docx）"),
			"operations": arraySchema(objectSchema(map[string]any{
				"op":  stringSchema("操作类型，v0 仅 replace"),
				"old": stringSchema("被替换的原文"),
				"new": stringSchema("替换后的新文本"),
			}, "op", "old"), "替换操作列表"),
		}, "id", "operations"),
		Output: objectSchema(map[string]any{
			"id":       stringSchema("文件 id"),
			"replaced": integerSchema("实际替换的条数"),
		}, "id", "replaced"),
	}, s.nativeEditWord)
	s.HandleNative(spv2.Method{
		Name: "edit_ppt", Tool: true, LongRunning: false,
		Description: "就地修改 .pptx 幻灯片文本 → {id, replaced}（v0 仅 op=replace）",
		Input: objectSchema(map[string]any{
			"id": stringSchema("文件 id（必须是 .pptx）"),
			"operations": arraySchema(objectSchema(map[string]any{
				"op":  stringSchema("操作类型，v0 仅 replace"),
				"old": stringSchema("被替换的原文"),
				"new": stringSchema("替换后的新文本"),
			}, "op", "old"), "替换操作列表"),
		}, "id", "operations"),
		Output: objectSchema(map[string]any{
			"id":       stringSchema("文件 id"),
			"replaced": integerSchema("实际替换的条数"),
		}, "id", "replaced"),
	}, s.nativeEditPpt)
	s.HandleNative(spv2.Method{
		Name: "edit_pdf", Tool: true, LongRunning: false,
		Description: "就地修改 .pdf → {id, applied}（v0 支持 set_info / rotate / delete_pages / keep_pages）",
		Input: objectSchema(map[string]any{
			"id": stringSchema("文件 id（必须是 .pdf）"),
			"operations": arraySchema(objectSchema(map[string]any{
				"op":         stringSchema("操作类型：set_info / rotate / delete_pages / keep_pages"),
				"pages":      stringSchema("页选集（如 \"1-3,5\"），空 = 全部；仅页操作使用"),
				"degrees":    integerSchema("rotate 的顺时针角度：90 / 180 / 270"),
				"properties": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "set_info 的元数据键值对（如 title / author）"},
			}, "op"), "编辑操作列表"),
		}, "id", "operations"),
		Output: objectSchema(map[string]any{
			"id":      stringSchema("文件 id"),
			"applied": integerSchema("实际生效的操作数"),
		}, "id", "applied"),
	}, s.nativeEditPdf)
	s.HandleNative(spv2.Method{
		Name: "outline", Tool: true, LongRunning: false,
		Description: "先看文档结构再决定读哪段 → {id, doc_kind, digest, exact_digest, nodes, node_count, chars, tables, parts, warnings}",
		Input: objectSchema(map[string]any{
			"id":    stringSchema("文件 id（当前仅 .docx）"),
			"depth": integerSchema("1（默认）= 只列正文块；2 = 展开表格到单元格段落"),
		}, "id"),
		Output: objectSchema(map[string]any{
			"id":           stringSchema("文件 id"),
			"doc_kind":     stringSchema("文档类型（docx）"),
			"digest":       stringSchema("规范化空白后的整篇指纹"),
			"exact_digest": stringSchema("未折叠空白的整篇指纹"),
			"nodes":        arraySchema(docNodeSchema(), "结构树顶层节点"),
			"node_count":   integerSchema("节点总数"),
			"chars":        integerSchema("正文字符数"),
			"tables":       integerSchema("表格数"),
			"parts":        integerSchema("文档部件数"),
			"warnings":     arraySchema(stringSchema("警告"), "解析时发现但不阻断的问题"),
		}, "id", "doc_kind", "digest", "exact_digest", "nodes", "node_count", "chars"),
	}, s.nativeOutline)
	s.HandleNative(spv2.Method{
		Name: "read_range", Tool: true, LongRunning: false,
		Description: "按节点分页读文档 → {id, doc_kind, digest, exact_digest, node, text, total, next}",
		Input: objectSchema(map[string]any{
			"id":     stringSchema("文件 id（当前仅 .docx）"),
			"node":   stringSchema("节点地址；空 = 整篇"),
			"offset": integerSchema("起始字符（rune）"),
			"limit":  integerSchema("本次最多返回多少字符；0 = 默认 4000"),
		}, "id"),
		Output: objectSchema(map[string]any{
			"id":           stringSchema("文件 id"),
			"doc_kind":     stringSchema("文档类型（docx）"),
			"digest":       stringSchema("规范化空白后的整篇指纹"),
			"exact_digest": stringSchema("未折叠空白的整篇指纹"),
			"node":         stringSchema("本次读取的节点地址"),
			"text":         stringSchema("本块文本"),
			"total":        integerSchema("节点或整篇的字符数"),
			"next":         integerSchema("下一块起点；0 = 已读完"),
		}, "id", "doc_kind", "digest", "exact_digest", "node", "text", "total"),
	}, s.nativeReadRange)
	s.HandleNative(spv2.Method{
		Name: "find", Tool: true, LongRunning: false,
		Description: "只找位置不改文档 → {id, digest, exact_digest, matches:[{node, run, start, end, context, exact_anchor}]}",
		Input: objectSchema(map[string]any{
			"id":    stringSchema("文件 id（当前仅 .docx）"),
			"text":  stringSchema("要查找的文本"),
			"node":  stringSchema("限定在某个节点内；空 = 全文"),
			"regex": booleanSchema("true = text 按 Go 正则解释"),
			"limit": integerSchema("最多命中数，默认 20、上限 200"),
		}, "id", "text"),
		Output: objectSchema(map[string]any{
			"id":           stringSchema("文件 id"),
			"digest":       stringSchema("规范化空白后的整篇指纹"),
			"exact_digest": stringSchema("未折叠空白的整篇指纹"),
			"matches": arraySchema(objectSchema(map[string]any{
				"node":         stringSchema("命中节点地址"),
				"run":          stringSchema("命中起点所在的 run 地址"),
				"start":        integerSchema("起始字符（rune，闭区间）"),
				"end":          integerSchema("结束字符（rune，开区间）"),
				"context":      stringSchema("命中上下文"),
				"exact_anchor": stringSchema("命中节点的原始文本指纹"),
			}, "node", "start", "end"), "命中列表，同段多处各占一条"),
		}, "id", "digest", "exact_digest", "matches"),
	}, s.nativeFind)
	s.HandleNative(spv2.Method{
		Name: "patch", Tool: true, LongRunning: false,
		Description: "按节点地址就地改 .docx（整批原子）→ {id, applied, digest, exact_digest, diff, warnings}",
		Input: objectSchema(map[string]any{
			"id":           stringSchema("文件 id（必须是 .docx）"),
			"anchor":       stringSchema("规范化整篇指纹；不匹配即整批拒绝"),
			"exact_anchor": stringSchema("未折叠空白的整篇指纹；不匹配即整批拒绝"),
			"operations": arraySchema(objectSchema(map[string]any{
				"op":           stringSchema("replace_text / delete_node"),
				"node":         stringSchema("节点地址（如 body/p12）"),
				"range":        objectSchema(map[string]any{"start": integerSchema("起始字符（rune）"), "end": integerSchema("结束字符（rune）")}, "start", "end"),
				"text":         stringSchema("replace_text 的新文本"),
				"anchor":       stringSchema("节点规范化文本指纹"),
				"exact_anchor": stringSchema("节点原始文本指纹；带 range 时优先使用"),
			}, "op", "node"), "编辑操作列表；全部生效或全部不动"),
		}, "id", "operations"),
		Output: objectSchema(map[string]any{
			"id":           stringSchema("文件 id"),
			"applied":      integerSchema("实际生效的操作数"),
			"digest":       stringSchema("改后的规范化整篇指纹"),
			"exact_digest": stringSchema("改后的未折叠空白整篇指纹"),
			"diff": arraySchema(objectSchema(map[string]any{
				"op":     stringSchema("操作类型"),
				"node":   stringSchema("节点地址"),
				"before": stringSchema("改前文本"),
				"after":  stringSchema("改后文本"),
			}, "op", "node"), "可回放的变更列表"),
			"warnings": arraySchema(stringSchema("警告"), "应用时发现但不阻断的问题"),
		}, "id", "applied", "digest", "exact_digest"),
	}, s.nativePatch)
	s.HandleNative(spv2.Method{
		Name: "verify", Tool: true, LongRunning: false,
		Description: "改完自查：指纹与片段核对 → {id, ok, digest, failures}",
		Input: objectSchema(map[string]any{
			"id":           stringSchema("文件 id（当前仅 .docx）"),
			"digest":       stringSchema("期望的整篇指纹"),
			"contains":     arraySchema(stringSchema("必须出现的片段"), "必须出现的片段列表"),
			"not_contains": arraySchema(stringSchema("必须不出现的片段"), "必须不出现的片段列表"),
		}, "id"),
		Output: objectSchema(map[string]any{
			"id":       stringSchema("文件 id"),
			"ok":       booleanSchema("true = 全部核对通过"),
			"digest":   stringSchema("当前整篇指纹"),
			"failures": arraySchema(stringSchema("失败原因"), "未通过的核对项"),
		}, "id", "ok", "digest"),
	}, s.nativeVerify)
}

// ---------- 原生 handler ----------

func (s *Server) nativeSave(_ context.Context, call *spv2.Call) error {
	out, err := s.save(call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeFetch(_ context.Context, call *spv2.Call) error {
	out, err := s.fetch(call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeRead(_ context.Context, call *spv2.Call) error {
	out, err := s.read(call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeWrite(_ context.Context, call *spv2.Call) error {
	out, err := s.write(call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeEditWord(_ context.Context, call *spv2.Call) error {
	out, err := s.editWord(call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeEditPpt(_ context.Context, call *spv2.Call) error {
	out, err := s.editPpt(call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeEditPdf(_ context.Context, call *spv2.Call) error {
	out, err := s.editPdf(call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeOutline(ctx context.Context, call *spv2.Call) error {
	out, err := s.docOutline(ctx, call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeReadRange(ctx context.Context, call *spv2.Call) error {
	out, err := s.docReadRange(ctx, call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeFind(ctx context.Context, call *spv2.Call) error {
	out, err := s.docFind(ctx, call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativePatch(ctx context.Context, call *spv2.Call) error {
	out, err := s.docPatch(ctx, call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeVerify(ctx context.Context, call *spv2.Call) error {
	out, err := s.docVerify(ctx, call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

// replyNativeError 把共享业务函数的错误映射成 SPv2 错误码后回包。
// 只做传输层分类：业务逻辑与错误文案仍由共享函数提供。
func replyNativeError(call *spv2.Call, err error) error {
	code := spv2.CodeInvalidArgument
	switch {
	case errors.Is(err, store.ErrNotFound):
		code = spv2.CodeNotFound
	case errors.Is(err, docmodel.ErrStale):
		code = spv2.CodeConflict
	}
	return call.ReplyError(code, err.Error(), false, nil)
}

// ---------- JSON Schema 小工具 ----------

func stringSchema(desc string) map[string]any {
	m := map[string]any{"type": "string"}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

func integerSchema(desc string) map[string]any {
	m := map[string]any{"type": "integer"}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

func booleanSchema(desc string) map[string]any {
	m := map[string]any{"type": "boolean"}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

func arraySchema(item map[string]any, desc string) map[string]any {
	m := map[string]any{"type": "array", "items": item}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, name := range required {
			req[i] = name
		}
		m["required"] = req
	}
	return m
}

// docNodeSchema 是结构树节点的线上形态（children 递归同形）。
func docNodeSchema() map[string]any {
	return objectSchema(map[string]any{
		"id":           stringSchema("节点地址"),
		"type":         stringSchema("节点类型：paragraph / table / row / cell"),
		"text":         stringSchema("节点文本"),
		"level":        integerSchema("标题级别"),
		"truncated":    booleanSchema("节点文本是否被截断"),
		"exact_anchor": stringSchema("原始文本指纹"),
		"children":     arraySchema(map[string]any{"type": "object"}, "子节点（同形递归）"),
	}, "id", "type")
}
