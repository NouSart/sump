// 命令 guardmutation 对**本轮整改立起来的守卫**做异变测试：把守卫改错，看测试是否翻脸。
//
// 与 scripts/mutation 的分工：那份清单守的是 evals 评测器自己的判定逻辑（根目录 = evals 模块），
// 这份守的是散在 17 个模块里的生产路径判断（根目录 = 仓库根，每条按所在模块跑测试）。
//
// 做法：把 file 里的 old 换成 new，跑该文件所在包的测试；
// 测试失败 = 异变被杀死（守卫真有人看着）；测试仍通过 = 存活（断言太弱，必须补，不许改弱异变）。
//
// 用法（在 evals 目录下）：
//
//	go run ./scripts/guardmutation             # 全部
//	go run ./scripts/guardmutation -v          # 附上每条的测试输出
//	go run ./scripts/guardmutation -only=会话键 # 只跑名字含该串的那些条
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// guard 是一次异变：把仓库相对路径 file 里的 old 换成 new，然后跑该文件所在包的测试。
type guard struct {
	name string
	file string // 相对仓库根，如 src/memory/redact/redact.go
	old  string
	new  string
	// slow 表示该包的测试要起真进程（十几秒）；这份清单目前全是纯逻辑包，留着这个字段
	// 是为了以后往里加端到端守卫时不用改结构。
	slow bool
}

func main() {
	verbose := flag.Bool("v", false, "打印每个异变的测试输出")
	skipSlow := flag.Bool("skip-slow", false, "跳过需要真进程的包")
	only := flag.String("only", "", "只跑名字包含该串的异变")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	list := guards
	if *only != "" {
		var hit []guard
		for _, g := range guards {
			if strings.Contains(g.name, *only) || strings.Contains(g.file, *only) {
				hit = append(hit, g)
			}
		}
		if len(hit) == 0 {
			fatal(fmt.Errorf("-only=%q 一条都没匹配上（清单里没有这个名字）", *only))
		}
		list = hit
	}
	code, _ := run(list, runOptions{root: root, skipSlow: *skipSlow, verbose: *verbose, out: os.Stdout})
	if code != 0 {
		os.Exit(code)
	}
}

type runOptions struct {
	root     string
	skipSlow bool
	verbose  bool
	out      io.Writer
}

type runStats struct {
	killed        int
	skipped       int
	survived      int // 存活 + 过期：两者都意味着"这条守卫没被测试守住"
	survivedNames []string
}

// run 跑一遍清单，返回退出码（0 = 全部杀死）与统计。
func run(list []guard, opts runOptions) (int, runStats) {
	out := opts.out
	if out == nil {
		out = io.Discard
	}
	var st runStats
	for _, g := range list {
		if opts.skipSlow && g.slow {
			fmt.Fprintf(out, "跳过  %s\n", g.name)
			st.skipped++
			continue
		}
		path := resolve(opts.root, g.file)
		orig, err := os.ReadFile(path)
		if err != nil {
			fatal(fmt.Errorf("异变 %q 指向的文件读不到: %w", g.name, err))
		}
		if strings.Count(string(orig), g.old) != 1 {
			// 清单与源码不同步：脚本会静默失效（"跑了个寂寞"），这本身就是缺陷，记为存活。
			fmt.Fprintf(out, "过期  %s（%s 里待替换片段命中 %d 次，异变定义需要更新）\n",
				g.name, g.file, strings.Count(string(orig), g.old))
			st.survived++
			st.survivedNames = append(st.survivedNames, g.name)
			continue
		}
		mutated := strings.Replace(string(orig), g.old, g.new, 1)

		dir, pkg, err := testTarget(opts.root, path)
		if err != nil {
			fatal(fmt.Errorf("异变 %q: %w", g.name, err))
		}
		code, testOut := runTest(dir, pkg, path, mutated, string(orig))
		if code == 0 {
			fmt.Fprintf(out, "存活  %s（%s）\n", g.name, g.file)
			st.survived++
			st.survivedNames = append(st.survivedNames, g.name)
			if opts.verbose {
				fmt.Fprintln(out, indent(testOut))
			}
		} else {
			fmt.Fprintf(out, "杀死  %s  [%s]\n", g.name, whereLabel(opts.root, dir, pkg))
			st.killed++
			if opts.verbose {
				fmt.Fprintln(out, indent(testOut))
			}
		}
	}

	fmt.Fprintf(out, "\n守卫异变 %d 条：杀死 %d，存活 %d，跳过 %d\n",
		st.killed+st.survived, st.killed, st.survived, st.skipped)
	if st.survived > 0 {
		fmt.Fprintln(out, "存活清单（测试太弱或清单过期，需要补断言，不许改弱异变）：")
		for _, s := range st.survivedNames {
			fmt.Fprintln(out, "  - "+s)
		}
		return 1, st
	}
	return 0, st
}

// resolve 把清单里的仓库相对路径拼成绝对路径；测试直接传绝对路径时原样用。
func resolve(root, file string) string {
	if filepath.IsAbs(file) {
		return file
	}
	return filepath.Join(root, filepath.FromSlash(file))
}

// whereLabel 打印"在哪个模块的哪个包跑的"，路径按仓库根缩写，测试里的临时目录退回全路径。
func whereLabel(root, dir, pkg string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = dir
	}
	return filepath.ToSlash(rel) + " " + pkg
}

// testTarget 由文件路径定出「在哪个模块目录跑」与「跑哪个包」。
// 本仓库是 17 个模块的 go.work 工作区，跨模块不能一次 go test ./...，所以逐条定位；
// 定位不到 go.mod 就必须报错——静默换个目录跑，等于一条测试都没跑还宣称守住了。
func testTarget(_, absFile string) (dir string, pkg string, err error) {
	file := absFile
	dir = filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", fmt.Errorf("%s 不属于任何一个模块（向上没有找到 go.mod）", filepath.ToSlash(file))
		}
		dir = parent
	}
	sub, err := filepath.Rel(dir, filepath.Dir(file))
	if err != nil {
		return "", "", err
	}
	if sub == "." {
		sub = ""
	}
	return dir, "./" + filepath.ToSlash(sub), nil
}

// goTest 跑某个包的测试。测试里可以换成戏台：一条真异变要跑几秒，
// 逐条验分类不值得真的等 go test。
var goTest = func(dir, pkg string) (int, string) {
	cmd := exec.Command("go", "test", pkg, "-count=1")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		return 1, string(out) + "\n" + err.Error()
	}
	return 0, string(out)
}

// runTest 把变异写进文件 → 跑测试 → 无论结果如何都把原文件写回去（出错 / panic 也一样）。
func runTest(dir, pkg, path, mutated, orig string) (int, string) {
	if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
		_ = os.WriteFile(path, []byte(orig), 0o644)
		fatal(err)
	}
	defer func() { _ = os.WriteFile(path, []byte(orig), 0o644) }()

	return goTest(dir, pkg)
}

// repoRoot 从当前目录向上找到工作区根（认 go.work，不认目录名：脚本可能从 scripts/ 里跑）。
func repoRoot() (string, error) {
	return repoRootFrom(cwd())
}

func repoRootFrom(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("找不到仓库根（没有 go.work；请在 SUMP 仓库内运行）")
		}
		dir = parent
	}
}

func cwd() string {
	dir, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	return dir
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "      | " + l
	}
	return strings.Join(lines, "\n")
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "守卫异变测试失败: %v\n", err)
	os.Exit(2)
}
