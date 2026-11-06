package spv2

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// 队列消息状态。
const (
	MessagePending   = "pending"
	MessageClaimed   = "claimed"
	MessageDelivered = "delivered"
	MessageFailed    = "failed"
	MessageExpired   = "expired"
)

// ErrWorkNotFound 表示 work 不存在。
var ErrWorkNotFound = errors.New("work not found")

// ErrLeaseExhausted 表示 lease 预算不足。
var ErrLeaseExhausted = errors.New("lease exhausted")

// QueuedMessage 是一条 A2A 投递。
type QueuedMessage struct {
	ID         string    `msgpack:"id" json:"id"`
	WorkID     string    `msgpack:"work_id" json:"work_id"`
	From       Ref       `msgpack:"from" json:"from"`
	To         Ref       `msgpack:"to" json:"to"`
	Method     string    `msgpack:"method" json:"method"`
	ReplyTo    string    `msgpack:"reply_to,omitempty" json:"reply_to,omitempty"`
	Body       []byte    `msgpack:"body,omitempty" json:"body,omitempty"`
	State      string    `msgpack:"state" json:"state"`
	Attempts   int       `msgpack:"attempts" json:"attempts"`
	LastError  string    `msgpack:"last_error,omitempty" json:"last_error,omitempty"`
	CreatedAt  time.Time `msgpack:"created_at" json:"created_at"`
	LeaseUntil time.Time `msgpack:"lease_until,omitempty" json:"lease_until,omitempty"`
}

// SweepResult 是一次清理的计数。
type SweepResult struct {
	Stalled int `msgpack:"stalled" json:"stalled"`
	Expired int `msgpack:"expired" json:"expired"`
	Retried int `msgpack:"retried" json:"retried"`
}

// WorkStore 是 memory 的 work/queue/lease 账本接口。
type WorkStore interface {
	EnsureWork(ctx context.Context, w Work) (Work, error)
	GetWork(ctx context.Context, id string) (Work, error)
	UpdateStatus(ctx context.Context, id, status string) error
	AddParticipant(ctx context.Context, workID string, p Participant) error
	Renew(ctx context.Context, workID string, lease Lease) (Work, error)
	Charge(ctx context.Context, workID string, c Charge) (Remaining, error)
	Release(ctx context.Context, workID string, c Charge) error
	Remaining(ctx context.Context, workID string) (Remaining, error)

	PostMessage(ctx context.Context, m QueuedMessage) (string, error)
	ClaimMessage(ctx context.Context, workID string, lease time.Duration) (*QueuedMessage, error)
	AckMessage(ctx context.Context, id string) error
	NackMessage(ctx context.Context, id, errMsg string) error
	QueueDepth(ctx context.Context, workID string) (int, error)

	Sweep(ctx context.Context, now time.Time) (SweepResult, error)
}

type workRecord struct {
	work      Work
	remaining Remaining
	messages  map[string]*QueuedMessage
}

// MemStore 是 WorkStore 的内存实现（参考/测试用）。
type MemStore struct {
	mu       sync.Mutex
	works    map[string]*workRecord
	maxTries int
	msgTTL   time.Duration
}

// NewMemStore 创建内存 work store。
func NewMemStore() *MemStore {
	return &MemStore{works: map[string]*workRecord{}, maxTries: 3, msgTTL: 24 * time.Hour}
}

func copyRef(r Ref) Ref { return r }

func copyWork(w Work) Work {
	out := w
	out.Owner = copyRef(w.Owner)
	out.Agent = copyRef(w.Agent)
	out.Entry = copyRef(w.Entry)
	out.Participants = append([]Participant(nil), w.Participants...)
	if w.Policy != nil {
		out.Policy = make(map[string]any, len(w.Policy))
		for k, v := range w.Policy {
			out.Policy[k] = v
		}
	}
	return out
}

func copyMessage(m QueuedMessage) QueuedMessage {
	out := m
	out.From = copyRef(m.From)
	out.To = copyRef(m.To)
	out.Body = append([]byte(nil), m.Body...)
	return out
}

