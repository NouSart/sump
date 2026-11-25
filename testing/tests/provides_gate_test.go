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

// handledButUndeclared 是允许"注册了但不进 provides"的动作：入站动作与内部调用。
// 它们不是给别的服务按名册调用的，声明出来反而会误导（名册是"我能提供什么"）。
var handledButUndeclared = map[string]map[string]string{
	// 别人调它（本服务是接收方），所以不进自己的 provides。
	"reasoner": {
		"report_result": "入站：工具/补报回灌",
	},
	"approval": {"report_result": "入站：裁决回执"},
	"memory": {
		"work_get":        "入站：work 账本由入口/reasoner 内部读，不进入名册",
		"lease_charge":    "入站：租约扣减由 reasoner 工具链内部调用，不进入名册",
		"lease_release":   "入站：租约归还由内部调用，不进入名册",
		"work_update":     "入站：work 状态推进由 reasoner 轮次边界内部调用，不进入名册",
		"work_sweep":      "运维：手动触发 sweep，不属于能力名册",
		"work_cancel":     "入站：取消 work 的内部 method",
		"work_renew":      "入站：续租的内部 method",
		"message_post":    "入站：A2A 队列写入（内部）",
		"message_claim":   "入站：A2A 队列领取（内部）",
		"message_ack":     "入站：A2A 队列确认（内部）",
		"message_nack":    "入站：A2A 队列失败重排（内部）",
		"context_base":    "入站：门面取会话侧上下文原材料（内部）",
		"inject":          "入站：门面取共享经验四节（内部）",
		"staged_pending":  "入站：experience 向 session 取整理池候选（内部）",
		"staged_mark":     "入站：experience 回写整理池状态（内部）",
		"staged_retry":    "入站：experience 重试整理池条目（内部）",
		"settings_schema": "入站：门面读取设置声明做聚合与冲突检查（内部）",
	},
	"mcp": {"cancel_import": "入站：外部取消导入", "report_result": "入站：导入进度补报"},
	// 服务骨架的 owner 控制面：memory 按名直接调用（call_status / call_control 的执行服务
	// 对端，R-029），不是名册里的能力，也不给模型当工具。实现挂在 src/service 骨架里，
	// 接线的服务自动具备，所以按模块登记在骨架这里。
	"service": {
		"owner_status":  "控制面：owner 侧对账（memory 的对端，非能力）",
		"owner_control": "控制面：owner 侧控制（memory 的对端，非能力）",
	},
	// reasoner 自己接收的入站动作（门户停手/继续）。
	"reasoner_self": {
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
		name := filepath.Base(mod) // 例外表按模块名登记，与所在分组无关
		// spv2 是协议参考实现库，不是可部署服务；它的 Handle 注册是 method 表，不是 provides。
		if name == "spv2" {
			continue
		}
		declared, handled, mentioned := scanActions(t, filepath.Join(root, mod))
		if len(declared) == 0 && len(handled) == 0 {
			continue // 没有服务骨架的模块（如 protocol）
		}
		if reason, ok := ownDispatcher[name]; ok {
			// 自己实现分发的模块：provides 必须真的在源码里出现过（否则是过期名片）。
			for _, action := range sortedKeys(declared) {
				if !mentioned[action] {
					t.Errorf("%s: provides 声明了 %q，但源码里没有这个动作名的任何实现痕迹（%s）", mod, action, reason)
				}
			}
			continue
		}
		undeclaredOK := handledButUndeclared[name]
		if extra, ok := handledButUndeclared[name+"_self"]; ok {
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
				_ = reason // 例外成立：入站 / 内部
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
	var out []string
	for _, group := range []string{"src", "testing"} {
		entries, err := os.ReadDir(filepath.Join(root, group))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, group, e.Name(), "go.mod")); err == nil {
				out = append(out, group+"/"+e.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}

// scanActions 解析模块下所有非测试 .go 文件，收集 provides 声明、Handle 注册与"源码里出现过的动作名字面量"。
func scanActions(t *testing.T, dir string) (declared, handled, mentioned map[string]bool) {
	t.Helper()
	declared, handled, mentioned = map[string]bool{}, map[string]bool{}, map[string]bool{}
	// 直接传 helper 返回值的注册（如 `s.HandleNative(contextMethod(), ...)`）：
	// 先记下 helper 名，扫完整个模块后从它的函数体里回填动作名。
	pendingHelpers := map[string]bool{}
	helperMethods := map[string][]string{}
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
		// s.Handle("x", ...) / s.HandleNative(spv2.Method{Name: "x"}, ...)：
		// 注册动作（HandleSetting / HandleReadOnly 不是动作）。
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			switch sel.Sel.Name {
			case "Handle":
				// 兼容 spv2.Server 的 mux 式注册（settings-center 等自派发模块）。
				if v, ok := stringLit(call.Args[0]); ok {
					// mux.Handle("/spv2", ...) 是 HTTP 路由，不是服务动作。
					if strings.HasPrefix(v, "/") {
						return true
					}
					handled[v] = true
				}
			case "HandleNative":
				lit, ok := call.Args[0].(*ast.CompositeLit)
				if !ok {
					// 直接把 helper 返回值注册进来（如 facade 的 contextMethod()）：
					// 名字先记下，等整个模块扫完再从函数体回填。
					if name, ok := directCallName(call.Args[0]); ok {
						pendingHelpers[name] = true
					}
					return true
				}
				for _, field := range lit.Elts {
					kv, ok := field.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok || key.Name != "Name" {
						continue
					}
					if v, ok := stringLit(kv.Value); ok {
						handled[v] = true
					}
				}
			}
			return true
		})
		// helper 函数体里的 spv2.Method{Name: "x"}：供上面的 pendingHelpers 回填。
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			helperMethods[fd.Name.Name] = append(helperMethods[fd.Name.Name], methodNameLits(fd.Body)...)
		}
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
	for name := range pendingHelpers {
		for _, action := range helperMethods[name] {
			handled[action] = true
		}
	}
	return declared, handled, mentioned
}

// directCallName 取 `f(...)` 形式的被调函数名；选择器（如 pkg.F）与变量表达式不解析。
func directCallName(e ast.Expr) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// methodNameLits 返回节点里 `spv2.Method{Name: "x", ...}` 字面量的动作名。
func methodNameLits(n ast.Node) []string {
	var out []string
	ast.Inspect(n, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isMethodType(lit.Type) {
			return true
		}
		for _, field := range lit.Elts {
			kv, ok := field.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Name" {
				continue
			}
			if v, ok := stringLit(kv.Value); ok {
				out = append(out, v)
			}
		}
		return true
	})
	return out
}

// isMethodType 判断类型是不是 `Method` / `spv2.Method`。
func isMethodType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "Method"
	case *ast.SelectorExpr:
		return t.Sel.Name == "Method"
	}
	return false
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
