package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestParseSettings：消融项的键必须是 <服务>.<项>——没有服务名就不知道该推给谁。
func TestParseSettings(t *testing.T) {
	got, err := parseSettings([]string{"tool.notify_mode=batch", " tool.timeout_ms = 1000 "})
	if err != nil {
		t.Fatal(err)
	}
	if got["tool.notify_mode"] != "batch" {
		t.Fatalf("解析结果不对: %+v", got)
	}
	if got["tool.timeout_ms"] != "1000" {
		t.Fatalf("应去掉空白: %+v", got)
	}
}

// TestParseSettingsRejectsBadFormat：格式错就报错，不能猜。
func TestParseSettingsRejectsBadFormat(t *testing.T) {
	for _, bad := range []string{"没有等号", "=只给值"} {
		if _, err := parseSettings([]string{bad}); err == nil {
			t.Fatalf("应拒绝 %q", bad)
		}
	}
}

// TestParseSettingsEmpty：没有 -set 时返回 nil（manifest 里不写空对象）。
func TestParseSettingsEmpty(t *testing.T) {
	got, err := parseSettings(nil)
	if err != nil || got != nil {
		t.Fatalf("空输入应返回 nil: %v %v", got, err)
	}
}

// datasetNames 列出 evals/dataset 下的任务集名。
func datasetNames(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "evals", "dataset"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".jsonl"))
	}
	sort.Strings(out)
	return out, nil
}

// TestSuitePresetsCoverDatasets：代码里声明的任务集与 dataset 目录下的文件必须对得上。
// 这条防的是"数据集加了、跑法没登记"——那样 run 会静默少起服务，任务全红却像代码问题。
func TestSuitePresetsCoverDatasets(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skip("不在仓库内运行，跳过")
	}
	files, err := datasetNames(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if _, ok := suitePresets[name]; !ok {
			t.Fatalf("任务集 %q 有数据集文件但没在 suitePresets 里登记跑法", name)
		}
	}
	for name := range suitePresets {
		found := false
		for _, f := range files {
			if f == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("suitePresets 里的 %q 没有对应的数据集文件", name)
		}
	}
}

// TestShellPresetPinsPolicy：shell 任务集必须钉死审批策略，
// 否则"能不能写文件"取决于默认策略恰好覆盖了哪个 cmdlet（结果不可比）。
func TestShellPresetPinsPolicy(t *testing.T) {
	p, ok := suitePresets["shell"]
	if !ok {
		t.Fatal("缺少 shell 任务集跑法")
	}
	if p.PolicyPath == "" {
		t.Fatal("shell 任务集必须钉死 policy 文件")
	}
	if !strings.Contains(strings.Join(p.Services, ","), "approval") {
		t.Fatal("shell 任务集必须起 approval（否则命令根本没被审批过）")
	}
	if p.Settings["approval.auto_approve"] == "" {
		t.Fatal("shell 任务集必须显式固定审批档位")
	}
}

// TestNodepPresetSkipsMemory：依赖缺席任务集必须真的不起 memory。
func TestNodepPresetSkipsMemory(t *testing.T) {
	if !suitePresets["nodep"].NoMemory {
		t.Fatal("nodep 任务集应配置为不启动 memory")
	}
}

// TestRunEndToEnd 直接驱动 CLI 主流程（真进程 + 假模型），确认产物齐全：
// 报告、CSV、证据包、manifest，以及"全绿时退出码 0"。
// 这条覆盖的是 main 流程本身——只靠 `go run` 手工验证的话，回归不会有任何提示。
func TestRunEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过需要真进程的用例")
	}
	out := t.TempDir()
	code := run([]string{"-suite", "smoke", "-trials", "1", "-out", out, "-only", "smoke-negctl"})
	if code != 0 {
		t.Fatalf("退出码 %d, want 0", code)
	}
	runs, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("应产出 1 个 run 目录，实际 %d", len(runs))
	}
	dir := filepath.Join(out, runs[0].Name())
	for _, name := range []string{"report.md", "results.csv"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s 没生成: %v", name, err)
		}
	}
	traces, err := os.ReadDir(filepath.Join(dir, "traces"))
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 {
		t.Fatalf("证据包应有 1 份，实际 %d", len(traces))
	}
	body, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	// 负向对照必须在报告里被单独标出，不能混在能力里。
	if !strings.Contains(string(body), "对照任务") {
		t.Fatal("报告应单独列出对照任务")
	}
	if !strings.Contains(string(body), runs[0].Name()) {
		t.Fatal("报告应带上 run_id")
	}
}