func ensureDefaults(w *Work, now time.Time) {
	if w.Root == "" {
		w.Root = w.ID
	}
	if w.Parent == "" {
		w.Parent = ""
	}
	if w.Status == "" {
		w.Status = WorkOpen
	}
	if w.CreatedAt == "" {
		w.CreatedAt = now.UTC().Format(time.RFC3339)
	}
	w.UpdatedAt = now.UTC().Format(time.RFC3339)
	fillLeaseDefaults(&w.Lease, now)
	if w.Class == "" {
		w.Class = ClassRequest
	}
}

func fillLeaseDefaults(l *Lease, now time.Time) {
	def := DefaultLease(now)
	if l.DeadlineAt == "" {
		l.DeadlineAt = def.DeadlineAt
	}
	if l.MaxToolCalls <= 0 {
		l.MaxToolCalls = def.MaxToolCalls
	}
	if l.MaxTokens <= 0 {
		l.MaxTokens = def.MaxTokens
	}
	if l.MaxApprovals <= 0 {
		l.MaxApprovals = def.MaxApprovals
	}
	if l.MaxChildren <= 0 {
		l.MaxChildren = def.MaxChildren
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = def.MaxDepth
	}
}

func remainingFromLease(l Lease) Remaining {
	return Remaining{
		DeadlineAt: l.DeadlineAt,
		ToolCalls:  l.MaxToolCalls,
		Tokens:     l.MaxTokens,
		Approvals:  l.MaxApprovals,
		Children:   l.MaxChildren,
		Depth:      l.MaxDepth,
	}
}

