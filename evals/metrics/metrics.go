// Package metrics 把逐条 trial 的结果聚合成可比较的指标。
//
// 三条口径必须一起给，缺一条就不算评完 harness：
//  1. pass^k —— k 次 trial 全过的任务比例（τ-bench 口径），用来量"稳不稳"；
//  2. 成本与耗时 —— 同配置的方差，harness 改动的成本方差常常大于换模型；
//  3. 契约违规 —— 交付前未判定、工具被闸门拒绝这类"编排层自己出错"的次数。
package metrics

import (
	"math"
	"sort"

	"github.com/NouSart/sump/evals/runner"
)

// Manifest 是一次 run 的快照：没有它，跑完就说不清当时是什么配置。
type Manifest struct {
	RunID      string            `json:"run_id"`
	Suite      string            `json:"suite"`
	Trials     int               `json:"trials"`
	Tasks      int               `json:"tasks"`
	Profile    string            `json:"profile"`
	Settings   map[string]string `json:"settings,omitempty"`
	LLMModel   string            `json:"llm_model"`
	Commit     string            `json:"commit,omitempty"`
	DatasetSHA string            `json:"dataset_sha256"`
	Host       string            `json:"host"`
	GoVersion  string            `json:"go_version"`
	StartedAt  string            `json:"started_at"`
	FinishedAt string            `json:"finished_at"`
	DurMS      int64             `json:"duration_ms"`
	Note       string            `json:"note,omitempty"`
}

// TaskSummary 是一条任务跨 k 次 trial 的聚合。
type TaskSummary struct {
	TaskID      string   `json:"task_id"`
	Category    string   `json:"category"`
	Trials      int      `json:"trials"`
	Successes   int      `json:"successes"`
	PassAll     bool     `json:"pass_all"` // k 次全过 = pass^k 计 1
	MeanHops    float64  `json:"mean_hops"`
	MeanWallMS  float64  `json:"mean_wall_ms"`
	MeanTokens  float64  `json:"mean_tokens"`
	GateRejects int      `json:"gate_rejects"`
	FailReasons []string `json:"fail_reasons,omitempty"`
	// NegativeControl 表示这是"必须被判失败"的对照任务：它的"通过"证明的是评测器没被糊弄，
	// 不是模型做成了事。报告必须把它和普通任务分开算，否则读者会把对照当能力。
	NegativeControl  bool   `json:"negative_control,omitempty"`
	ExpectFailReason string `json:"expect_fail_reason,omitempty"`
}

// Summary 是一次 run 的总览。
type Summary struct {
	Manifest Manifest      `json:"manifest"`
	Tasks    []TaskSummary `json:"tasks"`

	TotalRuns int     `json:"total_runs"`
	Successes int     `json:"successes"`
	PassAll   int     `json:"pass_all_tasks"`
	PassRate  float64 `json:"pass_rate"`
	PassK     float64 `json:"pass_k"`

	MeanHops   float64 `json:"mean_hops"`
	HopsStd    float64 `json:"hops_std"`
	MeanWallMS float64 `json:"mean_wall_ms"`
	WallStd    float64 `json:"wall_std"`
	MeanTokens float64 `json:"mean_tokens"`
	TokenStd   float64 `json:"token_std"`

	GateRejects int `json:"gate_rejects"`
	Reminders   int `json:"reminders"`
	Violations  int `json:"violations"`
}

// Aggregate 聚合全部 trial 结果。
func Aggregate(m Manifest, results []runner.Result, trials int) Summary {
	s := Summary{Manifest: m, TotalRuns: len(results)}

	byTask := map[string][]runner.Result{}
	order := make([]string, 0)
	for _, r := range results {
		if _, ok := byTask[r.TaskID]; !ok {
			order = append(order, r.TaskID)
		}
		byTask[r.TaskID] = append(byTask[r.TaskID], r)
		if r.Success {
			s.Successes++
		}
		s.GateRejects += r.GateRejects
		s.Reminders += r.Reminders
		s.Violations += len(r.Violations)
	}

	var hops, walls, tokens []float64
	for _, id := range order {
		rs := byTask[id]
		ts := TaskSummary{TaskID: id, Category: rs[0].Category, Trials: len(rs)}
		var th, tw, tt float64
		for _, r := range rs {
			if r.Success {
				ts.Successes++
			} else if r.FailReason != "" {
				ts.FailReasons = append(ts.FailReasons, r.FailReason)
			}
			if r.NegativeControl {
				ts.NegativeControl = true
			}
			if r.ExpectFailReason != "" {
				ts.ExpectFailReason = r.ExpectFailReason
			}
			th += float64(r.Hops)
			tw += float64(r.WallMS)
			tt += float64(r.TokensIn + r.TokensOut)
			ts.GateRejects += r.GateRejects
			hops = append(hops, float64(r.Hops))
			walls = append(walls, float64(r.WallMS))
			tokens = append(tokens, float64(r.TokensIn+r.TokensOut))
		}
		n := float64(len(rs))
		ts.MeanHops = th / n
		ts.MeanWallMS = tw / n
		ts.MeanTokens = tt / n
		ts.PassAll = ts.Successes == len(rs)
		if ts.PassAll {
			s.PassAll++
		}
		s.Tasks = append(s.Tasks, ts)
	}

	if len(results) > 0 {
		s.PassRate = float64(s.Successes) / float64(len(results))
	}
	if len(order) > 0 {
		s.PassK = float64(s.PassAll) / float64(len(order))
	}
	s.MeanHops, s.HopsStd = meanStd(hops)
	s.MeanWallMS, s.WallStd = meanStd(walls)
	s.MeanTokens, s.TokenStd = meanStd(tokens)
	sort.SliceStable(s.Tasks, func(i, j int) bool { return s.Tasks[i].TaskID < s.Tasks[j].TaskID })
	return s
}

func meanStd(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var sq float64
	for _, x := range xs {
		sq += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(sq / float64(len(xs)))
}
