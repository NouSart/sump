package jcs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// canonical 调用 Canonicalize 并要求成功，返回字符串形式的结果。
func canonical(t *testing.T, raw string) string {
	t.Helper()
	got, err := Canonicalize([]byte(raw))
	if err != nil {
		t.Fatalf("Canonicalize(%q) 出错：%v", raw, err)
	}
	return string(got)
}

// mustHash 调用 Hash 并要求成功。
func mustHash(t *testing.T, raw string) string {
	t.Helper()
	h, err := Hash([]byte(raw))
	if err != nil {
		t.Fatalf("Hash(%q) 出错：%v", raw, err)
	}
	return h
}

// TestCanonicalizeNumbers 覆盖 RFC 8785 附录 B 的数值向量与 ES6 定点 / 指数
// 分界，并钉住大整数（超过 2^53 的整数字面量）的处理方式。
func TestCanonicalizeNumbers(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"零", `0`, `0`},
		{"负零", `-0`, `0`},
		{"负零小数", `-0.0`, `0`},
		{"负零指数", `-0e5`, `0`},
		{"正整数", `100`, `100`},
		{"小整数", `7`, `7`},
		// RFC 8785 附录 B 的数值向量。
		{"附录B 333333333.33333329", `333333333.33333329`, `333333333.3333333`},
		{"附录B 1E30", `1E30`, `1e+30`},
		{"附录B 4.50", `4.50`, `4.5`},
		{"附录B 2e-3", `2e-3`, `0.002`},
		{"附录B 0.002", `0.002`, `0.002`},
		{"附录B 1e-27", `1e-27`, `1e-27`},
		{"最小次正规数", `5e-324`, `5e-324`},
		{"最大有限 double", `1.7976931348623157e308`, `1.7976931348623157e+308`},
		// ES6 的分界：n = 21 用定点，n = 22 用指数；小数同理（-6 < n ≤ 0 用定点）。
		{"1e20 定点", `1e20`, `100000000000000000000`},
		{"1e21 指数", `1e21`, `1e+21`},
		{"0.000001 定点", `0.000001`, `0.000001`},
		{"1e-7 指数", `1e-7`, `1e-7`},
		{"小数位较多", `3.141592653589793`, `3.141592653589793`},
		// 大整数：JSON number 按 IEEE 754 double 处理（RFC 8785）。
		// 2^53 可被 double 精确表示，输出整数形式。
		{"2^53", `9007199254740992`, `9007199254740992`},
		// 2^53+1 不可精确表示：double 就近舍入（这里是偶数 2^53）。
		{"2^53+1 舍入", `9007199254740993`, `9007199254740992`},
		{"负 2^53+1 舍入", `-9007199254740993`, `-9007199254740992`},
		// 2^64 可被 double 精确表示；但 ES6 输出的是「最短可往返」十进制，
		// 不是把字面量逐位照抄（详见包注释与报告里的歧义说明）。
		{"2^64 最短形式", `18446744073709551616`, `18446744073709552000`},
		// 10^21 可被 double 精确表示，但 ES6 的十进制指数 n = 22 > 21，
		// 要求指数形式，所以输出 1e+21。
		{"10^21 指数形式", `1000000000000000000000`, `1e+21`},
		{"超长整数", `123456789012345678901234567890`, `1.2345678901234568e+29`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canonical(t, tc.in)
			if got != tc.want {
				t.Fatalf("Canonicalize(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("规范化结果不是合法 JSON：%q", got)
			}
			// 规范化必须幂等。
			if again := canonical(t, got); again != got {
				t.Fatalf("二次规范化结果不同：%q → %q", got, again)
			}
		})
	}
}

