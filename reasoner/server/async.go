package server

// 异步受理与补报：派发前预留登记，回执决定 item_id 的归属。
// source + trace + item_id 共同关联；信封身份仍依赖 SUMP 可信网络，并非密码学认证。
// 仅待回执期间缓存早到消息，且数量、体积、寿命都有上限；不接管服务的业务任务状态。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
)

type asyncKey struct{ source, trace, item string }

type asyncJob struct {
	deliveryMu sync.Mutex // 单个句柄的进展与终态顺序处理，不阻塞其他调用。
	final      bool
	source     string
	cid        string
	callID     string
	name       string
	boss       string
	trace      string
	taskID     string
	taskName   string
	kind       string // once | resident; only the receipt establishes this classification
	origin     *taskState
	at         time.Time
}

const (
	asyncJobTTL       = 24 * time.Hour
	asyncRecordLimit  = 512 // 已受理 + 待回执；满额在工具派发前拒绝。
	asyncEarlyLimit   = 128
	asyncEarlyBytes   = 2 << 20
	asyncReportBytes  = 256 << 10
	asyncEarlyPerItem = 8
	asyncDoneTTL      = 10 * time.Minute
)

const followUpHint = "（上面的内容是先前工具的后续补报——任务可能已经结束。请给用户一句简短补充：这就是要发送给用户的内容。）"

func (s *Server) beginAsync(source string, st *taskState, call llm.ToolCall) (*asyncJob, error) {
	// task_status 可能同时改名；先取得一致的任务快照，再登记异步调用。
	// 两把锁分别持有，避免黑板锁与异步登记锁形成嵌套。
	st.mu.Lock()
	j := &asyncJob{source: source, cid: st.cid, callID: call.ID, name: call.Function.Name,
		boss: st.boss, trace: st.trace, taskID: st.taskID, taskName: st.taskName,
		origin: st, at: time.Now()}
	st.mu.Unlock()
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	s.sweepAsyncLocked(time.Now())
	if len(s.asyncJobs)+len(s.asyncPending) >= asyncRecordLimit {
		return nil, errors.New("异步回报登记已满，工具尚未派发")
	}
	s.asyncPending[j] = true
	return j, nil
}

// abandonAsync 仅取消本地待回执登记；绝不宣称远端工作已取消。
func (s *Server) abandonAsync(j *asyncJob) {
	s.asyncMu.Lock()
	wasPending := s.asyncPending[j]
	delete(s.asyncPending, j)
	s.pruneEarlyLocked()
	s.asyncMu.Unlock()
	if wasPending {
		s.noteAsyncDone(j.cid)
	}
}

func (s *Server) hasPendingLocked(k asyncKey) bool {
	for j := range s.asyncPending {
		if j.source == k.source && j.trace == k.trace {
			return true
		}
	}
	return false
}

func (s *Server) pruneEarlyLocked() {
	for k := range s.asyncEarly {
		if !s.hasPendingLocked(k) {
			delete(s.asyncEarly, k)
		}
	}
}

// finishAsync 在同一锁内将预留槽转成句柄登记并领取早到消息。
// 发布登记前锁住该 job，防止新消息越过早到消息（尤其终态）。
func (s *Server) finishAsync(j *asyncJob, item, kind string) error {
	k := asyncKey{j.source, j.trace, item}
	j.deliveryMu.Lock()
	defer j.deliveryMu.Unlock()
	s.asyncMu.Lock()
	if !s.asyncPending[j] {
		s.asyncMu.Unlock()
		return errors.New("异步受理回执已失去本地登记；执行结果未知")
	}
	if s.asyncJobs[k] != nil || !s.asyncDone[k].IsZero() {
		s.pruneEarlyLocked()
		s.asyncMu.Unlock()
		return errors.New("异步服务复用了在途或刚完成的 item_id；执行结果未知")
	}
	delete(s.asyncPending, j)
	j.kind = kind
	s.asyncJobs[k] = j
	early := s.asyncEarly[k]
	delete(s.asyncEarly, k)
	s.pruneEarlyLocked()
	s.asyncMu.Unlock()
	for _, p := range early {
		s.deliverAsyncLocked(k, j, p)
	}
	// A resident receipt may remove the last unknown, executable reservation.
	s.noteAsyncDone(j.cid)
	return nil
}

