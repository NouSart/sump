// Package report 把一次 run 的结果写成报告（Markdown 给人看，CSV 给比对）。
package report

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/NouSart/sump/evals/metrics"
	"github.com/NouSart/sump/evals/runner"
)

// Options 控制报告内容；零值即"只出本次报告"。
type Options struct {
	// Baseline 非空时，报告里追加"与基线对比"一节。
	Baseline *metrics.Summary
}

// Write 产出 report.md 与 results.csv 到 dir。
func Write(dir string, s metrics.Summary, results []runner.Result) error {
	return WriteWith(dir, s, results, Options{})
}

// WriteWith 与 Write 相同，但可带上基线对比。
func WriteWith(dir string, s metrics.Summary, results []runner.Result, opts Options) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body := markdown(s, opts)
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(body), 0o644); err != nil {
		return err
	}
	return writeCSV(filepath.Join(dir, "results.csv"), results)
}

func markdown(s metrics.Summary, opts Options) string {
	var b strings.Builder
	m := s.Manifest

	fmt.Fprintf(&b, "# SUMP harness 评测报告\n\n")
	fmt.Fprintf(&b, "> 模型是**冻结**的（剧本化假 LLM），harness 是唯一自变量。判定只看终态断言，不看模型自述。\n\n")

	fmt.Fprintf(&b, "| 项目 | 值 |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| run_id | `%s` |\n", m.RunID)
	fmt.Fprintf(&b, "| suite | `%s` |\n", m.Suite)
	fmt.Fprintf(&b, "| profile | `%s` |\n", valueOr(m.Profile, "default"))
	fmt.Fprintf(&b, "| commit | `%s` |\n", valueOr(m.Commit, "unknown"))
	fmt.Fprintf(&b, "| dataset_sha256 | `%s` |\n", short(m.DatasetSHA))
	fmt.Fprintf(&b, "| trials / tasks | %d / %d |\n", m.Trials, m.Tasks)
	fmt.Fprintf(&b, "| 模型 | `%s`（假 LLM 剧本） |\n", m.LLMModel)
	if len(m.Settings) > 0 {
		keys := make([]string, 0, len(m.Settings))
		for k := range m.Settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		kv := make([]string, 0, len(keys))
		for _, k := range keys {
			kv = append(kv, fmt.Sprintf("`%s=%s`", k, m.Settings[k]))
		}
		fmt.Fprintf(&b, "| 消融设置 | %s |\n", strings.Join(kv, " "))
	}
	fmt.Fprintf(&b, "| 起止 | %s → %s（%dms） |\n", m.StartedAt, m.FinishedAt, m.DurMS)
	fmt.Fprintf(&b, "| 主机 / Go | `%s` / go%s |\n\n", m.Host, m.GoVersion)

	fmt.Fprintf(&b, "## 总览\n\n")
	fmt.Fprintf(&b, "| 指标 | 值 |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| 单次成功率 | %.1f%%（%d/%d） |\n", s.PassRate*100, s.Successes, s.TotalRuns)
	fmt.Fprintf(&b, "| **pass^k**（%d 次全过的任务占比） | **%.1f%%**（%d/%d） |\n", m.Trials, s.PassK*100, s.PassAll, len(s.Tasks))
	fmt.Fprintf(&b, "| 平均跳数 | %.1f ± %.1f |\n", s.MeanHops, s.HopsStd)
	fmt.Fprintf(&b, "| 平均耗时 | %.0fms ± %.0fms |\n", s.MeanWallMS, s.WallStd)
	fmt.Fprintf(&b, "| 平均 token | %.0f ± %.0f |\n", s.MeanTokens, s.TokenStd)
	fmt.Fprintf(&b, "| 工具被闸门拒绝 | %d |\n", s.GateRejects)
	fmt.Fprintf(&b, "| 收尾门禁提醒轮 | %d |\n", s.Reminders)
	fmt.Fprintf(&b, "| 契约违规 | %d |\n\n", s.Violations)

	fmt.Fprintf(&b, "## 逐任务\n\n")
	fmt.Fprintf(&b, "| 任务 | 类别 | pass^k | 成功/trial | 平均跳数 | 平均耗时(ms) | 平均 token | 闸门拒绝 |\n")
	fmt.Fprintf(&b, "| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, t := range s.Tasks {
		mark := "✅"
		if !t.PassAll {
			mark = "❌"
		}
		if t.NegativeControl {
			mark = "🛡"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %d/%d | %.1f | %.0f | %.0f | %d |\n",
			t.TaskID, t.Category, mark, t.Successes, t.Trials, t.MeanHops, t.MeanWallMS, t.MeanTokens, t.GateRejects)
	}

	var controls []metrics.TaskSummary
	for _, t := range s.Tasks {
		if t.NegativeControl {
			controls = append(controls, t)
		}
	}
	if len(controls) > 0 {
		fmt.Fprintf(&b, "\n### 🛡 对照任务（证明评测器没被糊弄，不等于能力）\n\n")
		for _, t := range controls {
			fmt.Fprintf(&b, "- `%s`：模型只说不做，评测器按预期判失败（%s）——因此这条记通过。\n",
				t.TaskID, valueOr(t.ExpectFailReason, "无原因记录"))
		}
	}

	var failed []metrics.TaskSummary
	for _, t := range s.Tasks {
		if !t.PassAll {
			failed = append(failed, t)
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\n## 失败原因\n\n")
		for _, t := range failed {
			reasons := dedupe(t.FailReasons)
			if len(reasons) == 0 {
				reasons = []string{"（无原因记录）"}
			}
			fmt.Fprintf(&b, "- `%s`：%s\n", t.TaskID, strings.Join(reasons, "；"))
		}
	}

	if opts.Baseline != nil {
		fmt.Fprintf(&b, "\n%s", metrics.Compare(*opts.Baseline, s).Markdown())
	}

	fmt.Fprintf(&b, "\n## 怎么读这份报告\n\n")
	fmt.Fprintf(&b, "- **只比同 suite、同 trials 的数字**：换任务集或换 k，分数不可比。\n")
	fmt.Fprintf(&b, "- **pass^k 才是可靠性**：单次成功率会因为一次侥幸而虚高。\n")
	fmt.Fprintf(&b, "- **看方差**：跳数/token 的 ± 值比均值更能说明 harness 稳不稳。\n")
	fmt.Fprintf(&b, "- **噪声带**：同配置连跑两次，落在噪声带内的差异不算改进（用 `-baseline` 对比才看得到判定）。\n")
	fmt.Fprintf(&b, "- **🛡 是评测器自检，不是模型能力**：对照任务里模型只说不做，评测器必须判它失败。\n")
	return b.String()
}

func writeCSV(path string, results []runner.Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()

	header := []string{
		"task_id", "category", "trial", "success", "fail_reason", "delivered",
		"hops", "tool_calls", "tool_results", "gate_rejects", "reminders",
		"wall_ms", "tokens_in", "tokens_out", "violations", "matched_steps",
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, r := range results {
		row := []string{
			r.TaskID, r.Category, strconv.Itoa(r.Trial), strconv.FormatBool(r.Success), r.FailReason,
			strconv.FormatBool(r.Delivered), strconv.Itoa(r.Hops), strconv.Itoa(r.ToolCalls),
			strconv.Itoa(r.ToolResults), strconv.Itoa(r.GateRejects), strconv.Itoa(r.Reminders),
			strconv.FormatInt(r.WallMS, 10), strconv.Itoa(r.TokensIn), strconv.Itoa(r.TokensOut),
			strings.Join(r.Violations, " | "), strings.Join(r.MatchedSteps, ">"),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return w.Error()
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func valueOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