// TestFormatNumberRFC8785AppendixB 直接用附录 B 的 IEEE 754 位模式钉住
// ES6 Number::toString 的排版规则（不经过 JSON 字面量，覆盖更细）。
func TestFormatNumberRFC8785AppendixB(t *testing.T) {
	cases := []struct {
		bits uint64
		want string
	}{
		{0x0000000000000000, "0"},
		{0x8000000000000000, "0"}, // -0
		{0x0000000000000001, "5e-324"},
		{0x8000000000000001, "-5e-324"},
		{0x7fefffffffffffff, "1.7976931348623157e+308"},
		{0xffefffffffffffff, "-1.7976931348623157e+308"},
		{0x4340000000000000, "9007199254740992"},
		{0xc340000000000000, "-9007199254740992"},
		{0x4430000000000000, "295147905179352830000"},
		{0x44b52d02c7e14af5, "9.999999999999997e+22"},
		{0x44b52d02c7e14af6, "1e+23"},
		{0x44b52d02c7e14af7, "1.0000000000000001e+23"},
		{0x444b1ae4d6e2ef4e, "999999999999999700000"},
		{0x444b1ae4d6e2ef4f, "999999999999999900000"},
		{0x444b1ae4d6e2ef50, "1e+21"},
		{0x444b1ae4d6e2ef51, "1.0000000000000001e+21"},
		{0x3eb0c6f7a0b5ed8d, "0.000001"},
		{0x3e7ad7f29abcaf48, "1e-7"},
		{0x41b3de4355555553, "333333333.3333332"},
		{0x41b3de4355555554, "333333333.33333325"},
		{0x41b3de4355555555, "333333333.3333333"},
		{0x41b3de4355555556, "333333333.3333334"},
		{0x41b3de4355555557, "333333333.33333343"},
		{0xbecbf647612f3696, "-0.0000033333333333333333"},
		{0x43143ff3c1cb0959, "1424953923781206.2"},
	}
	for _, tc := range cases {
		f := math.Float64frombits(tc.bits)
		if got := formatNumber(f); got != tc.want {
			t.Errorf("formatNumber(0x%016x) = %q，期望 %q", tc.bits, got, tc.want)
		}
		// 期望串自身也必须能解析并规范化回同一个串（最短表示的自洽性）。
		if got := canonical(t, tc.want); got != tc.want {
			t.Errorf("Canonicalize(%q) = %q，期望原样", tc.want, got)
		}
	}
}

// TestObjectKeyOrder 覆盖键序无关性与 RFC 8785 §3.2.3 的 UTF-16 code unit 排序。
func TestObjectKeyOrder(t *testing.T) {
	t.Run("键序不同语义相同", func(t *testing.T) {
		want := `{"a":2,"b":1}`
		for _, in := range []string{
			`{"b":1,"a":2}`,
			`{"a":2,"b":1}`,
			`{ "b" : 1 , "a" : 2 }`,
		} {
			if got := canonical(t, in); got != want {
				t.Fatalf("Canonicalize(%q) = %q，期望 %q", in, got, want)
			}
		}
	})

	t.Run("hash 相同", func(t *testing.T) {
		h1 := mustHash(t, `{"b":1,"a":2}`)
		h2 := mustHash(t, `{"a":2,"b":1}`)
		if h1 != h2 {
			t.Fatalf("键序不同但语义相同的 JSON，hash 应相同：%s vs %s", h1, h2)
		}
	})

	t.Run("UTF-16 序不是码点序", func(t *testing.T) {
		// U+1F600（emoji）的码点大于 U+E000，但它的首个 UTF-16 code unit 是
		// 0xD83D，小于 U+E000——必须按 UTF-16 code unit 排序。
		got := canonical(t, `{"\uE000":1,"\uD83D\uDE00":2}`)
		want := "{\"😀\":2,\"\ue000\":1}"
		if got != want {
			t.Fatalf("键排序应按 UTF-16 code unit：%q，期望 %q", got, want)
		}
	})

	t.Run("非 ASCII 键", func(t *testing.T) {
		// "z"（U+007A）< "é"（U+00E9）
		got := canonical(t, `{"\u00e9":1,"z":2}`)
		want := `{"z":2,"é":1}`
		if got != want {
			t.Fatalf("Canonicalize = %q，期望 %q", got, want)
		}
	})

	t.Run("数字样式的键按字符串排序", func(t *testing.T) {
		got := canonical(t, `{"10":1,"2":2,"1":3}`)
		want := `{"1":3,"10":1,"2":2}`
		if got != want {
			t.Fatalf("Canonicalize = %q，期望 %q", got, want)
		}
	})
}

