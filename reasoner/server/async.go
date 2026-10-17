package server

// 异步受理的补报（report_result）：一个“受理即回执”的动作（Provide.Async）在受理之后
// 由该服务发回后续消息——可能晚于任务收尾。
//
// 边界：
//   - 受理回执就是这次工具调用的结果位（tool_calls 配对已闭合）；补报是独立的一跳；
//   - 补报不进入任务的 pending 表：受理已经完成，模型的收尾不该被一条长命令扣住
//     （wait 会如实披露“已受理未回报”的条数，但不阻塞等待它们）；
//   - 任务还在跑 → 注入黑板并唤醒；任务已收尾 → 以补报开启“补报轮”，结果不丢。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
)

// asyncJob 记录一次已受理、等待补报的异步工具调用。
type asyncJob struct {
	cid      string // 会话（补报回到哪个任务）
	callID   string // 该次工具调用的 job id（模型上下文里也是它）
	name     string // 工具名（服务__动作）
	boss     string // 任务原老板（补报轮交付用）
	trace    string // 链路 id（原样沿用）
	taskID   string // 归属任务（补报开的轮次也算在这个任务里）
	taskName string
	at       time.Time
}

// asyncJobTTL 是记录的兜底寿命：服务若中途死掉可能永远不发终态补报，过期即清（防无界增长）。
// 取 24 小时——远大于任何合理的执行超时（`exec.timeout_ms` 预设 5 分钟），日常不会误清。
const asyncJobTTL = 24 * time.Hour

// followUpHint 是补报轮里追加给模型的说明（这是要发给用户的补充，不是内部思考）。
const followUpHint = "（上面的内容是先前工具的后续补报——任务可能已经结束。请给用户一句简短补充：这就是要发送给用户的内容。）"

// registerAsync 登记一条在途的异步受理（句柄来自受理回执）。
func (s *Server) registerAsync(item string, j asyncJob) {
	j.at = time.Now()
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	s.asyncJobs[item] = j
	s.sweepAsyncLocked(time.Now())
}

// takeAsync 取出某句柄的登记；final 为真（终态补报）时同时删除。
// 顺带清理过期登记（服务死掉时的兜底）。
func (s *Server) takeAsync(item string, final bool) (asyncJob, bool) {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	j, ok := s.asyncJobs[item]
	if ok && final {
		delete(s.asyncJobs, item)
	}
	s.sweepAsyncLocked(time.Now())
	return j, ok
}

// outstandingAsync 返回某会话“已受理但尚未回报”的异步任务（wait 披露用）。
func (s *Server) outstandingAsync(cid string) []string {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	var out []string
	for item, j := range s.asyncJobs {
		if j.cid == cid {
			out = append(out, fmt.Sprintf("%s(job=%s, item=%s)", j.name, j.callID, item))
		}
	}
	return out
}

// sweepAsyncLocked 清掉过期登记（调用方持有 asyncMu）。
func (s *Server) sweepAsyncLocked(now time.Time) {
	for item, j := range s.asyncJobs {
		if now.Sub(j.at) > asyncJobTTL {
			delete(s.asyncJobs, item)
			s.logger.Printf("异步登记过期清理: item=%s（%s）", item, j.name)
		}
	}
}

// actionReportResult 承接异步补报：先前受理的工具调用（Provide.Async）的后续进展。
func (s *Server) actionReportResult(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.AsyncResultPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if p.ItemID == "" {
		return nil, errors.New("report_result: item_id 必填")
	}
	j, ok := s.takeAsync(p.ItemID, p.Final)
	if !ok {
		s.logger.Printf("补报 %s（phase=%s）没有对应的在途受理（已被取代 / 终态已收 / 服务重启过），忽略", p.ItemID, p.Phase)
		return map[string]any{"status": "ignored"}, nil
	}
	where := s.deliverFollowUp(j, reportText(j, p))
	// 终态补报里有**真正的输出**（异步工具的回执只是"已排队"，输出在补报里）——
	// 记一条轨迹，界面才看得见终端到底吐了什么。
	if p.Final && strings.TrimSpace(p.Text) != "" {
		s.record(j.cid, j.boss, j.trace, j.taskID, kindToolResult, "tool", p.Text, map[string]any{
			"tool": j.name, "call_id": j.callID, "item_id": p.ItemID, "phase": p.Phase,
		})
	}
	s.logger.Printf("会话 %s：补报 %s（phase=%s, final=%v）→ %s", j.cid, p.ItemID, p.Phase, p.Final, where)
	return map[string]any{"status": "accepted", "delivery": where}, nil
}

// deliverFollowUp 把一条补报送进对应的任务：任务在跑就注入并唤醒，否则开一轮补报任务。
func (s *Server) deliverFollowUp(j asyncJob, text string) string {
	st := s.boardGet(j.cid)
	if st != nil {
		st.mu.Lock()
		if !st.closed {
			st.messages = append(st.messages, llm.Message{Role: "user", Content: text})
			st.gen++
			st.mu.Unlock()
			s.boardKick(st)
			return "injected"
		}
		st.mu.Unlock()
	}
	if j.boss == "" {
		// 没有老板就没有交付对象：补报轮无处可去，如实记下（不假装已送达）。
		s.logger.Printf("会话 %s：补报到达但任务已收尾且无 boss，无法补交给用户", j.cid)
		return "dropped_no_boss"
	}
	st = s.boardOpen(j.cid, j.boss, j.trace, j.taskID, j.taskName, []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: text + "\n\n" + followUpHint},
	})
	s.boardKick(st)
	return "new_task"
}

// reportText 把一条补报整理成注入消息链的文本（与 [工具结果] 同构：role=user 的外部回报）。
func reportText(j asyncJob, p protocol.AsyncResultPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[后续补报] %s（job=%s, item=%s, phase=%s）", j.name, j.callID, p.ItemID, p.Phase)
	if !p.Final {
		b.WriteString("——不是最终结果，结果还会再送达一条")
	}
	if p.Text == "" {
		b.WriteString("（无文本）")
		return b.String()
	}
	b.WriteString("：\n")
	b.WriteString(p.Text)
	return b.String()
}
