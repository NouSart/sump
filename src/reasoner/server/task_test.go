// 任务判定的直接测试。
//
// `task_status` 是收尾门禁的输入：模型说 done 才算 done，说 continue 任务就继续。
// 判定取反会让「继续」被按「已完成」记账（task_finish 发给记忆服务，任务被错误终结）。
// 之前 task.go 在包内零覆盖 —— 12 个变异体全存活。
package server

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/NouSart/sump/reasoner/llm"
)

func newTaskStatusServer() *Server {
	return &Server{
		logger: log.New(io.Discard, "", 0),
		cfg:    Config{Name: "reasoner"},
	}
}

func statusCall(args string) llm.ToolCall {
	return llm.ToolCall{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "task_status", Arguments: args}}
}

// 判定 done：judged/judgedDone 要立起来，标题跟着改。
func TestApplyTaskStatusDone(t *testing.T) {
	s := newTaskStatusServer()
	st := &taskState{cid: "c", taskName: "旧标题"}

	s.applyTaskStatus(st, statusCall(`{"status":"done","title":"改个名"}`))

	if !st.judged || !st.judgedDone {
		t.Fatalf("done 应记为已判定且已完成: judged=%v judgedDone=%v", st.judged, st.judgedDone)
	}
	if st.taskName != "改个名" || st.judgedTitle != "改个名" {
		t.Fatalf("标题没跟上: taskName=%q judgedTitle=%q", st.taskName, st.judgedTitle)
	}
}

// 判定 continue：judged 立起来，但 judgedDone 必须保持 false —— 这是「不许收尾」的依据。
func TestApplyTaskStatusContinue(t *testing.T) {
	s := newTaskStatusServer()
	st := &taskState{cid: "c", taskName: "旧标题"}

	s.applyTaskStatus(st, statusCall(`{"status":"continue"}`))

	if !st.judged {
		t.Fatal("continue 也要算「已判定」，否则收尾门禁会一直催")
	}
	if st.judgedDone {
		t.Fatal("continue 不能被当成完成")
	}
	if st.taskName != "旧标题" {
		t.Fatalf("没给 title 时不该改名: %q", st.taskName)
	}
}

// 非法 status / 坏 JSON：不算判定（保守），且要把用法告诉模型。
func TestApplyTaskStatusRejectsBadInput(t *testing.T) {
	s := newTaskStatusServer()
	for _, tc := range []struct{ name, args, wantHint string }{
		{name: "坏 JSON", args: `{`, wantHint: "参数解析失败"},
		{name: "未知取值", args: `{"status":"maybe"}`, wantHint: "只能是 done 或 continue"},
		{name: "空取值", args: `{}`, wantHint: "只能是 done 或 continue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &taskState{cid: "c"}
			msg := s.applyTaskStatus(st, statusCall(tc.args))
			if st.judged {
				t.Fatal("非法输入不能算判定")
			}
			if !strings.Contains(msg, tc.wantHint) {
				t.Fatalf("回给模型的提示应含 %q，实际 %q", tc.wantHint, msg)
			}
		})
	}
}

// 任务已关闭：判定不再记录（迟到的工具结果不许改写终态）。
func TestApplyTaskStatusOnClosedTask(t *testing.T) {
	s := newTaskStatusServer()
	st := &taskState{cid: "c", closed: true}

	msg := s.applyTaskStatus(st, statusCall(`{"status":"done"}`))

	if st.judged || st.judgedDone {
		t.Fatal("已关闭的任务不该被判定改写")
	}
	if !strings.Contains(msg, "任务已结束") {
		t.Fatalf("应明确告诉模型任务已结束，实际 %q", msg)
	}
}

// clampTitle：压成一行、超 24 字截断加省略号。
func TestClampTitle(t *testing.T) {
	if got := clampTitle("  多  个\n空白 "); got != "多 个 空白" {
		t.Fatalf("空白归一化不符: %q", got)
	}
	long := strings.Repeat("字", 30)
	if got := clampTitle(long); len([]rune(got)) != 25 || !strings.HasSuffix(got, "…") {
		t.Fatalf("超长标题应截到 24 字 + 省略号: %q", got)
	}
	if got := clampTitle(""); got != "" {
		t.Fatalf("空标题应保持空: %q", got)
	}
}