// TestRunBaselineFlow 覆盖基线流程：存基线 → 同配置复跑判定"未超出噪声带" → 换套件判"不可比"。
// 这条同时验证 manifest.json / summary.json 落盘与 baseline.json 回读。
func TestRunBaselineFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过需要真进程的用例")
	}
	out := t.TempDir()
	if code := run([]string{"-suite", "smoke", "-trials", "1", "-out", out, "-only", "smoke-negctl", "-save-baseline"}); code != 0 {
		t.Fatalf("存基线那次退出码 %d", code)
	}
	if _, err := os.Stat(filepath.Join(out, "baseline.json")); err != nil {
		t.Fatalf("baseline.json 没生成: %v", err)
	}

	if code := run([]string{"-suite", "smoke", "-trials", "1", "-out", out, "-only", "smoke-negctl", "-baseline", "baseline.json"}); code != 0 {
		t.Fatalf("对比那次退出码 %d", code)
	}
	// 最近一次 run 的报告必须带上对比小节，并说明噪声带内的差异不算改进。
	runs := runDirs(t, out)
	body := readReport(t, filepath.Join(out, runs[len(runs)-1]))
	if !strings.Contains(body, "与基线对比") {
		t.Fatal("报告应包含与基线对比小节")
	}
	if !strings.Contains(body, "未超出噪声带") {
		t.Fatal("k=1 且同配置时应判未超出噪声带")
	}
	// manifest/summary 必须落盘（下一次 run 靠它当基线）。
	for _, name := range []string{"manifest.json", "summary.json"} {
		if _, err := os.Stat(filepath.Join(out, runs[len(runs)-1], name)); err != nil {
			t.Fatalf("%s 没生成: %v", name, err)
		}
	}

	// 换套件：必须判不可比，而不是硬比。
	if code := run([]string{"-suite", "gate", "-trials", "1", "-out", out, "-baseline", "baseline.json"}); code != 0 {
		t.Fatalf("跨套件那次退出码 %d", code)
	}
	runs = runDirs(t, out)
	body = readReport(t, filepath.Join(out, runs[len(runs)-1]))
	if !strings.Contains(body, "不可比") {
		t.Fatal("跨套件对比必须判不可比")
	}
}

// TestRunRejectsMissingBaseline：基线文件不存在要报错，不能悄悄当成"没有基线"。
func TestRunRejectsMissingBaseline(t *testing.T) {
	out := t.TempDir()
	if code := run([]string{"-suite", "smoke", "-trials", "1", "-out", out, "-baseline", "nope.json"}); code == 0 {
		t.Fatal("缺基线文件应返回非零退出码")
	}
}

func runDirs(t *testing.T, out string) []string {
	t.Helper()
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("没有产出 run 目录")
	}
	return names
}

func readReport(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRunRejectsUnknownSuite：拼错任务集要报错，不能"0 条任务全绿"。
func TestRunRejectsUnknownSuite(t *testing.T) {
	if code := run([]string{"-suite", "no-such-suite", "-out", t.TempDir()}); code == 0 {
		t.Fatal("不存在的任务集应返回非零退出码")
	}
}

// TestRunTrialsMustBePositive：k<1 无意义，必须拒绝。
func TestRunTrialsMustBePositive(t *testing.T) {
	if code := run([]string{"-suite", "smoke", "-trials", "0", "-out", t.TempDir()}); code == 0 {
		t.Fatal("trials=0 应返回非零退出码")
	}
}
