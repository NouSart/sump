package main

// 异变工具自己的测试：只守两件会**静默产出假结论**的事。
//
//   1. 清单与源码不同步 —— 某条异变的 old 片段找不到时，它不会报错，只是"跑了个寂寞"；
//      于是报告里一片"杀死"，而那条本该守住的判断已经没人看了。TestMutationsMatchSource 盯它。
//   2. 分类与退出码 —— 存活写成杀死、过期不算存活、改完源码不还原，结论都会失真。
//      TestRunClassifies / TestRunAllKilledExitZero 盯它（戏台替掉真 go test，不去真跑）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGoTest 换掉真的 `go test`：只按剧本返回退出码。
func fakeGoTest(t *testing.T, codes map[string]int) {
	t.Helper()
	orig := goTest
	goTest = func(_ string, pkg string) (int, string) { return codes[pkg], "剧本输出：" + pkg }
	t.Cleanup(func() { goTest = orig })
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.go")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunClassifies 杀死 / 存活 / 过期 / 跳过四种结局各归各位，源码还原，退出码只在全杀死时为 0。
func TestRunClassifies(t *testing.T) {
	const original = "func f() { return 1 }\n"
	path := writeTemp(t, original)
	fakeGoTest(t, map[string]int{"./alive": 0, "./dead": 1})

	var buf strings.Builder
	code, stats := run([]mutation{
		{name: "被杀死的", file: path, old: "return 1", new: "return 2", pkg: "./dead"},
		{name: "活下来的", file: path, old: "return 1", new: "return 3", pkg: "./alive"},
		{name: "定义过期", file: path, old: "根本没有这段", new: "x", pkg: "./alive"},
		{name: "慢包跳过", file: path, old: "return 1", new: "return 4", pkg: "./slow", slow: true},
	}, runOptions{skipSlow: true, out: &buf})

	if code != 1 || stats.killed != 1 || stats.survived != 2 || stats.skipped != 1 {
		t.Fatalf("分类或退出码不符：code=%d stats=%+v\n%s", code, stats, buf.String())
	}
	// 过期定义必须进存活清单：它是"这条异变没人守了"，不是"跳过"。
	if len(stats.survivedNames) != 2 || !strings.Contains(buf.String(), "过期") {
		t.Fatalf("过期定义要单独说清楚：%v\n%s", stats.survivedNames, buf.String())
	}
	// 活下来那条跑完必须把文件还原（下一次读到的不能是脏源码）。
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("异变跑完必须还原源码：%q", got)
	}
}

// TestRunAllKilledExitZero 全杀死时退出码 0（CI 据此判断"测试守得住"）。
func TestRunAllKilledExitZero(t *testing.T) {
	const original = "func f() { return 1 }\n"
	path := writeTemp(t, original)
	fakeGoTest(t, map[string]int{"./dead": 1})

	var buf strings.Builder
	code, stats := run([]mutation{{name: "唯一", file: path, old: "return 1", new: "return 9", pkg: "./dead"}},
		runOptions{out: &buf})

	if code != 0 || stats.killed != 1 || stats.survived != 0 {
		t.Fatalf("全杀死应退出 0：code=%d stats=%+v\n%s", code, stats, buf.String())
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("异变跑完必须还原源码：%q", got)
	}
}

// TestMutationsMatchSource 清单必须与源码同步：每条的 old 片段在目标文件里恰好出现一次。
// 这是清单腐化的唯一防线——改了实现或调了缩进时，它不会报错，只会静默失效。
func TestMutationsMatchSource(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := moduleRootFrom(wd)
	if err != nil {
		t.Fatal(err)
	}
	if len(mutations) == 0 {
		t.Fatal("异变清单空了")
	}
	for _, m := range mutations {
		if m.pkg == "" {
			t.Fatalf("异变 %q 没写要跑的包", m.name)
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(m.file)))
		if err != nil {
			t.Fatalf("异变 %q 指向的文件读不到: %v", m.name, err)
		}
		if n := strings.Count(string(data), m.old); n != 1 {
			t.Fatalf("异变 %q 的 old 片段在 %s 里命中 %d 次（应恰好 1 次）", m.name, m.file, n)
		}
	}
}
