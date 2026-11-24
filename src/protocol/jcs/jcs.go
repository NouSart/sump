// Package jcs 实现 RFC 8785（JSON Canonicalization Scheme，JCS）规范化与
// payload 哈希：payload_hash = sha256(JCS(payload))。
//
// 用途：store 的幂等与 report_result 去重要求「语义相同、只是键序 / 转义 /
// 数字写法不同」的 JSON 得到同一个 hash，所以必须先规范化再哈希。
//
// 规范化的三条核心规则：
//
//  1. 对象键按 UTF-16 code unit 排序（RFC 8785 §3.2.3），数组保持原顺序；
//  2. 字符串按 ECMAScript JSON.stringify 的规则转义（§3.2.2.2）：只转义
//     `"`、`\` 与 U+0000–U+001F 控制字符，`/` 不转义，非 ASCII 字符（中文、
//     emoji、U+2028/U+2029 等）一律直接输出 UTF-8；
//  3. 数字按 ECMAScript Number::toString 序列化（§3.2.2.3）：JSON number 先按
//     IEEE 754 double（就近舍入）解析，再输出最短可往返十进制形式；-0 输出 "0"。
//
// 输入必须是合法 JSON（RFC 8259）且满足 I-JSON（RFC 7493）对字符串的要求：
// 非法 UTF-8、孤立代理项（lone surrogate）、重复键、尾随垃圾一律返回错误。
// 这些情况若被静默「修正」，不同实现就会算出不同 hash，幂等去重随之失效，
// 因此这里宁可报错也不降级。
//
// 关于大整数（超过 2^53 的整数字面量）：RFC 8785 规定 JSON number 按 IEEE 754
// double 处理，所以本实现把字面量解析成 double 后再按 ES6 规则输出——能被
// double 精确表示的整数会输出整数形式，不能被精确表示的按 double 的最近值
// 输出；但「整数形式」以 ES6 为准：double 的十进制指数 n 落在 1..21 时输出
// 定点整数，超出时输出指数形式（例如 10^21 输出 "1e+21"）。见 numberString。
package jcs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Canonicalize 把 JSON 字节规范化为 RFC 8785 形式（UTF-8）。
//
// 输入里的空白、键序、转义写法、数字写法都会被归一；输出是确定性的规范形式，
// 同样内容重复调用结果一致。遇到非法 JSON、重复键、非法 / 不可表示的数字时
// 返回错误（错误文案以 "jcs: " 开头，并尽量带上字节位置或具体字面量）。
func Canonicalize(raw []byte) ([]byte, error) {
	root, err := parse(raw)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(raw))
	out, err = appendValue(out, root)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Hash 返回 sha256(Canonicalize(raw)) 的小写十六进制字符串。
