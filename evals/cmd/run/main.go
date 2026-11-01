// 命令 run 跑一次 harness 评测：冻结模型 → 起真进程 → 跑任务 → 出报告。
//
// 用法（在 evals 目录下）：
//
//	go run ./cmd/run -suite smoke -trials 3
//	go run ./cmd/run -list
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/NouSart/sump/evals/dataset"
	"github.com/NouSart/sump/evals/fakeopenai"
	"github.com/NouSart/sump/evals/metrics"
	"github.com/NouSart/sump/evals/report"
	"github.com/NouSart/sump/evals/runner"
)

func main() {
	// os.Exit 会跳过 defer：服务进程与临时目录都在 defer 里收，所以退出码要从 run() 带出来。
	os.Exit(run(os.Args[1:]))
}

// run 是真正的入口（接受参数是为了能被测试直接驱动，而不是只能靠命令行验证）。
func run(args []string) int {
	fs := flag.NewFlagSet("evals", flag.ContinueOnError)
	suite := fs.String("suite", "smoke", "任务集名（对应 dataset/<name>.jsonl）")
	trials := fs.Int("trials", 1, "每条任务跑几次（pass^k 的 k）")
	out := fs.String("out", "", "结果根目录（默认 <仓库>/data/evals）")
	profileName := fs.String("profile", "default", "本次 harness 配置的名字（写进 manifest）")
	only := fs.String("only", "", "只跑这些任务（逗号分隔的 id）")
	list := fs.Bool("list", false, "只列出任务，不跑")
	keep := fs.Bool("keep", false, "保留临时目录（排查用）")
	rebuild := fs.Bool("rebuild", false, "强制重新编译服务（默认按源码指纹缓存）")
	baseline := fs.String("baseline", "", "基线：某个 run 目录或其 summary.json（给了就出对比）")
	saveBaseline := fs.Bool("save-baseline", false, "把本次结果存为 <out>/baseline.json")
	verbose := fs.Bool("v", false, "打印服务与假 LLM 日志")
	var settings stringSliceFlag
	fs.Var(&settings, "set", "消融项，形如 -set reasoner.tool.notify_mode=batch（= <服务名>.<设置项>，可重复）")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}

	root, err := repoRoot()
	if err != nil {
		return fail(err)
	}
	dsPath := filepath.Join(root, "evals", "dataset", *suite+".jsonl")
	tasks, err := dataset.Load(dsPath)
	if err != nil {
		return fail(err)
	}
	if v := strings.TrimSpace(*only); v != "" {
		want := map[string]bool{}
		for _, id := range strings.Split(v, ",") {
			want[strings.TrimSpace(id)] = true
		}
		var keepTasks []dataset.Task
		for _, t := range tasks {
			if want[t.ID] {
				keepTasks = append(keepTasks, t)
			}
		}
		if len(keepTasks) == 0 {
			return fail(fmt.Errorf("-only %q 没匹配到任何任务", v))
		}
		tasks = keepTasks
	}
	if *list {
		for _, t := range tasks {
			fmt.Printf("%-16s %-12s %s\n", t.ID, t.Category, t.Prompt)
		}
		return 0
	}
	if *trials < 1 {
		return fail(fmt.Errorf("-trials 必须 ≥ 1"))
	}

	logf := func(format string, args ...any) {}
	if *verbose {
		logf = func(format string, args ...any) { fmt.Printf("  · "+format+"\n", args...) }
	}

	runID := time.Now().Format("20060102-150405")
	outRoot := *out
	if outRoot == "" {
		outRoot = filepath.Join(root, "data", "evals")
	}
	runDir := filepath.Join(outRoot, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fail(err)
	}

	started := time.Now()
	fmt.Printf("run %s：suite=%s tasks=%d trials=%d\n", runID, *suite, len(tasks), *trials)

	kv, err := parseSettings(settings)
	if err != nil {
		return fail(err)
	}
	preset := suitePresets[*suite]
	merged := map[string]string{}
	for k, v := range preset.Settings {
		merged[k] = v
	}
	for k, v := range kv {
		merged[k] = v
	}
	if len(merged) == 0 {
		merged = nil
	}
	// 二进制缓存固定在仓库的 data/evals/bin（构建缓存，不是结果），
	// 这样 -out 只装结果，换 out 目录也不会重编一遍。
	binDir := filepath.Join(root, "data", "evals", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fail(err)
	}
	binNames := append([]string{"settings-center", "memory", "reasoner"}, preset.Services...)
	bins, err := runner.BuildBinariesCached(root, binDir, binNames, *rebuild)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("  服务就绪（%d 个，缓存目录 %s）\n", len(bins), binDir)

	fake, err := fakeopenai.New(fakeopenai.Script{}, logf)
	if err != nil {
		return fail(err)
	}
	defer fake.Close()

	workDir, err := os.MkdirTemp("", "sump-eval-stack-")
	if err != nil {
		return fail(err)
	}
	if !*keep {
		defer os.RemoveAll(workDir)
	}

	policyPath := preset.PolicyPath
	if policyPath != "" && !filepath.IsAbs(policyPath) {
		policyPath = filepath.Join(root, policyPath)
	}
	stack, err := runner.StartStack(runner.StackOptions{
		Dir:           workDir,
		Bin:           bins,
		LLMBase:       fake.URL(),
		ExtraServices: preset.Services,
		Settings:      merged,
		PolicyPath:    policyPath,
		SkipMemory:    preset.NoMemory,
		Logf:          logf,
	})
	if err != nil {
		return fail(fmt.Errorf("启动服务栈失败: %w", err))
	}
	defer stack.Close()
	fmt.Printf("  服务栈就绪 center=%s memory=%s reasoner=%s\n", stack.CenterURL, stack.MemoryURL, stack.ReasonerURL)
	if *keep {
		fmt.Printf("  临时目录（保留）：%s\n", workDir)
	}

	var results []runner.Result
	for _, t := range tasks {
		fake.SetScript(t.Script)
		for i := 1; i <= *trials; i++ {
			convID := fmt.Sprintf("%s-t%d-%s", t.ID, i, runID)
			res := runner.RunTask(stack, fake, t, i, convID)
			if err := runner.SaveTrace(filepath.Join(runDir, "traces"), res, fake.Records()); err != nil {
				logf("保存轨迹失败: %v", err)
			}
			results = append(results, res)
			status := "FAIL"
			if res.Success {
				status = "ok"
			}
			fmt.Printf("  [%s] %-16s t%d  hops=%d wall=%dms %s\n", status, t.ID, i, res.Hops, res.WallMS, res.FailReason)
		}
	}

	dsSHA, _ := fileSHA(dsPath)
	manifest := metrics.Manifest{
		RunID: runID, Suite: *suite, Trials: *trials, Tasks: len(tasks),
		Profile: *profileName, Settings: merged, LLMModel: "frozen-model",
		Commit: gitCommit(root), DatasetSHA: dsSHA,
		Host: hostname(), GoVersion: strings.TrimPrefix(runtime.Version(), "go"),
		StartedAt: started.Format(time.RFC3339), FinishedAt: time.Now().Format(time.RFC3339),
		DurMS: time.Since(started).Milliseconds(),
	}
	summary := metrics.Aggregate(manifest, results, *trials)

	var base *metrics.Summary
	if p := strings.TrimSpace(*baseline); p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(outRoot, p)
		}
		prev, err := metrics.LoadSummary(p)
		if err != nil {
			return fail(fmt.Errorf("读基线失败（%s）: %w", p, err))
		}
		base = &prev
	}
	if err := report.WriteWith(runDir, summary, results, report.Options{Baseline: base}); err != nil {
		return fail(err)
	}
	if err := metrics.SaveJSON(runDir, summary); err != nil {
		return fail(err)
	}
	if *saveBaseline {
		if err := metrics.SaveBaseline(filepath.Join(outRoot, "baseline.json"), summary); err != nil {
			return fail(err)
		}
		fmt.Printf("基线已保存：%s\n", filepath.Join(outRoot, "baseline.json"))
	}

	fmt.Printf("\n单次成功率 %.1f%%　pass^%d %.1f%%　平均跳数 %.1f±%.1f　平均耗时 %.0fms\n",
		summary.PassRate*100, *trials, summary.PassK*100, summary.MeanHops, summary.HopsStd, summary.MeanWallMS)
	if base != nil {
		d := metrics.Compare(*base, summary)
		if d.Compatible {
			verdict := "未超出噪声带"
			if d.Significant {
				verdict = "超出噪声带"
			}
			fmt.Printf("对比基线：pass^k %+.1f 个百分点　%s\n", d.PassKDelta*100, verdict)
		} else {
			fmt.Printf("对比基线：不可比（%s）\n", d.Reason)
		}
	}
	fmt.Printf("报告：%s\n", filepath.Join(runDir, "report.md"))
	if summary.PassAll < len(summary.Tasks) {
		return 1
	}
	return 0
}

