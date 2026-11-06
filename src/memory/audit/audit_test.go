package audit

// 这组测试只喂**造的**样本值，形状同类、值不对应任何真实凭据。
// 第二把尺最重要的是"它真的会亮"，所以校准用的正例与防误伤的反例都要在场。

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// 独立性的机器检查：这条测试是包注释里那句硬约束的看守。
// 谁把 redact / shared / repair 接回来，这里先变红——共享谓词的第二把尺不是第二把尺。
func TestProductionCodeStaysIndependent(t *testing.T) {
	fset := token.NewFileSet()
	dirs, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var imports []string
	for _, name := range dirs {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", name, err)
		}
		for _, spec := range file.Imports {
			imports = append(imports, strings.Trim(spec.Path.Value, `"`))
		}
	}
	for _, imp := range imports {
		for _, forbidden := range []string{"/memory/redact", "/memory/shared", "/memory/repair"} {
			if strings.HasSuffix(imp, forbidden) {
				t.Errorf("audit 的生产代码 import 了 %s：两把尺合成一把，复检说了不算", imp)
			}
		}
	}
	if len(imports) == 0 {
		t.Error("一个 import 都没解析到，检查本用例的解析逻辑")
	}

	// 反过来也得成立：形状确实是手抄的第二份，不是空壳。
	src, err := os.ReadFile("audit.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "令牌|token|secret|password") {
		t.Error("audit 里没有自己那份密钥上下文正则，独立性无从谈起")
	}
}

func TestProbesFlagEachShape(t *testing.T) {
	known := []string{"a1b2c3d4e5f6g7"}
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"已知凭据·原文", `把 NapCat 令牌 a1b2c3d4e5f6g7 写成默认`, []string{ProbeKnown, ProbeContext}},
		{"上下文但值未知", "api_key = zz112233445566", []string{ProbeContext}},
		{"高熵串", "轮转后 abCdEf12345GhIjKlMnOp", []string{ProbeEntropy}},
		{"长数字·10 位账号", "联系 9876543210 处理", []string{ProbeLongDigit}},
		{"长数字·手机号形状", "日志见 13812345678", []string{ProbeLongDigit}},
		{"正常提炼物", "shell 的退出码与 stdout 一起送达，不必再查", nil},
		{"掩码后的形状", "默认地址 [链接]，令牌 [已隐去]，路径 [路径]", nil},
		{"关键词后面不是值", "password 字段留空即可", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := probes(c.text, known)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("probes(%q) = %v，期望 %v", c.text, got, c.want)
			}
		})
	}
}

// 尺子的边界也要有用例盯着：够不着的类别不能被当成"已经覆盖了"。
// 这一条记的是**没覆盖**，将来扩了就要把期望一起改过来，别只是删掉。
func TestProbesBlindSpots(t *testing.T) {
	for _, c := range []struct{ name, text string }{
		{"裸账号短于 10 位", "联系 987654321 处理"},
		{"无前缀无关键词的令牌", "默认值是 Q7tZk3Xp9Lm2Wn"},
	} {
		if got := probes(c.text, nil); len(got) > 0 {
			t.Errorf("%s：本不该命中，实际 = %v（扩了这一路就得同步 audit 的包注释）", c.name, got)
		}
	}
}

// 熵这一路只认真正像随机串的：16 位以上、字符分布散。
// 全同字符（熵 0）与中文句子都不该过阈，否则每份正常经验都冒红。
func TestEntropyGateRejectsLowEntropyRuns(t *testing.T) {
	for _, s := range []string{"aaaaaaaaaaaaaaaa", "1234123412341234", strings.Repeat("ab", 12)} {
		if e := entropyOf(s); e >= minEntropyBits {
			t.Errorf("entropyOf(%q) = %.2f，这种重复串不该过阈", s, e)
		}
	}
	for _, s := range []string{"a1B2c3D4e5F6g7H8", "Q7tZk3Xp9Lm2Wn8R"} {
		if e := entropyOf(s); e < minEntropyBits {
			t.Errorf("entropyOf(%q) = %.2f，随机凭据该过阈", s, e)
		}
	}
	if hasHighEntropy("排查连接问题，默认地址改成本机端口") {
		t.Error("中文提炼物不该被高熵探针命中")
	}
	// 16 位是下限：短一段就放过去，是这一路的取舍，写死在这里免得日后被"顺手放宽"。
	if hasHighEntropy("Q7tZk3Xp9Lm2Wn8") {
		t.Error("15 位串按设计不该进高熵探针")
	}
}

