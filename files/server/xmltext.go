// 只在 OOXML 文本节点内替换。
//
// 之前 edit_word / edit_ppt 是对整段 XML 做字符串替换，会打到标签名、命名空间与属性上：
// 把 old="t" 替换成 "Z" 会把 <a:t> 改成 <a:Z>，文本节点再也提不出来，
// 而调用方拿到的是「成功 13 处」和一个空文档（#15）。
//
// 扫描与字节拼接交给 docmodel/ooxml（Scan / TextElements / Apply），这里只留两件
// 格式相关的事：哪种元素算文本节点，以及换行 / 制表符展开成哪个标签。
package server

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/NouSart/sump/files/docmodel/ooxml"
)

// textReplace 是一次纯文本替换。
type textReplace struct {
	Old string
	New string
}

// textNodeSpec 描述一种 OOXML 方言的文本节点：元素名与换行/制表符标签。
type textNodeSpec struct {
	Element string // 目标元素 local name（docx 的 w:t / pptx 的 a:t 都是 "t"）
	Break   string // 换行标签，如 "<w:br/>"
	Tab     string // 制表符标签，如 "<w:tab/>"
}

var (
	// docx 的 <w:br/> / <w:tab/> 是 <w:r> 的子元素，和 <w:t> 同级，展开合法。
	docxTextSpec = textNodeSpec{Element: "t", Break: "<w:br/>", Tab: "<w:tab/>"}
	// pptx 的 <a:br/> 是段落（<a:p>）的子元素，和 <a:r> 同级；<a:r> 里再塞 <a:br/>
	// 是非法结构，PowerPoint 会当成需要修复的文件。改 run 结构超出 v0 范围，
	// 所以这里不给标签：遇到换行 / 制表符宁可报错，也不写出坏文件。
	pptxTextSpec = textNodeSpec{Element: "t"}
)

// errTextNotFound 表示某个替换操作在文本节点里一处都没匹配到。
// 调用方据此决定是整体报错（docx：所有操作必须全部命中）还是跳过本文件（pptx：逐页应用）。
type errTextNotFound struct {
	Op  int // 1 起的操作序号
	Old string
}

func (e errTextNotFound) Error() string {
	return fmt.Sprintf("第 %d 个 replace 未找到文本 %q", e.Op, e.Old)
}

// replaceInTextNodes 按顺序在文本节点内应用 ops，返回新的 XML 与每个 op 的替换处数。
// 只有内容真的变了的文本节点才产生一次字节替换，其余字节原样保留。
// 任一 op 一处都没命中时返回 errTextNotFound（此时 out 无意义，调用方不应使用）。
func replaceInTextNodes(src []byte, spec textNodeSpec, ops []textReplace) ([]byte, []int, error) {
	if len(ops) == 0 {
		return src, nil, nil
	}
	els, err := ooxml.TextElements(src, spec.Element)
	if err != nil {
		return nil, nil, fmt.Errorf("解析 XML 失败: %w", err)
	}

	counts := make([]int, len(ops))
	var edits []ooxml.Edit
	for _, el := range els {
		next := el.Text
		local := make([]int, len(ops))
		for i, op := range ops {
			if n := strings.Count(next, op.Old); n > 0 {
				next = strings.ReplaceAll(next, op.Old, op.New)
				local[i] = n
			}
		}
		if next == el.Text {
			continue
		}
		rendered, err := renderTextNode(src[el.Start:el.InnerStart], next, spec)
		if err != nil {
			return nil, nil, err
		}
		for i, n := range local {
			counts[i] += n
		}
		edits = append(edits, ooxml.Edit{
			Start:   el.InnerStart,
			End:     el.InnerEnd,
			Replace: rendered,
		})
	}

	for i := range ops {
		if counts[i] == 0 {
			return nil, nil, errTextNotFound{Op: i + 1, Old: ops[i].Old}
		}
	}
	if len(edits) == 0 {
		return src, counts, nil
	}
	out, err := ooxml.Apply(src, edits)
	if err != nil {
		return nil, nil, err
	}
	return out, counts, nil
}

// renderTextNode 把新文本渲染成文本节点的内容：换行 / 制表符展开成兄弟标签，
// 其余字符按 XML 规则转义。展开要先关掉当前文本节点、插入标签、再用原始开始
// 标签重开——OOXML 里 w:br / w:tab 是 w:t 的兄弟节点，不能嵌在字符数据里；
// 重开复用原始开始标签，才能保住 xml:space="preserve" 之类的属性。
func renderTextNode(startTag []byte, text string, spec textNodeSpec) ([]byte, error) {
	closeTag := "</" + startTagName(startTag) + ">"
	var buf bytes.Buffer
	for _, seg := range splitKeeps(text) {
		if seg.tag == "" {
			buf.WriteString(ooxml.EscapeText(seg.text))
			continue
		}
		tag := spec.Break
		if seg.tag == "\t" {
			tag = spec.Tab
		}
		if tag == "" {
			return nil, fmt.Errorf("该格式的文本节点无法表达换行或制表符（v0 不改 run 结构）")
		}
		buf.WriteString(closeTag)
		buf.WriteString(tag)
		buf.Write(startTag)
	}
	return buf.Bytes(), nil
}

// startTagName 取开始标签的限定名：`<w:t xml:space="preserve">` → `w:t`。
func startTagName(tag []byte) string {
	s := bytes.TrimPrefix(tag, []byte("<"))
	if i := bytes.IndexAny(s, " \t\r\n/>"); i >= 0 {
		return string(s[:i])
	}
	return string(s)
}

type textSeg struct {
	text string
	tag  string // 非空 = 这里要插入一个 br/tab 标签
}

// splitKeeps 把文本切成「字符段 / 换行标签 / 制表符标签」的序列。
func splitKeeps(s string) []textSeg {
	var out []textSeg
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, textSeg{text: cur.String()})
			cur.Reset()
		}
	}
	for _, r := range s {
		switch r {
		case '\n':
			flush()
			out = append(out, textSeg{tag: "\n"}) // 占位，由调用方换成具体标签
		case '\t':
			flush()
			out = append(out, textSeg{tag: "\t"})
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
