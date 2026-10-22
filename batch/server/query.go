package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// actionStatus 查单个任务的进度（内存优先：内存里有在飞的实时数）。
func (s *Server) actionStatus(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.BatchStatusPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(p.ItemID)
	if id == "" {
		return nil, fmt.Errorf("BATCH_JOB_UNKNOWN: item_id 必填")
	}
	j, ok, err := s.store.Job(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("BATCH_JOB_UNKNOWN: 没有这个任务（或已过期）: %s", id)
	}

	s.mu.Lock()
	live := s.jobs[id]
	s.mu.Unlock()

	res := protocol.BatchStatusResult{
		ItemID: j.ItemID, Service: j.Service, Action: j.Action, Kind: j.Kind, Phase: j.Phase,
		Total: j.Total, Ok: j.Ok, Failed: j.Failed, Skipped: j.Skipped,
		KeyTotal: j.KeyTotal, KeyDone: j.KeyDone, Woken: j.Woken,
	}
	if live != nil {
		live.mu.Lock()
		res.Phase = live.phaseLocked()
		res.Ok, res.Failed, res.Skipped = live.ok, live.failed, live.skipped
		res.KeyDone, res.Woken = live.keyDone, live.woken
		res.Pending = len(live.items) + live.inflight
		live.mu.Unlock()
	} else {
		counts, err := s.store.CountByStatus(id)
		if err == nil {
			res.Pending = counts[protocol.BatchItemPending] + counts[protocol.BatchItemRunning]
		}
	}
	start := j.StartedAt
	if start.IsZero() {
		start = j.CreatedAt
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	if !j.FinishedAt.IsZero() {
		res.ElapsedMS = j.FinishedAt.Sub(start).Milliseconds()
	}
	return res, nil
}

// phaseLocked 从内存任务推断当前 phase（调用方必须已持有 j.mu）。
//
// 取消不在这里报：cancel 是终态，但在飞的项还要跑完——在那之前它仍在干活，
// 报 cancelled 会让"全局在跑"的统计少算一个正在收尾的任务。真正落终态在 finishJob。
func (j *runningJob) phaseLocked() string {
	if j.paused {
		return protocol.BatchPhasePaused
	}
	return protocol.BatchPhaseRunning
}

// actionResult 取明细（分页）。
func (s *Server) actionResult(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.BatchResultPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(p.ItemID)
	if id == "" {
		return nil, fmt.Errorf("BATCH_JOB_UNKNOWN: item_id 必填")
	}
	if _, ok, err := s.store.Job(id); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("BATCH_JOB_UNKNOWN: 没有这个任务（或已过期）: %s", id)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset := p.Offset
	if offset < 0 {
		offset = 0
	}
	status := strings.ToLower(strings.TrimSpace(p.Status))
	items, matched, err := s.store.Items(id, status, offset, limit)
	if err != nil {
		return nil, err
	}
	out := protocol.BatchResultResult{ItemID: id, Offset: offset, Limit: limit, Matched: matched}
	for _, it := range items {
		out.Items = append(out.Items, protocol.BatchItemResult{
			Seq: it.Seq, Tag: it.Tag, Key: it.Key, Status: it.Status,
			Error: it.Error, Preview: string(it.Preview), Size: it.Size, MS: it.MS,
		})
	}
	return out, nil
}

// actionList 查任务队列（模型与前端共用）。
func (s *Server) actionList(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.BatchListPayload
	if len(in.Input) > 0 {
		if err := protocol.DecodeRaw(in.Input, &p); err != nil {
			return nil, err
		}
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	phase := strings.ToLower(strings.TrimSpace(p.Phase))
	jobs, err := s.store.List(phase, strings.TrimSpace(p.Caller), limit)
	if err != nil {
		return nil, err
	}
	out := protocol.BatchListResult{Jobs: make([]protocol.BatchJobView, 0, len(jobs))}
	for _, j := range jobs {
		v := protocol.BatchJobView{
			ItemID: j.ItemID, Service: j.Service, Action: j.Action, Caller: j.Caller,
			Kind: j.Kind, Phase: j.Phase, Total: j.Total, Ok: j.Ok, Failed: j.Failed,
			Skipped: j.Skipped, KeyTotal: j.KeyTotal, KeyDone: j.KeyDone, Summary: j.Summary,
		}
		if live := s.liveJob(j.ItemID); live != nil {
			live.mu.Lock()
			v.Phase = live.phaseLocked()
			v.Ok, v.Failed, v.Skipped = live.ok, live.failed, live.skipped
			v.Running = live.inflight
			v.Pending = len(live.items)
			v.KeyDone = live.keyDone
			live.mu.Unlock()
		} else {
			if counts, err := s.store.CountByStatus(j.ItemID); err == nil {
				v.Running = counts[protocol.BatchItemRunning]
				v.Pending = counts[protocol.BatchItemPending]
			}
		}
		start := j.StartedAt
		if start.IsZero() {
			start = j.CreatedAt
		}
		end := time.Now()
		if !j.FinishedAt.IsZero() {
			end = j.FinishedAt
		}
		v.ElapsedMS = end.Sub(start).Milliseconds()
		out.Jobs = append(out.Jobs, v)
	}
	if out.Counts, err = s.store.Counts(); err != nil {
		return nil, err
	}
	return out, nil
}

// liveJob 取内存里的活动任务（没有返回 nil）。
func (s *Server) liveJob(id string) *runningJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}
