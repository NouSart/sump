// 设置项校验（configure 直达骨架）：越界 / 非法值一律拒绝，服务保持旧值。
package tests

import (
	"strings"
	"testing"

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
		{"exec.concurrency", "0", false},
		{"exec.concurrency", "2", true},
		{"exec.queue_limit", "0", false},
		{"exec.queue_limit", "4", true},
		{"approval.service", "approval", false}, // 协议未定：只接受空值
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
