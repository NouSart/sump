package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// 文档不是散文：它得能被人点开。这两个用例守住两件最容易悄悄坏掉的事——
// 相对链接指向不存在的文件、服务清单和真实目录对不上。

func repoRootForDocs(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("定位本文件失败")
	}
	return filepath.Dir(filepath.Dir(file))
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
		// evals ?????????? go.mod????????????????????
		// ???????????????????????
		"evals": true,
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读仓库根失败: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || excluded[e.Name()] {
			continue
		}
		dir := filepath.Join(root, e.Name())
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
		add(filepath.Join(root, name, "README.md"))
	}
	add(filepath.Join(root, "memory", "memnet", "README.md"))
	sort.Strings(docs)
	return docs
}
