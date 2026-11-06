package audit

// 这组测试只喂**造的**样本值，形状同类、值不对应任何真实凭据。
// 第二把尺最重要的是"它真的会亮"，所以校准用的正例与防误伤的反例都要在场。

import (
	"database/sql"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
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
	// 阈值下界要有"差一点就过"的反例：经验里最常见的过阈误伤源就是驼峰 / 蛇形标识符，
	// 实测 19 位的 WorkflowExpandPanel 形状只有 3.83。没有这一组，把 3.9 改成 3.7 不会有任何测试变红。
	for _, s := range []string{"WorkflowExpandPanel", "workflowExpandPanel", "conversation_list_messages"} {
		if e := entropyOf(s); e >= minEntropyBits {
			t.Errorf("entropyOf(%q) = %.4f，标识符形状不该过阈", s, e)
		}
		if hasHighEntropy("详见 " + s + " 一节") {
			t.Errorf("%q 这种标识符不该被高熵探针命中", s)
		}
	}
	// 记代价，不粉饰：长蛇形英文短语确实过阈（实测 4.16），这一路躲不开它。
	// 真要加值判别器把它压下去，这条得一起改——别让它悄悄变成"已经覆盖了"。
	if !hasHighEntropy("the_quickest_brown_fox_jumps") {
		t.Errorf("这条钉的是已知误报面：它现在会过阈，探针没改就别让它变绿")
	}
	// 16 位是下限：短一段就放过去，是这一路的取舍，写死在这里免得日后被"顺手放宽"。
	if hasHighEntropy("Q7tZk3Xp9Lm2Wn8") {
		t.Error("15 位串按设计不该进高熵探针")
	}
}