// 没有已知凭据文件时，第一路探针必须整体闭嘴——不然它是个凭空猜的误报器。
func TestKnownProbeSilentWithoutKnownValues(t *testing.T) {
	if got := probes("令牌 a1b2c3d4e5f6g7", nil); strings.Join(got, ",") == ProbeKnown || len(got) > 1 {
		t.Errorf("known 为空时不该报已知凭据，实际 = %v", got)
	}
}

// 逐段摘要这一路在"值本身就是一段连续字母数字"时与子串包含重合，
// 用例把这条重合钉住：它多出来的只是"位置定位方式与当初发现泄露的手段一致"。
func TestKnownDigestPathAgreesWithSubstring(t *testing.T) {
	known := []string{"a1b2c3d4e5f6g7"}
	for _, text := range []string{"a1b2c3d4e5f6g7", "前缀a1b2c3d4e5f6g7后缀", "x.a1b2c3d4e5f6g7"} {
		if !hitsKnown(text, known) {
			t.Errorf("hitsKnown(%q) 应命中", text)
		}
	}
	if hitsKnown("a1b2c3d4e5f6g8", known) {
		t.Error("差一位的相近值不该命中")
	}
}

func TestKnownValuesExtractsLeavesRecursively(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "qq-secret.json")
	raw := `{
	  "napcat": {"token": "a1b2c3d4e5f6g7", "ws": "ws://127.0.0.1:3000"},
	  "admin": ["9876543210", {"qq": "1234567890"}],
	  "enable": true, "short": "abc", "blank": "   "
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := KnownValues(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1234567890", "9876543210", "a1b2c3d4e5f6g7", "ws://127.0.0.1:3000"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("KnownValues = %v，期望 %v（短的 / 布尔 / 空白要滤掉，其余递归取全）", got, want)
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("输出没排序：%v", got)
	}
}

func TestKnownValuesReportsBadInput(t *testing.T) {
	dir := t.TempDir()
	if _, err := KnownValues(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("文件不存在要报错，不能当成\"没有已知凭据\"")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := KnownValues(bad); err == nil {
		t.Error("非法 JSON 要报错")
	} else if !strings.Contains(err.Error(), bad) {
		t.Errorf("错误信息要指明是哪个文件：%v", err)
	}
}

// Scope() 是本包唯一的"扫哪里"定义：漏一张表就等于整库复检说了不算。
func TestScopeCoversAllFiveTables(t *testing.T) {
	want := map[string]int{
		"memories": 2, "mem_entries": 1, "mem_nodes": 1,
		"shared_sessions": 1, "shared_tasks": 4,
	}
	got := map[string]int{}
	for _, c := range Scope() {
		got[c.Table]++
		if c.Name == "" {
			t.Errorf("%s 有一列名为空", c.Table)
		}
	}
	for table, n := range want {
		if got[table] != n {
			t.Errorf("Scope 里 %s 有 %d 列，期望 %d 列", table, got[table], n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("Scope 出现了计划外的表：%v", got)
	}
}

func TestFindingStringCarriesOnlyLocation(t *testing.T) {
	f := Finding{Table: "memories", RowKey: 5, Column: "content", Probe: ProbeKnown}
	if got := f.String(); got != "memories.5.content(已知凭据)" {
		t.Errorf("Finding.String = %q", got)
	}
}
