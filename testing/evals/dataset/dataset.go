// Package dataset 定义并加载评测任务集。
//
// 一条任务 = 一次「给定输入 → 期望终态」的完整工作。判定只看世界状态，
// **不看模型自述**：reasoner 自己调 task_status done 不算成功，只有 expect 全过才算。
package dataset

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NouSart/sump/evals/fakeopenai"
)

// Budget 是一次任务的资源上限；超出即判失败，失败原因写明是哪一项。
type Budget struct {
	MaxHops   int `json:"max_hops,omitempty"`
	MaxWallMS int `json:"max_wall_ms,omitempty"`
}

// FileContains 断言某文件包含某段文本。
type FileContains struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// MemoryHas 断言记忆里存在某类记录且内容包含指定文本。
type MemoryHas struct {
	Kind     string `json:"kind,omitempty"` // reasoning / tool_call / tool_result / assistant / user
	Contains string `json:"contains"`
}

// Expectation 是一条终态断言；一条任务的全部断言都过才算成功。
// 只允许写一个字段。
type Expectation struct {
	FileExists      string        `json:"file_exists,omitempty"`
	FileContains    *FileContains `json:"file_contains,omitempty"`
	MemoryHas       *MemoryHas    `json:"memory_has,omitempty"`
	DeliverContains string        `json:"deliver_contains,omitempty"`
	Delivered       *bool         `json:"delivered,omitempty"`
	// FailureNotified 断言 harness 把故障明确告知了入口方（而不是假装成功）。
	FailureNotified *bool `json:"failure_notified,omitempty"`
	// NoToolFailure 断言工具调用没有失败（tool_result 里没有失败标记）。
	NoToolFailure bool `json:"no_tool_failure,omitempty"`
	// RemindersAtLeast 断言收尾门禁至少催了几轮（测"没判定就不许交付"）。
	RemindersAtLeast int `json:"reminders_at_least,omitempty"`
	// GateRejectsAtLeast 断言工具闸门拒了几次（测"未暴露的动作调不动"）。
	GateRejectsAtLeast int `json:"gate_rejects_at_least,omitempty"`
	// HopsAtMost 断言编排没有白跑（跳数上限）。
	HopsAtMost int `json:"hops_at_most,omitempty"`
	// FileAbsent 断言文件**不存在**——负向对照用：模型说做完了，但世界没变就必须判失败。
	FileAbsent string `json:"file_absent,omitempty"`
}

// Task 是一条评测任务。
type Task struct {
	ID       string        `json:"id"`
	Category string        `json:"category"`
	Prompt   string        `json:"prompt"`
	Tags     []string      `json:"tags,omitempty"`
	Budget   Budget        `json:"budget,omitempty"`
	Expect   []Expectation `json:"expect"`
	// Script 是"被冻结的模型"：harness 是唯一自变量，模型行为由剧本固定。
	Script fakeopenai.Script `json:"script"`
	// Sandbox 为真表示任务会破坏工作区，必须在可丢弃环境里跑（当前未实现，加载时拒绝）。
	Sandbox bool `json:"sandbox,omitempty"`
	// MustFail 标记负向对照：这条任务**必须被判失败**，用来证明评测器不会给"只说不做"盖章。
	MustFail bool `json:"must_fail,omitempty"`
	// Setup 是任务开始前要铺好的工作区（文件名 → 内容），断言据此检查终态。
	Setup Setup `json:"setup,omitempty"`
}

// Setup 描述任务的前置条件。
type Setup struct {
	Files map[string]string `json:"files,omitempty"`
}

// Load 读一个 JSONL 任务集（空行与 # 开头的行跳过）。
func Load(path string) ([]Task, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Task
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var t Task
		if err := json.Unmarshal([]byte(text), &t); err != nil {
			return nil, fmt.Errorf("%s:%d 解析失败: %w", filepath.Base(path), line, err)
		}
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("%s:%d %w", filepath.Base(path), line, err)
		}
		if t.Budget.MaxHops <= 0 {
			t.Budget.MaxHops = 20
		}
		if t.Budget.MaxWallMS <= 0 {
			t.Budget.MaxWallMS = 120000
		}
		out = append(out, t)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s 里没有任务", path)
	}
	return out, nil
}

// Validate 检查任务是否可执行：ID/prompt/expect 必填，断言只能写一个字段。
func (t Task) Validate() error {
	if strings.TrimSpace(t.ID) == "" {
		return fmt.Errorf("缺少 id")
	}
	if strings.TrimSpace(t.Prompt) == "" {
		return fmt.Errorf("%s: 缺少 prompt", t.ID)
	}
	if len(t.Expect) == 0 {
		return fmt.Errorf("%s: 缺少 expect（没有终态断言就无法判定成功）", t.ID)
	}
	if t.Sandbox {
		return fmt.Errorf("%s: sandbox=true 的任务当前不支持（可丢弃环境未实现）", t.ID)
	}
	for i, e := range t.Expect {
		n := 0
		if e.FileExists != "" {
			n++
		}
		if e.FileContains != nil {
			n++
		}
		if e.MemoryHas != nil {
			n++
		}
		if e.DeliverContains != "" {
			n++
		}
		if e.Delivered != nil {
			n++
		}
		if e.FailureNotified != nil {
			n++
		}
		if e.NoToolFailure {
			n++
		}
		if e.RemindersAtLeast > 0 {
			n++
		}
		if e.GateRejectsAtLeast > 0 {
			n++
		}
		if e.HopsAtMost > 0 {
			n++
		}
		if e.FileAbsent != "" {
			n++
		}
		if n != 1 {
			return fmt.Errorf("%s: expect[%d] 必须且只能写一个断言字段（当前 %d 个）", t.ID, i, n)
		}
	}
	return nil
}
