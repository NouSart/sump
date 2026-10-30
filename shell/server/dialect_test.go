package server

// POSIX shell 的方言名必须原样进 plan（bash/sh/dash/zsh/ksh），
// approval 侧再按 bash 族接受；两边任何一处改名都会在这里露馅。

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestPosixShellDialectsPreserved(t *testing.T) {
	root := t.TempDir()
	names := []string{"bash", "sh", "dash", "zsh", "ksh"}
	for _, name := range names {
		fileName := name
		if runtime.GOOS == "windows" {
			fileName += ".exe"
		}
		if err := os.WriteFile(filepath.Join(root, fileName), []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", root)
	s := New(Config{Name: "shell", Root: root, ApprovalService: "approval"}, log.New(io.Discard, "", 0))

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			s.shell = name
			plan, _, err := s.preparePlan(&job{command: "pwd", workDir: root})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Dialect != name {
				t.Fatalf("dialect 应保留解释器名 %q，实际 %q", name, plan.Dialect)
			}
			if !slices.Equal(plan.Args, []string{"-c"}) {
				t.Fatalf("POSIX shell 参数应为 -c: %v", plan.Args)
			}
			if plan.Profile == "" {
				t.Fatal("profile 不能为空（当前非 PowerShell 用 unsupported 占位，后续可换 posix 约定）")
			}
		})
	}
}
