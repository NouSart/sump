package metrics

import (
	"testing"

	"github.com/NouSart/sump/evals/runner"
)

// TestAggregatePassK：pass^k 的口径是"k 次全过"，一次侥幸不能算过。
func TestAggregatePassK(t *testing.T) {
	results := []runner.Result{
		{TaskID: "a", Category: "c", Trial: 1, Success: true, Hops: 2, WallMS: 100, TokensIn: 10, TokensOut: 5},
		{TaskID: "a", Category: "c", Trial: 2, Success: true, Hops: 4, WallMS: 200, TokensIn: 20, TokensOut: 5},
		{TaskID: "b", Category: "c", Trial: 1, Success: true, Hops: 2, WallMS: 100, TokensIn: 10, TokensOut: 5},
		{TaskID: "b", Category: "c", Trial: 2, Success: false, Hops: 2, WallMS: 100, TokensIn: 10, TokensOut: 5, FailReason: "boom"},
	}
	s := Aggregate(Manifest{Trials: 2}, results, 2)

	if s.TotalRuns != 4 || s.Successes != 3 {
		t.Fatalf("总览计数不对: %+v", s)
	}
	if s.PassAll != 1 || len(s.Tasks) != 2 {
		t.Fatalf("pass^k 任务数 %d, want 1（共 %d 个任务）", s.PassAll, len(s.Tasks))
	}
	if got := s.PassK; got != 0.5 {
		t.Fatalf("pass^k=%v, want 0.5", got)
	}
	if got := s.PassRate; got != 0.75 {
		t.Fatalf("单次成功率=%v, want 0.75", got)
	}
}

// TestAggregatePerTaskMeans：逐任务均值与标准差（同配置的方差比均值更能说明稳不稳）。
func TestAggregatePerTaskMeans(t *testing.T) {
	results := []runner.Result{
		{TaskID: "a", Trial: 1, Success: true, Hops: 2, WallMS: 100, TokensIn: 10},
		{TaskID: "a", Trial: 2, Success: true, Hops: 4, WallMS: 300, TokensIn: 30},
	}
	s := Aggregate(Manifest{Trials: 2}, results, 2)
	if len(s.Tasks) != 1 {
		t.Fatalf("任务数 %d", len(s.Tasks))
	}
	ts := s.Tasks[0]
	if ts.MeanHops != 3 {
		t.Fatalf("平均跳数 %v, want 3", ts.MeanHops)
	}
	if ts.MeanWallMS != 200 {
		t.Fatalf("平均耗时 %v, want 200", ts.MeanWallMS)
	}
	if s.HopsStd != 1 {
		t.Fatalf("跳数标准差 %v, want 1", s.HopsStd)
	}
	if s.WallStd != 100 {
		t.Fatalf("耗时标准差 %v, want 100", s.WallStd)
	}
}

// TestAggregateCollectsFailReasons：失败原因必须原样保留（报告要能读，不能只剩一个 ❌）。
func TestAggregateCollectsFailReasons(t *testing.T) {
	results := []runner.Result{
		{TaskID: "a", Trial: 1, Success: false, FailReason: "文件不存在: a.txt", GateRejects: 1, Reminders: 2, Violations: []string{"x"}},
	}
	s := Aggregate(Manifest{Trials: 1}, results, 1)
	if len(s.Tasks[0].FailReasons) != 1 || s.Tasks[0].FailReasons[0] != "文件不存在: a.txt" {
		t.Fatalf("失败原因没留下来: %+v", s.Tasks[0])
	}
	if s.GateRejects != 1 || s.Reminders != 2 || s.Violations != 1 {
		t.Fatalf("门禁/违规计数不对: %+v", s)
	}
}

// TestAggregateEmpty：空结果不能崩（也不能因为 0/0 出 NaN 让人误读）。
func TestAggregateEmpty(t *testing.T) {
	s := Aggregate(Manifest{}, nil, 1)
	if s.TotalRuns != 0 || s.PassRate != 0 || s.PassK != 0 {
		t.Fatalf("空结果应全零: %+v", s)
	}
	if s.MeanHops != 0 || s.HopsStd != 0 {
		t.Fatalf("空结果的均值应为 0: %+v", s)
	}
}

// TestAggregateSortedByTaskID：报告顺序稳定（否则两次 run 的报告没法 diff）。
func TestAggregateSortedByTaskID(t *testing.T) {
	results := []runner.Result{
		{TaskID: "z", Trial: 1, Success: true},
		{TaskID: "a", Trial: 1, Success: true},
		{TaskID: "m", Trial: 1, Success: true},
	}
	s := Aggregate(Manifest{Trials: 1}, results, 1)
	want := []string{"a", "m", "z"}
	for i, w := range want {
		if s.Tasks[i].TaskID != w {
			t.Fatalf("第 %d 个应是 %s，实际 %s", i, w, s.Tasks[i].TaskID)
		}
	}
}
