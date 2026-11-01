package tests

// provides ↔ Handle 一致性门禁：协议里 `provides` 是**唯一的发现面**。
//
// 注册了动作却没在 provides 里声明，别人只能靠硬编码调用它——第三方按名册实现同一个服务时
// 永远不知道要实现那些动作（memory 的 7 个任务动作就这样藏了半年，reasoner 一直硬编码调）。
// 反过来声明了却没实现，是更直接的说谎。
//
// 用源码 AST 比对而不是起服务：15 个模块逐个起进程太慢，而这里要的就是"字面量对不对"。
// 允许的例外必须**具名**并写清理由——往表里加一条等于放宽协议真实性。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// handledButUndeclared 是允许"注册了但不进 provides"的动作：入站动作与内部自跳。
// 它们不是给别的服务按名册调用的，声明出来反而会误导（名册是"我能提供什么"）。
var handledButUndeclared = map[string]map[string]string{
	// 别人调它（本服务是接收方），所以不进自己的 provides。
	"reasoner": {
		"report_result": "入站：工具/补报回灌",
		"resume":        "入站：记忆服务回发上下文",
	},
	"approval": {"report_result": "入站：裁决回执"},
	"mcp":      {"cancel_import": "入站：外部取消导入", "report_result": "入站：导入进度补报"},
	// 服务自己发给自己的跳转（单步推理的自跳）。
	"reasoner_self": {
		"step":      "自跳：一轮结束推进下一轮",
		"user_hold": "入站：门户停手/继续",
	},
}

// ownDispatcher 是"不用 service 骨架、自己实现动作分发"的模块：它们的 provides 是真的，
// 只是不走 s.Handle。对这类模块改用弱一点的检查——声明过的动作名必须在源码里出现过，
// 这样"删了实现却忘了撤声明"仍然抓得到。
var ownDispatcher = map[string]string{
	"settings-center": "中心自己写 mux 分发（ping / roster / report 由它直接应答），不经 service 骨架",
	"evals":           "评测 harness 的假入口（冒充 boss 收 deliver），不是可部署服务",
}

// TestProvidesMatchHandle 比对每个模块的 provides 声明与 Handle 注册。
func TestProvidesMatchHandle(t *testing.T) {
	root := repoRoot(t)
	mods := goModules(t, root)
	if len(mods) == 0 {
		t.Fatal("没找到任何 Go 模块")
	}

	for _, mod := range mods {
		declared, handled, mentioned := scanActions(t, filepath.Join(root, mod))
		if len(declared) == 0 && len(handled) == 0 {
			continue // 没有服务骨架的模块（如 protocol）
		}
		if reason, ok := ownDispatcher[mod]; ok {
			// 自己实现分发的模块：provides 必须真的在源码里出现过（否则是过期名片）。
			for _, action := range sortedKeys(declared) {
				if !mentioned[action] {
					t.Errorf("%s: provides 声明了 %q，但源码里没有这个动作名的任何实现痕迹（%s）", mod, action, reason)
				}
			}
			continue
		}
		undeclaredOK := handledButUndeclared[mod]
		if extra, ok := handledButUndeclared[mod+"_self"]; ok {
			if undeclaredOK == nil {
				undeclaredOK = map[string]string{}
			}
			for k, v := range extra {
				undeclaredOK[k] = v
			}
		}
		for _, action := range sortedKeys(handled) {
			if _, ok := declared[action]; ok {
				continue
			}
			if reason, ok := undeclaredOK[action]; ok {
				_ = reason // 例外成立：入站 / 自跳
				continue
			}
			t.Errorf("%s: 动作 %q 已 Handle 但 provides 里没有声明——协议面看不见它，"+
				"别人只能硬编码调用；要么补进 provides，要么在 handledButUndeclared 里写明它是入站动作", mod, action)
		}
		for _, action := range sortedKeys(declared) {
			if !handled[action] {
				t.Errorf("%s: provides 声明了 %q 但没有 Handle 实现——名片在承诺一个做不到的动作", mod, action)
			}
		}
	}
}

// goModules 列出仓库根下含 go.mod 的目录名。
func goModules(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "go.mod")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// scanActions 解析模块下所有非测试 .go 文件，收集 provides 声明、Handle 注册与"源码里出现过的动作名字面量"。
func scanActions(t *testing.T, dir string) (declared, handled, mentioned map[string]bool) {
	t.Helper()
	declared, handled, mentioned = map[string]bool{}, map[string]bool{}, map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// 不进去的目录：依赖、构建产物、前端资源。
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
		// provides 列表：`[]protocol.Provide{{Action: "x", ...}, ...}` —— 元素是**省略类型**的
		// 复合字面量，所以要先认外层切片，再看它的元素。
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isProvideSlice(lit.Type) {
				return true
			}
			for _, elt := range lit.Elts {
				inner, ok := elt.(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, field := range inner.Elts {
					kv, ok := field.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok || key.Name != "Action" {
						continue
					}
					if v, ok := stringLit(kv.Value); ok {
						declared[v] = true
					}
				}
			}
			return true
		})
		// s.Handle("x", ...)：注册动作（HandleSetting / HandleReadOnly 不是动作）。
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Handle" || len(call.Args) != 2 {
				return true
			}
			if v, ok := stringLit(call.Args[0]); ok {
				handled[v] = true
			}
			return true
		})
		// 所有字符串字面量：给自己写分发的模块（settings-center）用它做"声明是否还活着"的弱检查。
		ast.Inspect(file, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if v, ok := stringLit(bl); ok {
					mentioned[v] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 %s 失败: %v", dir, err)
	}
	return declared, handled, mentioned
}

// isProvideSlice 判断类型是不是 `[]Provide` / `[]protocol.Provide`。
func isProvideSlice(e ast.Expr) bool {
	arr, ok := e.(*ast.ArrayType)
	if !ok {
		return false
	}
	return isTypeName(arr.Elt, "Provide")
}

// isTypeName 判断表达式是不是叫 name 的类型（兼容 protocol.Provide 与裸 Provide）。
func isTypeName(e ast.Expr, name string) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == name
	case *ast.SelectorExpr:
		return t.Sel.Name == name
	}
	return false
}

// stringLit 取出字符串字面量的值。
func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
