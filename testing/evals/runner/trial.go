package runner

// 一次 trial：驱动一条任务，采事实，判终态。
//
// 判定原则：**只看世界状态**。reasoner 自己调了 task_status done 不算成功——
// 那是被测对象的自述。成功 = expect 里的断言全过、没超预算、没有契约违规。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NouSart/sump/evals/dataset"
	"github.com/NouSart/sump/evals/fakeopenai"
)

// idleWindow 是"什么都不再发生"的判定窗口：超时或卡死的任务不该干等到墙钟上限。
// 取 5s：正常链路从 user_message 到第一次 LLM 调用是毫秒级，留够余量给慢模型。
const idleWindow = 5 * time.Second

// Result 是一次 trial 的全部事实与判定。
type Result struct {
	TaskID   string `json:"task_id"`
	Category string `json:"category"`
	Trial    int    `json:"trial"`
	ConvID   string `json:"conversation_id"`
	WorkDir  string `json:"work_dir,omitempty"`

	Success    bool   `json:"success"`
	FailReason string `json:"fail_reason,omitempty"`

	Delivered bool     `json:"delivered"`
	Deliver   []string `json:"deliver,omitempty"`
	// FailureNotified 表示 harness 把故障明确告知了入口方（"本轮推理失败…"），而不是假装成功。
	FailureNotified bool     `json:"failure_notified"`
	Answer          []string `json:"answer,omitempty"`

	Hops         int      `json:"hops"`
	ToolCalls    int      `json:"tool_calls"`
	ToolResults  int      `json:"tool_results"`
	GateRejects  int      `json:"gate_rejects"`
	Reminders    int      `json:"reminders"`
	WallMS       int64    `json:"wall_ms"`
	TokensIn     int      `json:"tokens_in"`
	TokensOut    int      `json:"tokens_out"`
	Judged       bool     `json:"judged"`
	MatchedSteps []string `json:"matched_steps,omitempty"`

	Violations []string `json:"violations,omitempty"`
	Failed     []string `json:"failed_expect,omitempty"`
	// Invalid 表示这次 trial 的结论不可用（例如故障没真的注入）。
	Invalid bool `json:"invalid,omitempty"`
	// NegativeControl 表示这是"必须被判失败"的对照任务，且评测器确实判了失败。
	NegativeControl bool `json:"negative_control,omitempty"`
	// ExpectFailReason 是负向对照"本该失败"的原因（证据；不参与通过/失败判定）。
	ExpectFailReason string `json:"expect_fail_reason,omitempty"`
	// Trajectory 是记忆里这条会话的全部记录（含 reasoning/tool_call/tool_result）——归因的证据。
	Trajectory []HistoryEntry `json:"trajectory,omitempty"`
	// TrajectoryUnavailable 表示本次 trial 起栈时没有记忆服务，所以没有轨迹可看。
	TrajectoryUnavailable bool `json:"trajectory_unavailable,omitempty"`
}

