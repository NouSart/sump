package server

// 执行结果文案的包内测试：这些分支在真实测试里难以稳定触发（比如 taskkill 失败），
// 但文案一旦说错就是“把没杀掉的进程说成已终止”——直接钉住字面。

import (
	"strings"
	"testing"

	"github.com/QAQ-awa-QAQ/sump/protocol"
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
