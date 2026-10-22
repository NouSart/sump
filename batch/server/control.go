package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// actionControl 暂停 / 恢复 / 停止。
//
// 语义（见 batch/DESIGN.md §3.5）：
//   - pause  可恢复：未开始的项保持 pending，在飞的项跑完；
//   - resume 从断点继续，已完成的项不重跑；
//   - cancel 终态：未开始的项转 skipped，在飞的项跑完。
//
// 对已结束的任务操作返回 affected=0，不报错（幂等）；只有任务不存在或已过期才报
// BATCH_JOB_UNKNOWN。
func (s *Server) actionControl(ctx context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.BatchControlPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	op := strings.ToLower(strings.TrimSpace(p.Op))
	switch op {
	case protocol.BatchOpPause, protocol.BatchOpResume, protocol.BatchOpCancel:
	default:
		return nil, fmt.Errorf("BATCH_OP: op 只能是 pause / resume / cancel，收到 %q", p.Op)
	}
	scope := strings.ToLower(strings.TrimSpace(p.Scope))
	switch scope {
	case "", protocol.BatchScopeOne, protocol.BatchScopeAll:
	default:
		return nil, fmt.Errorf("BATCH_OP: scope 只能是 one 或 all，收到 %q", p.Scope)
	}
	if scope == "" {
		scope = protocol.BatchScopeOne
	}

	if scope == protocol.BatchScopeAll {
		return s.controlAll(op)
	}

	id := strings.TrimSpace(p.ItemID)
	if id == "" {
		return nil, fmt.Errorf("BATCH_OP: scope=one 时 item_id 必填")
	}
	if _, ok, err := s.store.Job(id); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("BATCH_JOB_UNKNOWN: 没有这个任务（或已过期）: %s", id)
	}
	affected := 0
	if s.controlOne(op, id) {
		affected = 1
	}
	// 恢复拿不到槽位时要明说，不能只回 affected=0 让人以为"本来就在跑"。
	if affected == 0 && op == protocol.BatchOpResume {
		if j, ok, err := s.store.Job(id); err == nil && ok && j.Phase == protocol.BatchPhasePaused {
			return nil, fmt.Errorf("BATCH_BUSY: 全局在飞槽位不足，任务 %s 仍是暂停，稍后再 resume", id)
		}
	}
	return s.controlResult(op, scope, affected), nil
}

// controlAll 对所有非终态任务批量执行。
func (s *Server) controlAll(op string) (any, error) {
	jobs, err := s.store.Running()
	if err != nil {
		return nil, err
	}
	affected := 0
	for _, j := range jobs {
		if s.controlOne(op, j.ItemID) {
			affected++
		}
	}
	return s.controlResult(op, protocol.BatchScopeAll, affected), nil
}

// controlOne 对单个任务执行，返回是否改变了状态。
func (s *Server) controlOne(op, id string) bool {
	s.mu.Lock()
	j := s.jobs[id]
	s.mu.Unlock()

	if j != nil {
		return s.controlMemory(op, j)
	}
	// 不在内存里：可能是重启后转暂停的任务（resume 时重建）。
	stored, ok, err := s.store.Job(id)
	if err != nil || !ok {
		return false
	}
	switch stored.Phase {
	case protocol.BatchPhasePaused:
		switch op {
		case protocol.BatchOpResume:
			rj, err := s.rebuildJob(stored)
			if err != nil {
				s.logger.Printf("恢复任务 %s 失败: %v", id, err)
				return false
			}
			if !s.reserve(rj.reserved) {
				s.logger.Printf("恢复任务 %s 失败：全局槽位不足", id)
				return false
			}
			s.mu.Lock()
			s.jobs[id] = rj
			s.mu.Unlock()
			go s.runJob(rj)
			return true
		case protocol.BatchOpCancel:
			if _, err := s.store.SkipPending(id); err != nil {
				s.logger.Printf("取消任务 %s 时跳过未开始项失败: %v", id, err)
			}
			counts, _ := s.store.CountByStatus(id)
			_ = s.store.Finish(id, protocol.BatchPhaseCancelled,
				counts[protocol.BatchItemOK], counts[protocol.BatchItemFailed],
				counts[protocol.BatchItemSkipped], "已取消（未开始的项作废）", time.Now())
			return true
		}
	case protocol.BatchPhaseAccepted, protocol.BatchPhaseRunning:
		// 不在内存却仍是 running/accepted：只可能是并发窗口里的瞬间状态，忽略。
	}
	return false
}

// controlMemory 对内存里的活动任务执行。
func (s *Server) controlMemory(op string, j *runningJob) bool {
	j.mu.Lock()
	if j.finished {
		j.mu.Unlock()
		return false
	}
	switch op {
	case protocol.BatchOpPause:
		if j.paused {
			j.mu.Unlock()
			return false
		}
		j.paused = true
		j.mu.Unlock()
		_ = s.store.SetPhase(j.id, protocol.BatchPhasePaused)
		// 槽位不在这里放：在飞的项还占着真实并发。等调度循环把在飞的收完、确认暂停后
		// 再一次性归还（见 runScheduler 的暂停出口）。
		s.logger.Printf("任务 %s 已暂停（在飞的项会跑完）", j.id)
		return true
	case protocol.BatchOpCancel:
		if j.cancelled {
			j.mu.Unlock()
			return false
		}
		j.cancelled = true
		// 暂停中的任务没有调度循环在跑，没人会替它收尾——这里必须自己落终态。
		idle := !j.schedRunning
		j.mu.Unlock()
		if idle {
			s.finishJob(j)
		}
		s.logger.Printf("任务 %s 已取消（未开始的项作废，在飞的跑完）", j.id)
		return true
	case protocol.BatchOpResume:
		if !j.paused {
			j.mu.Unlock()
			return false
		}
		need := j.concurrency
		if n := len(j.items) + j.inflight; need > n {
			need = n
		}
		if need < 1 {
			need = 1
		}
		// 锁序：j.mu → s.mu（别处没有反过来的嵌套）。
		if !s.reserve(need) {
			j.mu.Unlock()
			s.logger.Printf("恢复任务 %s 失败：全局槽位不足（需要 %d）", j.id, need)
			return false
		}
		j.reserved = need
		j.paused = false
		j.mu.Unlock()
		_ = s.store.SetPhase(j.id, protocol.BatchPhaseRunning)
		go s.runScheduler(j)
		s.logger.Printf("任务 %s 已恢复", j.id)
		return true
	}
	j.mu.Unlock()
	return false
}

// controlResult 汇总当前 paused / running 计数。
func (s *Server) controlResult(op, scope string, affected int) protocol.BatchControlResult {
	counts, err := s.store.Counts()
	if err != nil {
		s.logger.Printf("统计任务状态失败: %v", err)
	}
	return protocol.BatchControlResult{Op: op, Scope: scope, Affected: affected,
		Paused: counts.Paused, Running: counts.Running}
}
