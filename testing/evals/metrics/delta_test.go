package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NouSart/sump/evals/runner"
)

// sum 造一份可比较的总览（同 suite / 同 k / 同数据集）。
func sum(passK, hops, hopsStd float64, tasks int) Summary {
	s := Summary{
		Manifest:   Manifest{Suite: "smoke", Trials: 2, DatasetSHA: "abc", Tasks: tasks},
		PassK:      passK,
		MeanHops:   hops,
		HopsStd:    hopsStd,
		MeanWallMS: 100,
		WallStd:    10,
		MeanTokens: 500,
		TokenStd:   20,
	}
	for i := 0; i < tasks; i++ {
		s.Tasks = append(s.Tasks, TaskSummary{TaskID: string(rune('a' + i))})
	}
	return s
}

// TestCompareRejectsIncomparable：换了任务集/k/数据集就不能比——
// 这条防的是"拿上一版另一个套件的分数当基线"，那是最典型的声东击西。
func TestCompareRejectsIncomparable(t *testing.T) {
	cur := sum(1, 3, 0.5, 2)

	otherSuite := cur
	otherSuite.Manifest.Suite = "shell"
	if d := Compare(otherSuite, cur); d.Compatible || !strings.Contains(d.Reason, "任务集不同") {
		t.Fatalf("任务集不同应判不可比: %+v", d)
	}

	otherK := cur
	otherK.Manifest.Trials = 3
	if d := Compare(otherK, cur); d.Compatible || !strings.Contains(d.Reason, "k 不同") {
		t.Fatalf("k 不同应判不可比: %+v", d)
	}

	otherData := cur
	otherData.Manifest.DatasetSHA = "def"
	if d := Compare(otherData, cur); d.Compatible || !strings.Contains(d.Reason, "数据集") {
		t.Fatalf("数据集不同应判不可比: %+v", d)
	}

	fewer := sum(1, 3, 0.5, 1)
	if d := Compare(fewer, cur); d.Compatible || !strings.Contains(d.Reason, "任务数不同") {
		t.Fatalf("任务数不同应判不可比: %+v", d)
	}
}

// TestCompareNoiseBand：差异落在噪声带内时不得宣称改进。
func TestCompareNoiseBand(t *testing.T) {
	prev := sum(1, 3.0, 1.0, 2)
	cur := sum(1, 3.5, 1.0, 2) // 差 0.5 < 噪声带 1.0
	d := Compare(prev, cur)
	if !d.Compatible {
		t.Fatalf("应可比: %s", d.Reason)
	}
	if d.Significant {
		t.Fatalf("噪声带内的差异不该算显著: %+v", d)
	}
	if !strings.Contains(strings.Join(d.Notes, " "), "都在噪声带内") {
		t.Fatalf("应明确写出结论: %v", d.Notes)
	}
}

// TestCompareSignificant：超出噪声带的差异要报出来，并指出方向。
func TestCompareSignificant(t *testing.T) {
	prev := sum(1, 3.0, 0.2, 2)
	cur := sum(1, 5.0, 0.2, 2) // 差 2.0 > 噪声带 0.2
	d := Compare(prev, cur)
	if !d.Significant {
		t.Fatalf("应判为显著: %+v", d)
	}
	joined := strings.Join(d.Notes, " ")
	if !strings.Contains(joined, "跳数 +2.0") || !strings.Contains(joined, "变差") {
		t.Fatalf("应指出跳数变差: %v", d.Notes)
	}
}

// TestComparePassKAlwaysCounts：pass^k 是比例指标，只要有变化就算显著（它本身已是聚合值）。
func TestComparePassKAlwaysCounts(t *testing.T) {
	prev := sum(1.0, 3, 0.5, 2)
	cur := sum(0.5, 3, 0.5, 2)
	d := Compare(prev, cur)
	if !d.Significant {
		t.Fatalf("pass^k 变化应算显著: %+v", d)
	}
	if !strings.Contains(strings.Join(d.Notes, " "), "-50.0 个百分点") {
		t.Fatalf("应给出百分点差: %v", d.Notes)
	}
}

// TestCompareWithoutNoiseData：k=1 时没有波动数据，只能报差值，不能宣称显著。
func TestCompareWithoutNoiseData(t *testing.T) {
	prev := sum(1, 3, 0, 2)
	cur := sum(1, 9, 0, 2)
	d := Compare(prev, cur)
	if d.Significant {
		t.Fatalf("没有噪声数据时不该宣称显著: %+v", d)
	}
	if !strings.Contains(strings.Join(d.Notes, " "), "无法判断是否显著") {
		t.Fatalf("应说明无法判断: %v", d.Notes)
	}
}

// TestDeltaMarkdownBothBranches：报告要么给对比表，要么明确写"不可比"。
func TestDeltaMarkdownBothBranches(t *testing.T) {
	if got := Compare(sum(1, 3, 0.5, 2), sum(1, 3.1, 0.5, 2)).Markdown(); !strings.Contains(got, "| 指标 | 基线 |") {
		t.Fatalf("可比时应给对比表: %s", got)
	}
	incomparable := sum(1, 3, 0.5, 2)
	incomparable.Manifest.Suite = "other"
	if got := Compare(incomparable, sum(1, 3, 0.5, 2)).Markdown(); !strings.Contains(got, "不可比") {
		t.Fatalf("不可比时应明说: %s", got)
	}
}

// TestSaveAndLoadSummary：manifest.json 与 summary.json 都要落盘，且能读回用于下一次对比。
func TestSaveAndLoadSummary(t *testing.T) {
	dir := t.TempDir()
	s := sum(0.75, 4, 0.5, 2)
	s.Manifest.RunID = "r1"
	if err := SaveJSON(dir, s); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ManifestFile, SummaryFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s 没生成: %v", name, err)
		}
	}

	// 目录与文件两种入参都要能读。
	got, err := LoadSummary(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.PassK != 0.75 || got.Manifest.RunID != "r1" {
		t.Fatalf("读回的内容不对: %+v", got)
	}
	got2, err := LoadSummary(filepath.Join(dir, SummaryFile))
	if err != nil {
		t.Fatal(err)
	}
	if got2.MeanHops != 4 {
		t.Fatalf("按文件读回的内容不对: %+v", got2)
	}
}

// TestLoadSummaryMissing：基线文件不存在要报错，不能悄悄当成"没有基线"。
func TestLoadSummaryMissing(t *testing.T) {
	if _, err := LoadSummary(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("缺文件应报错")
	}
}

// TestCompareUsesAggregatedResults：Compare 吃的是 Aggregate 的产物——
// 用真实聚合结果走一遍，确认字段名字对得上（这类接错只有端到端才看得出来）。
func TestCompareUsesAggregatedResults(t *testing.T) {
	mk := func(hops int) Summary {
		return Aggregate(Manifest{Suite: "smoke", Trials: 1, DatasetSHA: "x"},
			[]runner.Result{
				{TaskID: "a", Trial: 1, Success: true, Hops: hops, WallMS: 100, TokensIn: 10},
			}, 1)
	}
	d := Compare(mk(3), mk(4))
	if !d.Compatible {
		t.Fatalf("应可比: %s", d.Reason)
	}
	if d.HopsPrev != 3 || d.HopsNow != 4 {
		t.Fatalf("均值没接上: %+v", d)
	}
}