// EnsureWork 幂等创建 work；已存在则原样返回（不覆盖）。
func (s *MemStore) EnsureWork(_ context.Context, w Work) (Work, error) {
	now := time.Now()
	ensureDefaults(&w, now)
	if err := w.Validate(); err != nil {
		return Work{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.works[w.ID]; ok {
		return copyWork(rec.work), nil
	}
	s.works[w.ID] = &workRecord{work: copyWork(w), remaining: remainingFromLease(w.Lease), messages: map[string]*QueuedMessage{}}
	return copyWork(w), nil
}

// GetWork 返回 work。
func (s *MemStore) GetWork(_ context.Context, id string) (Work, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.works[id]
	if !ok {
		return Work{}, ErrWorkNotFound
	}
	return copyWork(rec.work), nil
}

// UpdateStatus 按状态机更新 work 状态。
func (s *MemStore) UpdateStatus(_ context.Context, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.works[id]
	if !ok {
		return ErrWorkNotFound
	}
	if rec.work.Status == status {
		return nil
	}
	if !CanTransition(rec.work.Status, status) {
		return ErrCode(CodeConflict, fmt.Sprintf("work %s 不能从 %s 转到 %s", id, rec.work.Status, status), false, nil)
	}
	if status == WorkDone {
		for _, m := range rec.messages {
			if m.State == MessagePending || m.State == MessageClaimed {
				return ErrCode(CodeConflict, fmt.Sprintf("work %s 还有未完成的 A2A 消息", id), false, nil)
			}
		}
		for _, other := range s.works {
			if other.work.Parent == id && !IsTerminal(other.work.Status) {
				return ErrCode(CodeConflict, fmt.Sprintf("work %s 还有未完成的 child work", id), false, nil)
			}
		}
	}
	rec.work.Status = status
	rec.work.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if IsTerminal(status) {
		rec.work.ClosedAt = rec.work.UpdatedAt
	}
	return nil
}

// AddParticipant 添加/更新参与者。
func (s *MemStore) AddParticipant(_ context.Context, workID string, p Participant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.works[workID]
	if !ok {
		return ErrWorkNotFound
	}
	if p.Role == "" {
		p.Role = "member"
	}
	if p.Status == "" {
		p.Status = "active"
	}
	for i := range rec.work.Participants {
		if rec.work.Participants[i].Actor.Kind == p.Actor.Kind && rec.work.Participants[i].Actor.ID == p.Actor.ID {
			rec.work.Participants[i] = p
			rec.work.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			return nil
		}
	}
	rec.work.Participants = append(rec.work.Participants, p)
	rec.work.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return nil
}

// Renew 续租：只增加预算/延后 deadline，不减少已用余额。
func (s *MemStore) Renew(_ context.Context, workID string, lease Lease) (Work, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.works[workID]
	if !ok {
		return Work{}, ErrWorkNotFound
	}
	if IsTerminal(rec.work.Status) {
		return Work{}, ErrCode(CodeConflict, "终态 work 不能续租", false, nil)
	}
	if lease.DeadlineAt != "" {
		old, _ := time.Parse(time.RFC3339, rec.work.Lease.DeadlineAt)
		nw, err := time.Parse(time.RFC3339, lease.DeadlineAt)
		if err != nil {
			return Work{}, ErrCode(CodeInvalidArgument, "lease.deadline_at 不是 RFC3339", false, nil)
		}
		if nw.After(old) {
			rec.work.Lease.DeadlineAt = lease.DeadlineAt
			rec.remaining.DeadlineAt = lease.DeadlineAt
		}
	}
	add := func(oldMax, newMax, remaining int) (int, int) {
		if newMax > oldMax {
			remaining += newMax - oldMax
			oldMax = newMax
		}
		return oldMax, remaining
	}
	rec.work.Lease.MaxToolCalls, rec.remaining.ToolCalls = add(rec.work.Lease.MaxToolCalls, lease.MaxToolCalls, rec.remaining.ToolCalls)
	rec.work.Lease.MaxTokens, rec.remaining.Tokens = add(rec.work.Lease.MaxTokens, lease.MaxTokens, rec.remaining.Tokens)
	rec.work.Lease.MaxApprovals, rec.remaining.Approvals = add(rec.work.Lease.MaxApprovals, lease.MaxApprovals, rec.remaining.Approvals)
	rec.work.Lease.MaxChildren, rec.remaining.Children = add(rec.work.Lease.MaxChildren, lease.MaxChildren, rec.remaining.Children)
	rec.work.Lease.MaxDepth, rec.remaining.Depth = add(rec.work.Lease.MaxDepth, lease.MaxDepth, rec.remaining.Depth)
	rec.work.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return copyWork(rec.work), nil
}

// Charge 原子扣减租约。
func (s *MemStore) Charge(_ context.Context, workID string, c Charge) (Remaining, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.works[workID]
	if !ok {
		return Remaining{}, ErrWorkNotFound
	}
	if rec.remaining.DeadlineAt != "" {
		if t, err := time.Parse(time.RFC3339, rec.remaining.DeadlineAt); err == nil && time.Now().After(t) {
			return rec.remaining, ErrCode(CodeDeadlineExceeded, "work deadline exceeded", false, nil)
		}
	}
	if c.ToolCalls > rec.remaining.ToolCalls || c.Tokens > rec.remaining.Tokens ||
		c.Approvals > rec.remaining.Approvals || c.Children > rec.remaining.Children || c.Depth > rec.remaining.Depth {
		return rec.remaining, ErrLeaseExhausted
	}
	rec.remaining.ToolCalls -= c.ToolCalls
	rec.remaining.Tokens -= c.Tokens
	rec.remaining.Approvals -= c.Approvals
	rec.remaining.Children -= c.Children
	rec.remaining.Depth -= c.Depth
	return rec.remaining, nil
}

// Release 归还租约（不超过初始上限）。
func (s *MemStore) Release(_ context.Context, workID string, c Charge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.works[workID]
	if !ok {
		return ErrWorkNotFound
	}
	rec.remaining.ToolCalls += c.ToolCalls
	if rec.remaining.ToolCalls > rec.work.Lease.MaxToolCalls {
		rec.remaining.ToolCalls = rec.work.Lease.MaxToolCalls
	}
	rec.remaining.Tokens += c.Tokens
	if rec.remaining.Tokens > rec.work.Lease.MaxTokens {
		rec.remaining.Tokens = rec.work.Lease.MaxTokens
	}
	rec.remaining.Approvals += c.Approvals
	if rec.remaining.Approvals > rec.work.Lease.MaxApprovals {
		rec.remaining.Approvals = rec.work.Lease.MaxApprovals
	}
	rec.remaining.Children += c.Children
	if rec.remaining.Children > rec.work.Lease.MaxChildren {
		rec.remaining.Children = rec.work.Lease.MaxChildren
	}
	rec.remaining.Depth += c.Depth
	if rec.remaining.Depth > rec.work.Lease.MaxDepth {
		rec.remaining.Depth = rec.work.Lease.MaxDepth
	}
	return nil
}

// Remaining 返回租约余额。
func (s *MemStore) Remaining(_ context.Context, workID string) (Remaining, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.works[workID]
	if !ok {
		return Remaining{}, ErrWorkNotFound
	}
	return rec.remaining, nil
}

// PostMessage 入队一条 A2A 消息。
func (s *MemStore) PostMessage(_ context.Context, m QueuedMessage) (string, error) {
	if m.WorkID == "" || m.Method == "" {
		return "", ErrCode(CodeInvalidArgument, "queued message 需要 work_id/method", false, nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.works[m.WorkID]
	if !ok {
		return "", ErrWorkNotFound
	}
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.State == "" {
		m.State = MessagePending
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	cp := copyMessage(m)
	rec.messages[cp.ID] = &cp
	return cp.ID, nil
}

// ClaimMessage 领取一条 pending 消息。
func (s *MemStore) ClaimMessage(_ context.Context, workID string, lease time.Duration) (*QueuedMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.works[workID]
	if !ok {
		return nil, ErrWorkNotFound
	}
	ids := make([]string, 0, len(rec.messages))
	for id, m := range rec.messages {
		if m.State == MessagePending {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return rec.messages[ids[i]].CreatedAt.Before(rec.messages[ids[j]].CreatedAt)
	})
	if len(ids) == 0 {
		return nil, nil
	}
	m := rec.messages[ids[0]]
	m.State = MessageClaimed
	m.LeaseUntil = time.Now().UTC().Add(lease)
	m.Attempts++
	cp := copyMessage(*m)
	return &cp, nil
}

// AckMessage 标记消息已投递。
func (s *MemStore) AckMessage(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.works {
		if m, ok := rec.messages[id]; ok {
			m.State = MessageDelivered
			m.LeaseUntil = time.Time{}
			return nil
		}
	}
	return ErrCode(CodeNotFound, "message not found: "+id, false, nil)
}

// NackMessage 重试或标记失败。
func (s *MemStore) NackMessage(_ context.Context, id, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.works {
		if m, ok := rec.messages[id]; ok {
			m.LastError = errMsg
			m.LeaseUntil = time.Time{}
			if m.Attempts >= s.maxTries {
				m.State = MessageFailed
			} else {
				m.State = MessagePending
			}
			return nil
		}
	}
	return ErrCode(CodeNotFound, "message not found: "+id, false, nil)
}

// QueueDepth 返回某 work 的 pending+claimed 数。
func (s *MemStore) QueueDepth(_ context.Context, workID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.works[workID]
	if !ok {
		return 0, ErrWorkNotFound
	}
	n := 0
	for _, m := range rec.messages {
		if m.State == MessagePending || m.State == MessageClaimed {
			n++
		}
	}
	return n, nil
}

// Sweep 修复过期租约、标记 stalled/expired。
func (s *MemStore) Sweep(_ context.Context, now time.Time) (SweepResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res SweepResult
	for _, rec := range s.works {
		if !IsTerminal(rec.work.Status) && rec.remaining.DeadlineAt != "" {
			if t, err := time.Parse(time.RFC3339, rec.remaining.DeadlineAt); err == nil && now.After(t) {
				if CanTransition(rec.work.Status, WorkStalled) {
					rec.work.Status = WorkStalled
					rec.work.UpdatedAt = now.UTC().Format(time.RFC3339)
					res.Stalled++
				}
			}
		}
		for _, m := range rec.messages {
			switch m.State {
			case MessageClaimed:
				if !m.LeaseUntil.IsZero() && now.After(m.LeaseUntil) {
					m.LeaseUntil = time.Time{}
					if m.Attempts >= s.maxTries {
						m.State = MessageFailed
					} else {
						m.State = MessagePending
					}
					res.Retried++
				}
			case MessagePending:
				if now.Sub(m.CreatedAt) > s.msgTTL {
					m.State = MessageExpired
					res.Expired++
				}
			}
		}
	}
	return res, nil
}
