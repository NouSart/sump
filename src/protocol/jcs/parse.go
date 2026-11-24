package jcs

import (
	"encoding/json"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// maxDepth 限制数组 / 对象的嵌套层数，防止恶意输入把递归解析拖成栈溢出。
// 与 encoding/json 的上限（10000）保持一致。
const maxDepth = 10000

// nodeKind 是规范化中间树（一个 JSON 值）的种类。
type nodeKind uint8

const (
	kindNull nodeKind = iota
	kindBool
	kindNumber
	kindString
	kindArray
	kindObject
)

// node 是解析后的 JSON 值。
//
// 数字只保留原始字面量（json.Number），不在解析阶段转成 float64：
// 转 double 只发生在序列化（numberString），一次转换、一个口径，
// 不会出现「解析一次、序列化又一次」的精度分歧。
type node struct {
	kind nodeKind
	b    bool
	num  json.Number
	str  string
	arr  []*node
	obj  []member
}

// member 是对象的成员。units 是键的 UTF-16 code unit 序列：解析时一次性算好，
// 供 RFC 8785 §3.2.3 的排序使用，避免比较时反复做 UTF-8 → UTF-16 转换。
type member struct {
	key   string
	units []uint16
	val   *node
}

// parse 把 raw 解析成 JSON 值树，并完成 JCS 要求的全部输入校验：
// 语法、UTF-8 合法性、孤立代理项（lone surrogate）、重复键、尾随垃圾。
func parse(raw []byte) (*node, error) {
	p := &parser{data: raw}
	p.skipSpace()
	if p.pos >= len(raw) {
		return nil, p.errorf("空输入，期望一个 JSON 值")
	}
	v, err := p.parseValue(0)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(raw) {
		return nil, p.errorf("JSON 值之后还有多余内容")
	}
	return v, nil
}

// parser 是递归下降的 JSON 解析器。pos 是当前读到的字节下标。
type parser struct {
	data []byte
	pos  int
}

// errorf 生成带字节位置的中文错误。位置按 1 起算，方便对照输入排查。
func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("jcs: 非法 JSON（第 %d 字节附近）：%s", p.pos+1, fmt.Sprintf(format, args...))
}

// skipSpace 跳过 JSON 允许的空白（空格、制表符、换行、回车）。其余空白字符不合法。
func (p *parser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

// parseValue 解析一个 JSON 值。depth 是当前嵌套深度（顶层为 0）。
func (p *parser) parseValue(depth int) (*node, error) {
	if depth > maxDepth {
		return nil, p.errorf("数组 / 对象嵌套超过 %d 层", maxDepth)
	}
	if p.pos >= len(p.data) {
		return nil, p.errorf("JSON 值意外结束")
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.parseObject(depth)
	case c == '[':
		return p.parseArray(depth)
	case c == '"':
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return &node{kind: kindString, str: s}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		num, err := p.parseNumber()
		if err != nil {
			return nil, err
		}
		return &node{kind: kindNumber, num: num}, nil
	case c == 't':
		if !p.consumeLiteral("true") {
			return nil, p.errorf("无法识别的值，期望 true")
		}
		return &node{kind: kindBool, b: true}, nil
	case c == 'f':
		if !p.consumeLiteral("false") {
			return nil, p.errorf("无法识别的值，期望 false")
		}
		return &node{kind: kindBool, b: false}, nil
	case c == 'n':
		if !p.consumeLiteral("null") {
			return nil, p.errorf("无法识别的值，期望 null")
		}
		return &node{kind: kindNull}, nil
	default:
		return nil, p.errorf("无法识别的值起始字符 %q", c)
	}
}

// consumeLiteral 消费一个固定字面量（true / false / null）。
func (p *parser) consumeLiteral(lit string) bool {
	if len(p.data)-p.pos < len(lit) || string(p.data[p.pos:p.pos+len(lit)]) != lit {
		return false
	}
	p.pos += len(lit)
	return true
}

// parseObject 解析对象，并在此处拒绝重复键：重复键无法定义「哪一个生效」，
// 不同实现各取一个就会得到不同 hash，幂等去重随之失效。
func (p *parser) parseObject(depth int) (*node, error) {
	p.pos++ // 跳过 '{'
	obj := &node{kind: kindObject}
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return obj, nil
	}
	seen := make(map[string]struct{})
	for {
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, p.errorf("对象的键必须是字符串")
		}
		key, err := p.parseString()
		if err != nil {
			return nil, err
		}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("jcs: 对象中存在重复的键 %q（重复键无法规范化）", key)
		}
		seen[key] = struct{}{}
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, p.errorf("键 %q 之后缺少冒号", key)
		}
		p.pos++ // 跳过 ':'
		p.skipSpace()
		val, err := p.parseValue(depth + 1)
		if err != nil {
			return nil, err
		}
		obj.obj = append(obj.obj, member{key: key, units: encodeUTF16(key), val: val})
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, p.errorf("对象没有闭合的 }")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++ // 下一个成员
		case '}':
			p.pos++
			return obj, nil
		default:
			return nil, p.errorf("对象成员之间应当是逗号或 }")
		}
	}
}

// parseArray 解析数组。数组元素保持原有顺序（JCS 只对对象键排序）。
func (p *parser) parseArray(depth int) (*node, error) {
	p.pos++ // 跳过 '['
	arr := &node{kind: kindArray}
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return arr, nil
	}
	for {
		p.skipSpace()
		val, err := p.parseValue(depth + 1)
		if err != nil {
			return nil, err
		}
		arr.arr = append(arr.arr, val)
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, p.errorf("数组没有闭合的 ]")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++ // 下一个元素
		case ']':
			p.pos++
			return arr, nil
		default:
			return nil, p.errorf("数组元素之间应当是逗号或 ]")
		}
	}
}

