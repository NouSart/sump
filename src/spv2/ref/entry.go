package ref

import (
	"context"
	"fmt"
	"time"

	"github.com/NouSart/sump/spv2"
)

// EntryConfig 配置入口服务。
type EntryConfig struct {
	// MemoryURL 用于 deliver 时读取 work 行并校验 authority，必填。
	MemoryURL   string
	DialTimeout time.Duration
	CallTimeout time.Duration
}

// Entry 是 service:frontend 的参考实现：把 deliver 送进人类通道。
type Entry struct {
	srv         *spv2.Server
	deliveries  chan Delivery
	memory      *spv2.Client
	callTimeout time.Duration
}

// NewEntry 创建入口服务。通道有界，满了 deliver 会回 resource_exhausted。
func NewEntry(cfg EntryConfig) (*Entry, error) {
	srv, err := spv2.NewServer(EntryCard())
	if err != nil {
		return nil, err
	}
	if cfg.MemoryURL == "" {
		return nil, fmt.Errorf("ref: entry 需要 memory 地址来校验 deliver authority")
	}
	e := &Entry{srv: srv, deliveries: make(chan Delivery, 8), callTimeout: cfg.CallTimeout}
	if e.callTimeout <= 0 {
		e.callTimeout = 10 * time.Second
	}
	dialTimeout := cfg.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	memory, err := spv2.Dial(ctx, cfg.MemoryURL)
	if err != nil {
		return nil, fmt.Errorf("ref: entry 连接 memory 失败: %w", err)
	}
	e.memory = memory
	srv.Handle("deliver", e.handleDeliver)
	return e, nil
}

// Server 返回底层 SPv2.1 服务端。
func (e *Entry) Server() *spv2.Server { return e.srv }

// Deliveries 返回人类通道；测试/前端从这里消费投递。
func (e *Entry) Deliveries() <-chan Delivery { return e.deliveries }

// Close 关闭到 memory 的连接。
func (e *Entry) Close() error {
	if e.memory == nil {
		return nil
	}
	return e.memory.Close()
}

func (e *Entry) handleDeliver(ctx context.Context, call *spv2.Call) error {
	var req DeliverRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "entry.deliver body 无效: "+err.Error(), false, nil)
	}
	if req.Text == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "entry.deliver 需要 text", false, nil)
	}
	workID := call.WorkID()
	if workID == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "entry.deliver 需要 frame.work", false, nil)
	}
	// §7.3：entry 没有 work 行时可以向 memory 查 work -> owner/agent。
	// §6.5：deliver 只允许 work.owner(owner) 或 work.agent(runtime)。
	work, err := e.lookupWork(ctx, workID)
	if err != nil {
		return err
	}
	if err := spv2.AuthorizeDeliver(work, call.Frame.From); err != nil {
		return err
	}
	delivery := Delivery{
		Work:         workID,
		From:         call.Frame.From,
		Conversation: req.Conversation,
		Text:         req.Text,
		Final:        req.Final,
	}
	select {
	case e.deliveries <- delivery:
	default:
		return spv2.ErrCode(spv2.CodeResourceExhausted, "entry.deliver 通道已满", true, nil)
	}
	return call.Reply(DeliverResponse{Delivered: true, Conversation: req.Conversation})
}

func (e *Entry) lookupWork(ctx context.Context, workID string) (spv2.Work, error) {
	var resp WorkGetResponse
	if err := callPeer(ctx, e.memory, spv2.AgentRef(ServiceEntry), workID, ServiceMemory, "work_get",
		WorkGetRequest{Work: workID}, &resp, e.callTimeout); err != nil {
		return spv2.Work{}, err
	}
	return resp.Work, nil
}
