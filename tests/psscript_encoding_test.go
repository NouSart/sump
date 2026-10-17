// 仓库脚本的编码护栏：含非 ASCII 的 .ps1 必须带 UTF-8 BOM。
//
// 为什么要有这条：Windows PowerShell 5.1（本项目的开发平台）读**无 BOM** 的 UTF-8 文件会按
// 系统 ANSI 代码页（中文机器上是 GBK）解释——脚本里的中文注释/提示全变乱码，引号一旦失配，
// 整个脚本**解析失败**。实测事故：`scripts/run-local.ps1` 在一次合流解冲突时丢了 BOM，
// 「一键启动」在 Windows 上直接跑不起来，而 CI 与 pwsh（PS 7，默认按 UTF-8 读）都看不见。
//
// 这条检查只看**仓库跟踪**的 .ps1（`git ls-files`），所以 .venv / node_modules 里的第三方脚本不参与。
package tests

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// ps1EncodingExempt 是「只在内存里执行、PowerShell 从不按文件读」的脚本，磁盘编码对它们没有意义。
// 例外必须是具名的：往这里加条目等于放宽规则，得说清理由。
var ps1EncodingExempt = map[string]string{
	// //go:embed 后经 -EncodedCommand 以 UTF-16LE 传给 PowerShell（approval/analysis/powershell.go
	// encodedScript），脚本正文不落盘。给它加 BOM 反而会把 U+FEFF 塞进脚本正文。
	"approval/analysis/parse.ps1": "embed + -EncodedCommand(UTF-16LE)，不作为文件读取",
}

// TestPowerShellScriptsKeepBOM 扫全部被跟踪的 .ps1：含非 ASCII 的必须带 BOM。
func TestPowerShellScriptsKeepBOM(t *testing.T) {
	root := repoRoot(t)
	// -C root：让 ls-files 以仓库根为基准（否则只列当前目录下的路径）
	out, err := exec.Command("git", "-C", root, "ls-files", "*.ps1").Output()
	if err != nil {
		t.Fatalf("git ls-files 失败: %v", err)
	}
	files := strings.Fields(strings.ReplaceAll(string(out), "\r\n", "\n"))
	if len(files) == 0 {
		t.Fatal("没扫到任何 .ps1——检查逻辑本身出问题了")
	}

	checked, exempt := 0, 0
	for _, rel := range files {
		if _, ok := ps1EncodingExempt[rel]; ok {
			exempt++
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", rel, err)
		}
		if !hasNonASCII(data) {
			continue // 纯 ASCII 的脚本不受代码页影响
		}
		checked++
		if !bytes.HasPrefix(data, utf8BOM) {
			t.Errorf("%s 含非 ASCII 但没有 UTF-8 BOM：Windows PowerShell 5.1 会按 GBK 读 → "+
				"中文乱码、引号失配、脚本解析失败（合流/改写时很容易丢，请用带 BOM 的 UTF-8 保存）", rel)
		}
	}
	if exempt != len(ps1EncodingExempt) {
		t.Errorf("例外表里有 %d 条，实际只匹配到 %d 个被跟踪脚本——路径写错了或文件没了，别让例外变成僵尸",
			len(ps1EncodingExempt), exempt)
	}
	if checked == 0 {
		t.Fatal("含非 ASCII 的 .ps1 一个都没扫到——护栏没起作用，检查一下扫描逻辑")
	}
	t.Logf("已检查 %d 个含非 ASCII 的 .ps1（共 %d 个被跟踪脚本，%d 个例外）", checked, len(files), exempt)
}

// hasNonASCII 判断内容里是否有非 ASCII 字符（BOM 本身不算）。
func hasNonASCII(data []byte) bool {
	body := bytes.TrimPrefix(data, utf8BOM)
	for _, b := range body {
		if b > 127 {
			return true
		}
	}
	return false
}
