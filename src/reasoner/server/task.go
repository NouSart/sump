// 任务判定：模型用内置工具 task_status 判定"这次结果算不算把任务做完"。
//
// 判定权在这里（模型说 done 才算 done），记忆服务只负责记账。
// 判定是**收尾门禁**的一部分（见 board.go 的 tryClose）：没判定就不许交付。
package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
)

// commitJudgement 把这一轮的判定落到记忆里的任务上：done → task_finish（顺带改名），
// 否则只改名（任务保持进行中）。**只在真正收尾时调**（交付成功 / 静默收尾）。
//
// 为什么不在 task_status 当场落：判定之后这一轮可能被拒收尾（还有工具在跑），或者紧接着又
// 派发了新工具（判定当场作废）。提前把任务标成 done，后面的工作就会挂在一个"已完成"的任务里，
// 而记忆服务不会把 done 的任务重新打开（task_resume 拒绝、task_rename 不改状态）——模型下半程
// 说的 continue 就成了一句空话。
func (s *Server) commitJudgement(st *taskState, trace string) {
	st.mu.Lock()
	taskID, done, title := st.taskID, st.judgedDone, st.judgedTitle
	st.mu.Unlock()
	if taskID == "" {
		return // 没有任务就没有判定对象（记忆服务没给任务的那一轮）
	}
	if done {
		// 完结是终态：顺带把任务名改成模型给的那个（更像人写的标题）。
		s.Fire(s.memoryName(), "task_finish", protocol.TaskFinishPayload{TaskID: taskID, Name: title}, trace, s.cfg.Name)
		return
	}
	if title != "" {
		s.Fire(s.memoryName(), "task_rename", protocol.TaskRenamePayload{TaskID: taskID, Name: title}, trace, s.cfg.Name)
	}
}

// clampTitle 把任务名压成一行、截到 24 个字（历史列表的标题要能一眼看完）。
// 规则本身住在协议包里（protocol.ClampTaskName）：qq 直接落库时用的是同一份。
func clampTitle(title string) string {
	return protocol.ClampTaskName(title)
}

// applyTaskStatus 记录模型对当前任务的判定，返回给模型的工具结果文本。
// 解析不出来 / 值不合法时按"没判清"处理：不算完成（保守），并把用法告诉模型。
//
// 判定只记在黑板里，**当场不落库**：这一轮可能随后被拒收尾（还有工具在跑），也可能又派发了
// 新工具（判定随之作废）。真正把它交给记忆服务的是收尾时的 commitJudgement。
func (s *Server) applyTaskStatus(st *taskState, call llm.ToolCall) string {
	var args struct {
		Status string `json:"status"`
		Title  string `json:"title"`
		Note   string `json:"note"`
		Silent bool   `json:"silent"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "task_status 参数解析失败：需要 JSON 对象 {status: \"done\"|\"continue\", title?, note?, silent?}。这次不算判定。"
	}
	status := strings.TrimSpace(strings.ToLower(args.Status))
	if status != "done" && status != "continue" {
		return fmt.Sprintf("task_status 的 status 只能是 done 或 continue（收到 %q）。这次不算判定。", args.Status)
	}
	done := status == "done"
	title := clampTitle(args.Title)

	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return "任务已结束，判定未记录。"
	}
	st.judged = true
	st.judgedDone = done
	// 静默 = 这一轮不发出去（群里判断不该插话时用）。它随每次判定重设：
	// 上一轮选了静默，这一轮想说话就正常说。
	st.judgedSilent = args.Silent
	if title != "" {
		st.judgedTitle = title
		st.taskName = title
	}
	st.mu.Unlock()

	st.mu.Lock()
	name := st.taskName
	st.mu.Unlock()
	if name == "" {
		name = "未命名任务"
	}
	verb := "未完成（继续）"
	if done {
		verb = "已完成"
	}
	if args.Note != "" {
		s.logger.Printf("会话 %s：任务判定 %s（%s）—— %s", st.cid, verb, name, args.Note)
	} else {
		s.logger.Printf("会话 %s：任务判定 %s（%s）", st.cid, verb, name)
	}
	if args.Silent {
		s.logger.Printf("会话 %s：这一轮选择静默（不发出去）", st.cid)
		return fmt.Sprintf("已记录：任务《%s》判定为 %s；这一轮的答复不会发出去（只留在记录里）。", name, verb)
	}
	return fmt.Sprintf("已记录：任务《%s》判定为 %s。", name, verb)
}