// parseString 解析 JSON 字符串并返回其 Unicode 值。
//
// 这里没有直接复用 encoding/json 的字符串解码，因为标准库会把非法 UTF-8 与
// 孤立代理项**静默替换**成 U+FFFD。对 JCS 来说这是不可接受的降级：
// 两个不同的输入会被折叠成同一个 hash。本实现一律报错。
func (p *parser) parseString() (string, error) {
	// 调用方已确认 p.data[p.pos] == '"'
	p.pos++
	start := p.pos
	var buf []byte // 只在出现转义时才分配
	for {
		if p.pos >= len(p.data) {
			return "", p.errorf("字符串没有结束引号")
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			if buf == nil {
				s := string(p.data[start:p.pos])
				p.pos++
				return s, nil
			}
			buf = append(buf, p.data[start:p.pos]...)
			p.pos++
			return string(buf), nil
		case c == '\\':
			buf = append(buf, p.data[start:p.pos]...)
			p.pos++
			if p.pos >= len(p.data) {
				return "", p.errorf("字符串中的转义序列不完整")
			}
			switch esc := p.data[p.pos]; esc {
			case '"', '\\', '/':
				buf = append(buf, esc)
				p.pos++
			case 'b':
				buf = append(buf, '\b')
				p.pos++
			case 'f':
				buf = append(buf, '\f')
				p.pos++
			case 'n':
				buf = append(buf, '\n')
				p.pos++
			case 'r':
				buf = append(buf, '\r')
				p.pos++
			case 't':
				buf = append(buf, '\t')
				p.pos++
			case 'u':
				r, err := p.parseUnicodeEscape()
				if err != nil {
					return "", err
				}
				buf = utf8.AppendRune(buf, r)
			default:
				return "", p.errorf("未知的转义字符 \\%c", esc)
			}
			start = p.pos
		case c < 0x20:
			return "", p.errorf("字符串中不能出现未转义的控制字符 U+%04X", c)
		case c < utf8.RuneSelf:
			p.pos++
		default:
			r, size := utf8.DecodeRune(p.data[p.pos:])
			if r == utf8.RuneError && size == 1 {
				return "", p.errorf("字符串包含非法 UTF-8 字节 0x%02X", c)
			}
			p.pos += size
		}
	}
}

// parseUnicodeEscape 处理 \uXXXX（p.pos 指向 'u'），并校验 UTF-16 代理对。
// 孤立代理项直接报错，理由同 parseString 的注释。
func (p *parser) parseUnicodeEscape() (rune, error) {
	p.pos++ // 跳过 'u'
	hi, ok := p.readHex4()
	if !ok {
		return 0, p.errorf("\\u 之后必须是 4 位十六进制数字")
	}
	if hi >= 0xD800 && hi <= 0xDBFF {
		if p.pos+1 >= len(p.data) || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
			return 0, p.errorf("高代理 \\u%04X 没有配对的低代理", hi)
		}
		p.pos += 2 // 跳过 "\u"
		lo, ok := p.readHex4()
		if !ok {
			return 0, p.errorf("\\u 之后必须是 4 位十六进制数字")
		}
		if lo < 0xDC00 || lo > 0xDFFF {
			return 0, p.errorf("高代理 \\u%04X 之后不是低代理（\\u%04X）", hi, lo)
		}
		return utf16.DecodeRune(rune(hi), rune(lo)), nil
	}
	if hi >= 0xDC00 && hi <= 0xDFFF {
		return 0, p.errorf("低代理 \\u%04X 没有配对的高代理", hi)
	}
	return rune(hi), nil
}

// readHex4 读取 4 位十六进制数字并前移 pos（失败时 pos 不动）。
func (p *parser) readHex4() (uint16, bool) {
	if p.pos+4 > len(p.data) {
		return 0, false
	}
	var v uint16
	for i := 0; i < 4; i++ {
		c := p.data[p.pos+i]
		var d uint16
		switch {
		case c >= '0' && c <= '9':
			d = uint16(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint16(c-'A') + 10
		default:
			return 0, false
		}
		v = v<<4 | d
	}
	p.pos += 4
	return v, true
}

// parseNumber 按 RFC 8259 的文法解析 JSON 数字字面量。
//
// 这里只做文法校验；是否能用 IEEE 754 double 表示（上溢 / 下溢）由
// numberString 在序列化时判定。
func (p *parser) parseNumber() (json.Number, error) {
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
	}
	// 整数部分：0 单独出现，或者 [1-9][0-9]*（不允许前导 0）。
	if p.pos >= len(p.data) {
		return "", p.errorf("数字不完整")
	}
	if p.data[p.pos] == '0' {
		p.pos++
	} else if p.data[p.pos] >= '1' && p.data[p.pos] <= '9' {
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	} else {
		return "", p.errorf("数字的整数部分不合法")
	}
	// 小数部分
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		if p.pos >= len(p.data) || !isDigit(p.data[p.pos]) {
			return "", p.errorf("小数点之后必须至少有一位数字")
		}
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	}
	// 指数部分
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if p.pos >= len(p.data) || !isDigit(p.data[p.pos]) {
			return "", p.errorf("指数部分必须至少有一位数字")
		}
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	}
	return json.Number(string(p.data[start:p.pos])), nil
}

// isDigit 判断 b 是否是十进制数字。
func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// encodeUTF16 返回 s 的 UTF-16 code unit 序列（RFC 8785 §3.2.3 的排序单位）。
// 进入这里的字符串已经过 UTF-8 与代理对校验，因此 encode 不会遇到孤立代理项。
func encodeUTF16(s string) []uint16 {
	return utf16.Encode([]rune(s))
}
