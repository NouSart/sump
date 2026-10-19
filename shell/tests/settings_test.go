// 设置项校验（configure 直达骨架）：越界 / 非法值一律拒绝，服务保持旧值。
package tests

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/shell/server"
)

// TestSettingsValidation：逐键验证“校验权在服务”——非法值被拒且不生效。
func TestSettingsValidation(t *testing.T) {
	root := t.TempDir()
	sc := startStubCenter(t)
	startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: root, Approver: allowAll{}})
	c := dial(t, s.WsURL())

	cases := []struct {
		key   string
		value string
		ok    bool
	}{
		{"exec.root", root, true},
		{"exec.root", root + string('/') + "nope", false}, // 不存在
		{"exec.shell", "definitely-not-a-shell-" + strings.Repeat("x", 8), false},
		{"exec.timeout_ms", "99", false}, // 小于 1000ms
		{"exec.timeout_ms", "2000", true},
		{"exec.max_output_kb", "0", false},
		{"exec.max_output_kb", "1", true},
		{"exec.max_running", "0", false},
		{"exec.max_running", "2", true},
		{"approval.service", "approval", true}, // 已接入第一层命令审批协议。
		{"approval.service", "invalid service!", false},
		{"approval.service", "", true},
		{"approval.timeout_ms", "10", false},
		{"unknown.key", "1", false}, // 未声明的设置项
	}
	for _, tc := range cases {
		rp := jump(t, c, "shell", "configure", protocol.ConfigurePayload{Key: tc.key, Value: tc.value})
		if rp.OK != tc.ok {
			t.Errorf("configure %s=%q: ok=%v, want %v（err=%s）", tc.key, tc.value, rp.OK, tc.ok, rp.Error)
		}
	}

	// 生效值随最新一次成功下发变化（校验不过的值不落地）。
	if got := s.SettingValue("exec.max_output_kb"); got != "1" {
		t.Fatalf("exec.max_output_kb 生效值 = %q, want 1", got)
	}
	if got := s.SettingValue("exec.timeout_ms"); got != "2000" {
		t.Fatalf("exec.timeout_ms 生效值 = %q, want 2000", got)
	}
}

// TestOutputLimitSnapshotOncePerRun：单次执行只读一次输出上限——执行中途改小上限，
// 本次执行的“实际截断”与“补报文案里的数字”仍用同一个口径（执行开始时的快照）；
// 不得出现“正文有 64KB、文案却说保留前 1024 字节”的自相矛盾。
// 回归背景：写入与文案原是两次读取（中间隔着整条命令的执行），中途改设置会让两者打架。
func TestOutputLimitSnapshotOncePerRun(t *testing.T) {
	sh := outputShell(t)
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{
		Root: t.TempDir(), Shell: sh, Approver: allowAll{}, MaxOutputBytes: 64 << 10,
	})
	c := dial(t, s.WsURL())

	// 输出 70000+ 字节（> 64KB 上限 → 必截断）后停 1 秒：给测试留出“执行途中改设置”的窗口。
	run(t, c, "yes | head -c 70000; sleep 1", "")
	waitReport(t, cl, "approved")
	time.Sleep(300 * time.Millisecond)
	if rp := jump(t, c, "shell", "configure", protocol.ConfigurePayload{Key: "exec.max_output_kb", Value: "1"}); !rp.OK {
		t.Fatalf("下发 exec.max_output_kb=1 失败: %s", rp.Error)
	}

	p := waitReport(t, cl, "done")
	d := eventData(t, p)
	if !d.Truncated || d.StdoutBytes <= 64<<10 || len(d.Stdout) != 64<<10 {
		t.Fatalf("截断口径不符: truncated=%v stdout_bytes=%d len=%d", d.Truncated, d.StdoutBytes, len(d.Stdout))
	}
	if !strings.Contains(p.Text, "保留前 65536 字节") {
		t.Fatalf("文案应使用本次执行的快照（65536）: %q", p.Text)
	}
	if strings.Contains(p.Text, "保留前 1024 字节") {
		t.Fatalf("文案不得使用中途改后的新值: %q", p.Text)
	}
}

// outputShell 找一条 POSIX shell（用例需要管道 / head / sleep）；找不到就跳过。
func outputShell(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"bash", "sh"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("找不到 POSIX shell，跳过输出上限快照用例")
	return ""
}

// TestOutputLimitTakesEffectLive：改小输出上限后，下一条命令即按新上限截断（设置下发即时生效）。
func TestOutputLimitTakesEffectLive(t *testing.T) {
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Approver: allowAll{}})
	c := dial(t, s.WsURL())

	if rp := jump(t, c, "shell", "configure", protocol.ConfigurePayload{Key: "exec.max_output_kb", Value: "1"}); !rp.OK {
		t.Fatalf("下发 exec.max_output_kb=1 失败: %s", rp.Error)
	}
	run(t, c, echoCommand(strings.Repeat("y", 3000)), "")
	waitReport(t, cl, "approved")
	p := waitReport(t, cl, "done")
	d := eventData(t, p)
	if !d.Truncated || len(d.Stdout) > 1024 {
		t.Fatalf("应按新上限截断: truncated=%v len=%d", d.Truncated, len(d.Stdout))
	}
}
