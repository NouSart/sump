package main

// 守卫异变工具自己的测试。它比真跑一遍便宜得多，守的是三件会**静默产出假结论**的事：
//
//   1. 清单与源码不同步 —— old 片段找不到（或命中多次）时，脚本不会报错，
//      只是"跑了个寂寞"，报告里一片"杀死"而那条守卫早没人看着了。
//      TestRunClassifies 验分类，TestGuardsMatchSource 逐条验清单本身。
//   2. 分类与退出码 —— 存活写成杀死、过期不算存活、改完源码不还原，结论都会失真。
//      这几条用戏台替掉真 go test，不去等真编译。
//   3. 跨模块定位 —— 本仓库是 17 个模块的工作区，跑错目录 = 一条测试都没跑，
//      却仍能报出"杀死/存活"。TestTestTarget 钉住模块根与包名。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGoTest 换掉真的 go test：按包名返回剧本里的退出码。
func fakeGoTest(t *testing.T, codes map[string]int) {
	t.Helper()
	fakeGoTestOut(t, codes, nil)
}

// fakeGoTestOut 同上，还能按包名剧本化输出——分类靠的就是输出里的标记，
// 只给退出码的那半边测不出"编译失败被当成杀死"。
func fakeGoTestOut(t *testing.T, codes map[string]int, outs map[string]string) {
	t.Helper()
	orig := goTest
	goTest = func(_, pkg string) (int, string) {
		out, ok := outs[pkg]
		if !ok {
			out = "剧本输出：" + pkg
		}
		return codes[pkg], out
	}
	t.Cleanup(func() { goTest = orig })
}

// tempModule 造一个最小的"模块"：根下有 go.mod，源码在 sub/ 里。
// 必须真有 go.mod，否则 testTarget 会直接报错——那正好是本工具要在真仓库里躲开的情况。
func tempModule(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/tmp\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sub", "src.go")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunClassifies(t *testing.T) {
	const original = "func f() { return 1 }\nfunc g() { return 2 }\nfunc h() { return 1 }\n"
	path := tempModule(t, original)
	fakeGoTest(t, map[string]int{"./sub": 1})

	var buf strings.Builder
	code, stats := run([]guard{
		{name: "被杀死的", file: path, old: "return 2", new: "return 9"},
	}, runOptions{out: &buf})

	// 这条只验"杀死"：./sub 剧本返回 1（测试失败）。
	if code != 0 || stats.killed != 1 || stats.survived != 0 {
		t.Fatalf("被杀死的异变应记为杀死且退出 0：code=%d stats=%+v\n%s", code, stats, buf.String())
	}
	if !strings.Contains(buf.String(), "./sub") {
		t.Fatalf("输出里要能看到跑的是哪个包（结论必须指得回测试目标）：%s", buf.String())
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("异变跑完必须还原源码：%q", got)
	}
}

// TestRunSurvivalAndStale 存活与过期都算"没守住"，都必须进存活清单并把退出码打成 1。
func TestRunSurvivalAndStale(t *testing.T) {
	const original = "func f() { return 1 }\nfunc g() { return 2 }\nfunc h() { return 1 }\n"
	path := tempModule(t, original)
	fakeGoTest(t, map[string]int{"./sub": 0}) // 测试全过 = 异变活下来

	var buf strings.Builder
	code, stats := run([]guard{
		{name: "活下来的", file: path, old: "return 2", new: "return 7"},
		{name: "片段命中两次", file: path, old: "return 1", new: "return 8"},
		{name: "片段根本不存在", file: path, old: "return 42", new: "return 8"},
	}, runOptions{out: &buf})

	if code != 1 || stats.killed != 0 || stats.survived != 3 || len(stats.survivedNames) != 3 {
		t.Fatalf("存活与过期都要计进 survived：code=%d stats=%+v\n%s", code, stats, buf.String())
	}
	if !strings.Contains(buf.String(), "过期") {
		t.Fatalf("清单过期要单独说清楚，不能混成「跳过」：%s", buf.String())
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("过期判定不能改动源码：%q", got)
	}
}