func (s *Server) outstandingAsync(cid string) []string {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	s.sweepAsyncLocked(time.Now())
	var out []string
	for k, j := range s.asyncJobs {
		if j.cid == cid {
			out = append(out, fmt.Sprintf("%s(job=%s, item=%s)", j.name, j.callID, k.item))
		}
	}
	return out
}

func (s *Server) sweepAsyncLocked(now time.Time) {
	for k, j := range s.asyncJobs {
		if now.Sub(j.at) > asyncJobTTL {
			delete(s.asyncJobs, k)
		}
	}
	for j := range s.asyncPending {
		if now.Sub(j.at) > asyncJobTTL {
			delete(s.asyncPending, j)
		}
	}
	for k, at := range s.asyncDone {
		if now.Sub(at) > asyncDoneTTL {
			delete(s.asyncDone, k)
		}
	}
	s.pruneEarlyLocked()
}

// rememberDoneLocked 用有限的墓碑避免重复终态被当作下一次待回执的消息。
func (s *Server) rememberDoneLocked(k asyncKey) {
	if len(s.asyncDone) >= asyncRecordLimit {
		var oldest asyncKey
		var at time.Time
		for key, t := range s.asyncDone {
			if at.IsZero() || t.Before(at) {
				oldest, at = key, t
			}
		}
		delete(s.asyncDone, oldest)
	}
	s.asyncDone[k] = time.Now()
}

func asyncPayloadSize(p protocol.AsyncResultPayload) int {
	return len(p.ItemID) + len(p.Phase) + len(p.Text) + len(p.Data)
}

func (s *Server) actionReportResult(_ context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	if len(in.Input) > asyncReportBytes {
		return nil, errors.New("report_result: 载荷超限")
	}
	var p protocol.AsyncResultPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if p.ItemID == "" || len(p.ItemID) > 512 || len(p.Phase) > 128 {
		return nil, errors.New("report_result: item_id 或 phase 无效")
	}
	if env.From == "" || env.Trace == "" || env.To != s.cfg.Name {
		return nil, errors.New("report_result: 来源、trace 或目标无效")
	}
	k := asyncKey{env.From, env.Trace, p.ItemID}
	s.asyncMu.Lock()
	s.sweepAsyncLocked(time.Now())
	j := s.asyncJobs[k]
	if j == nil {
		if !s.asyncDone[k].IsZero() {
			s.asyncMu.Unlock()
			return map[string]any{"status": "duplicate"}, nil
		}
		if !s.hasPendingLocked(k) {
			s.asyncMu.Unlock()
			return map[string]any{"status": "ignored"}, nil
		}
		early := s.asyncEarly[k]
		for _, old := range early {
			if old.Final {
				s.asyncMu.Unlock()
				return map[string]any{"status": "duplicate"}, nil
			}
		}
		count, bytes := 0, 0
		for _, reports := range s.asyncEarly {
			count += len(reports)
			for _, report := range reports {
				bytes += asyncPayloadSize(report)
			}
		}
		if count >= asyncEarlyLimit || bytes+asyncPayloadSize(p) > asyncEarlyBytes || len(early) >= asyncEarlyPerItem {
			s.asyncMu.Unlock()
			return nil, errors.New("report_result: 早到回报缓冲已满，未接收该回报")
		}
		s.asyncEarly[k] = append(early, p)
		s.asyncMu.Unlock()
		return map[string]any{"status": "buffered"}, nil
	}
	s.asyncMu.Unlock()
	j.deliveryMu.Lock()
	defer j.deliveryMu.Unlock()
	return s.deliverAsyncLocked(k, j, p), nil
}