// suitePresets 声明每个任务集需要哪些附加服务与固定设置。
// 任务集是数据（dataset/*.jsonl），但"起哪些服务"属于跑法，写在代码里才不会跑偏。
var suitePresets = map[string]struct {
	Services   []string
	Settings   map[string]string
	PolicyPath string
	NoMemory   bool
}{
	"smoke":         {},
	"gate":          {},
	"orchestration": {},
	"fault":         {},
	// nodep：故意不起记忆服务——最真实的"依赖缺席"故障。
	"nodep": {NoMemory: true},
	"shell": {
		Services: []string{"approval", "shell"},
		// 评测里没有人点"批准"：把审批放宽到「工作区内全放行」，
		// 这样测的是 harness 的编排，而不是审批策略本身的松紧（那由 approval 自己的测试覆盖）。
		Settings: map[string]string{"approval.auto_approve": "workspace"},
		// 但 PowerShell 路径仍走 policy 白名单：用固定策略文件，别让"能不能写文件"
		// 取决于默认策略恰好覆盖了哪个 cmdlet。
		PolicyPath: "evals/policy/shell-allow.json",
	},
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func parseSettings(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, kv := range raw {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("-set 需要 key=value，收到 %q", kv)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// repoRoot 从当前目录向上找 go.work（evals 是工作区成员之一）。
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("向上找不到 go.work（请在仓库内运行）")
		}
		dir = parent
	}
}

func gitCommit(root string) string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func fileSHA(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "评测失败: %v\n", err)
	return 2
}
