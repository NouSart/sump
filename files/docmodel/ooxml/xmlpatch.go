// Package ooxml 是 OOXML（docx / pptx / xlsx）的容器与字节级 XML 补丁层。
//
// 两个关键决定：
//
//  1. 不用 encoding/xml 把整个 part marshal 回去。那样会丢掉命名空间前缀、属性
//     顺序、w:rsid*、mc:AlternateContent 与所有未知扩展——Word 打开就报"内容有
//     问题"。这里只做"原始字节的定向替换"，未命中的字节一个都不动。
//  2. 扫描用 xml.Decoder.RawToken + InputOffset，拿到的是**原始字节**区间，所以
//     拼回去就是原文（测试里锁了一条不变量：所有 token 区间首尾相接 == 原文）。
package ooxml

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// TokenKind 是扫描出的 token 类别。
type TokenKind int

const (
	TokOther     TokenKind = iota // 未归类（保留位）
	TokStart                      // <a>
	TokEnd                        // </a>（自闭合元素的 End 长度为 0）
	TokCharData                   // 文本（保留实体引用的原始字节）
	TokComment                    // <!-- -->
	TokProcInst                   // <?xml ... ?>
	TokDirective                  // <!DOCTYPE ...> / <![CDATA[ ... ]]>
)

// Token 是一个 token 及其在原始字节中的区间 [Start, End)。
// Depth 是 token 开始时的嵌套深度（根元素 0）；Parent 是父元素在切片中的下标（-1 = 无）。
type Token struct {
	Kind   TokenKind
	Name   string // 元素本地名（Start / End 有值；命名空间前缀本层不关心）
	Start  int
	End    int
	Depth  int
	Parent int
}

// Scan 扫描 XML，返回按出现顺序排列的 token。
// 只做语法级扫描（不解析命名空间、不展开实体），失败时带偏移报错。
func Scan(data []byte) ([]Token, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		toks  []Token
		stack []int
		off   int
	)
	for {
		rt, err := dec.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("XML 扫描失败（偏移 %d）: %w", off, err)
		}
		end := int(dec.InputOffset())
		if end < off || end > len(data) {
			return nil, fmt.Errorf("XML 扫描偏移异常（%d..%d，总长 %d）", off, end, len(data))
		}
		t := Token{Start: off, End: end, Depth: len(stack), Parent: -1}
		if len(stack) > 0 {
			t.Parent = stack[len(stack)-1]
		}
		switch v := rt.(type) {
		case xml.StartElement:
			t.Kind, t.Name = TokStart, v.Name.Local
			toks = append(toks, t)
			stack = append(stack, len(toks)-1)
			off = end
			continue
		case xml.EndElement:
			// 自闭合元素不消耗字节：区间长度可以是 0，但仍要出栈（否则深度全错）。
			t.Kind, t.Name = TokEnd, v.Name.Local
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			t.Depth = len(stack)
			t.Parent = -1
			if len(stack) > 0 {
				t.Parent = stack[len(stack)-1]
			}
		case xml.CharData:
			t.Kind = TokCharData
		case xml.Comment:
			t.Kind = TokComment
		case xml.ProcInst:
			t.Kind = TokProcInst
		case xml.Directive:
			t.Kind = TokDirective
		default:
			t.Kind = TokOther
		}
		toks = append(toks, t)
		off = end
	}
	if len(stack) > 0 {
		return nil, fmt.Errorf("XML 未闭合：还有 %d 个元素没有结束", len(stack))
	}
	return toks, nil
}

// TextElement 是一个文本叶子元素（docx 的 <w:t>、pptx 的 <a:t>）及其在原文中的区间。
// 区间是全含的：[Start, End) 覆盖 <w:t ...>内容</w:t>，Inner 是不含标签的内容区间。
type TextElement struct {
	Name       string
	Start      int
	End        int
	InnerStart int
	InnerEnd   int
	Text       string // 内部字符数据（解码后；可能有多个片段，已拼接）
	Index      int    // 同名字符串元素的出现序号（0 起）
	Depth      int
}