// TestCanonicalizeStructures 覆盖嵌套对象 / 数组、空白归一与数组保序。
func TestCanonicalizeStructures(t *testing.T) {
	in := `{"z":[{"b":2,"a":1},{"d":4,"c":[3,{"f":6,"e":5}]}],` +
		`"a":true,"n":null,"t":"x","arr":[1,2,3]}`
	want := `{"a":true,"arr":[1,2,3],"n":null,"t":"x",` +
		`"z":[{"a":1,"b":2},{"c":[3,{"e":5,"f":6}],"d":4}]}`
	if got := canonical(t, in); got != want {
		t.Fatalf("嵌套结构规范化错误：\n得到 %q\n期望 %q", got, want)
	}

	// 数组顺序绝不能被重排（只有对象键排序）。
	if got := canonical(t, `[3,1,2]`); got != `[3,1,2]` {
		t.Fatalf("数组顺序不应改变，得到 %q", got)
	}

	// 空白与换行全部去掉。
	spaced := "  {\n\t\"b\" : 1 ,\r\n \"a\" : [ 1 , 2 ]\n}  "
	if got := canonical(t, spaced); got != `{"a":[1,2],"b":1}` {
		t.Fatalf("空白归一错误：%q", got)
	}

	// 顶层任意 JSON 值都可以规范化。
	topLevel := []struct{ in, want string }{
		{`null`, `null`},
		{`true`, `true`},
		{`false`, `false`},
		{`"x"`, `"x"`},
		{`42`, `42`},
		{`1.5`, `1.5`},
		{`[]`, `[]`},
		{`{}`, `{}`},
	}
	for _, tc := range topLevel {
		if got := canonical(t, tc.in); got != tc.want {
			t.Fatalf("Canonicalize(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestCanonicalizeStrings 覆盖 RFC 8785 §3.2.2.2 的字符串转义规则。
func TestCanonicalizeStrings(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通 ASCII", `"abc"`, `"abc"`},
		// 只转义 "、\、控制字符；"/" 不转义。
		{"必须转义与斜杠", `"a\"b\\c\/d\b\f\n\r\t"`, `"a\"b\\c/d\b\f\n\r\t"`},
		{"控制字符转义", `"\u0000\u0001\u001f"`, `"\u0000\u0001\u001f"`},
		{"中文转义还原为 UTF-8", `"\u4e2d\u6587"`, `"中文"`},
		{"emoji 代理对", `"\uD83D\uDE00"`, `"😀"`},
		{"斜杠不转义", `"a\/b"`, `"a/b"`},
		{"HTML 字符不转义", `"<&>"`, `"<&>"`},
		{"反斜杠", `"a\\b"`, `"a\\b"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonical(t, tc.in); got != tc.want {
				t.Fatalf("Canonicalize(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}

	// DEL（U+007F）与 U+2028 / U+2029 在 JSON 里合法，且不属于必须转义的
	// 控制字符（U+0000–U+001F），应原样输出。
	if got := canonical(t, `"\u007f\u2028\u2029"`); got != "\"\x7f\u2028\u2029\"" {
		t.Fatalf("DEL / U+2028 / U+2029 不应转义，得到 %q", got)
	}

	// 转义写法不同、语义相同的字符串，规范化后必须一致。
	if h1, h2 := mustHash(t, `{"s":"\u4f60\u597d"}`), mustHash(t, `{"s":"你好"}`); h1 != h2 {
		t.Fatalf("转义写法不同但语义相同，hash 应相同：%s vs %s", h1, h2)
	}
	if got := canonical(t, `{"\u00e9":1}`); got != "{\"é\":1}" {
		t.Fatalf("非 ASCII 键应输出 UTF-8：%q", got)
	}
	if h1, h2 := mustHash(t, `{"\u00e9":1}`), mustHash(t, `{"é":1}`); h1 != h2 {
		t.Fatalf("\\u00e9 与 é 的 hash 应相同：%s vs %s", h1, h2)
	}
}

// TestRFC8785FrenchExample 是 RFC 8785 文档 / 官方测试数据里的综合示例：
// 数字、非 ASCII 字符串、键序、字面量混在一起。
func TestRFC8785FrenchExample(t *testing.T) {
	in := `{"numbers":[333333333.33333329,1E30,4.50,2e-3,0.002,1e-27],` +
		`"string":"Pères de l'Église","literals":[null,true,false]}`
	want := `{"literals":[null,true,false],` +
		`"numbers":[333333333.3333333,1e+30,4.5,0.002,0.002,1e-27],` +
		`"string":"Pères de l'Église"}`
	if got := canonical(t, in); got != want {
		t.Fatalf("RFC 8785 综合示例规范化错误：\n得到 %q\n期望 %q", got, want)
	}
}

// TestDuplicateKeysRejected 覆盖重复键（含转义后同名、嵌套、不相邻）。
func TestDuplicateKeysRejected(t *testing.T) {
	cases := []string{
		`{"a":1,"a":2}`,
		`{"a":1,"\u0061":2}`,    // 转义后与 "a" 同名
		`{"a":1,"b":2,"a":3}`,   // 不相邻
		`{"a":{"b":1,"b":2}}`,   // 嵌套
		`{"\u00e9":1,"é":2}`,    // 转义与字面量同名
		`{ "a" : 1 , "a" : 2 }`, // 带空白
		`[{"a":1,"a":1}]`,       // 数组里的对象
		`{"a":1,"a":{"b":2}}`,   // 值不同也算重复
		`{"":1,"":2}`,           // 空键也算
	}
	for _, in := range cases {
		_, err := Canonicalize([]byte(in))
		if err == nil {
			t.Fatalf("重复键应当报错，但 Canonicalize(%q) 成功了", in)
		}
		if !strings.Contains(err.Error(), "重复的键") {
			t.Fatalf("重复键错误信息应指明问题，得到：%v", err)
		}
		if !strings.HasPrefix(err.Error(), "jcs: ") {
			t.Fatalf("错误应带 jcs: 前缀：%v", err)
		}
	}
}

// TestInvalidJSONRejected 覆盖非法 JSON、尾随垃圾与不可表示的数字。
func TestInvalidJSONRejected(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"空输入", ``},
		{"只有空白", " \t\r\n "},
		{"未闭合对象", `{`},
		{"未闭合数组", `[`},
		{"缺少值", `{"a":}`},
		{"缺少键", `{:1}`},
		{"缺少冒号", `{"a" 1}`},
		{"对象尾随逗号", `{"a":1,}`},
		{"数组尾随逗号", `[1,]`},
		{"数组缺元素", `[1,,2]`},
		{"成员之间缺少逗号", `{"a":1 "b":2}`},
		{"括号不匹配", `{"a":1]`},
		{"多余的右括号", `{}}`},
		{"未闭合字符串", `"abc`},
		{"未知转义字符", `"\q"`},
		{"\\u 位数不足", `"\u12"`},
		{"\\u 非十六进制", `"\u12g4"`},
		{"孤立高代理", `"\ud800"`},
		{"孤立低代理", `"\udc00"`},
		{"高代理后跟高代理", `"\ud800\ud800"`},
		{"高代理后跟普通字符", `"\ud800x"`},
		{"字符串内裸控制字符", "\"a\x01b\""},
		{"前导零", `01`},
		{"负前导零", `-01`},
		{"小数点后没有数字", `1.`},
		{"小数点前没有数字", `.5`},
		{"正号", `+1`},
		{"缺少指数数字", `1e`},
		{"指数符号后没有数字", `1e+`},
		{"十六进制", `0x10`},
		{"NaN", `NaN`},
		{"Infinity", `Infinity`},
		{"-Infinity", `-Infinity`},
		{"undefined", `undefined`},
		{"单引号字符串", `'a'`},
		{"true 拼错", `tru`},
		{"true 后跟垃圾", `truex`},
		{"尾随垃圾", `{} {}`},
		{"两个数字", `1 2`},
		{"值后跟字符", `{"a":1}x`},
		{"上溢成 Inf", `1e400`},
		{"负上溢成 Inf", `-1e400`},
		{"double 边界上溢", `1e309`},
		{"下溢为 0", `1e-400`},
		{"负下溢为 0", `-1e-400`},
		{"极端下溢", `1e-999999`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Canonicalize([]byte(tc.in))
			if err == nil {
				t.Fatalf("非法输入应当报错，但 Canonicalize(%q) 成功了", tc.in)
			}
			if !strings.HasPrefix(err.Error(), "jcs: ") {
				t.Fatalf("错误应带 jcs: 前缀：%v", err)
			}
		})
	}

	// 非法 UTF-8 / BOM 单独用字节表示。
	badBytes := []struct {
		name string
		in   []byte
	}{
		{"字符串内非法字节", []byte("\"\xff\"")},
		{"顶层非法字节", []byte{0xFF}},
		{"截断的多字节序列", []byte("\"a\xc3\"")},
		{"BOM 不是 JSON 文本", []byte("\xef\xbb\xbf{}")},
		{"UTF-8 编码的代理项", []byte("\"\xed\xa0\x80\"")},
	}
	for _, tc := range badBytes {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Canonicalize(tc.in); err == nil {
				t.Fatalf("非法字节输入应当报错，输入 % x", tc.in)
			}
		})
	}
}

// TestHash 用独立算出的 sha256 向量钉住 hash 的输出，并验证去重语义。
func TestHash(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空对象", `{}`, "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"},
		{"空数组", `[]`, "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"},
		{"null", `null`, "74234e98afe7498fb5daf1f36ac2d78acc339464f950703b8c019892f982b90b"},
		// sha256(`{"a":1,"b":2}`) / sha256(`{"a":2,"b":1}`)
		{"键序 a,b", `{"a":2,"b":1}`, "d3626ac30a87e6f7a6428233b3c68299976865fa5508e4267c5415c76af7a772"},
		{"键序 b,a", `{"b":1,"a":2}`, "d3626ac30a87e6f7a6428233b3c68299976865fa5508e4267c5415c76af7a772"},
		// sha256(`"你好"`) 与 sha256(`{"emoji":"😀","s":"你好"}`)
		{"中文字符串", `"你好"`, "0922515335495b677fa44d53bcf0f33d9fbfc8fda4aba447afa805f0f206a8b9"},
		{"中文与 emoji 对象", `{"emoji":"😀","s":"你好"}`, "7b4ffe7d54ef87c60757eaa8333069db731ac880a4a59f96f70ef37ca3eb42aa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustHash(t, tc.in)
			if got != tc.want {
				t.Fatalf("Hash(%q) = %s，期望 %s", tc.in, got, tc.want)
			}
			// 小写十六进制、64 个字符。
			if len(got) != 64 || strings.ToLower(got) != got {
				t.Fatalf("hash 应为 64 位小写十六进制：%q", got)
			}
			if strings.ContainsFunc(got, func(r rune) bool {
				return !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f')
			}) {
				t.Fatalf("hash 含非十六进制字符：%q", got)
			}
			// Hash == sha256(Canonicalize(raw)) 的定义自洽性。
			canon, err := Canonicalize([]byte(tc.in))
			if err != nil {
				t.Fatalf("Canonicalize(%q) 出错：%v", tc.in, err)
			}
			sum := sha256.Sum256(canon)
			if got != hex.EncodeToString(sum[:]) {
				t.Fatalf("Hash 不等于 sha256(Canonicalize)：%s vs %s", got, hex.EncodeToString(sum[:]))
			}
		})
	}

	t.Run("store 幂等场景：meta 键序不同", func(t *testing.T) {
		h1 := mustHash(t, `{"priority":3,"note":"睡一会","tags":["a","b"]}`)
		h2 := mustHash(t, `{"tags":["a","b"],"note":"睡一会","priority":3}`)
		if h1 != h2 {
			t.Fatalf("语义相同的 meta 应得到同一 hash：%s vs %s", h1, h2)
		}
	})

	t.Run("非法输入返回错误与空 hash", func(t *testing.T) {
		h, err := Hash([]byte(`{"a":1,"a":2}`))
		if err == nil {
			t.Fatal("重复键应当让 Hash 报错")
		}
		if h != "" {
			t.Fatalf("出错时 hash 应为空，得到 %q", h)
		}
	})
}