// RunTask 跑一条任务的一次 trial。
func RunTask(st *Stack, fake *fakeopenai.Server, task dataset.Task, trial int, convID string) Result {
	res := Result{TaskID: task.ID, Category: task.Category, Trial: trial, ConvID: convID, WorkDir: st.WorkDir()}

	fake.Reset()
	st.Boss.Reset()

	// 铺好工作区：断言看的是终态，前置条件必须显式（不能靠"上一轮留下的文件"）。
	for name, content := range task.Setup.Files {
		p := filepath.Join(res.WorkDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			res.FailReason = "铺工作区失败: " + err.Error()
			return finish(task, res)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			res.FailReason = "铺工作区失败: " + err.Error()
			return finish(task, res)
		}
	}

	started := time.Now()
	if err := st.Send(convID, task.Prompt); err != nil {
		res.WallMS = time.Since(started).Milliseconds()
		res.FailReason = "发起失败: " + err.Error()
		return finish(task, res)
	}

	deadline := started.Add(time.Duration(task.Budget.MaxWallMS) * time.Millisecond)
	lastHops := 0
	lastProgress := time.Now()
	for time.Now().Before(deadline) {
		if len(st.Boss.Delivered()) > 0 {
			break
		}
		hops := fake.Calls()
		if hops != lastHops {
			lastHops = hops
			lastProgress = time.Now()
		}
		if hops > task.Budget.MaxHops {
			break
		}
		// 有进展但已经停住（例如模型始终不判定、链自然停住）：不必干等墙钟上限。
		// 一次进展都没有也要收：依赖缺席时链会停在入口，此时同样不该占满预算。
		if time.Since(lastProgress) > idleWindow {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	res.WallMS = time.Since(started).Milliseconds()

	res.Deliver = st.Boss.Delivered()
	res.Answer, res.FailureNotified = splitDeliveries(res.Deliver)
	res.Delivered = len(res.Answer) > 0

	records := fake.Records()
	res.Hops = len(records)
	for _, r := range records {
		res.TokensIn += r.Usage.PromptTokens
		res.TokensOut += r.Usage.CompletionTokens
		if r.StepName != "" {
			res.MatchedSteps = append(res.MatchedSteps, r.StepName)
		}
		if r.HasReminder {
			res.Reminders++
		}
	}

	var hist []HistoryEntry
	if st.HasMemory() {
		var herr error
		hist, herr = st.History(convID)
		if herr != nil {
			res.Violations = append(res.Violations, "拉取轨迹失败: "+herr.Error())
		}
		res.Trajectory = hist
	} else {
		// 依赖缺席：没有记忆服务就没有轨迹。这是被测环境的事实，
		// 记成"轨迹不可用"，**不能**记成契约违规（否则违规计数会被自己的脚手架污染）。
		res.TrajectoryUnavailable = true
	}
	for _, h := range hist {
		switch h.Kind {
		case "tool_call":
			res.ToolCalls++
		case "tool_result":
			res.ToolResults++
			if strings.Contains(h.Content, "工具不可用") {
				res.GateRejects++
			}
		}
	}

	res.Judged = judgeSeen(records)

	// 故障注入是否真的打进去了：剧本声明了故障却一次没命中，这条 trial 的结论无效。
	// （否则"扛住了故障"只是没测到故障——狐假虎威。）
	if unmatched := fake.UnmatchedFaults(); len(unmatched) > 0 {
		markInvalid(&res, unmatched)
		return finish(task, res)
	}

	// 契约违规：交付前必须判定过任务（收尾门禁的核心承诺）。
	// 催过至少一轮之后仍按 continue 记账并交付，是设计允许的；一次都不催就交付才是破约。
	res.Violations = append(res.Violations, contractViolations(res.Delivered, res.Judged, res.Reminders)...)

	// 预算。
	switch {
	case res.Hops > task.Budget.MaxHops:
		res.FailReason = fmt.Sprintf("超出跳数预算（%d > %d）", res.Hops, task.Budget.MaxHops)
	case res.WallMS > int64(task.Budget.MaxWallMS):
		res.FailReason = fmt.Sprintf("超出时长预算（%dms > %dms）", res.WallMS, task.Budget.MaxWallMS)
	case !res.Delivered && taskExpectsDelivery(task):
		res.FailReason = "未在预算内交付"
	}

	failed := evaluate(task, res, hist)
	if len(failed) > 0 {
		res.Failed = failed
		if res.FailReason == "" {
			res.FailReason = strings.Join(failed, "; ")
		}
		return finish(task, res)
	}
	if res.FailReason != "" {
		return finish(task, res)
	}
	if len(res.Violations) > 0 {
		res.FailReason = "契约违规: " + strings.Join(res.Violations, "; ")
		return finish(task, res)
	}
	res.Success = true
	return finish(task, res)
}

// contractViolations 返回本次 trial 违反的编排契约。
//
// 规则：交付了、模型没调过 task_status、而 harness 连一次都没催 → 破约。
// 催过之后仍交付是设计允许的（两次都不判按 continue 记账），所以不能一并算违规。
func contractViolations(delivered, judged bool, reminders int) []string {
	if delivered && !judged && reminders == 0 {
		return []string{"交付前没有 task_status 判定，也没有催过"}
	}
	return nil
}

// markInvalid 把"结论不可用"记到结果上：声明了故障却没命中，不能算通过也不能算失败，
// 只能算没测到。报告里它是一条红色的「注入无效」。
func markInvalid(res *Result, unmatched []string) {
	res.Invalid = true
	res.Success = false
	res.FailReason = "故障剧本未命中，注入无效: " + strings.Join(unmatched, ",")
}

// finish 收尾：处理负向对照与故障注入有效性，再返回。
func finish(task dataset.Task, res Result) Result {
	if res.Invalid {
		// 注入无效的 trial 不做负向对照倒置：连"测没测到"都不确定，就别再宣称评测器自检通过。
		return res
	}
	if task.MustFail {
		// 负向对照：确认"只说不做"确实被判失败，才算评测器通过。
		if res.Success {
			res.Success = false
			res.FailReason = "负向对照失效：模型只说不做，评测器却判了成功"
		} else {
			// 把"它为什么本该失败"留作证据，避免报告里出现"✅ 但失败原因=…"的自相矛盾。
			res.ExpectFailReason = res.FailReason
			res.FailReason = ""
			res.Success = true
			res.NegativeControl = true
		}
	}
	return res
}

// taskExpectsDelivery 判断这条任务是否要求"必须交付"。
func taskExpectsDelivery(t dataset.Task) bool {
	for _, e := range t.Expect {
		if e.DeliverContains != "" {
			return true
		}
		if e.Delivered != nil && *e.Delivered {
			return true
		}
	}
	return false
}

func judgeSeen(records []fakeopenai.Record) bool {
	for _, r := range records {
		if r.JudgeSeen {
			return true
		}
	}
	return false
}

// evaluate 跑终态断言，返回未通过的描述。
func evaluate(task dataset.Task, res Result, hist []HistoryEntry) []string {
	var failed []string
	for _, e := range task.Expect {
		switch {
		case e.FileExists != "":
			if _, err := os.Stat(filepath.Join(res.WorkDir, e.FileExists)); err != nil {
				failed = append(failed, "文件不存在: "+e.FileExists)
			}
		case e.FileContains != nil:
			b, err := os.ReadFile(filepath.Join(res.WorkDir, e.FileContains.Path))
			if err != nil {
				failed = append(failed, "文件读不到: "+e.FileContains.Path)
				break
			}
			if !strings.Contains(string(b), e.FileContains.Text) {
				failed = append(failed, fmt.Sprintf("文件 %s 不含 %q", e.FileContains.Path, e.FileContains.Text))
			}
		case e.MemoryHas != nil:
			if !memoryHas(hist, e.MemoryHas.Kind, e.MemoryHas.Contains) {
				failed = append(failed, fmt.Sprintf("记忆里没有 kind=%s 且含 %q 的记录", e.MemoryHas.Kind, e.MemoryHas.Contains))
			}
		case e.DeliverContains != "":
			if !anyContains(res.Answer, e.DeliverContains) {
				failed = append(failed, fmt.Sprintf("交付文本不含 %q", e.DeliverContains))
			}
		case e.FailureNotified != nil:
			if *e.FailureNotified && !res.FailureNotified {
				failed = append(failed, "故障没有告知入口方")
			}
			if !*e.FailureNotified && res.FailureNotified {
				failed = append(failed, "不该报故障却报了")
			}
		case e.Delivered != nil:
			if *e.Delivered && !res.Delivered {
				failed = append(failed, "没有交付")
			}
			if !*e.Delivered && res.Delivered {
				failed = append(failed, "不应交付却交付了")
			}
		case e.NoToolFailure:
			if res.GateRejects > 0 {
				failed = append(failed, fmt.Sprintf("有 %d 次工具被闸门拒绝", res.GateRejects))
			}
		case e.RemindersAtLeast > 0:
			if res.Reminders < e.RemindersAtLeast {
				failed = append(failed, fmt.Sprintf("收尾门禁只催了 %d 次（应 ≥ %d）", res.Reminders, e.RemindersAtLeast))
			}
		case e.GateRejectsAtLeast > 0:
			if res.GateRejects < e.GateRejectsAtLeast {
				failed = append(failed, fmt.Sprintf("闸门拒绝 %d 次（应 ≥ %d）", res.GateRejects, e.GateRejectsAtLeast))
			}
		case e.HopsAtMost > 0:
			if res.Hops > e.HopsAtMost {
				failed = append(failed, fmt.Sprintf("跳数 %d（应 ≤ %d）", res.Hops, e.HopsAtMost))
			}
		case e.FileAbsent != "":
			if _, err := os.Stat(filepath.Join(res.WorkDir, e.FileAbsent)); err == nil {
				failed = append(failed, "文件不该存在却存在: "+e.FileAbsent)
			}
		}
	}
	return failed
}

func memoryHas(hist []HistoryEntry, kind, contains string) bool {
	for _, h := range hist {
		if kind != "" && h.Kind != kind {
			continue
		}
		if strings.Contains(h.Content, contains) {
			return true
		}
	}
	return false
}

func anyContains(texts []string, sub string) bool {
	for _, t := range texts {
		if strings.Contains(t, sub) {
			return true
		}
	}
	return false
}

// isFailureNotice 认出 harness 的失败通知（board.go 的「本轮推理失败」模板）。
func isFailureNotice(text string) bool {
	return strings.Contains(text, "本轮推理失败") || strings.Contains(text, "[交付失败]")
}

// splitDeliveries 把 boss 收到的文本分成"答复"与"失败通知"两类。
//
// 失败通知不是"交付"：它是 harness 在认错。两者必须分开统计，
// 否则一次 LLM 500 会被记成"成功交付"——那正是掩耳盗铃。
func splitDeliveries(delivered []string) (answers []string, failureNotified bool) {
	for _, text := range delivered {
		if isFailureNotice(text) {
			failureNotified = true
			continue
		}
		answers = append(answers, text)
	}
	return answers, failureNotified
}

// SaveTrace 把一次 trial 的原始事实落到磁盘（报告之外的证据，供归因用）。
func SaveTrace(dir string, res Result, records []fakeopenai.Record) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-t%d.json", res.TaskID, res.Trial)
	payload := map[string]any{"result": res, "llm_requests": records}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), b, 0o644)
}