// 没有已知凭据文件时，第一路探针必须整体闭嘴——不然它是个凭空猜的误报器。
func TestKnownProbeSilentWithoutKnownValues(t *testing.T) {
	text := "令牌 a1b2c3d4e5f6g7"
	// 期望写成一整串，不是"不含某项"：旧写法 `== ProbeKnown || len(got) > 1` 的前半截
	// 在 known 为空时恒不成立（probes 第一行就短路了），那条断言等于没断。
	if got := probes(text, nil); strings.Join(got, ",") != ProbeContext {
		t.Errorf("known 为空时只该由上下文这一路认出，实际 = %v", got)
	}
	// 同一条文本给了真值必须多出一路——没有这个对照，上面的"闭嘴"证明不了任何事。
	if got := probes(text, []string{"a1b2c3d4e5f6g7"}); !strings.Contains(strings.Join(got, ","), ProbeKnown) {
		t.Errorf("给了真值就该报已知凭据，实际 = %v", got)
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
	  "dup": "a1b2c3d4e5f6g7",
	  "version": 1, "enable": true, "gone": null, "short": "abc", "blank": "   "
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, skipped, err := KnownValues(path)
	if err != nil {
		t.Fatal(err)
	}
	// dup 与 napcat.token 是同一个值：写两遍不许把"取到 N 个"撑虚。
	want := []string{"1234567890", "9876543210", "a1b2c3d4e5f6g7", "ws://127.0.0.1:3000"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("KnownValues = %v，期望 %v（短的 / 布尔 / 空白要滤掉，重复的要并掉，其余递归取全）", got, want)
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("输出没排序：%v", got)
	}
	// version/enable/gone 三个非字符串叶子必须被数出来：真凭据要写成 1234567890 这种
	// 不带引号的数，第一路探针就看不见它，静默丢掉等于假装看过了。
	if skipped != 3 {
		t.Errorf("非字符串叶子跳过数 = %d，期望 3（数字 / 布尔 / null 各一）", skipped)
	}
}

func TestKnownValuesReportsBadInput(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := KnownValues(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("文件不存在要报错，不能当成\"没有已知凭据\"")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := KnownValues(bad); err == nil {
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
	// 格式精确等值只在这里断（别处只断"含 key"这种弱形式），改 String() 前跑一次
	// 本用例自然变红，改完带新格式转绿——这是 String() 作为唯一序列化出口的看守。
	//
	// 期望串里的 key 是**实测金值**（2026-11-07 从现行库 -no-adjudicated 输出抄下）：
	// 它同时钉住规范编码与取整时机。谁改了 canonicalShape / roundEnt，键会全体漂移
	// 而闸门依旧自洽（白名单是从同一处抄的），只有这条会红——这就是"漂了没测试会红"
	// 那个洞的补法。
	entropy := Finding{
		Table: "memories", RowKey: 9, Column: "content", Probe: ProbeEntropy,
		Shape: Shape{Runs: 1, Lens: []int{17}, Ent: []float64{3.97}},
	}
	if got := entropy.String(); got != "memories.9.content(高熵串 runs=1 lens=[17] ent=[3.97] key=c33ed7da)" {
		t.Errorf("Finding.String = %q", got)
	}
	// 已知凭据那一路：lens 里是凭据的**长度**，这是有意的形状元数据（见 Shape 注释），
	// 与熵那一路同质，复原不出任何原值；ent 留空写 []，格式不留两种写法。
	known := Finding{
		Table: "memories", RowKey: 5, Column: "content", Probe: ProbeKnown,
		Shape: Shape{Runs: 1, Lens: []int{19}},
	}
	if got := known.String(); got != "memories.5.content(已知凭据 runs=1 lens=[19] ent=[] key="+known.Key()+")" {
		t.Errorf("Finding.String = %q", got)
	}
	// 两条都不许带出正文：这里是唯一序列化出口，shared 包那条"不含原值"的断言走的就是它。
	for _, f := range []Finding{entropy, known} {
		s := f.String()
		for _, banned := range []string{"a1b2c3d4", "Q7tZk", "令牌", "NapCat"} {
			if strings.Contains(s, banned) {
				t.Errorf("Finding.String 里出现了原值痕迹 %q：%s", banned, s)
			}
		}
	}
}

// 键必须随形状动：白名单钉的是"这一格长这个样子"，不是"这一格有人看过"。
// 这四条各挡一种退化——只记位置、只记首段、只记长度不看熵、编码不稳定。
func TestFindingKeyCarriesShape(t *testing.T) {
	base := Finding{
		Table: "memories", RowKey: 9, Column: "content", Probe: ProbeEntropy,
		Shape: Shape{Runs: 1, Lens: []int{17}, Ent: []float64{3.97}},
	}
	same := base
	same.Shape = Shape{Runs: 1, Lens: []int{17}, Ent: []float64{3.97}}
	if base.Key() != same.Key() {
		t.Errorf("同位置同形状算出两个键：%s vs %s（键不稳，白名单没法抄）", base.Key(), same.Key())
	}
	if !strings.HasSuffix(base.String(), "key="+base.Key()+")") {
		t.Errorf("String() 末尾不是 key=%s，人没法从输出抄键：%s", base.Key(), base.String())
	}
	// 同格后面新增一段：整格聚合变了，键必须变（只记首段的话这里就静默通过了）。
	grown := base
	grown.Shape = Shape{Runs: 2, Lens: []int{17, 21}, Ent: []float64{3.97, 4.02}}
	if grown.Key() == base.Key() {
		t.Error("同格新增一段过阈形状没改键：旧裁定会把它静默吸收")
	}
	// 长度不变、熵变了也要改键：形状不是只有长度一个维度。
	shifted := base
	shifted.Shape = Shape{Runs: 1, Lens: []int{17}, Ent: []float64{4.10}}
	if shifted.Key() == base.Key() {
		t.Error("熵变了键没变：canonicalShape 没把 Ent 编进去")
	}
	// 换行 / 换探针都不能撞键。
	elsewhere := base
	elsewhere.RowKey = 10
	otherProbe := base
	otherProbe.Probe = ProbeContext
	if elsewhere.Key() == base.Key() || otherProbe.Key() == base.Key() {
		t.Error("换行或换探针没改键：位置没进键")
	}
	// 空形状（只被摘要那一路认出的已知凭据）也要有确定的键，不是空串也不是 panic。
	empty := Finding{Table: "memories", RowKey: 5, Column: "content", Probe: ProbeKnown}
	if len(empty.Key()) != 8 {
		t.Errorf("空形状的键应该是 8 位摘要，实际 %q", empty.Key())
	}
	for _, k := range []string{base.Key(), empty.Key()} {
		if !isHexLower(k) {
			t.Errorf("键 %q 不是 8 位小写十六进制", k)
		}
	}
}

func isHexLower(s string) bool {
	if len(s) != 8 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

// 形状列要对着读：Lens 与 Ent 必须成对排序，只排一列会让人读出错（键不受影响）。
func TestShapeSortsLensAndEntTogether(t *testing.T) {
	got := shapeOf([]runStat{{Len: 21, Ent: 4.02}, {Len: 17, Ent: 3.97}, {Len: 17, Ent: 3.5}}, true)
	want := Shape{Runs: 3, Lens: []int{17, 17, 21}, Ent: []float64{3.5, 3.97, 4.02}}
	if shapeText(got) != shapeText(want) {
		t.Errorf("shapeOf 成对排序 = %s，期望 %s", shapeText(got), shapeText(want))
	}
	// 非熵探针不许造一串 0 占位：那会让同一个形状编出两种键。
	noEnt := shapeOf([]runStat{{Len: 19}, {Len: 8}}, false)
	if noEnt.Ent != nil {
		t.Errorf("非熵探针的 Ent 应该留空，实际 %v", noEnt.Ent)
	}
}

// shapeText 是测试里比对形状用的可读形式。生产侧唯一的序列化出口仍然只有
// Finding.String()——这里不给 Shape 挂方法，免得冒出第二个出口。
func shapeText(s Shape) string {
	return fmt.Sprintf("runs=%d lens=%v ent=%v", s.Runs, s.Lens, s.Ent)
}

// 四路探针各自的形状都要能报出来，且谓词命中与否与拆函数之前一致。
//
// 每条用例自带 known：已知凭据那一路的形状取决于"给了几个**不同**的值"，
// 全部用例共用一份 known 就表达不出"同一个值在清单里写了两遍"这种输入——
// 而那正是 knownRuns 里 `seen[k]` 去重唯一在干活的地方。
func TestProbeShapesCarryEachKind(t *testing.T) {
	// 两个明确虚构的假值，形状同类、不对应任何真实凭据。
	const (
		fakeA = "a1b2c3d4e5f6g7"   // 14 位
		fakeB = "zz9876543210abcd" // 16 位，与 A 不同值也不同长度
	)
	cases := []struct {
		name  string
		known []string
		text  string
		probe string
		want  Shape
	}{
		{"已知凭据按不同值计", []string{fakeA}, "令牌 " + fakeA + " 与 " + fakeA + " 出现两次", ProbeKnown,
			Shape{Runs: 1, Lens: []int{14}}},
		// 同一个值在清单里写两遍：Runs 仍是 1。删掉 knownRuns 的 `seen[k] ||` 去重，
		// 这里就会报 Runs=2、Lens=[14,14]，这条先红。
		{"同一值在清单里重复只算一次", []string{fakeA, fakeA}, "令牌 " + fakeA, ProbeKnown,
			Shape{Runs: 1, Lens: []int{14}}},
		// 两个不同的值都在同一格里：Runs=2，长度按 (长度, 熵) 成对升序（这一路没有熵）。
		{"两个不同值各计一次", []string{fakeA, fakeB}, "令牌 " + fakeB + " 备选 " + fakeA, ProbeKnown,
			Shape{Runs: 2, Lens: []int{14, 16}}},
		{"密钥上下文按段计", []string{fakeA}, "api_key = zz112233445566", ProbeContext, Shape{Runs: 1, Lens: []int{24}}},
		{"高熵串带熵", []string{fakeA}, "轮转后 abCdEf12345GhIjKlMnOp", ProbeEntropy, Shape{Runs: 1, Lens: []int{21}, Ent: []float64{4.39}}},
		{"长数字按段计", []string{fakeA}, "联系 9876543210 和 13812345678", ProbeLongDigit, Shape{Runs: 2, Lens: []int{10, 11}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hits := probeShapes(c.text, c.known)
			var got Shape
			var seen bool
			for _, h := range hits {
				if h.Probe == c.probe {
					got, seen = h.Shape, true
				}
			}
			if !seen {
				t.Fatalf("没报出 %s：%+v", c.probe, hits)
			}
			if shapeText(got) != shapeText(c.want) {
				t.Errorf("%s 的形状 = %s，期望 %s", c.probe, shapeText(got), shapeText(c.want))
			}
			// 谓词与形状的口径要一致：报出这一路时段数不能是 0（已知凭据的摘要那一路除外，
			// 那一路不携带长度信息，见 knownRuns 的注释）。
			if got.Runs == 0 {
				t.Errorf("%s 命中但 Runs=0", c.probe)
			}
			// 非熵那一路不许造出一串 0 占位：那会让同一个形状编出两种键。
			if c.probe != ProbeEntropy && got.Ent != nil {
				t.Errorf("%s 不该带熵，实际 %v", c.probe, got.Ent)
			}
		})
	}
}

// ---- 已裁定白名单闸门 ----

// adjudicatedFrom 把一条命中登记成裁定条目。测试里这么造是合法的（键由 Key() 算出，
// 代表"人抄对了"）；生产代码反过来——白名单里的 key 只能是从 String() 输出抄来的字符串，
// 不参与任何重算，见 Adjudicated 的注释。
func adjudicatedFrom(f Finding, reason string) Adjudicated {
	return Adjudicated{
		Table: f.Table, RowKey: f.RowKey, Column: f.Column, Probe: f.Probe,
		Key: f.Key(), Runs: f.Shape.Runs, Lens: f.Shape.Lens, Ent: f.Shape.Ent, Reason: reason,
	}
}

func rowExistsAlways(string, int64) bool { return true }
func rowExistsNever(string, int64) bool  { return false }

// ① 新增破闸：白名单一条都没有 / 有但键不同，命中都必须进 novel。
func TestFilterBreaksGateOnUnadjudicatedHit(t *testing.T) {
	f := Finding{
		Table: "memories", RowKey: 9, Column: "content", Probe: ProbeEntropy,
		Shape: Shape{Runs: 1, Lens: []int{17}, Ent: []float64{3.97}},
	}
	novel, stale, skipped, conflicted := Filter([]Finding{f}, nil, rowExistsAlways)
	// 数量与内容分两步断：合在一条 `len(novel) != 1 || novel[0]...` 里，
	// 长度不对时只 t.Errorf，下一句取 novel[0] 就当场 panic，异变被杀死时给出的是
	// 索引越界的栈而不是"哪一类没进对"的可读诊断。
	if len(novel) != 1 {
		t.Fatalf("没有白名单时命中该全部进 novel，实际 %d 条：%v", len(novel), novel)
	}
	if novel[0].Key() != f.Key() {
		t.Errorf("novel 里那条不是这个命中：key=%s，期望 %s", novel[0].Key(), f.Key())
	}
	if len(stale) != 0 || len(skipped) != 0 {
		t.Errorf("空白名单不该有 stale / skipped：%v / %v", stale, skipped)
	}
	if len(conflicted) != 0 {
		t.Errorf("空白名单不该有位置冲突：%v", conflicted)
	}

	// 同格新增一段形状：键变了，新命中进 novel，旧条目同时成为 stale（行还在）。
	grown := f
	grown.Shape = Shape{Runs: 2, Lens: []int{17, 21}, Ent: []float64{3.97, 4.02}}
	adj := []Adjudicated{adjudicatedFrom(f, "标识符，保留")}
	novel, stale, skipped, conflicted = Filter([]Finding{grown}, adj, rowExistsAlways)
	if len(novel) != 1 || novel[0].Key() != grown.Key() {
		t.Errorf("同格新增过阈段没破闸：novel = %v", novel)
	}
	if len(stale) != 1 || stale[0].Key != f.Key() {
		t.Errorf("旧裁定该行成为 stale：%v", stale)
	}
	if len(skipped) != 0 {
		t.Errorf("行还在库里，不该归 skipped：%v", skipped)
	}
	if len(conflicted) != 0 {
		t.Errorf("键没对上不该算位置冲突：%v", conflicted)
	}
}

// ② 已裁定放行：键相等既不进 novel，也不产生 stale / skipped。
func TestFilterLetsAdjudicatedHitThrough(t *testing.T) {
	f := Finding{
		Table: "mem_entries", RowKey: 104, Column: "content", Probe: ProbeEntropy,
		Shape: Shape{Runs: 1, Lens: []int{33}, Ent: []float64{4.16}},
	}
	adj := []Adjudicated{adjudicatedFrom(f, "标识符，保留")}
	novel, stale, skipped, conflicted := Filter([]Finding{f}, adj, rowExistsAlways)
	if len(novel) != 0 {
		t.Errorf("已裁定的键不该进 novel：%v", novel)
	}
	if len(stale) != 0 {
		t.Errorf("键对上了不该判过期：%v", stale)
	}
	if len(skipped) != 0 {
		t.Errorf("行存在且对上了不该归 skipped：%v", skipped)
	}
	if len(conflicted) != 0 {
		t.Errorf("位置也对上了不该算冲突（放行就是放行）：%v", conflicted)
	}
}

// ③ 过期判别：行在库里但键不匹配 → stale；行不在库里 → skipped 且不报错。
func TestFilterSeparatesStaleFromSkipped(t *testing.T) {
	gone := Finding{Table: "memories", RowKey: 9, Column: "content", Probe: ProbeEntropy,
		Shape: Shape{Runs: 1, Lens: []int{17}, Ent: []float64{3.97}}}
	// 白名单里两条，当前库一条命中都没有（外来库 / 已洗完的库）：
	// rowExists 决定它们过期还是跳过，两者都不该把 novel 撑起来。
	adj := []Adjudicated{adjudicatedFrom(gone, "标识符，保留")}
	novel, stale, skipped, conflicted := Filter(nil, adj, rowExistsAlways)
	if len(novel) != 0 {
		t.Errorf("没有命中就不该有 novel：%v", novel)
	}
	if len(stale) != 1 || len(skipped) != 0 {
		t.Errorf("行在库里但无对应命中，该判 stale：stale=%v skipped=%v", stale, skipped)
	}
	if len(conflicted) != 0 {
		t.Errorf("没有命中就不该有位置冲突：%v", conflicted)
	}
	if _, stale, skipped, _ = Filter(nil, adj, rowExistsNever); len(stale) != 0 || len(skipped) != 1 {
		t.Errorf("行不在库里，该归 skipped 而不是报错：stale=%v skipped=%v", stale, skipped)
	}
}

// ④ 位置冲突（conflicted）：键是同一条，但裁定记的格子与实测命中不是同一格。
// 四种位置字段各篡改一种都要检出，且**不放行、不重复计 novel**。
func TestFilterDetectsPositionConflict(t *testing.T) {
	hit := Finding{
		Table: "memories", RowKey: 9, Column: "content", Probe: ProbeEntropy,
		Shape: Shape{Runs: 1, Lens: []int{17}, Ent: []float64{3.97}},
	}
	cases := []struct {
		name  string
		tweak func(a *Adjudicated)
	}{
		{"表不同", func(a *Adjudicated) { a.Table = "mem_entries" }},
		{"行不同", func(a *Adjudicated) { a.RowKey = 999999 }},
		{"列不同", func(a *Adjudicated) { a.Column = "kind" }},
		{"探针不同", func(a *Adjudicated) { a.Probe = ProbeLongDigit }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entry := adjudicatedFrom(hit, "位置被改过的一条裁定")
			c.tweak(&entry)
			adj := []Adjudicated{entry}
			novel, stale, skipped, conflicted := Filter([]Finding{hit}, adj, rowExistsAlways)
			if len(conflicted) != 1 {
				t.Fatalf("篡改 %s 后该进 conflicted，实际 %v（novel=%v）", c.name, conflicted, novel)
			}
			if conflicted[0].Key() != hit.Key() {
				t.Errorf("冲突条目不是这条命中：%v", conflicted)
			}
			if len(novel) != 0 {
				t.Errorf("键已认下的命中不该重复计 novel：%v", novel)
			}
			if len(stale) != 0 || len(skipped) != 0 {
				t.Errorf("键匹配上的裁定就该被消耗，不该再落 stale / skipped：stale=%v skipped=%v", stale, skipped)
			}
		})
	}
	// 行不在库里也得照样消耗：位置冲突是"清单本身错位"，与那一行在不在无关。
	// 少了这一遍，实现把"行不存在"优先判成 skipped 时，闸门会把真冲突吞掉。
	entry := adjudicatedFrom(hit, "位置被改过的一条裁定")
	entry.RowKey = 999999
	novel, stale, skipped, conflicted := Filter([]Finding{hit}, []Adjudicated{entry}, rowExistsNever)
	if len(conflicted) != 1 {
		t.Fatalf("行不存在时同样该报位置冲突，实际 conflicted=%v", conflicted)
	}
	if len(stale) != 0 || len(skipped) != 0 || len(novel) != 0 {
		t.Errorf("裁定被消耗后三类都该是 0：novel=%v stale=%v skipped=%v", novel, stale, skipped)
	}
}

// 四类同时出现时互不吞没：一条放行、一条冲突、一条未裁定、一条过期、一条跳过。
func TestFilterKeepsAllFourCategories(t *testing.T) {
	ok := Finding{Table: "memories", RowKey: 9, Column: "content", Probe: ProbeEntropy,
		Shape: Shape{Runs: 1, Lens: []int{17}, Ent: []float64{3.97}}}
	clash := Finding{Table: "mem_entries", RowKey: 104, Column: "content", Probe: ProbeEntropy,
		Shape: Shape{Runs: 1, Lens: []int{33}, Ent: []float64{4.16}}}
	fresh := Finding{Table: "shared_tasks", RowKey: 3, Column: "goal", Probe: ProbeLongDigit,
		Shape: Shape{Runs: 1, Lens: []int{10}}}

	liveOnly := map[string]int64{"memories": 9, "mem_entries": 104, "shared_tasks": 3, "mem_nodes": 5}
	exists := func(table string, rowKey int64) bool { return liveOnly[table] == rowKey }

	adj := []Adjudicated{
		adjudicatedFrom(ok, "合法裁定，位置对得上"),
		func() Adjudicated { // 同键但行号被改 → conflicted，且这条被消耗，不再算 stale / skipped
			a := adjudicatedFrom(clash, "行号被改过")
			a.RowKey = 888888
			return a
		}(),
		func() Adjudicated { // 行还在库里、当前没这条命中 → stale
			a := adjudicatedFrom(Finding{Table: "mem_nodes", RowKey: 5, Column: "anchor", Probe: ProbeContext,
				Shape: Shape{Runs: 1, Lens: []int{14}}}, "已洗完的一格")
			a.Key = "aaaa0001"
			return a
		}(),
		func() Adjudicated { // 行不在库里 → skipped
			a := adjudicatedFrom(Finding{Table: "shared_sessions", RowKey: 77, Column: "title", Probe: ProbeLongDigit,
				Shape: Shape{Runs: 1, Lens: []int{10}}}, "被删掉的会话")
			a.Key = "bbbb0002"
			return a
		}(),
	}
	novel, stale, skipped, conflicted := Filter([]Finding{ok, clash, fresh}, adj, exists)
	if len(novel) != 1 || novel[0].Key() != fresh.Key() {
		t.Fatalf("未裁定的那条该独自进 novel，实际 %v", novel)
	}
	if len(conflicted) != 1 || conflicted[0].Key() != clash.Key() {
		t.Errorf("位置冲突的那条该进 conflicted，实际 %v", conflicted)
	}
	if len(stale) != 1 || stale[0].Key != "aaaa0001" {
		t.Errorf("行在而无对应命中该进 stale，实际 %v", stale)
	}
	if len(skipped) != 1 || skipped[0].Key != "bbbb0002" {
		t.Errorf("行不在该进 skipped，实际 %v", skipped)
	}
}

// N3：表都不存在时 rowExists 必须返回 false，不能把 no such table 冒上来——
// 闸门崩在错误路径上会吞掉同一次运行里的 novel。
func TestRowSetMissingTableIsFalseNotError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// 库里只建 mem_entries，memories 这张表压根不存在（外来库 / 空库的形状）。
	if _, err := db.Exec(`CREATE TABLE mem_entries (id text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO mem_entries VALUES ('e-1')`); err != nil {
		t.Fatal(err)
	}
	adj := []Adjudicated{
		{Table: "memories", RowKey: 9, Column: "content", Probe: ProbeEntropy, Key: "deadbeef", Reason: "表不存在"},
		{Table: "mem_entries", RowKey: 1, Column: "content", Probe: ProbeEntropy, Key: "cafebabe", Reason: "rowid 1 在库里"},
		{Table: "mem_entries", RowKey: 999, Column: "content", Probe: ProbeEntropy, Key: "00000000", Reason: "rowid 不在库里"},
	}
	set, err := NewRowSet(db, adj)
	if err != nil {
		t.Fatalf("表不存在应该归 false 而不是报错：%v", err)
	}
	if set.Has("memories", 9) {
		t.Error("表不存在时 Has 应该返回 false")
	}
	if !set.Has("mem_entries", 1) {
		t.Error("rowid 1 在库里，Has 应该返回 true（WHERE 打偏 / rowid 查法错了？）")
	}
	if set.Has("mem_entries", 999) {
		t.Error("rowid 999 不在库里，Has 应该返回 false")
	}
	// 空白名单不查库：外来库上跑闸门时不该因为清单里的表不存在而卡住。
	if set, err := NewRowSet(db, nil); err != nil || set.Has("memories", 9) {
		t.Errorf("空白名单应该直接返回恒 false 的判定器：%v", err)
	}
}

// 行键列必须由清单给，不能写死 rowid：这张测试表是 WITHOUT ROWID + id INTEGER PRIMARY KEY，
// 没有可依赖的隐式 rowid。实现若退回写死，这里要么报错、要么把真在库里的行答成"不存在"，
// 而"不存在"会让白名单条目错误归进 skipped，把该报的位置冲突吞掉。
func TestRowSetUsesScopeKeyColumn(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE mem_nodes (id INTEGER PRIMARY KEY, anchor TEXT) WITHOUT ROWID`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO mem_nodes (id, anchor) VALUES (7, '一条干净的锚点'), (9, '另一条')`); err != nil {
		t.Fatal(err)
	}

	// 注入一份临时清单：同一张表的三个行键，一次 IN 查询取回。
	cols := []Column{{Table: "mem_nodes", Name: "anchor", RowKey: "id"}}
	adj := []Adjudicated{
		{Table: "mem_nodes", RowKey: 7, Column: "anchor", Probe: ProbeContext, Key: "aaaa0007", Reason: "行在库里"},
		{Table: "mem_nodes", RowKey: 8, Column: "anchor", Probe: ProbeContext, Key: "aaaa0008", Reason: "行不在库里"},
		{Table: "mem_nodes", RowKey: 9, Column: "anchor", Probe: ProbeContext, Key: "aaaa0009", Reason: "同一次查询里的另一行"},
	}
	set, err := newRowSetColumns(db, adj, cols)
	if err != nil {
		t.Fatalf("按清单给的行键列查应当成功: %v", err)
	}
	if !set.Has("mem_nodes", 7) {
		t.Error("id=7 在库里，Has 该返回 true（实现写死 rowid 就是这条先红）")
	}
	if !set.Has("mem_nodes", 9) {
		t.Error("同一次查询里的 id=9 也该并进来（占位符拼错或结果覆盖会栽在这条）")
	}
	if set.Has("mem_nodes", 8) {
		t.Error("id=8 不在库里，Has 该返回 false")
	}

	// 清单里没有的二元组要出声报错，不能悄悄答"这行不存在"：那会把清单错写当成行被删。
	if _, err := newRowSetColumns(db, []Adjudicated{{
		Table: "mem_nodes", RowKey: 7, Column: "content", Probe: ProbeContext, Key: "aaaa0009", Reason: "列不在清单里",
	}}, cols); err == nil {
		t.Error("（表，列）不在清单里时要报错，不能静默归成 skipped")
	}
}

// ④ 内置白名单的合法性，以及每一份不合法形态都要被拒。
func TestDefaultAdjudicatedIsValid(t *testing.T) {
	adj, err := DefaultAdjudicated()
	if err != nil {
		t.Fatalf("内置白名单读不动（这份坏了闸门就是摆设）: %v", err)
	}
	if len(adj) == 0 {
		t.Fatal("内置白名单为空：TODO.md 记着的两处已裁定形状必须登记在里面")
	}
	seen := map[string]bool{}
	for _, a := range adj {
		if len(a.Key) != 8 || !isHexLower(a.Key) {
			t.Errorf("%s 的 key %q 不是 8 位小写十六进制：key 只能从 -audit 输出逐字抄", a, a.Key)
		}
		if seen[a.Key] {
			t.Errorf("key %q 重复", a.Key)
		}
		seen[a.Key] = true
		if strings.TrimSpace(a.Reason) == "" {
			t.Errorf("%s 没有 reason：%q", a, a.Reason)
		}
		if !isProbe(a.Probe) {
			t.Errorf("%s 的 probe %q 不是四路探针之一", a, a.Probe)
		}
		if !inScope(a.Table, a.Column) {
			t.Errorf("%s 指的 %s.%s 不在复检清单里", a, a.Table, a.Column)
		}
		// runs / lens / ent 是给人读的副本，但三者的自洽性要成立：
		// 一处对不上就说明这份 JSON 是手编的，不是抄的。
		if a.Runs != len(a.Lens) {
			t.Errorf("%s 的 runs=%d 与 lens=%v 段数不符", a, a.Runs, a.Lens)
		}
		if a.Probe == ProbeEntropy && len(a.Ent) != len(a.Lens) {
			t.Errorf("%s 的 ent=%v 与 lens=%v 不成对", a, a.Ent, a.Lens)
		}
		if a.Probe != ProbeEntropy && len(a.Ent) != 0 {
			t.Errorf("%s 不是熵那一路，ent 该留空，实际 %v", a, a.Ent)
		}
	}
}

func TestParseAdjudicatedRejectsBadLists(t *testing.T) {
	// 每一条都必须是"整份不认"，半认半废的白名单比没有更危险。
	cases := []struct {
		name string
		raw  string
		bad  string
	}{
		{"不是 JSON", `不是 JSON`, "解析"},
		{"缺 key", `[{"table":"memories","row_key":9,"column":"content","probe":"高熵串","reason":"r"}]`, "没有 key"},
		{"key 重复", `[{"table":"memories","row_key":9,"column":"content","probe":"高熵串","key":"aaaaaaaa","reason":"r"},
			{"table":"memories","row_key":10,"column":"content","probe":"高熵串","key":"aaaaaaaa","reason":"r"}]`, "重复"},
		{"reason 空", `[{"table":"memories","row_key":9,"column":"content","probe":"高熵串","key":"aaaaaaaa","reason":"  "}]`, "reason"},
		{"probe 不认识", `[{"table":"memories","row_key":9,"column":"content","probe":"低熵串","key":"aaaaaaaa","reason":"r"}]`, "probe"},
		{"表不在清单里", `[{"table":"secrets","row_key":9,"column":"content","probe":"高熵串","key":"aaaaaaaa","reason":"r"}]`, "不在复检清单"},
		{"列不在清单里", `[{"table":"memories","row_key":9,"column":"password","probe":"高熵串","key":"aaaaaaaa","reason":"r"}]`, "不在复检清单"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseAdjudicated([]byte(c.raw)); err == nil {
				t.Fatalf("这份白名单该被拒（%s），实际收下了", c.bad)
			} else if !strings.Contains(err.Error(), c.bad) {
				t.Errorf("错误信息没提 %q：%v", c.bad, err)
			}
		})
	}
	// 合法的一份要能过，且 -adjudicated 指到文件时错误信息带上路径（不然人不知道改哪份）。
	good := `[{"table":"memories","row_key":9,"column":"content","probe":"高熵串","key":"aaaaaaaa","runs":1,"lens":[17],"ent":[3.97],"reason":"标识符"}]`
	if _, err := ParseAdjudicated([]byte(good)); err != nil {
		t.Errorf("合法的白名单被拒了: %v", err)
	}
	dir := t.TempDir()
	broken := filepath.Join(dir, "mine.json")
	if err := os.WriteFile(broken, []byte(`{坏文件`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAdjudicated(broken); err == nil {
		t.Error("坏文件要报错")
	} else if !strings.Contains(err.Error(), broken) {
		t.Errorf("错误信息要指明是哪份白名单：%v", err)
	}
	if _, err := LoadAdjudicated(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("白名单文件不存在要报错，不能当成\"没有已裁定的条目\"")
	}
}