// 规范化失败时返回空字符串与同样的错误。
func Hash(raw []byte) (string, error) {
	canon, err := Canonicalize(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// appendValue 把值树按 JCS 规则序列化并追加到 dst。
func appendValue(dst []byte, n *node) ([]byte, error) {
	switch n.kind {
	case kindNull:
		return append(dst, "null"...), nil
	case kindBool:
		if n.b {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil
	case kindNumber:
		s, err := numberString(n.num)
		if err != nil {
			return nil, err
		}
		return append(dst, s...), nil
	case kindString:
		return appendString(dst, n.str), nil
	case kindArray:
		dst = append(dst, '[')
		for i, e := range n.arr {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendValue(dst, e)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case kindObject:
		// RFC 8785 §3.2.3：对象键按 UTF-16 code unit 排序。重复键已在解析阶段
		// 拒绝，因此不存在等值键，排序结果是唯一的（与 sort 是否稳定无关）。
		sort.Slice(n.obj, func(i, j int) bool {
			return compareUTF16(n.obj[i].units, n.obj[j].units) < 0
		})
		dst = append(dst, '{')
		for i := range n.obj {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendString(dst, n.obj[i].key)
			dst = append(dst, ':')
			var err error
			dst, err = appendValue(dst, n.obj[i].val)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	default:
		return nil, fmt.Errorf("jcs: 内部错误：未知的节点类型 %d", n.kind)
	}
}

// appendString 按 RFC 8785 §3.2.2.2（即 ECMAScript JSON.stringify 的
// QuoteJSONString）输出字符串：只转义必需字符，其余字节原样保留。
//
// s 已由解析器保证是合法 UTF-8，因此非 ASCII 字节直接透传就是合法的 UTF-8 输出；
// U+2028 / U+2029 在 JSON 里合法，也不转义。
func appendString(dst []byte, s string) []byte {
	// \u 转义用小写十六进制（ES6 与 RFC 8785 §3.2.2.2 的示例均为小写）。
	const hexDigits = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\r':
			dst = append(dst, '\\', 'r')
		default:
			if c < 0x20 {
				// 其余控制字符：\u00XX，小写十六进制。
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
			} else {
				dst = append(dst, c)
			}
		}
	}
	return append(dst, '"')
}

// numberString 把 JSON 数字字面量转成 RFC 8785 §3.2.2.3 要求的 ES6 形式。
//
// JSON number 在 JCS 里就是 IEEE 754 double：字面量先按就近舍入（银行家舍入）
// 转成 double，再输出该 double 的最短可往返十进制形式。以下情况返回错误，
// 不允许静默降级：
//   - 上溢成 ±Inf（如 1e400）；
//   - 字面量非零却下溢成 0（如 1e-400，double 无法表示）。
//
// -0 / -0.0 按 ES6 输出 "0"。
func numberString(lit json.Number) (string, error) {
	s := string(lit)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", fmt.Errorf("jcs: 数字 %s 超出 IEEE754 double 可表示范围", s)
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		// ParseFloat 上溢时返回 ±Inf 并带 ErrRange；NaN 在 JSON 文法里不可能出现，
		// 这里只是兜底。
		return "", fmt.Errorf("jcs: 数字 %s 超出 IEEE754 double 可表示范围", s)
	}
	if f == 0 && hasNonZeroDigit(s) {
		return "", fmt.Errorf("jcs: 数字 %s 下溢为 0，无法用 IEEE754 double 表示", s)
	}
	return formatNumber(f), nil
}

// hasNonZeroDigit 判断数字字面量的尾数部分是否有非零数字（用于区分「本来就是
// 0」与下溢）。指数部分不参与判断：-0e5、0e-400 的值都是 0。
func hasNonZeroDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == 'e' || c == 'E':
			return false
		case c >= '1' && c <= '9':
			return true
		}
	}
	return false
}

// formatNumber 按 ECMAScript Number::toString（ECMA-262 7.1.12.1，基数 10）
// 输出非负 double。规则（n、k、s 的定义见规范）：
//
//	s = 最短可往返的有效数字串，k = len(s)，使得 x = s × 10^(n-k)
//	- k ≤ n ≤ 21：整数形式，s 后补 n-k 个 0；
//	- 0 < n ≤ 21：小数点插在 s 的第 n 位之后；
//	- -6 < n ≤ 0：写成 0.000…s；
//	- 其余：指数形式，d[.ddd]e±(n-1)。
//
// 取有效数字用 strconv.FormatFloat 的 'e' 形式（精度 -1 = 最短可往返，Ryu），
// 它给出的数字串与 V8 的一致（同样的最短、同样的就近选取），只是排版规则不同，
// 所以这里只借它的数字串，排版完全按 ES6 规则重做。
func formatNumber(f float64) string {
	if f == 0 { // 0 与 -0 都输出 "0"
		return "0"
	}
	var b []byte
	if math.Signbit(f) {
		b = append(b, '-')
		f = -f
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // 形如 d[.ddd]e±dd
	epos := strings.IndexByte(s, 'e')
	exp, _ := strconv.Atoi(s[epos+1:])
	digits := strings.Replace(s[:epos], ".", "", 1)
	k := len(digits)
	n := exp + 1
	switch {
	case k <= n && n <= 21: // 整数
		b = append(b, digits...)
		for i := 0; i < n-k; i++ {
			b = append(b, '0')
		}
	case 0 < n && n <= 21: // 小数，点在有效数字内部
		b = append(b, digits[:n]...)
		b = append(b, '.')
		b = append(b, digits[n:]...)
	case -6 < n && n <= 0: // 小数，点在有效数字之前
		b = append(b, '0', '.')
		for i := 0; i < -n; i++ {
			b = append(b, '0')
		}
		b = append(b, digits...)
	default: // 指数形式
		b = append(b, digits[0])
		if k > 1 {
			b = append(b, '.')
			b = append(b, digits[1:]...)
		}
		b = append(b, 'e')
		if n-1 >= 0 {
			b = append(b, '+')
		}
		b = append(b, strconv.Itoa(n-1)...)
	}
	return string(b)
}

// compareUTF16 按 UTF-16 code unit 序列比较两个键（RFC 8785 §3.2.3）。
// 注意这不是 UTF-8 字节序 / Unicode 码点序：辅助平面字符（如 emoji）的
// 首个 code unit 落在 0xD800–0xDBFF，会排在 U+E000 等 BMP 字符之前。
func compareUTF16(a, b []uint16) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}
