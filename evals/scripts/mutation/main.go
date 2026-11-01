// 命令 mutation 对 evals 做异变测试：确认评测器自己的测试真的守得住判定逻辑。
//
// 做法：对判定代码逐个植入"看起来合理但语义错了"的改动，跑对应包的测试；
// 测试失败 = 该异变被杀死（好）；测试仍通过 = 异变存活（测试太弱，必须补断言）。
//
// 为什么用 Go 而不是脚本：Go 源码片段里全是引号、反引号与制表符，
// 用 shell 的字符串转义去改 Go 源码，脚本自己先出错。这里用原始字符串，改多少行都原样。
//
// 用法（在 evals 目录下）：
//
//	go run ./scripts/mutation              # 全部异变
//	go run ./scripts/mutation -skip-slow   # 跳过需要真进程的包（快）
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// mutation 是一次异变：把 file 里的 old 换成 new，然后跑 pkg 的测试。
type mutation struct {
	name string
	file string
	old  string
	new  string
	pkg  string
	// slow 表示该包的测试要起真进程（十几秒），-skip-slow 时跳过。
	slow bool
}

func main() {
	skipSlow := flag.Bool("skip-slow", false, "跳过需要真进程的包")
	verbose := flag.Bool("v", false, "打印每个异变的测试输出")
	flag.Parse()

	root, err := moduleRoot()
	if err != nil {
		fatal(err)
	}

	var survived []string
	killed := 0
	skipped := 0
	for _, m := range mutations {
		if *skipSlow && m.slow {
			fmt.Printf("跳过  %s\n", m.name)
			skipped++
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(m.file))
		orig, err := os.ReadFile(path)
		if err != nil {
			fatal(err)
		}
		if !strings.Contains(string(orig), m.old) {
			// 异变定义与源码不同步：这本身就是缺陷（脚本会静默失效），记为存活。
			fmt.Printf("过期  %s（%s 里找不到待替换片段，异变定义需要更新）\n", m.name, m.file)
			survived = append(survived, m.name)
			continue
		}
		mutated := strings.Replace(string(orig), m.old, m.new, 1)

		code, out := runTest(root, m.pkg, path, mutated, string(orig))
		if code == 0 {
			fmt.Printf("存活  %s（%s）\n", m.name, m.file)
			survived = append(survived, m.name)
			if *verbose {
				fmt.Println(indent(out))
			}
		} else {
			fmt.Printf("杀死  %s\n", m.name)
			killed++
			if *verbose {
				fmt.Println(indent(out))
			}
		}
	}

	fmt.Printf("\n异变 %d 条：杀死 %d，存活 %d，跳过 %d\n", killed+len(survived), killed, len(survived), skipped)
	if len(survived) > 0 {
		fmt.Println("存活清单（测试太弱，需要补断言）：")
		for _, s := range survived {
			fmt.Println("  - " + s)
		}
		os.Exit(1)
	}
}

// runTest 把变异写进文件 → 跑测试 → 无论结果如何都把原文件写回去。
func runTest(root, pkg, path, mutated, orig string) (int, string) {
	if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
		_ = os.WriteFile(path, []byte(orig), 0o644)
		fatal(err)
	}
	defer func() { _ = os.WriteFile(path, []byte(orig), 0o644) }()

	cmd := exec.Command("go", "test", pkg, "-count=1")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		return 1, string(out) + "\n" + err.Error()
	}
	return 0, string(out)
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if filepath.Base(dir) == "evals" {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("找不到 evals 模块根（请在 evals 目录或子目录运行）")
		}
		dir = parent
	}
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "      | " + l
	}
	return strings.Join(lines, "\n")
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "异变测试失败: %v\n", err)
	os.Exit(2)
}
