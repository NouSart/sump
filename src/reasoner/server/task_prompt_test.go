package server

// 任务提示插入位置（特征化测试，钉住「实际发生什么」）：
// 第 0 条原样保留、任务提示插在索引 1（三值切片 `[:1:1]` 防止外层 append 覆写）。
// 本文件把这个形状钉住：
//   - 一条不丢、一条不被覆盖（内容逐字保留）；
//   - 角色顺序 = [原第 0 条, system(任务提示), 其余原样]，即使第 0 条是 user。

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/reasoner/llm"
)

// TestUserFirstChainKeepsFirstMessage 是本次的核心用例：上下文以 user 开头时，
// 任务提示插在索引 1，第 0 条用户消息原样留着。
func TestUserFirstChainKeepsFirstMessage(t *testing.T) {
	in := []llm.Message{
		{Role: "user", Content: "第一条就是用户先开口"},
		{Role: "assistant", Content: "上一轮的助手发言"},
		{Role: "user", Content: "这一轮用户又说了"},
	}
	got := withTaskPrompt(in, "排查端口", "task-7")
	// 实际结果的角色顺序（不是「[0] 一定是 system」）：任务提示夹在第 0 条之后。
	if strings.Join(rolesOf(got), ",") != "user,system,assistant,user" {
		t.Fatalf("角色顺序应为 [user, system, assistant, user]，实际 %+v（消息 %+v）", rolesOf(got), got)
	}
	if len(got) != len(in)+1 {
		t.Fatalf("应只多出 1 条任务提示：得到 %d 条，输入 %d 条", len(got), len(in))
	}
	// 一条不丢、一条不被覆盖。
	assertKept(t, got, 0, in[0])
	assertKept(t, got, 2, in[1])
	assertKept(t, got, 3, in[2])
	// 多出来的那条是任务提示，位置在索引 1。
	if got[1].Role != "system" || !strings.Contains(got[1].Content, "当前任务：《排查端口》（id=task-7）") {
		t.Fatalf("索引 1 应是任务提示: %+v", got[1])
	}
	if strings.Contains(got[0].Content, "当前任务：") {
		t.Fatalf("第 0 条被任务提示顶掉了（审计报告描述的行为，不该发生）: %q", got[0].Content)
	}
}

// TestSystemFirstChainKeepsSystemPromptAtZero 记录今天真实的形状：
// 生产者给的上下文以 system 开头，于是插入后是两条 system（骨架提示 + 任务提示）。
// 第 0 条仍然是原文，不是被覆盖后的任务提示。
func TestSystemFirstChainKeepsSystemPromptAtZero(t *testing.T) {
	in := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "现在几点了"},
	}
	got := withTaskPrompt(in, "查时间", "task-8")
	if strings.Join(rolesOf(got), ",") != "system,system,user" {
		t.Fatalf("角色顺序应为 [system, system, user]，实际 %+v", rolesOf(got))
	}
	if got[0].Content != systemPrompt {
		t.Fatalf("索引 0 必须还是原 system 提示（逐字）: %q", clip(got[0].Content, 120))
	}
	if !strings.Contains(got[1].Content, "当前任务：《查时间》（id=task-8）") {
		t.Fatalf("索引 1 应是任务提示: %q", got[1].Content)
	}
	assertKept(t, got, 2, in[1])
}

// TestTaskPromptNamesTask 验证提示文本里同时带任务名与 id（模型据此知道在为哪个任务工作）。
func TestTaskPromptNamesTask(t *testing.T) {
	got := taskPrompt("查时间", "task-8")
	if !strings.Contains(got, "《查时间》") || !strings.Contains(got, "id=task-8") {
		t.Fatalf("任务提示应带任务名与 id: %q", got)
	}
}
