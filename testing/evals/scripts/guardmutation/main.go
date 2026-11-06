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
}

func main() {
	verbose := flag.Bool("v", false, "打印每个异变的测试输出")
	only := flag.String("only", "", "只跑名字或文件包含该串的异变")
	not := flag.String("not", "", "排除名字或文件包含该串的异变（分批跑用；结论要写清实际跑了几条）")
	flag.Parse()

	list, err := selectGuards(guards, *only, *not)
	if err != nil {
		fatal(err)
	}
	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	code, _ := run(list, runOptions{root: root, verbose: *verbose, out: os.Stdout})
	if code != 0 {
		os.Exit(code)
	}
}

type runOptions struct {
	root    string
	verbose bool
	out     io.Writer
}

type runStats struct {
	killed        int
	survived      int // 存活 + 过期：两者都意味着"这条守卫没被测试守住"
	survivedNames []string
	// broken 是"改完编译不过"：既不是杀死也不是存活，它没证明任何判断被测试拦下。
	// 把它算进杀死 = 拿一个坏掉的异变冒充覆盖率，所以单列并让退出码非 0。
	broken      int
	brokenNames []string
}

// run 跑一遍清单，返回退出码（0 = 全部杀死）与统计。
func run(list []guard, opts runOptions) (int, runStats) {
	out := opts.out
	if out == nil {
		out = io.Discard
	}
	var st runStats
	for _, g := range list {
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
		switch classify(code, testOut) {
		case outcomeBroken:
			// 编译不过 / 没跑起来：这条异变什么都没证明。算成"杀死"就是拿坏掉的异变冒充覆盖率，
			// 算成"存活"又会骗人去补断言——所以单列，退出码非 0，逼着回头改这条异变本身。
			fmt.Fprintf(out, "无效  %s（改完编译不过或测试没跑起来，不能算被杀死）\n", g.name)
			st.broken++
			st.brokenNames = append(st.brokenNames, g.name)
			fmt.Fprintln(out, indent(firstLines(testOut, 12)))
		case outcomeSurvived:
			fmt.Fprintf(out, "存活  %s（%s）\n", g.name, g.file)
			st.survived++
			st.survivedNames = append(st.survivedNames, g.name)
			if opts.verbose {
				fmt.Fprintln(out, indent(testOut))
			}
		default:
			fmt.Fprintf(out, "杀死  %s  [%s]\n", g.name, whereLabel(opts.root, dir, pkg))
			st.killed++
			if opts.verbose {
				fmt.Fprintln(out, indent(testOut))
			}
		}
	}

	fmt.Fprintf(out, "\n守卫异变 %d 条：杀死 %d，存活 %d，无效 %d\n",
		st.killed+st.survived+st.broken, st.killed, st.survived, st.broken)
	if st.survived > 0 {
		fmt.Fprintln(out, "存活清单（测试太弱或清单过期，需要补断言，不许改弱异变）：")
		for _, s := range st.survivedNames {
			fmt.Fprintln(out, "  - "+s)
		}
	}
	if st.broken > 0 {
		fmt.Fprintln(out, "无效清单（异变写法本身有问题：要么换成仍能编译的错法，要么这条守卫其实没被验过）：")
		for _, s := range st.brokenNames {
			fmt.Fprintln(out, "  - "+s)
		}
	}
	if st.survived > 0 || st.broken > 0 {
		return 1, st
	}
	return 0, st
}

// outcome 是一条异变的三种结局。"退出码非 0"不等于"被测试杀死"：编译失败同样非 0，
// 而它证明的是异变写坏了，不是守卫有人看着。
type outcome int

const (
	outcomeKilled outcome = iota
	outcomeSurvived
	outcomeBroken // 编译不过 / 测试没跑起来 / 超时 / 这个包根本没有测试
)

// classify 按 go test 自己打的标记分类，不靠退出码猜。
// 注意"panic:" 不在标记里：异变把被测代码跑出 panic，正是"测试发现了它"，属于杀死；
// 只有超时、编译不过、没有测试文件这三种是"这条异变没证明任何判断"。
func classify(code int, out string) outcome {
	for _, mark := range []string{"[build failed]", "[setup failed]", "test timed out", "no test files"} {
		if strings.Contains(out, mark) {
			return outcomeBroken
		}
	}
	if code == 0 {
		return outcomeSurvived
	}
	return outcomeKilled
}

// firstLines 取输出的前 n 行：无效异变必须当场露出编译错误，不然只看到一个词猜不透。
func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…（截断，-v 看全文）")
	}
	return strings.Join(lines, "\n")
}

// selectGuards 按 -only / -not 取子集。两个过滤器都必须真的命中，否则直接失败：
// 拼错的 -only 会跑出"0 条，全部杀死"，拼错的 -not 会以为躲开了某个文件其实没躲开
// ——两种都是拿着空集中门说旗开得胜。
func selectGuards(list []guard, only, not string) ([]guard, error) {
	out := list
	if only != "" {
		var hit []guard
		for _, g := range out {
			if strings.Contains(g.name, only) || strings.Contains(g.file, only) {
				hit = append(hit, g)
			}
		}
		if len(hit) == 0 {
			return nil, fmt.Errorf("-only=%q 一条都没匹配上（清单里没有这个名字）", only)
		}
		out = hit
	}
	if not != "" {
		var hit []guard
		for _, g := range out {
			if !strings.Contains(g.name, not) && !strings.Contains(g.file, not) {
				hit = append(hit, g)
			}
		}
		if len(hit) == len(out) {
			return nil, fmt.Errorf("-not=%q 没排除掉任何一条（拼错了？宁可报错也不要假装躲开了）", not)
		}
		out = hit
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-only 与 -not 互相抵消，一条都不剩")
	}
	return out, nil
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
