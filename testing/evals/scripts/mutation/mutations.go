package main

// 异变清单：每条都是"如果实现真这么写，看起来也说得通，但结论会错"。
//
// 选这些而不是随机改，是因为它们对应 README §6 里那几种许愿式形态：
// 判定的每一步被绕过，都会让报告变成一句漂亮但假的结论。
var mutations = []mutation{
	// ---- 掩耳盗铃：把失败说成成功 ----
	{
		name: "失败通知被当成成功交付",
		file: "runner/trial.go",
		old: `		if isFailureNotice(text) {
			failureNotified = true
			continue
		}`,
		new:  `		_ = text`,
		pkg:  "./runner",
		slow: true,
	},
	{
		name: "交付前未判定的破约检查被短路",
		file: "runner/trial.go",
		old:  `	if delivered && !judged && reminders == 0 {`,
		new:  `	if false && delivered && !judged && reminders == 0 {`,
		pkg:  "./runner",
		slow: true,
	},

	// ---- 声东击西：换了指标/漏了维度 ----
	{
		name: "pass^k 退化成「过过一次」",
		file: "metrics/metrics.go",
		old:  `ts.PassAll = ts.Successes == len(rs)`,
		new:  `ts.PassAll = ts.Successes > 0`,
		pkg:  "./metrics",
	},

	// ---- 狐假虎威：没测到却宣称有效 ----
	{
		name: "注入无效仍记成功",
		file: "runner/trial.go",
		old: `res.Invalid = true
	res.Success = false`,
		new:  `res.Invalid = true`,
		pkg:  "./runner",
		slow: true,
	},
	{
		name: "未命中故障不再报出来",
		file: "fakeopenai/server.go",
		old: `		if !s.used[i] {
			out = append(out, st.Name)
		}`,
		new: `		_ = i`,
		pkg: "./fakeopenai",
	},
	{
		name: "截断故障注入失效",
		file: "fakeopenai/server.go",
		old: `	case FaultTruncate:
		s.finish(rec, started)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(` + "`" + `{"choices":[{"message":{"role":"assist` + "`" + `))
		return`,
		new: ``,
		pkg: "./fakeopenai",
	},

	// ---- 概念漂移：把"模型自述"当成事实 ----
	{
		name: "判定回执识别失效",
		file: "fakeopenai/server.go",
		old: `		if strings.Contains(text, "判定为") {
			judgeSeen = true
		}`,
		new: ``,
		pkg: "./fakeopenai",
	},

	// ---- 断言被绕过（终态检查形同虚设）----
	{
		name: "file_contains 断言被短路",
		file: "runner/trial.go",
		old:  `if !strings.Contains(string(b), e.FileContains.Text) {`,
		new:  `if false && !strings.Contains(string(b), e.FileContains.Text) {`,
		pkg:  "./runner",
		slow: true,
	},
	{
		name: "file_absent 断言语义反转",
		file: "runner/trial.go",
		old:  `if _, err := os.Stat(filepath.Join(res.WorkDir, e.FileAbsent)); err == nil {`,
		new:  `if _, err := os.Stat(filepath.Join(res.WorkDir, e.FileAbsent)); err != nil {`,
		pkg:  "./runner",
		slow: true,
	},
	{
		name: "no_tool_failure 断言失灵",
		file: "runner/trial.go",
		old: `		case e.NoToolFailure:
			if res.GateRejects > 0 {`,
		new: `		case e.NoToolFailure:
			if res.GateRejects > 1<<20 {`,
		pkg:  "./runner",
		slow: true,
	},

	// ---- 只说不做：负向对照失效 ----
	{
		name: "负向对照分支反转",
		file: "runner/trial.go",
		old: `		if res.Success {
			res.Success = false
			res.FailReason = "负向对照失效：模型只说不做，评测器却判了成功"`,
		new: `		if !res.Success {
			res.Success = false
			res.FailReason = "负向对照失效：模型只说不做，评测器却判了成功"`,
		pkg:  "./runner",
		slow: true,
	},
	{
		name: "负向对照不再留证据（通过即抹掉原因）",
		file: "runner/trial.go",
		old:  `			res.ExpectFailReason = res.FailReason`,
		new:  `			res.ExpectFailReason = ""`,
		pkg:  "./runner",
		slow: true,
	},

	// ---- 噪声带：把波动当改进 ----
	{
		name: "噪声带判定失效（永远不显著）",
		file: "metrics/delta.go",
		old: `	if absf(delta) <= noise {
		return false
	}`,
		new: `	if true {
		return false
	}`,
		pkg: "./metrics",
	},
	{
		name: "基线可比性不校验数据集",
		file: "metrics/delta.go",
		old: `	case prev.Manifest.DatasetSHA != cur.Manifest.DatasetSHA:
		d.Reason = "数据集内容不同（sha256 不一致）"
`,
		new: ``,
		pkg: "./metrics",
	},
	{
		name: "基线可比性不校验 k",
		file: "metrics/delta.go",
		old: `	case prev.Manifest.Trials != cur.Manifest.Trials:
		d.Reason = fmt.Sprintf("k 不同（%d vs %d）", prev.Manifest.Trials, cur.Manifest.Trials)
`,
		new: ``,
		pkg: "./metrics",
	},
	{
		name: "summary.json 不再落盘（下次没有基线可读）",
		file: "metrics/store.go",
		old:  `	return writeJSON(filepath.Join(dir, SummaryFile), s)`,
		new:  `	return nil`,
		pkg:  "./metrics",
	},

	// ---- 多此一举的反面：校验被放宽 ----
	{
		name: "任务允许写成两个断言字段",
		file: "dataset/dataset.go",
		old:  `		if n != 1 {`,
		new:  `		if n > 1 {`,
		pkg:  "./dataset",
	},
	{
		name: "空任务集当成合法（0 条 = 满分）",
		file: "dataset/dataset.go",
		old: `	if len(out) == 0 {
		return nil, fmt.Errorf("%s 里没有任务", path)
	}`,
		new: "",
		pkg: "./dataset",
	},
	{
		name: "带 BOM 的任务集静默通过",
		file: "dataset/dataset.go",
		old:  `		if err := json.Unmarshal([]byte(text), &t); err != nil {`,
		new:  `		if err := json.Unmarshal([]byte(text), &t); false && err != nil {`,
		pkg:  "./dataset",
	},

	// ---- 报告：数字与结论接错 ----
	{
		name: "报告把 pass^k 印成单次成功率",
		file: "report/report.go",
		old:  `m.Trials, s.PassK*100, s.PassAll, len(s.Tasks))`,
		new:  `m.Trials, s.PassRate*100, s.PassAll, len(s.Tasks))`,
		pkg:  "./report",
	},
	{
		name: "报告不再列出失败原因",
		file: "report/report.go",
		old: `		if !t.PassAll {
			failed = append(failed, t)
		}`,
		new: `		if false {
			failed = append(failed, t)
		}`,
		pkg: "./report",
	},
	{
		name: "对照任务不再单独成节",
		file: "report/report.go",
		old: `		if t.NegativeControl {
			controls = append(controls, t)`,
		new: `		if false {
			controls = append(controls, t)`,
		pkg: "./report",
	},

	// ---- 环境偷偷换 ----
	{
		name: "消融键允许不带服务名",
		file: "runner/stack.go",
		old:  `	if dot <= 0 || dot == len(k)-1 {`,
		new:  `	if dot < 0 || dot == len(k)-1 {`,
		pkg:  "./runner",
		slow: true,
	},
	{
		name: "端口重复发放（两个服务抢一个端口）",
		file: "runner/stack.go",
		old: `	if portTaken[port] {
		return false
	}`,
		new: `	if false {
		return false
	}`,
		pkg:  "./runner",
		slow: true,
	},
	{
		name: "源码指纹不随内容变化（拿旧二进制测新代码）",
		file: "runner/stack.go",
		old:  `			h.Write(b)`,
		new:  `			h.Write([]byte{0})`,
		pkg:  "./runner",
		slow: true,
	},
}
