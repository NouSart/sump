package report

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NouSart/sump/evals/metrics"
	"github.com/NouSart/sump/evals/runner"
)

// sample 造一份"单次成功率 75%、pass^k 50%"的数据：
// 两个任务各 2 次 trial，任务 a 全过、任务 b 只过一次 —— 两个指标刻意不同，
// 这样"把 pass^k 印成单次成功率"的错误才抓得到。
func sample() (metrics.Summary, []runner.Result) {
	results := []runner.Result{
		{TaskID: "a", Category: "chain", Trial: 1, Success: true, Hops: 3, WallMS: 120, TokensIn: 100, TokensOut: 20, Delivered: true},
		{TaskID: "a", Category: "chain", Trial: 2, Success: true, Hops: 3, WallMS: 120, TokensIn: 100, TokensOut: 20, Delivered: true},
		{TaskID: "b", Category: "gate", Trial: 1, Success: true, Hops: 5, WallMS: 300, TokensIn: 200, TokensOut: 40, Delivered: true},
		{TaskID: "b", Category: "gate", Trial: 2, Success: false, Hops: 5, WallMS: 300, TokensIn: 200, TokensOut: 40, FailReason: "文件不存在: a.txt"},
	}
	m := metrics.Manifest{RunID: "r1", Suite: "smoke", Trials: 2, Tasks: 2, Profile: "default", LLMModel: "frozen-model", DatasetSHA: "abcdef0123456789"}
	return metrics.Aggregate(m, results, 2), results
}

// TestWriteProducesBothFiles：报告必须同时给人和给机器。
func TestWriteProducesBothFiles(t *testing.T) {
	dir := t.TempDir()
	s, results := sample()
	if err := Write(dir, s, results); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"report.md", "results.csv"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s 没生成: %v", name, err)
		}
	}
}

// TestMarkdownCarriesTheNumbersThatMatter：报告里必须能看到 pass^k、噪声带提示与失败原因，
// 否则读者只会看到"成功率 50%"就下结论。
//
// 断言带**数值**：只检查出现 "pass^k" 这三个字的测试，抓不到"把 pass^k 印成单次成功率"这种错。
func TestMarkdownCarriesTheNumbersThatMatter(t *testing.T) {
	dir := t.TempDir()
	s, results := sample()
	if err := Write(dir, s, results); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, want := range []string{
		"pass^k", "单次成功率", "平均跳数", "契约违规",
		"噪声带", "文件不存在: a.txt", "`r1`", "`smoke`", "frozen-model",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("报告应含 %q", want)
		}
	}
	// sample() 的 pass^k=50.0%、单次成功率=75.0%：两个数都必须各就各位。
	if s.PassK != 0.5 || s.PassRate != 0.75 {
		t.Fatalf("样例数据不符合预期: passK=%v passRate=%v", s.PassK, s.PassRate)
	}
	if !strings.Contains(body, "**50.0%**") {
		t.Fatalf("pass^k 那一行应显示 50.0%%，实际报告:\n%s", body)
	}
	if !strings.Contains(body, "75.0%") {
		t.Fatalf("单次成功率应显示 75.0%%，实际报告:\n%s", body)
	}
}

// TestNegativeControlReportedSeparately：对照任务必须单独成节并标 🛡，
// 否则读者会把"评测器自检通过"当成"模型做成了事"。
func TestNegativeControlReportedSeparately(t *testing.T) {
	results := []runner.Result{
		{TaskID: "a", Category: "chain", Trial: 1, Success: true, Hops: 2},
		{
			TaskID: "ctl", Category: "control", Trial: 1, Success: true,
			NegativeControl: true, ExpectFailReason: "文件不存在: ok.txt",
		},
	}
	m := metrics.Manifest{RunID: "r9", Suite: "smoke", Trials: 1, Tasks: 2, DatasetSHA: "d9"}
	s := metrics.Aggregate(m, results, 1)

	dir := t.TempDir()
	if err := Write(dir, s, results); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, want := range []string{"🛡", "对照任务", "文件不存在: ok.txt", "不等于能力"} {
		if !strings.Contains(body, want) {
			t.Fatalf("报告应含 %q，实际:\n%s", want, body)
		}
	}
}

// TestMarkdownDeclaresFrozenModel：报告必须写清"模型是冻结的"，
// 否则读者会以为这是模型能力评测（概念漂移的常见入口）。
func TestMarkdownDeclaresFrozenModel(t *testing.T) {
	dir := t.TempDir()
	s, results := sample()
	if err := Write(dir, s, results); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "report.md"))
	if !strings.Contains(string(b), "冻结") {
		t.Fatal("报告必须声明模型被冻结")
	}
}

// TestCSVRoundTrip：CSV 表头与行数正确，且失败原因不被吞掉。
func TestCSVRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, results := sample()
	if err := Write(dir, s, results); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "results.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(results)+1 {
		t.Fatalf("行数 %d, want %d（表头 + %d 条）", len(rows), len(results)+1, len(results))
	}
	if len(rows[0]) != len(rows[1]) {
		t.Fatalf("列数不一致: %d vs %d", len(rows[0]), len(rows[1]))
	}
	joined := strings.Join(rows[len(rows)-1], ",")
	if !strings.Contains(joined, "文件不存在") {
		t.Fatalf("失败原因应在 CSV 里: %s", joined)
	}
}

// TestWriteWithBaselineRendersDelta：给了基线就必须渲染对比小节（不能只在命令行上打印）。
func TestWriteWithBaselineRendersDelta(t *testing.T) {
	dir := t.TempDir()
	s, results := sample()
	base := s
	base.PassK = 0.5
	base.MeanHops = s.MeanHops + 5 // 明显变差
	if err := WriteWith(dir, s, results, Options{Baseline: &base}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, want := range []string{"与基线对比", "噪声带", "结论：", "-5.0"} {
		if !strings.Contains(body, want) {
			t.Fatalf("报告应含 %q", want)
		}
	}
}

// TestWriteWithBaselineIncomparable：不可比时报告要明说，而不是默默不显示。
func TestWriteWithBaselineIncomparable(t *testing.T) {
	dir := t.TempDir()
	s, results := sample()
	base := s
	base.Manifest.Suite = "another-suite"
	if err := WriteWith(dir, s, results, Options{Baseline: &base}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "report.md"))
	if !strings.Contains(string(b), "不可比") {
		t.Fatal("不可比时报告必须明说")
	}
}

// TestEmptySummaryStillWrites：结果为空也要产出报告（否则 CI 会以为"没跑=通过"）。
func TestEmptySummaryStillWrites(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, metrics.Aggregate(metrics.Manifest{}, nil, 1), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "report.md")); err != nil {
		t.Fatal(err)
	}
}

// TestDedupeKeepsOrder：失败原因去重但保持首次出现顺序（可读性）。
func TestDedupeKeepsOrder(t *testing.T) {
	got := dedupe([]string{"b", "a", "b", "", "a", "c"})
	want := []string{"b", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("去重后 %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("去重结果 %v, want %v", got, want)
		}
	}
}
