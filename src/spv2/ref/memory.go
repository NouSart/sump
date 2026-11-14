package ref

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/NouSart/sump/spv2"
)

// Memory 是 service:memory 的参考实现，work/lease/队列都落在 spv2.MemStore。
type Memory struct {
	store *spv2.MemStore
	srv   *spv2.Server

	// workStatus 是 work id → 最近一次观察到的状态；sump_work_active{status} 由它聚合。
	// spec §12.2 禁止 work/actor 这类高基数 label，所以 gauge 只按 status 聚合。
	workMu     sync.Mutex
	workStatus map[string]string
	statusSeen map[string]bool
}

// NewMemory 创建 memory 服务。
func NewMemory(store *spv2.MemStore) (*Memory, error) {
	if store == nil {
		return nil, errors.New("ref: NewMemory 需要非 nil 的 spv2.MemStore")
	}
	srv, err := spv2.NewServer(MemoryCard())
	if err != nil {
		return nil, err
	}
	m := &Memory{store: store, srv: srv, workStatus: map[string]string{}, statusSeen: map[string]bool{}}
	srv.Handle("context", m.handleContext)
	srv.Handle("work_get", m.handleWorkGet)
	srv.Handle("cancel", m.handleCancel)
	srv.Handle("renew", m.handleRenew)
	srv.Handle("lease_charge", m.handleLeaseCharge)
	srv.Handle("message_post", m.handleMessagePost)
	srv.Handle("message_claim", m.handleMessageClaim)
	srv.Handle("message_ack", m.handleMessageAck)
	return m, nil
}

// Server 返回底层 SPv2.1 服务端。
func (m *Memory) Server() *spv2.Server { return m.srv }

// Store 返回 work 账本。
func (m *Memory) Store() *spv2.MemStore { return m.store }

func (m *Memory) metrics() *spv2.Registry { return m.srv.Metrics() }

// trackWork 记录 work 的最近状态，并按状态重算 sump_work_active{status}。
func (m *Memory) trackWork(w spv2.Work) {
	m.workMu.Lock()
	m.workStatus[w.ID] = w.Status
	counts := map[string]int{}
	for _, status := range m.workStatus {
		counts[status]++
		m.statusSeen[status] = true
	}
	m.workMu.Unlock()
	// 状态计数归零也要把 series 显式写 0，否则旧值会永远挂在 gauge 上。
	for status := range m.statusSeen {
		m.metrics().Gauge("sump_work_active", "Active works by status", map[string]string{"status": status}, float64(counts[status]))
	}
}

// trackA2A 记一条 A2A 队列消息的状态（pending/claimed/delivered）。
func (m *Memory) trackA2A(state string) {
	m.metrics().Counter("sump_a2a_messages_total", "A2A messages by state", map[string]string{"state": state}, 1)
}

func (m *Memory) handleContext(ctx context.Context, call *spv2.Call) error {
	workID := call.WorkID()
	if workID == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.context 需要 frame.work", false, nil)
	}
	var req ContextRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.context body 无效: "+err.Error(), false, nil)
	}
	work, err := m.store.EnsureWork(ctx, spv2.Work{
		ID:    workID,
		Owner: refOr(req.Owner, spv2.HumanRef(DefaultOwnerID)),
		Agent: refOr(req.Agent, spv2.AgentRef(ServiceReasoner)),
		Entry: refOr(req.Entry, spv2.ServiceRef(ServiceEntry)),
	})
	if err != nil {
		return storeError(err, "memory.context")
	}
	m.trackWork(work)
	return call.Reply(ContextResponse{Messages: req.Messages, Work: work})
}

func (m *Memory) handleWorkGet(ctx context.Context, call *spv2.Call) error {
	var req WorkGetRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.work_get body 无效: "+err.Error(), false, nil)
	}
	workID := req.Work
	if workID == "" {
		workID = call.WorkID()
	}
	if workID == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.work_get 需要 work id", false, nil)
	}
	work, err := m.store.GetWork(ctx, workID)
	if err != nil {
		return storeError(err, "memory.work_get")
	}
	return call.Reply(WorkGetResponse{Work: work})
}

func (m *Memory) handleCancel(ctx context.Context, call *spv2.Call) error {
	var req CancelRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.cancel body 无效: "+err.Error(), false, nil)
	}
	workID := req.Work
	if workID == "" {
		workID = call.WorkID()
	}
	if workID == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.cancel 需要 work id", false, nil)
	}
	work, err := m.store.GetWork(ctx, workID)
	if err != nil {
		return storeError(err, "memory.cancel")
	}
	if err := spv2.AuthorizeControl(work, call.Frame.From); err != nil {
		return err
	}
	if err := m.store.UpdateStatus(ctx, workID, spv2.WorkCancelled); err != nil {
		return storeError(err, "memory.cancel")
	}
	work, err = m.store.GetWork(ctx, workID)
	if err != nil {
		return storeError(err, "memory.cancel")
	}
	m.trackWork(work)
	return call.Reply(CancelResponse{Work: work, Cancelled: true})
}

