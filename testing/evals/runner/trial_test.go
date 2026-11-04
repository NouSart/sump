package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NouSart/sump/evals/dataset"
	"github.com/NouSart/sump/evals/fakeopenai"
)

func tmpWork(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// TestEvaluateFileAssertions：文件存在/内容/不存在三种断言都要真的看磁盘。
func TestEvaluateFileAssertions(t *testing.T) {
	dir := tmpWork(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Result{WorkDir: dir}

	ok := dataset.Task{Expect: []dataset.Expectation{
		{FileExists: "a.txt"},
		{FileContains: &dataset.FileContains{Path: "a.txt", Text: "hell"}},
		{FileAbsent: "b.txt"},
	}}
	if got := evaluate(ok, res, nil); len(got) != 0 {
		t.Fatalf("应全部通过，实际失败项: %v", got)
	}

	bad := dataset.Task{Expect: []dataset.Expectation{
		{FileExists: "b.txt"},
		{FileContains: &dataset.FileContains{Path: "a.txt", Text: "world"}},
		{FileContains: &dataset.FileContains{Path: "missing.txt", Text: "x"}},
		{FileAbsent: "a.txt"},
	}}
	got := evaluate(bad, res, nil)
	if len(got) != 4 {
		t.Fatalf("应有 4 项失败，实际 %d: %v", len(got), got)
	}
}

// TestEvaluateMemoryAndDeliver：记忆断言与交付断言。
func TestEvaluateMemoryAndDeliver(t *testing.T) {
	hist := []HistoryEntry{
		{Kind: "tool_call", Content: "Set-Content a.txt hello"},
		{Kind: "tool_result", Content: "命令执行完成（退出码 0）"},
	}
	res := Result{WorkDir: t.TempDir(), Delivered: true, Answer: []string{"已创建 a.txt"}, FailureNotified: true}

	ok := dataset.Task{Expect: []dataset.Expectation{
		{MemoryHas: &dataset.MemoryHas{Kind: "tool_result", Contains: "退出码 0"}},
		{DeliverContains: "a.txt"},
		{FailureNotified: &res.FailureNotified},
	}}
	if got := evaluate(ok, res, hist); len(got) != 0 {
		t.Fatalf("应全部通过: %v", got)
	}

	bad := dataset.Task{Expect: []dataset.Expectation{
		{MemoryHas: &dataset.MemoryHas{Kind: "reasoning", Contains: "退出码 0"}},
		{DeliverContains: "不存在的话"},
		{Delivered: &[]bool{false}[0]},
	}}
	if got := evaluate(bad, res, hist); len(got) != 3 {
		t.Fatalf("应有 3 项失败，实际 %d: %v", len(got), got)
	}
}

// TestEvaluateGateMetrics：门禁/闸门这类"harness 自己产生的事实"也要能被断言。
func TestEvaluateGateMetrics(t *testing.T) {
	res := Result{WorkDir: t.TempDir(), Reminders: 1, GateRejects: 2, Hops: 5}
	ok := dataset.Task{Expect: []dataset.Expectation{
		{RemindersAtLeast: 1},
		{GateRejectsAtLeast: 2},
		{HopsAtMost: 5},
		{NoToolFailure: true}, // GateRejects>0 时这条必须失败
	}}
	got := evaluate(ok, res, nil)
	if len(got) != 1 || !strings.Contains(got[0], "闸门拒绝") {
		t.Fatalf("只有 no_tool_failure 应失败，实际 %v", got)
	}

	good := Result{WorkDir: t.TempDir(), Reminders: 2, GateRejects: 0, Hops: 3}
	task := dataset.Task{Expect: []dataset.Expectation{
		{RemindersAtLeast: 1}, {GateRejectsAtLeast: 0}, {HopsAtMost: 4}, {NoToolFailure: true},
	}}
	if got := evaluate(task, good, nil); len(got) != 0 {
		t.Fatalf("应全部通过: %v", got)
	}
}

// TestEvaluateTooFewReminders：催办次数不够必须判失败（防止"没催也放行"）。
func TestEvaluateTooFewReminders(t *testing.T) {
	res := Result{WorkDir: t.TempDir(), Reminders: 0}
	task := dataset.Task{Expect: []dataset.Expectation{{RemindersAtLeast: 2}}}
	got := evaluate(task, res, nil)
	if len(got) != 1 || !strings.Contains(got[0], "只催了 0 次") {
		t.Fatalf("应指出催办次数不足: %v", got)
	}
}

// TestFinishNegativeControl：负向对照的语义——
// 必须被判失败的任务，真的失败了才算"评测器没被糊弄"，并把它记成通过。
func TestFinishNegativeControl(t *testing.T) {
	task := dataset.Task{MustFail: true}

	failed := finish(task, Result{Success: false, FailReason: "文件不存在: a.txt"})
	if !failed.Success || !failed.NegativeControl {
		t.Fatalf("负向对照被正确判失败时应记通过: %+v", failed)
	}
	if failed.FailReason != "" {
		t.Fatalf("通过时不该保留失败原因: %q", failed.FailReason)
	}
	if failed.ExpectFailReason != "文件不存在: a.txt" {
		t.Fatalf("原始失败原因应留作证据，实际 %q", failed.ExpectFailReason)
	}

	fooled := finish(task, Result{Success: true})
	if fooled.Success || fooled.NegativeControl {
		t.Fatalf("负向对照被判成功时说明评测器被糊弄了: %+v", fooled)
	}
	if !strings.Contains(fooled.FailReason, "负向对照失效") {
		t.Fatalf("应明确指出负向对照失效: %q", fooled.FailReason)
	}
}

// TestMarkInvalid：「声明了故障却没命中」必须记成无效，而且不能被负向对照倒置成通过。
func TestMarkInvalid(t *testing.T) {
	res := Result{Success: true}
	markInvalid(&res, []string{"boom"})
	if !res.Invalid || res.Success {
		t.Fatalf("应记为无效且不通过: %+v", res)
	}
	if !strings.Contains(res.FailReason, "注入无效") || !strings.Contains(res.FailReason, "boom") {
		t.Fatalf("失败原因应指明注入无效与步骤名: %q", res.FailReason)
	}
	got := finish(dataset.Task{MustFail: true}, res)
	if got.Success || got.NegativeControl {
		t.Fatalf("注入无效时不允许被倒置成通过: %+v", got)
	}
}

// TestSplitDeliveries：失败通知必须从"答复"里摘出来——
// 一次 LLM 500 不能被记成"成功交付"。
func TestSplitDeliveries(t *testing.T) {
	answers, notified := splitDeliveries([]string{
		"⚠️ 本轮推理失败：LLM 调用失败\n任务还开着（没有关闭）",
	})
	if len(answers) != 0 || !notified {
		t.Fatalf("只有失败通知时不该有答复: %v %v", answers, notified)
	}

	answers, notified = splitDeliveries([]string{"已创建 a.txt。", "[交付失败] 送不回去"})
	if len(answers) != 1 || answers[0] != "已创建 a.txt。" || !notified {
		t.Fatalf("应保留答复并标出通知: %v %v", answers, notified)
	}

	answers, notified = splitDeliveries([]string{"已创建 a.txt。"})
	if len(answers) != 1 || notified {
		t.Fatalf("正常交付不该被标成失败: %v %v", answers, notified)
	}

	if answers, notified := splitDeliveries(nil); len(answers) != 0 || notified {
		t.Fatalf("没有交付时应为空: %v %v", answers, notified)
	}
}

// TestContractViolations：门禁契约的四种组合都要按规则判。
func TestContractViolations(t *testing.T) {
	cases := []struct {
		delivered, judged bool
		reminders         int
		want              bool
	}{
		{true, false, 0, true},   // 交付了、没判定、也没催 —— 破约
		{true, false, 2, false},  // 催过两次仍交付：设计允许
		{true, true, 0, false},   // 判定过
		{false, false, 0, false}, // 没交付
	}
	for i, c := range cases {
		got := contractViolations(c.delivered, c.judged, c.reminders)
		if (len(got) > 0) != c.want {
			t.Fatalf("case %d（delivered=%v judged=%v reminders=%d）应违规=%v，实际 %v",
				i, c.delivered, c.judged, c.reminders, c.want, got)
		}
	}
}

// TestSplitSettingKey：消融键必须带服务名，否则不知道该推给谁。
func TestSplitSettingKey(t *testing.T) {
	svc, key, err := splitSettingKey("reasoner.tool.notify_mode")
	if err != nil || svc != "reasoner" || key != "tool.notify_mode" {
		t.Fatalf("拆分结果不对: %q %q %v", svc, key, err)
	}
	// 设置项本身带点：只按第一个点切，不能把 tool.notify_mode 当成服务名。
	if svc, key, err := splitSettingKey("approval.auto_approve"); err != nil || svc != "approval" || key != "auto_approve" {
		t.Fatalf("第二个键拆分不对: %q %q %v", svc, key, err)
	}
	for _, bad := range []string{"", ".x", "svc."} {
		if _, _, err := splitSettingKey(bad); err == nil {
			t.Fatalf("应拒绝 %q（缺服务名或缺设置项）", bad)
		}
	}
	// 只带一级点但对不上真实服务名的键在语法上是合法的：服务是否存在由设置中心判定，
	// 拆键这一层不猜（它没有名册）。
	if svc, _, err := splitSettingKey("tool.notify_mode"); err != nil || svc != "tool" {
		t.Fatalf("语法合法的键应原样拆开: %q %v", svc, err)
	}
}

// TestClaimPort：同一个端口号只能被一次 run 认领。
func TestClaimPort(t *testing.T) {
	const p = 1 // 最小端口号：FreePort 不会发它，测试用不冲突
	if !claimPort(p) {
		t.Fatal("首次认领应成功")
	}
	if claimPort(p) {
		t.Fatal("同一端口不得被认领两次（两个服务会抢一个端口）")
	}
}

// TestFinishLeavesNormalTaskAlone：普通任务不受负向对照逻辑影响。
func TestFinishLeavesNormalTaskAlone(t *testing.T) {
	if got := finish(dataset.Task{}, Result{Success: true}); !got.Success || got.NegativeControl {
		t.Fatalf("普通任务应原样返回: %+v", got)
	}
	if got := finish(dataset.Task{}, Result{Success: false, FailReason: "x"}); got.Success {
		t.Fatalf("失败应保持失败: %+v", got)
	}
}

// TestTaskExpectsDelivery：只有显式要求交付的任务才在"没交付"时判失败，
// 否则故障类任务（预期就是不交付）会被误判。
func TestTaskExpectsDelivery(t *testing.T) {
	if taskExpectsDelivery(dataset.Task{Expect: []dataset.Expectation{{Delivered: ptr(false)}}}) {
		t.Fatal("delivered=false 的任务不该要求交付")
	}
	if !taskExpectsDelivery(dataset.Task{Expect: []dataset.Expectation{{Delivered: ptr(true)}}}) {
		t.Fatal("delivered=true 的任务应要求交付")
	}
	if !taskExpectsDelivery(dataset.Task{Expect: []dataset.Expectation{{DeliverContains: "x"}}}) {
		t.Fatal("deliver_contains 的任务应要求交付")
	}
	if taskExpectsDelivery(dataset.Task{Expect: []dataset.Expectation{{NoToolFailure: true}}}) {
		t.Fatal("只断言工具的任务不该要求交付")
	}
}

func ptr(b bool) *bool { return &b }

// TestIsFailureNotice：失败通知不能被统计成"成功交付"。
func TestIsFailureNotice(t *testing.T) {
	yes := []string{
		"⚠️ 本轮推理失败：LLM 调用失败\n任务还开着（没有关闭）",
		"[交付失败] 上一轮任务的结果没能送回来",
	}
	for _, s := range yes {
		if !isFailureNotice(s) {
			t.Fatalf("应识别为失败通知: %q", s)
		}
	}
	for _, s := range []string{"已创建 a.txt。", ""} {
		if isFailureNotice(s) {
			t.Fatalf("不该识别为失败通知: %q", s)
		}
	}
}

// TestJudgeSeen：判定证据来自 harness 发出的回执，而不是模型自述。
func TestJudgeSeen(t *testing.T) {
	if judgeSeen(nil) {
		t.Fatal("没有请求时不该认为判定过")
	}
	if judgeSeen([]fakeopenai.Record{{JudgeSeen: false}}) {
		t.Fatal("没有判定回执时不该认为判定过")
	}
	if !judgeSeen([]fakeopenai.Record{{JudgeSeen: false}, {JudgeSeen: true}}) {
		t.Fatal("任一轮看到判定回执就算判定过")
	}
}

// TestFreePortUnique：同一次 run 里端口不能重复发（两个服务抢一个端口的经典故障）。
func TestFreePortUnique(t *testing.T) {
	seen := map[int]bool{}
	for i := 0; i < 30; i++ {
		p, err := FreePort()
		if err != nil {
			t.Fatal(err)
		}
		if seen[p] {
			t.Fatalf("端口 %d 重复发放", p)
		}
		seen[p] = true
	}
}

// TestSourceFingerprintChangesWithCode：源码指纹必须随内容变化（否则会拿旧二进制测新代码）。
func TestSourceFingerprintChangesWithCode(t *testing.T) {
	root := t.TempDir()
	svc := filepath.Join(root, "svc")
	if err := os.MkdirAll(svc, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(svc, "main.go")
	if err := os.WriteFile(f, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := sourceFingerprint(root, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("package main\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := sourceFingerprint(root, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("源码变了指纹必须变")
	}
	// 内容不变时两次必须一致（缓存才有意义）。
	again, err := sourceFingerprint(root, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if again != second {
		t.Fatal("内容不变时指纹应稳定")
	}
}