// 调用方持有 job.deliveryMu；终态只处理一次，后续进度不能覆盖它。
func (s *Server) deliverAsyncLocked(k asyncKey, j *asyncJob, p protocol.AsyncResultPayload) map[string]any {
	if j.final {
		return map[string]any{"status": "duplicate"}
	}
	// 等待同句柄的前一条投递期间，登记可能已经过期。
	s.asyncMu.Lock()
	current := s.asyncJobs[k] == j && time.Since(j.at) <= asyncJobTTL
	s.asyncMu.Unlock()
	if !current {
		return map[string]any{"status": "ignored"}
	}
	if p.Final {
		j.final = true
	}
	where := s.deliverFollowUp(j, reportText(j, p))
	if p.Final && strings.TrimSpace(p.Text) != "" {
		s.record(j.cid, j.boss, j.trace, j.taskID, kindToolResult, "tool", p.Text, map[string]any{
			"tool": j.name, "call_id": j.callID, "item_id": p.ItemID, "phase": p.Phase,
		})
	}
	if p.Final {
		s.asyncMu.Lock()
		if s.asyncJobs[k] == j {
			delete(s.asyncJobs, k)
		}
		s.rememberDoneLocked(k)
		s.asyncMu.Unlock()
		// Remove the final job before checking whether secondary hold can resume.
		s.noteAsyncDone(j.cid)
	}
	s.logger.Printf("会话 %s：补报 %s（phase=%s, final=%v）→ %s", j.cid, p.ItemID, p.Phase, p.Final, where)
	return map[string]any{"status": "accepted", "delivery": where}
}

// 同一任务可注入或开补报轮；已切换任务时保留原 task_id 直接交付补报，
// 绝不把旧内容塞进新任务，也不为了补报而替换用户当前正在进行的任务。
func (s *Server) deliverFollowUp(j *asyncJob, text string) string {
	s.boardMu.Lock()
	st := s.board[j.cid]
	if st != nil {
		st.mu.Lock()
		same := j.origin != nil && (st == j.origin || st.followUpOf == j.origin) && st.trace == j.trace && st.taskID == j.taskID && st.boss == j.boss
		if !st.closed && !st.finishing && same {
			st.messages = append(st.messages, llm.Message{Role: "user", Content: text})
			st.gen++
			st.mu.Unlock()
			s.boardMu.Unlock()
			s.boardKick(st)
			return "injected"
		}
		// 这一轮正在交付最终答复、马上收尾：注入进去的消息不会再开新轮次
		// （boardKick 看到 running），收尾后 gen 检查也认为没有未处理进展，
		// 等于静默丢掉。提前按已收尾处理（置 closed + 摘板），改走下面的开补报轮。
		closing := st.finishing && same
		if closing {
			st.closed = true
		}
		st.mu.Unlock()
		if closing {
			if s.board[j.cid] == st {
				delete(s.board, j.cid)
			}
			st = nil
		}
	}
	if j.boss == "" {
		s.boardMu.Unlock()
		return "dropped_no_boss"
	}
	// 原任务仍是最近的任务且已收尾：沿用原有的补报轮行为。
	// boardMu 覆盖检查与发布，避免新消息在两步之间被 boardOpen 取代。
	if st == nil {
		st = &taskState{followUpOf: j.origin, cid: j.cid, boss: j.boss, trace: j.trace, taskID: j.taskID, taskName: j.taskName,
			pending: map[string]pendingJob{}, gen: 1,
			messages: []llm.Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: text + "\n\n" + followUpHint}}}
		s.board[j.cid] = st
		s.boardMu.Unlock()
		s.boardKick(st)
		return "new_task"
	}
	s.boardMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Call(ctx, j.boss, "deliver", protocol.DeliverPayload{Text: text, ConversationID: j.cid, TaskID: j.taskID}, j.trace, ""); err != nil {
		s.logger.Printf("会话 %s：原任务 %s 补报未确认送达: %v", j.cid, j.taskID, err)
		return "delivery_failed"
	}
	return "delivered_original_task"
}

// reportText 把一条补报整理成注入消息链的文本（与 [工具结果] 同构：role=user 的外部回报）。
func reportText(j *asyncJob, p protocol.AsyncResultPayload) string {
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
