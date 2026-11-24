package assemble

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSourcesStayPure 是纯函数边界的闸门（R-011 §2 / sump-v2-memory-boundary.md §2.3）：
//   - 只允许标准库（白名单）与 github.com/NouSart/sump/protocol；
//   - 禁止 database/sql、文件/网络/随机数（白名单天然挡住）；
//   - 禁止 time.Now()（纯函数只认 in.Now）。
func TestSourcesStayPure(t *testing.T) {
	// 实现文件当前实际用到的标准库；新增依赖必须显式过评审再改这里。
	allowedStd := map[string]bool{
		"fmt":     true,
		"sort":    true,
		"strings": true,
		"time":    true, // 只用于 Input.Now 的类型，不允许取墙钟
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("列出源文件失败: %v", err)
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", name, err)
		}
		checked++

		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s 的 import 路径无法解析: %v", name, err)
			}
			if path == "github.com/NouSart/sump/protocol" {
				continue
			}
			if !allowedStd[path] {
				t.Errorf("%s 引入了不允许的包 %q（只允许标准库白名单 + protocol）", name, path)
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" && sel.Sel.Name == "Now" {
				t.Errorf("%s 调用了 time.Now（纯函数不允许读墙钟，需要时间就用 in.Now）", name)
			}
			return true
		})
	}
	if checked < 4 {
		t.Fatalf("只检查了 %d 个实现文件，glob 可能没匹配到预期文件", checked)
	}
}