func (m *Memory) handleRenew(ctx context.Context, call *spv2.Call) error {
	var req RenewRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.renew body 无效: "+err.Error(), false, nil)
	}
	workID := req.Work
	if workID == "" {
		workID = call.WorkID()
	}
	if workID == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.renew 需要 work id", false, nil)
	}
	work, err := m.store.GetWork(ctx, workID)
	if err != nil {
		return storeError(err, "memory.renew")
	}
	if err := spv2.AuthorizeControl(work, call.Frame.From); err != nil {
		return err
	}
	work, err = m.store.Renew(ctx, workID, req.Lease)
	if err != nil {
		return storeError(err, "memory.renew")
	}
	m.trackWork(work)
	return call.Reply(RenewResponse{Work: work})
}

func (m *Memory) handleLeaseCharge(ctx context.Context, call *spv2.Call) error {
	var req LeaseChargeRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.lease_charge body 无效: "+err.Error(), false, nil)
	}
	workID := req.Work
	if workID == "" {
		workID = call.WorkID()
	}
	if workID == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.lease_charge 需要 work id", false, nil)
	}
	remaining, err := m.store.Charge(ctx, workID, req.Delta)
	if err != nil {
		if errors.Is(err, spv2.ErrLeaseExhausted) {
			m.metrics().Counter("sump_lease_exhausted_total", "Lease exhausted", nil, 1)
		}
		return storeError(err, "memory.lease_charge")
	}
	return call.Reply(LeaseChargeResponse{Remaining: remaining})
}

func (m *Memory) handleMessagePost(ctx context.Context, call *spv2.Call) error {
	var req MessagePostRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.message_post body 无效: "+err.Error(), false, nil)
	}
	if req.Work == "" {
		req.Work = call.WorkID()
	}
	if req.Work == "" || req.Method == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.message_post 需要 work/method", false, nil)
	}
	id, err := m.store.PostMessage(ctx, spv2.QueuedMessage{
		WorkID:  req.Work,
		From:    req.From,
		To:      req.To,
		Method:  req.Method,
		ReplyTo: req.ReplyTo,
		Body:    req.Body,
	})
	if err != nil {
		return storeError(err, "memory.message_post")
	}
	m.trackA2A(spv2.MessagePending)
	return call.Reply(MessagePostResponse{ID: id})
}

func (m *Memory) handleMessageClaim(ctx context.Context, call *spv2.Call) error {
	var req MessageClaimRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.message_claim body 无效: "+err.Error(), false, nil)
	}
	if req.Work == "" {
		req.Work = call.WorkID()
	}
	if req.Work == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.message_claim 需要 work id", false, nil)
	}
	lease := 30 * time.Second
	if req.LeaseMS > 0 {
		lease = time.Duration(req.LeaseMS) * time.Millisecond
	}
	msg, err := m.store.ClaimMessage(ctx, req.Work, lease)
	if err != nil {
		return storeError(err, "memory.message_claim")
	}
	if msg != nil {
		m.trackA2A(spv2.MessageClaimed)
	}
	return call.Reply(MessageClaimResponse{Message: msg})
}

func (m *Memory) handleMessageAck(ctx context.Context, call *spv2.Call) error {
	var req MessageAckRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.message_ack body 无效: "+err.Error(), false, nil)
	}
	if req.ID == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "memory.message_ack 需要 id", false, nil)
	}
	if err := m.store.AckMessage(ctx, req.ID); err != nil {
		return storeError(err, "memory.message_ack")
	}
	m.trackA2A(spv2.MessageDelivered)
	return call.Reply(MessageAckResponse{Acked: true})
}

// refOr 在请求体没给出完整 Ref 时返回默认值。
func refOr(r, def spv2.Ref) spv2.Ref {
	if r.Kind == "" || r.ID == "" {
		return def
	}
	return r
}

// storeError 把 store 的错误翻译成 SPv2.1 错误码。
func storeError(err error, op string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, spv2.ErrWorkNotFound):
		return spv2.ErrCode(spv2.CodeNotFound, op+": work 不存在", false, nil)
	case errors.Is(err, spv2.ErrLeaseExhausted):
		return spv2.ErrCode(spv2.CodeResourceExhausted, op+": lease 预算不足", true, nil)
	}
	var e *spv2.Error
	if errors.As(err, &e) {
		return e
	}
	return spv2.ErrCode(spv2.CodeInternal, op+": "+err.Error(), true, nil)
}
