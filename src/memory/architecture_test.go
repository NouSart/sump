package main

// 架构测试：三条 import 规则不靠自觉，靠编译/测试钉住。
//   - session 只能依赖自己的 store；
//   - experience 只能依赖 experience/*；
//   - facade 只能依赖 facade/compose。
// 共享物只允许 protocol / spv2 / service（以及标准库）。

import (
	"os/exec"
	"strings"
	"testing"
)

func importsOf(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, pkg).Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pkg, err)
	}
	return strings.Fields(string(out))
}

func TestDomainImportBoundaries(t *testing.T) {
	cases := []struct {
		pkg     string
		allowed []string
	}{
		{"./session", []string{"github.com/NouSart/sump/memory/session/store"}},
		{"./experience", []string{
			"github.com/NouSart/sump/memory/experience/compose",
			"github.com/NouSart/sump/memory/experience/longterm",
			"github.com/NouSart/sump/memory/experience/memnet",
			"github.com/NouSart/sump/memory/experience/redact",
			"github.com/NouSart/sump/memory/experience/shared",
		}},
		{"./facade", []string{"github.com/NouSart/sump/memory/facade/compose"}},
	}
	for _, c := range cases {
		allowed := map[string]bool{}
		for _, p := range c.allowed {
			allowed[p] = true
		}
		for _, imp := range importsOf(t, c.pkg) {
			if !strings.HasPrefix(imp, "github.com/NouSart/sump/memory/") {
				continue
			}
			if !allowed[imp] {
				t.Errorf("%s 不允许 import %s（域边界被打破）", c.pkg, imp)
			}
		}
	}
}
