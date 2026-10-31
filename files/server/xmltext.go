// 只在 OOXML 文本节点内替换。
//
// 之前 edit_word / edit_ppt 是对整段 XML 做字符串替换，会打到标签名、命名空间与属性上：
// 把 old="t" 替换成 "Z" 会把 <a:t> 改成 <a:Z>，文本节点再也提不出来，
// 而调用方拿到的是「成功 13 处」和一个空文档（#15）。
//
// 这里的做法是逐 token 拷贝原始字节（RawToken + InputOffset），只改目标元素的 CharData，
// 其余字节一个都不碰。换行/制表符仍按 OOXML 的兄弟节点写法展开
// （</w:t><w:br/><w:t>），保留原有语义，且复用原始开始标签以保留 xml:space 等属性。
package server

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
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
	docxTextSpec = textNodeSpec{Element: "t", Break: "<w:br/>", Tab: "<w:tab/>"}
	pptxTextSpec = textNodeSpec{Element: "t", Break: "<a:br/>", Tab: "<a:tab/>"}
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
// 任一 op 一处都没命中时返回 errTextNotFound（此时 out 无意义，调用方不应使用）。
func replaceInTextNodes(src []byte, spec textNodeSpec, ops []textReplace) ([]byte, []int, error) {
	if len(ops) == 0 {
		return src, nil, nil
	}
	counts := make([]int, len(ops))
	out, err := walkTextNodes(src, spec, func(text string) (string, []int, bool) {
		next := text
		local := make([]int, len(ops))
		for i, op := range ops {
			if n := strings.Count(next, op.Old); n > 0 {
				next = strings.ReplaceAll(next, op.Old, op.New)
				local[i] = n
			}
		}
		if next == text {
			return text, nil, false
		}
		return next, local, true
	}, counts)
	if err != nil {
		return nil, nil, err
	}
	for i := range ops {
		if counts[i] == 0 {
			return nil, nil, errTextNotFound{Op: i + 1, Old: ops[i].Old}
		}
	}
	return out, counts, nil
}

// walkTextNodes 逐 token 拷贝 src，只在目标元素的 CharData 上调用 fn；
// fn 返回 changed=true 时用新文本重写该节点（并展开换行/制表符），否则原样拷贝。
// counts 会被累加 fn 返回的每个 op 的命中数。
func walkTextNodes(src []byte, spec textNodeSpec, fn func(string) (string, []int, bool), counts []int) ([]byte, error) {
	var buf bytes.Buffer
	dec := xml.NewDecoder(bytes.NewReader(src))

	var (
		depth    int    // 目标元素嵌套深度
		qname    string // 目标元素的限定名（含前缀），用于重新打开
		startTag []byte // 目标元素的原始开始标签，重新打开时复用（保留属性）
	)

	for {
		before := dec.InputOffset()
		tok, err := dec.RawToken()
		after := dec.InputOffset()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("解析 XML 失败: %w", err)
		}
		raw := src[before:after]

		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == spec.Element {
				if depth == 0 {
					qname = t.Name.Space
					if qname != "" {
						qname += ":"
					}
					qname += t.Name.Local
					startTag = raw
				}
				depth++
			}
			buf.Write(raw)

		case xml.EndElement:
			if t.Name.Local == spec.Element && depth > 0 {
				depth--
				if depth == 0 {
					startTag = nil
				}
			}
			buf.Write(raw)

		case xml.CharData:
			if depth > 0 {
				next, local, changed := fn(string(t))
				if changed {
					for i, n := range local {
						if i < len(counts) {
							counts[i] += n
						}
					}
					writeExpanded(&buf, next, spec, qname, startTag)
					continue
				}
			}
			buf.Write(raw)

		default:
			buf.Write(raw)
		}
	}
	return buf.Bytes(), nil
}

// writeExpanded 写出替换后的文本节点内容，把 \n / \t 展开成兄弟标签。
// 展开需要先关掉当前文本节点、插入标签、再用原始开始标签重开——OOXML 里
// w:br / w:tab 是 w:t 的兄弟节点，不能嵌在字符数据里。
func writeExpanded(buf *bytes.Buffer, text string, spec textNodeSpec, qname string, startTag []byte) {
	closeTag := "</" + qname + ">"
	for _, seg := range splitKeeps(text) {
		if seg.tag != "" {
			tag := spec.Break
			if seg.tag == "\t" {
				tag = spec.Tab
			}
			buf.WriteString(closeTag)
			buf.WriteString(tag)
			buf.Write(startTag)
			continue
		}
		escapeText(buf, seg.text)
	}
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

func escapeText(buf *bytes.Buffer, s string) {
	_ = xml.EscapeText(buf, []byte(s))
}