// TestRunSkipsSlow -skip-slow 只跳过标了 slow 的条目，且跳过不算存活也不算杀死。
func TestRunSkipsSlow(t *testing.T) {
	const original = "func f() { return 1 }\n"
	path := tempModule(t, original)
	fakeGoTest(t, map[string]int{"./sub": 1})

	var buf strings.Builder
	code, stats := run([]guard{
		{name: "慢的", file: path, old: "return 1", new: "return 2", slow: true},
		{name: "快的", file: path, old: "return 1", new: "return 3"},
	}, runOptions{skipSlow: true, out: &buf})

	if code != 0 || stats.killed != 1 || stats.skipped != 1 || stats.survived != 0 {
		t.Fatalf("跳过不能计入存活：code=%d stats=%+v\n%s", code, stats, buf.String())
	}
}

// TestTargetNeedsModule 找不到 go.mod 时必须报错，不能退回"在文件所在目录跑"——
// 那种退路会让跨模块的包一条测试都没跑到，却仍然报出结论。
func TestTargetNeedsModule(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lonely.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := testTarget(dir, path); err == nil || !strings.Contains(err.Error(), "go.mod") {
		t.Fatalf("没有 go.mod 应当报错，实际: %v", err)
	}
}

// TestGuardsMatchSource 每条异变的 old 片段必须在目标文件里恰好命中一次。
// 这是清单腐化的唯一防线——改了实现或调了缩进时，脚本不会报错，只会静默失效。
func TestGuardsMatchSource(t *testing.T) {
	root := repoRootForTest(t)
	if len(guards) == 0 {
		t.Fatal("守卫异变清单空了")
	}
	seen := map[string]bool{}
	for _, g := range guards {
		if seen[g.name] {
			t.Fatalf("异变名重复：%q（存活清单会指不出是哪一条）", g.name)
		}
		seen[g.name] = true
		if g.old == "" {
			t.Fatalf("异变 %q 没写 old 片段", g.name)
		}
		if g.old == g.new {
			t.Fatalf("异变 %q 的 old 与 new 相同：改了等于没改，只会白跑一趟", g.name)
		}
		abs := filepath.Join(root, filepath.FromSlash(g.file))
		data, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("异变 %q 指向的文件读不到: %v", g.name, err)
		}
		if n := strings.Count(string(data), g.old); n != 1 {
			t.Fatalf("异变 %q 的 old 片段在 %s 里命中 %d 次（应恰好 1 次）", g.name, g.file, n)
		}
		if _, _, err := testTarget(root, abs); err != nil {
			t.Fatalf("异变 %q 定位不到测试目标: %v", g.name, err)
		}
	}
}

