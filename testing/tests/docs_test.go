package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 文档不是散文：它得能被人点开。这三个用例守住三件最容易悄悄坏掉的事——
// 相对链接指向不存在的文件、服务清单和真实目录对不上、模块数量和各处写死的数字对不上。

func repoRootForDocs(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("定位本文件失败")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

var mdLinkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

// TestDocsLinksResolve 检查项目文档里的相对 Markdown 链接都指向存在的文件。
// 外链、纯锚点与非 .md 目标跳过（图片/脚本由各自的使用处保证）。
func TestDocsLinksResolve(t *testing.T) {
	root := repoRootForDocs(t)
	docs := collectDocs(t, root)
	if len(docs) == 0 {
		t.Fatal("没找到任何文档，检查 collectDocs")
	}
	for _, path := range docs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range mdLinkPattern.FindAllStringSubmatch(string(data), -1) {
			target := m[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
				continue
			}
			target = strings.SplitN(target, "#", 2)[0]
			if target == "" || !strings.HasSuffix(strings.ToLower(target), ".md") {
				continue
			}
			resolved := filepath.Join(filepath.Dir(path), filepath.FromSlash(target))
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s: 链接目标不存在: %s", filepath.ToSlash(rel), target)
			}
		}
	}
}

// TestDocsServiceListMatchesDirs 检查 protocol.md 的「服务名」基线与仓库真实目录一致：
// 少写一个 = 新人照着名单接不上；多写一个 = 名单里躺着不存在的服务。
func TestDocsServiceListMatchesDirs(t *testing.T) {
	root := repoRootForDocs(t)
	protocolDoc := filepath.Join(root, "docs", "concepts", "protocol.md")
	data, err := os.ReadFile(protocolDoc)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", protocolDoc, err)
	}

	documented := namesFromBaseline(t, string(data))
	actual := serviceDirs(t, root)

	if strings.Join(documented, ",") != strings.Join(actual, ",") {
		t.Fatalf("服务清单与目录不一致\n文档列出: %v\n仓库实际: %v", documented, actual)
	}
}

// moduleCountClaims 覆盖文档里写模块数量的两种中文语序：
// 「N 个模块」（数字在前，README）和「Go 模块 N 个」（数字在后，CONTRIBUTING）。
// 只写一种语序，另一种就是死代码——所以自证时要同时改两份文档。
var moduleCountClaims = []*regexp.Regexp{
	regexp.MustCompile(`([0-9]+)\s*个模块`),
	regexp.MustCompile(`模块\s*([0-9]+)\s*个`),
}

// TestDocsModuleCountMatchesGoWork 检查 README / CONTRIBUTING 里写明的模块数量与 go.work 一致。
// go.work 是「本仓库有几个模块」的唯一权威：文档里的数字一旦手打就会漂，
// 所以这里当场解析 go.work，不预先写死任何数量。
func TestDocsModuleCountMatchesGoWork(t *testing.T) {
	root := repoRootForDocs(t)
	fromGoWork := countGoWorkModules(t, filepath.Join(root, "go.work"))

	found := 0
	for _, name := range []string{"README.md", "CONTRIBUTING.md"} {
		path := filepath.Join(root, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			seen := map[string]bool{} // 同一行只报一次，免得两种语序重复计数
			for _, re := range moduleCountClaims {
				for _, m := range re.FindAllStringSubmatch(line, -1) {
					if seen[m[0]] {
						continue
					}
					seen[m[0]] = true
					found++
					n, err := strconv.Atoi(m[1])
					if err != nil {
						t.Fatalf("%s:%d: 模块数量 %q 读不懂: %v", name, i+1, m[1], err)
					}
					if n != fromGoWork {
						t.Errorf("%s:%d: 文档写 %d 个模块，go.work 实际登记 %d 个\n%s",
							name, i+1, n, fromGoWork, strings.TrimSpace(line))
					}
				}
			}
		}
	}
	// 两份文档都不提数量也是允许的（没有可校对的说法），但要在日志里留下痕迹，
	// 免得日后文档改成不写数量时这个用例悄悄变成空转。
	if found == 0 {
		t.Logf("README / CONTRIBUTING 里没出现模块数量，无可校对项（go.work 登记 %d 个）", fromGoWork)
	}
}

// countGoWorkModules 解析 go.work 的 use ( ... ) 块，返回登记的模块数。
// go.work 缺失或一条都没有时直接判失败——那种情况下本用例什么都证明不了。
func countGoWorkModules(t *testing.T, goWorkPath string) int {
	t.Helper()
	data, err := os.ReadFile(goWorkPath)
	if err != nil {
		t.Fatalf("读 go.work 失败: %v（模块数量没有权威来源，用例无法校验）", err)
	}
	count := 0
	inUse := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if !inUse {
			switch {
			case strings.HasPrefix(line, "use ("):
				inUse = true
			case len(strings.Fields(line)) == 2 && strings.HasPrefix(line, "use "):
				count++ // 单行写法 use ./src/foo
			}
			continue
		}
		if line == ")" {
			inUse = false
			continue
		}
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		count++
	}
	if count == 0 {
		t.Fatal("go.work 的 use 块里一个模块都没解析出来，用例无从校验（检查 go.work 或本用例的解析逻辑）")
	}
	return count
}

func namesFromBaseline(t *testing.T, doc string) []string {
	t.Helper()
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "| 服务名 |") {
			continue
		}
		var names []string
		for _, m := range regexp.MustCompile("`([a-z][a-z0-9-]*)`").FindAllStringSubmatch(line, -1) {
			names = append(names, m[1])
		}
		sort.Strings(names)
		return names
	}
	t.Fatal("protocol.md 里找不到「| 服务名 |」这一行（命名基线被删了？）")
	return nil
}

// serviceDirs 返回仓库里真正的服务目录：带 go.mod / pyproject.toml / package.json 的目录，
// 去掉库与测试模块（protocol、service、tests、approval-integration）。
func serviceDirs(t *testing.T, root string) []string {
	t.Helper()
	excluded := map[string]bool{
		"protocol": true, "service": true, "tests": true, "approval-integration": true,
		// spv2 是协议参考实现库（不是独立服务），不进入服务清单。
		"spv2": true,
		// evals ?????????? go.mod????????????????????
		// ???????????????????????
		"evals": true,
	}
	entries, err := os.ReadDir(filepath.Join(root, "src"))
	if err != nil {
		t.Fatalf("读仓库根失败: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || excluded[e.Name()] {
			continue
		}
		dir := filepath.Join(root, "src", e.Name())
		for _, marker := range []string{"go.mod", "pyproject.toml", "package.json"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				names = append(names, e.Name())
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

// collectDocs 收集需要检查链接的文档：根目录几篇、docs 下全部、每个服务目录的 README。
// 跳过依赖目录（node_modules / .venv / third_party）。
func collectDocs(t *testing.T, root string) []string {
	t.Helper()
	var docs []string
	add := func(path string) {
		if _, err := os.Stat(path); err == nil {
			docs = append(docs, path)
		}
	}
	for _, name := range []string{"README.md", "CONTRIBUTING.md", "TODO.md", "AGENTS.md"} {
		add(filepath.Join(root, name))
	}
	skip := map[string]bool{"node_modules": true, ".venv": true, "third_party": true, ".git": true}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && skip[d.Name()] {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			docs = append(docs, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 docs 失败: %v", err)
	}
	for _, name := range serviceDirs(t, root) {
		add(filepath.Join(root, "src", name, "README.md"))
	}
	add(filepath.Join(root, "src", "memory", "memnet", "README.md"))
	sort.Strings(docs)
	return docs
}