// TextElements 取出所有指定名字的元素的完整区间与文本。
// 用元素（而不是逐段字符数据）作为单位，是因为 docx 的替换要能整段换掉 <w:t> 的内容，
// 也要能在跨 run 时按 run 边界拆开写。
func TextElements(data []byte, names ...string) ([]TextElement, error) {
	toks, err := Scan(data)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var out []TextElement
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.Kind != TokStart || !want[t.Name] {
			continue
		}
		// 配对的结束标签：同深度、同名、在 i 之后的第一个 TokEnd。
		endIdx := -1
		for j := i + 1; j < len(toks); j++ {
			if toks[j].Kind == TokEnd && toks[j].Name == t.Name && toks[j].Depth == t.Depth {
				endIdx = j
				break
			}
		}
		if endIdx < 0 {
			return nil, fmt.Errorf("XML 结构异常：<%s> 没有配对结束（偏移 %d）", t.Name, t.Start)
		}
		el := TextElement{
			Name:       t.Name,
			Start:      t.Start,
			End:        toks[endIdx].End,
			InnerStart: t.End,
			InnerEnd:   toks[endIdx].Start,
			Index:      len(out),
			Depth:      t.Depth,
		}
		var b strings.Builder
		for j := i + 1; j < endIdx; j++ {
			if toks[j].Kind == TokCharData {
				b.Write(data[toks[j].Start:toks[j].End])
			}
		}
		el.Text = UnescapeText(b.String())
		out = append(out, el)
	}
	return out, nil
}

// Edit 是一次字节级替换：把原文的 [Start, End) 换成 Replace。
// Replace 为 nil 即删除该区间；Start == End 即插入。
type Edit struct {
	Start   int
	End     int
	Replace []byte
}

// Apply 按偏移升序应用编辑：区间必须落在数据内且互不重叠，其余字节原样保留。
// 无编辑时原样返回（不复制，调用方可据此判断"什么都没改"）。
func Apply(data []byte, edits []Edit) ([]byte, error) {
	if len(edits) == 0 {
		return data, nil
	}
	es := append([]Edit(nil), edits...)
	sort.Slice(es, func(i, j int) bool {
		if es[i].Start != es[j].Start {
			return es[i].Start < es[j].Start
		}
		return es[i].End < es[j].End
	})
	for i, e := range es {
		if e.Start < 0 || e.End < e.Start || e.End > len(data) {
			return nil, fmt.Errorf("第 %d 个编辑区间越界：[%d,%d)（总长 %d）", i+1, e.Start, e.End, len(data))
		}
		if i > 0 && e.Start < es[i-1].End {
			return nil, fmt.Errorf("编辑区间重叠：第 %d 个 [%d,%d) 与第 %d 个 [%d,%d)",
				i, es[i-1].Start, es[i-1].End, i+1, e.Start, e.End)
		}
	}
	var buf bytes.Buffer
	buf.Grow(len(data) + 64)
	prev := 0
	for _, e := range es {
		buf.Write(data[prev:e.Start])
		buf.Write(e.Replace)
		prev = e.End
	}
	buf.Write(data[prev:])
	return buf.Bytes(), nil
}

// EscapeText 转义文本节点的内容，只处理必须处理的三个字符（& < >）。
// 换行 / 制表不在这里转换：docx 要写 <w:br/> / <w:tab/>，pptx 要写 <a:br/>，
// 由各格式层决定（放到这里会把"格式知识"漏进公共层）。
func EscapeText(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// UnescapeText 还原文本节点内容里的实体引用（命名实体 + 十进制 / 十六进制字符引用）。
// 无法识别的实体原样保留——宁可让模型看到 &foo; ，也不要静默吞字符。
func UnescapeText(raw string) string {
	if !strings.Contains(raw, "&") {
		return raw
	}
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); {
		if raw[i] != '&' {
			b.WriteByte(raw[i])
			i++
			continue
		}
		j := strings.IndexByte(raw[i:], ';')
		if j <= 1 || j > 12 {
			b.WriteByte('&')
			i++
			continue
		}
		ent := raw[i+1 : i+j]
		switch ent {
		case "amp":
			b.WriteByte('&')
		case "lt":
			b.WriteByte('<')
		case "gt":
			b.WriteByte('>')
		case "quot":
			b.WriteByte('"')
		case "apos":
			b.WriteByte('\'')
		default:
			if r, ok := numericEntity(ent); ok {
				b.WriteRune(r)
			} else {
				b.WriteString(raw[i : i+j+1])
			}
		}
		i += j + 1
	}
	return b.String()
}

// numericEntity 解析 &#123; / &#x1F600; 形式的字符引用。
func numericEntity(ent string) (rune, bool) {
	if len(ent) < 2 || ent[0] != '#' {
		return 0, false
	}
	var (
		n   int64
		err error
	)
	if ent[1] == 'x' || ent[1] == 'X' {
		n, err = strconv.ParseInt(ent[2:], 16, 32)
	} else {
		n, err = strconv.ParseInt(ent[1:], 10, 32)
	}
	if err != nil || n <= 0 || n > unicode.MaxRune {
		return 0, false
	}
	return rune(n), true
}
