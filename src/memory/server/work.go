package server

// work 账本的原生入口（spec §6.8 内部 method）：由入口/reasoner 内部调用，
// 不进 provides 名册、不暴露给模型。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// ensureContextWork 在 context 时登记 work（memory 是唯一 work 登记处，spec §1.6）。
// 生产链路没有独立的 human/agent 身份：owner 记部署默认人，agent 记调用方服务。
func (s *Server) ensureContextWork(ctx context.Context, meta callMeta, tc protocol.TaskContext, goal string) error {
	if meta.trace == "" {
		return errors.New("recall: 缺少 frame.work，无法登记 work")
	}
	entry := spv2.ServiceRef(meta.from)
	if tc.Boss != "" {
		entry = spv2.ServiceRef(tc.Boss)
	}
	_, err := s.cfg.Store.EnsureWork(ctx, spv2.Work{
		ID:     meta.trace,
		Root:   meta.trace,
		Class:  spv2.ClassRequest,
		Owner:  spv2.HumanRef("owner"),
		Agent:  spv2.AgentRef(meta.from),
		Entry:  entry,
		Goal:   goal,
		Status: spv2.WorkRunning,
	})
	if err != nil {
		return fmt.Errorf("登记 work %s: %w", meta.trace, err)
	}
	return nil
}

// ---------- 请求体（线路键名即契约） ----------

type workGetRequest struct {
	Work string `msgpack:"work,omitempty" json:"work,omitempty"`
}

type leaseChargeRequest struct {
	Work  string      `msgpack:"work,omitempty" json:"work,omitempty"`
	Delta spv2.Charge `msgpack:"delta" json:"delta"`
}

type leaseReleaseRequest struct {
	Work  string      `msgpack:"work,omitempty" json:"work,omitempty"`
	Delta spv2.Charge `msgpack:"delta" json:"delta"`
}

type workUpdateRequest struct {
	Work   string `msgpack:"work,omitempty" json:"work,omitempty"`
	Status string `msgpack:"status" json:"status"`
}

func workIDOf(call *spv2.Call, reqID string) string {
	if reqID != "" {
		return reqID
	}
	return call.WorkID()
}

// authorizeWork 是 §6.5 矩阵在生产的最小实现：owner/runtime 可控制任意 work；
// 其余调用方必须就是这条 work 的 agent/owner/entry（生产里 actor 是服务，按 ID 比对）。
func authorizeWork(w spv2.Work, from spv2.Ref) error {
	switch from.Authority {
	case spv2.AuthorityOwner, spv2.AuthorityRuntime:
		return nil
	}
	if from.ID != "" && (from.ID == w.Agent.ID || from.ID == w.Owner.ID || from.ID == w.Entry.ID) {
		return nil
	}
	return spv2.ErrCode(spv2.CodePermissionDenied, "无权控制 work "+w.ID, false, nil)
}

func replyWorkError(call *spv2.Call, err error) error {
	var e *spv2.Error
	if errors.As(err, &e) {
		return call.ReplyError(e.Code, e.Message, e.Retryable, e.Details)
	}
	return call.ReplyError(spv2.CodeInternal, err.Error(), true, nil)
}

func (s *Server) nativeWorkGet(_ context.Context, call *spv2.Call) error {
	var req workGetRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "work_get body 无效: "+err.Error(), false, nil)
	}
	id := workIDOf(call, req.Work)
	if id == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "work_get 需要 work id", false, nil)
	}
	w, err := s.cfg.Store.GetWork(context.Background(), id)
	if err != nil {
		return replyWorkError(call, err)
	}
	if err := authorizeWork(w, call.Frame.From); err != nil {
		return replyWorkError(call, err)
	}
	rem, err := s.cfg.Store.Remaining(context.Background(), id)
	if err != nil {
		return replyWorkError(call, err)
	}
	return call.Reply(map[string]any{"work": w, "remaining": rem})
}

// nativeWorkUpdate 按 §6.3 状态机推进 work 状态（生产里由 reasoner 在轮次边界调用）。
func (s *Server) nativeWorkUpdate(ctx context.Context, call *spv2.Call) error {
	var req workUpdateRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "work_update body 无效: "+err.Error(), false, nil)
	}
	id := workIDOf(call, req.Work)
	if id == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "work_update 需要 work id", false, nil)
	}
	w, err := s.cfg.Store.GetWork(ctx, id)
	if err != nil {
		return replyWorkError(call, err)
	}
	if err := authorizeWork(w, call.Frame.From); err != nil {
		return replyWorkError(call, err)
	}
	if err := s.cfg.Store.UpdateStatus(ctx, id, req.Status); err != nil {
		return replyWorkError(call, err)
	}
	s.recordWorkTransition(req.Status)
	return call.Reply(map[string]any{"status": req.Status})
}

func (s *Server) nativeLeaseCharge(ctx context.Context, call *spv2.Call) error {
	var req leaseChargeRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "lease_charge body 无效: "+err.Error(), false, nil)
	}
	id := workIDOf(call, req.Work)
	if id == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "lease_charge 需要 work id", false, nil)
	}
	w, err := s.cfg.Store.GetWork(ctx, id)
	if err != nil {
		return replyWorkError(call, err)
	}
	if err := authorizeWork(w, call.Frame.From); err != nil {
		return replyWorkError(call, err)
	}
	rem, err := s.cfg.Store.Charge(ctx, id, req.Delta)
	if err != nil {
		return replyWorkError(call, err)
	}
	return call.Reply(map[string]any{"remaining": rem})
}

func (s *Server) nativeLeaseRelease(ctx context.Context, call *spv2.Call) error {
	var req leaseReleaseRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "lease_release body 无效: "+err.Error(), false, nil)
	}
	id := workIDOf(call, req.Work)
	if id == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "lease_release 需要 work id", false, nil)
	}
	w, err := s.cfg.Store.GetWork(ctx, id)
	if err != nil {
		return replyWorkError(call, err)
	}
	if err := authorizeWork(w, call.Frame.From); err != nil {
		return replyWorkError(call, err)
	}
	if err := s.cfg.Store.Release(ctx, id, req.Delta); err != nil {
		return replyWorkError(call, err)
	}
	return call.Reply(map[string]any{"released": true})
}

// sweepWorks 是 spec §6.9 的维护入口：把过期未终态的 work 扫成 stalled，并记指标。
// 失败只记日志（维护动作不阻塞服务），但不会静默改状态：状态只由 SQL 里的条件决定。
func (s *Server) sweepWorks(ctx context.Context) int {
	n, err := s.cfg.Store.SweepExpired(ctx, time.Now())
	if err != nil {
		s.logger.Printf("work sweep 失败: %v", err)
		return 0
	}
	if n > 0 {
		if m := s.Metrics(); m != nil {
			m.Counter("sump_work_stalled_total", "work 停滞数", map[string]string{"reason": "deadline"}, float64(n))
		}
		s.logger.Printf("work sweep: %d 条过期 work 置 stalled", n)
	}
	return n
}

// nativeWorkSweep 暴露一次手动 sweep（运维/测试用；周期 sweep 由 Start 的后台循环负责）。
func (s *Server) nativeWorkSweep(ctx context.Context, call *spv2.Call) error {
	return call.Reply(map[string]any{"stalled": s.sweepWorks(ctx)})
}
