package server

// 执行结果文案的包内测试：这些分支在真实测试里难以稳定触发（比如 taskkill 失败），
// 但文案一旦说错就是“把没杀掉的进程说成已终止”——直接钉住字面。

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

func TestRenderDoneTimeoutTerminated(t *testing.T) {
	j := &job{command: "sleep 999", workDir: "/tmp"}
	text := renderDone(j, protocol.ShellEventData{TimedOut: true, ExitCode: -1, DurationMS: 1000, Shell: "bash"}, 1024)
	if !strings.Contains(text, "命令超时并被终止") {
		t.Fatalf("终止成功应写“被终止”: %q", text)
	}
}

func TestRenderDoneTimeoutKillFailed(t *testing.T) {
	j := &job{command: "sleep 999", workDir: "/tmp"}
	reason := "taskkill: 拒绝访问"
	text := renderDone(j, protocol.ShellEventData{
		TimedOut: true, ExitCode: -1, DurationMS: 1000, Shell: "bash", Error: reason,
	}, 1024)
	if strings.Contains(text, "并被终止") {
		t.Fatalf("终止失败不得宣称“被终止”: %q", text)
	}
	if !strings.Contains(text, "未能终止") || !strings.Contains(text, reason) {
		t.Fatalf("应说明未能终止并附原因原文: %q", text)
	}
}

// TestDecodeOutput：子进程输出解码。Windows PowerShell 5.1 在中文系统上按 GBK 写 stdout，
// 直接当 UTF-8 解会把每个汉字换成替换符（实测：`Get-Date` 变成 `2026?10?17?`）。
// 三条路径用确定字节钉住：合法 UTF-8 原样 / 截断切在多字节中间 / GBK 转码。
func TestDecodeOutput(t *testing.T) {
	// 1) 本来就是合法 UTF-8：一个字节都不该动
	if got := decodeOutput([]byte("中文 ok")); got != "中文 ok" {
		t.Fatalf("合法 UTF-8 被改动了: %q", got)
	}
	// 2) 截断切在多字节字符中间（「中」3 字节 + 「文」的第 1 字节）：保留完整的那个字
	if got := decodeOutput([]byte("中文")[:4]); got != "中" {
		t.Fatalf("截断残尾处理不对: %q", got)
	}
	// 3) GBK 字节（中=D6D0 文=CEC4）：只有 Windows 按代码页输出时才走这条路
	if filepath.Separator == '\\' {
		gbk := []byte{0xD6, 0xD0, 0xCE, 0xC4}
		if got := decodeOutput(gbk); got != "中文" {
			t.Fatalf("GBK 解码不对: %q（应得到「中文」）", got)
		}
		if strings.ContainsRune(decodeOutput(gbk), 0xFFFD) {
			t.Fatal("GBK 解码里出现了替换符")
		}
		if got := decodeOutput([]byte{0xD6, 0xD0, 0xCE}); got != "中" {
			t.Fatalf("GBK 截断处理不对: %q", got)
		}
	}
}