// TestCanonicalizeIdempotent 用一组混合语料验证「输出是合法 JSON、二次规范化
// 不变、hash 稳定」。
func TestCanonicalizeIdempotent(t *testing.T) {
	corpus := []string{
		`null`,
		`true`,
		`false`,
		`0`,
		`-0`,
		`"x"`,
		`[]`,
		`{}`,
		`{"b":[1,2,{"d":4,"c":3}],"a":"中文😀"}`,
		`{ "a" : [ 1.0 , 2e-3 , -0 ] , "b" : { "z" : null } }`,
		`{"\uE000":1,"\uD83D\uDE00":2}`,
		`["\u0000\u001f","a/b","<&>","\u2028"]`,
		`{"del":"\u007f","sep":"\u2028\u2029"}`,
		`[{"b":1,"a":2},{"d":3,"c":4}]`,
	}
	for _, raw := range corpus {
		first := canonical(t, raw)
		if !json.Valid([]byte(first)) {
			t.Fatalf("Canonicalize(%q) 输出不是合法 JSON：%q", raw, first)
		}
		if second := canonical(t, first); second != first {
			t.Fatalf("二次规范化结果不同：%q → %q", first, second)
		}
		if h1, h2 := mustHash(t, raw), mustHash(t, first); h1 != h2 {
			t.Fatalf("hash 不稳定：%s vs %s", h1, h2)
		}
	}
}

// TestNestingLimit 验证深层嵌套可用、超限报错而不是栈溢出。
func TestNestingLimit(t *testing.T) {
	nest := func(n int) string {
		return strings.Repeat("[", n) + "0" + strings.Repeat("]", n)
	}
	if got := canonical(t, nest(500)); got != nest(500) {
		t.Fatalf("500 层嵌套应可正常规范化")
	}
	// 与 encoding/json 相同的上限：maxDepth 层可用。
	if got := canonical(t, nest(maxDepth)); got != nest(maxDepth) {
		t.Fatalf("%d 层嵌套应可正常规范化", maxDepth)
	}
	// 超过上限必须报错（而不是把递归打成栈溢出）。
	if _, err := Canonicalize([]byte(nest(maxDepth + 1))); err == nil {
		t.Fatalf("%d 层嵌套应报错", maxDepth+1)
	} else if !strings.Contains(err.Error(), "嵌套") {
		t.Fatalf("嵌套超限的错误信息应说明问题：%v", err)
	}
}
