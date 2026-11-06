package ref

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/NouSart/sump/spv2"
)

// ReasonerConfig 是参考 reasoner 的对端地址（ws://host:port/spv2）。
type ReasonerConfig struct {
	MemoryURL   string
	ToolURL     string
	EntryURL    string
	DialTimeout time.Duration
	CallTimeout time.Duration
}

// Reasoner 是 service:reasoner 的参考实现：
// message -> memory.context -> tool.echo -> entry.deliver，不接 LLM。
type Reasoner struct {
	srv         *spv2.Server
	memory      *spv2.Client
	tool        *spv2.Client
	entry       *spv2.Client
	callTimeout time.Duration
}

// NewReasoner 依次连接 memory/tool/entry，然后创建 reasoner 服务。
func NewReasoner(cfg ReasonerConfig) (*Reasoner, error) {
	if cfg.MemoryURL == "" || cfg.ToolURL == "" || cfg.EntryURL == "" {
		return nil, errors.New("ref: reasoner 需要 memory/tool/entry 三个 WS 地址")
	}
	dialTimeout := cfg.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	memory, err := spv2.Dial(ctx, cfg.MemoryURL)
	if err != nil {
		return nil, fmt.Errorf("ref: 连接 memory 失败: %w", err)
	}
	tool, err := spv2.Dial(ctx, cfg.ToolURL)
	if err != nil {
		_ = memory.Close()
		return nil, fmt.Errorf("ref: 连接 tool 失败: %w", err)
	}
	entry, err := spv2.Dial(ctx, cfg.EntryURL)
	if err != nil {
		_ = memory.Close()
		_ = tool.Close()
		return nil, fmt.Errorf("ref: 连接 entry 失败: %w", err)
	}

	srv, err := spv2.NewServer(ReasonerCard())
	if err != nil {
		_ = memory.Close()
		_ = tool.Close()
		_ = entry.Close()
		return nil, err
	}
	callTimeout := cfg.CallTimeout
	if callTimeout <= 0 {
		callTimeout = 10 * time.Second
	}
	r := &Reasoner{srv: srv, memory: memory, tool: tool, entry: entry, callTimeout: callTimeout}
	srv.Handle("message", r.handleMessage)
	return r, nil
}

// Server 返回底层 SPv2.1 服务端。
func (r *Reasoner) Server() *spv2.Server { return r.srv }

// Close 关闭到三个对端的连接。
func (r *Reasoner) Close() error {
	var errs []error
	for _, c := range []*spv2.Client{r.memory, r.tool, r.entry} {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Reasoner) handleMessage(ctx context.Context, call *spv2.Call) error {
	var req MessageRequest
	if len(call.Frame.Body) == 0 {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "reasoner.message 需要 body", false, nil)
	}
	if err := call.Frame.DecodeBody(&req); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "reasoner.message body 无效: "+err.Error(), false, nil)
	}
	if req.Text == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "reasoner.message 需要 text", false, nil)
	}
	workID := call.WorkID()
	if workID == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "reasoner.message 需要 frame.work", false, nil)
	}
	from := call.Frame.From

	// human 入口必须是 owner；§6.5 的 allowlist 属于入口服务，参考链路不实现。
	if from.Kind == "human" && from.Authority != spv2.AuthorityOwner {
		return spv2.ErrCode(spv2.CodePermissionDenied, "reasoner.message: human 必须 authority=owner", false, nil)
	}
	// agent 消息必须是 authority=none（§6.5）；runtime 只用于 deliver/cancel 等执行动作。
	if from.Kind == "agent" && from.Authority != spv2.AuthorityNone {
		return spv2.ErrCode(spv2.CodePermissionDenied, "reasoner.message: agent 必须 authority=none", false, nil)
	}
	// agent 消息在 memory.context 之前先查已有 work，避免未授权调用创建 work。
	if from.Kind == "agent" {
		var existing WorkGetResponse
		if err := callPeer(ctx, r.memory, spv2.AgentRef(ServiceReasoner), workID, ServiceMemory, "work_get",
			WorkGetRequest{Work: workID}, &existing, r.callTimeout); err != nil {
			return err
		}
		if err := spv2.AuthorizeMessage(existing.Work, from); err != nil {
			return err
		}
	}

	contextReq := ContextRequest{
		Messages:     []Message{{Role: "user", Text: req.Text}},
		Conversation: req.Conversation,
	}
	if from.Kind == "human" {
		// 人类入口就是 work owner；memory 会在首次 context 时登记。
		contextReq.Owner = from
	}
	var contextResp ContextResponse
	if err := callPeer(ctx, r.memory, spv2.AgentRef(ServiceReasoner), workID, ServiceMemory, "context",
		contextReq, &contextResp, r.callTimeout); err != nil {
		return err
	}
	// 防止把一个已存在、owner 不同的 work 当成自己的。
	if err := spv2.AuthorizeMessage(contextResp.Work, from); err != nil {
		return err
	}

	var echoResp EchoResponse
	if err := callPeer(ctx, r.tool, spv2.AgentRef(ServiceReasoner), workID, ServiceTool, "echo",
		EchoRequest{Text: req.Text, Conversation: req.Conversation}, &echoResp, r.callTimeout); err != nil {
		return err
	}
	if echoResp.Text == "" {
		return spv2.ErrCode(spv2.CodeInternal, "tool.echo 返回空 text", false, nil)
	}

	// deliver 是 runtime 动作：§6.5 要求 from == work.agent && authority=runtime。
	var deliverResp DeliverResponse
	if err := callPeer(ctx, r.entry, spv2.AgentRef(ServiceReasoner).WithAuthority(spv2.AuthorityRuntime), workID, ServiceEntry, "deliver",
		DeliverRequest{Conversation: req.Conversation, Text: echoResp.Text, Final: true}, &deliverResp, r.callTimeout); err != nil {
		return err
	}
	if !deliverResp.Delivered {
		return spv2.ErrCode(spv2.CodeInternal, "entry.deliver 未确认投递", false, nil)
	}

	return call.Reply(MessageResponse{Text: echoResp.Text, Work: workID})
}