// TestTestTarget 跨模块定位：文件在哪个模块，就在哪个目录跑；包名是模块内的相对路径。
// 这三条路径分属三种形状（子包 / 模块根包 / 另一个模块的子包），覆盖定位逻辑的三种出口。
func TestTestTarget(t *testing.T) {
	root := repoRootForTest(t)
	cases := []struct {
		file    string
		wantMod string
		wantPkg string
	}{
		{"src/memory/redact/redact.go", "src/memory", "./redact"},
		{"src/spv2/server.go", "src/spv2", "./"},
		{"src/approval/engine/tier_medium.go", "src/approval", "./engine"},
	}
	for _, c := range cases {
		abs := filepath.Join(root, filepath.FromSlash(c.file))
		if _, err := os.Stat(abs); err != nil {
			t.Fatalf("%s 读不到（清单里的路径要跟着仓库走）: %v", c.file, err)
		}
		dir, pkg, err := testTarget(root, abs)
		if err != nil {
			t.Fatalf("%s 定位失败: %v", c.file, err)
		}
		if pkg != c.wantPkg {
			t.Errorf("%s 包名 = %q，期望 %q", c.file, pkg, c.wantPkg)
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil || filepath.ToSlash(rel) != c.wantMod {
			t.Errorf("%s 模块根 = %q，期望 %q（err=%v）", c.file, filepath.ToSlash(rel), c.wantMod, err)
		}
	}
}

// TestSelectGuardsFilters 过滤器拼错时必须报错，不能静默跑出空集或全集。
func TestSelectGuardsFilters(t *testing.T) {
	list := []guard{
		{name: "甲", file: "src/spv2/server.go"},
		{name: "乙", file: "src/memory/redact/redact.go"},
		{name: "丙", file: "src/qq/napcat/napcat.go"},
	}
	cases := []struct {
		only, not string
		want      int
		wantErr   string
	}{
		{want: 3},
		{only: "spv2", want: 1},
		{not: "spv2/server.go", want: 2},
		{only: "甲", not: "甲", want: 0, wantErr: "一条都不剩"},
		{only: "根本没有的名字", want: 0, wantErr: "一条都没匹配上"},
		{not: "根本没有的名字", want: 0, wantErr: "没排除掉任何一条"},
	}
	for _, c := range cases {
		got, err := selectGuards(list, c.only, c.not)
		switch {
		case c.wantErr != "" && err == nil:
			t.Errorf("only=%q not=%q 期望报错（%s），实际返回 %d 条", c.only, c.not, c.wantErr, len(got))
		case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
			t.Errorf("only=%q not=%q 报错文案要指到原因，实际: %v", c.only, c.not, err)
		case c.wantErr == "" && err != nil:
			t.Errorf("only=%q not=%q 不该报错: %v", c.only, c.not, err)
		case c.wantErr == "" && len(got) != c.want:
			t.Errorf("only=%q not=%q 命中 %d 条，期望 %d", c.only, c.not, len(got), c.want)
		}
	}
	// 真实清单上必须真能躲开 spv2/server.go：分批跑靠的就是它。
	hit, err := selectGuards(guards, "", "spv2/server.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range hit {
		if strings.Contains(g.file, "spv2/server.go") {
			t.Fatalf("-not 之后还剩 %s，等于会去改正在被别的活儿动的文件", g.file)
		}
	}
	if len(hit) >= len(guards) {
		t.Fatalf("-not 没减少条数（%d/%d）", len(hit), len(guards))
	}
}

// TestBuildFailureIsNotAKill 改完编译不过 ≠ 守卫被测试守住。
// 这一条是整个工具最容易自欺的地方：go test 非 0 有两种完全不同的含义。
func TestBuildFailureIsNotAKill(t *testing.T) {
	const original = "func f() { return 1 }\n"
	path := tempModule(t, original)
	fakeGoTestOut(t, map[string]int{"./sub": 2},
		map[string]string{"./sub": "# example.com/tmp/sub\n./sub/src.go:1:1: syntax error\nFAIL\texample.com/tmp/sub [build failed]\n"})

	var buf strings.Builder
	code, stats := run([]guard{{name: "编译不过的", file: path, old: "return 1", new: "return"}},
		runOptions{out: &buf})

	if code != 1 {
		t.Fatalf("有无效异变时不能报成功（退出码 %d）", code)
	}
	if stats.killed != 0 || stats.broken != 1 || stats.survived != 0 {
		t.Fatalf("编译失败必须记为无效，不能记为杀死：stats=%+v\n%s", stats, buf.String())
	}
	if !strings.Contains(buf.String(), "syntax error") {
		t.Fatalf("无效要当场露出原因，不能只给一个词：%s", buf.String())
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("无效异变也必须还原源码：%q", got)
	}
}

// TestClassifyMarks 分类认 go test 自己打的标记；退出码单独不可信（超时也是非 0）。
func TestClassifyMarks(t *testing.T) {
	cases := []struct {
		code int
		out  string
		want outcome
	}{
		{0, "ok  \tpkg 0.5s", outcomeSurvived},
		{1, "--- FAIL: TestX\nFAIL\tpkg 0.5s\n", outcomeKilled},
		{1, "FAIL\tpkg [build failed]\n", outcomeBroken},
		{1, "FAIL\tpkg [setup failed]\n", outcomeBroken},
		{2, "panic: test timed out after 300s\n", outcomeBroken},
		{0, "?   \tpkg\t[no test files]", outcomeBroken},
	}
	for _, c := range cases {
		if got := classify(c.code, c.out); got != c.want {
			t.Errorf("classify(%d, %q) = %d，期望 %d", c.code, c.out, got, c.want)
		}
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	root, err := repoRootFrom(cwd())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
