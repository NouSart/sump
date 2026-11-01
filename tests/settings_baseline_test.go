package tests

// 设置键基线：protocol.md 的「设置键」一行是**契约**，不是清单摘抄。
//
// 这一行过去只靠人手维护：注册了新键忘了登记、删了实现忘了撤行，两边都不会有反应。
// 这里把它变成双向门禁——文档里的键必须在代码里注册过，代码里注册的键必须在文档里登记过。
// 文档说 A、代码做 B 是最难查的一类漂移：读文档的人据此推断行为，然后推错。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// settingsRegistration 是"登记一个设置项"的四个骨架入口（见 service/service.go）。
var settingsRegistration = map[string]bool{
	"HandleSetting":          true,
	"HandleSecret":           true,
	"HandleSecretWithCommit": true,
	"HandleReadOnly":         true,
}

// TestSettingsBaseline 双向比对 protocol.md 的设置键行与各服务的注册代码。
//
// 三种声明方式都要覆盖，少一种就有盲区：
//   - 骨架的 HandleSetting / HandleSecret* / HandleReadOnly（大多数 Go 服务）；
//   - 自己写分发的 settings-center：声明成 protocol.Setting{Key: "x"} 字面量；
//   - 门户（TypeScript）：设置项只在该文件的 portalSettings 里，用 `key: 'x'` 写。
func TestSettingsBaseline(t *testing.T) {
	root := repoRoot(t)
	documented := documentedSettings(t, filepath.Join(root, "docs", "concepts", "protocol.md"))
	if len(documented) == 0 {
		t.Fatal("protocol.md 的「设置键」行没解析出任何键——门禁本身失效了")
	}
	registered := registeredSettings(t, root)
	if len(registered) == 0 {
		t.Fatal("代码里没扫到任何设置键——门禁本身失效了")
	}

	for _, key := range sortedSet(documented) {
		if _, ok := registered[key]; !ok {
			t.Errorf("protocol.md 登记了设置键 %q，但没有任何服务注册它（文档承诺了不存在的东西）", key)
		}
	}
	for _, key := range sortedSet(registered) {
		if _, ok := documented[key]; !ok {
			t.Errorf("服务注册了设置键 %q，但 protocol.md 的「设置键」行没有它（协议面看不见这个键）", key)
		}
	}
}

// documentedSettings 取 protocol.md 里「| 设置键 |」那一行**最后一列**（现有值）里的键。
// 中间那列是写法规则（`<域>.<名>`），不是键，不能算进来。
func documentedSettings(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	backtick := regexp.MustCompile("`([^`]+)`")
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "| 设置键 |") {
			continue
		}
		// 表格行：| 类别 | 规则 | 现有值 | —— 按未转义竖线切开后取最后一格。
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			t.Fatalf("设置键那一行的表格形状变了，门禁要跟着改：%q", line)
		}
		last := cells[len(cells)-2]
		for _, m := range backtick.FindAllStringSubmatch(last, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// registeredSettings 扫所有模块的设置声明：Go 的三种声明方式 + 门户的 TS 声明。
func registeredSettings(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, mod := range goModules(t, root) {
		fset := token.NewFileSet()
		dir := filepath.Join(root, mod)
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				switch info.Name() {
				case "node_modules", "dist", ".venv", "vendor", ".git", "data":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			// 同一个文件里的字符串常量（settings-center 用 settingSweepMS 这类常量声明键）。
			consts := map[string]string{}
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
						continue
					}
					if v, ok := stringLit(vs.Values[0]); ok {
						consts[vs.Names[0].Name] = v
					}
				}
			}
			resolve := func(e ast.Expr) (string, bool) {
				if v, ok := stringLit(e); ok {
					return v, true
				}
				if id, ok := e.(*ast.Ident); ok {
					v, ok := consts[id.Name]
					return v, ok
				}
				return "", false
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					sel, ok := node.Fun.(*ast.SelectorExpr)
					if !ok || !settingsRegistration[sel.Sel.Name] || len(node.Args) == 0 {
						return true
					}
					if key, ok := resolve(node.Args[0]); ok {
						out[key] = true
					}
				case *ast.CompositeLit:
					// `[]protocol.Setting{{Key: "x"}, ...}`：元素是省略类型的字面量，先认外层切片。
					if !isSettingSlice(node.Type) {
						return true
					}
					for _, elt := range node.Elts {
						inner, ok := elt.(*ast.CompositeLit)
						if !ok {
							continue
						}
						for _, field := range inner.Elts {
							kv, ok := field.(*ast.KeyValueExpr)
							if !ok {
								continue
							}
							if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Key" {
								if key, ok := resolve(kv.Value); ok {
									out[key] = true
								}
							}
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("扫描 %s 失败: %v", dir, err)
		}
	}
	// 门户（TypeScript）：设置项只声明在 service.ts 的 portalSettings 里。
	portalSettings := filepath.Join(root, "frontend", "src", "server", "service.ts")
	data, err := os.ReadFile(portalSettings)
	if err != nil {
		t.Fatalf("读不到门户的设置声明（路径变了就一起改这里）: %v", err)
	}
	keyLine := regexp.MustCompile(`\{\s*key:\s*'([^']+)'`)
	for _, m := range keyLine.FindAllStringSubmatch(string(data), -1) {
		out[m[1]] = true
	}
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isSettingSlice 判断类型是不是 `[]Setting` / `[]protocol.Setting`。
func isSettingSlice(e ast.Expr) bool {
	arr, ok := e.(*ast.ArrayType)
	if !ok {
		return false
	}
	return isTypeName(arr.Elt, "Setting")
}

// 让 strconv 的引用不被 goimports 判成未使用（stringLit 在本文件与 provides_gate_test.go 共用）。
var _ = strconv.Unquote
