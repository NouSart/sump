package ref_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NouSart/sump/spv2"
	"github.com/NouSart/sump/spv2/ref"
)

type chain struct {
	store     *spv2.MemStore
	memory    *ref.Memory
	tool      *ref.Tool
	entry     *ref.Entry
	reasoner  *ref.Reasoner
	client    *spv2.Client
	memoryURL string
	entryURL  string
}

// newChain 用 httptest 起 4 个真实 WS server，并连好 reasoner 的三个对端。
func newChain(t *testing.T) *chain {
	t.Helper()

	store := spv2.NewMemStore()
	memory, err := ref.NewMemory(store)
	if err != nil {
		t.Fatalf("创建 memory 失败: %v", err)
	}

	serve := func(h http.Handler) *httptest.Server {
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		return ts
	}
	memoryTS := serve(memory.Server().Handler())
	memoryURL := spv2.HTTPToWS(memoryTS.URL)

	entry, err := ref.NewEntry(ref.EntryConfig{MemoryURL: memoryURL})
	if err != nil {
		t.Fatalf("创建 entry 失败: %v", err)
	}
	t.Cleanup(func() { _ = entry.Close() })

	tool, err := ref.NewTool()
	if err != nil {
		t.Fatalf("创建 tool 失败: %v", err)
	}
	toolTS := serve(tool.Server().Handler())
	entryTS := serve(entry.Server().Handler())
	entryURL := spv2.HTTPToWS(entryTS.URL)

	reasoner, err := ref.NewReasoner(ref.ReasonerConfig{
		MemoryURL: memoryURL,
		ToolURL:   spv2.HTTPToWS(toolTS.URL),
		EntryURL:  entryURL,
	})
	if err != nil {
		t.Fatalf("创建 reasoner 失败: %v", err)
	}
	t.Cleanup(func() { _ = reasoner.Close() })

	reasonerTS := serve(reasoner.Server().Handler())
	client := dial(t, spv2.HTTPToWS(reasonerTS.URL))

	return &chain{
		store:     store,
		memory:    memory,
		tool:      tool,
		entry:     entry,
		reasoner:  reasoner,
		client:    client,
		memoryURL: memoryURL,
		entryURL:  entryURL,
	}
}

func dial(t *testing.T, url string) *spv2.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, url)
	if err != nil {
		t.Fatalf("连接 %s 失败: %v", url, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func testContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// TestEntryReasonerMemoryToolDeliver 是任务要求的完整链：
// entry -> reasoner -> memory.context -> tool.echo -> reasoner -> entry.deliver。
func TestEntryReasonerMemoryToolDeliver(t *testing.T) {
	c := newChain(t)
	ctx, cancel := testContext(t)
	defer cancel()

	work := spv2.NewID()
	const (
		text         = "把门户配色改成深色"
		conversation = "web:private:owner"
	)

	call, err := spv2.NewCall(work, spv2.HumanRef("portal"), spv2.ServiceRef(ref.ServiceReasoner), "message",
		ref.MessageRequest{Text: text, Conversation: conversation})
	if err != nil {
		t.Fatalf("构造入口 call 失败: %v", err)
	}
	resp, err := c.client.Call(ctx, call)
	if err != nil {
		t.Fatalf("reasoner.message 失败: %v", err)
	}
	if resp.Status != spv2.StatusOK {
		t.Fatalf("reasoner.message status = %q, want ok", resp.Status)
	}
	var msgResp ref.MessageResponse
	if err := resp.DecodeBody(&msgResp); err != nil {
		t.Fatalf("解码 reasoner.message 响应失败: %v", err)
	}
	if msgResp.Text != text {
		t.Fatalf("reasoner.message text = %q, want %q", msgResp.Text, text)
	}
	if msgResp.Work != work {
		t.Fatalf("reasoner.message work = %q, want %q", msgResp.Work, work)
	}

	select {
	case d := <-c.entry.Deliveries():
		if d.Text != text {
			t.Errorf("deliver.text = %q, want %q", d.Text, text)
		}
		if d.Conversation != conversation {
			t.Errorf("deliver.conversation = %q, want %q", d.Conversation, conversation)
		}
		if d.Work != work {
			t.Errorf("deliver.work = %q, want %q", d.Work, work)
		}
		if !d.Final {
			t.Error("deliver.final = false, want true")
		}
		if d.From.Kind != "agent" || d.From.ID != ref.ServiceReasoner {
			t.Errorf("deliver.from = %+v, want agent:reasoner", d.From)
		}
	case <-ctx.Done():
		t.Fatal("等待 entry.deliver 超时")
	}

	w, err := c.store.GetWork(context.Background(), work)
	if err != nil {
		t.Fatalf("memory store 里没有 work %s: %v", work, err)
	}
	if w.Status == spv2.WorkFailed {
		t.Fatalf("work 状态是 failed: %+v", w)
	}
	if w.Status != spv2.WorkOpen {
		t.Errorf("work.status = %q, want %q", w.Status, spv2.WorkOpen)
	}
	if w.Owner.Kind != "human" || w.Owner.ID != "portal" {
		t.Errorf("work.owner = %+v, want human:portal", w.Owner)
	}
	if w.Agent.ID != ref.ServiceReasoner {
		t.Errorf("work.agent = %+v, want agent:reasoner", w.Agent)
	}
	if w.Entry.ID != ref.ServiceEntry {
		t.Errorf("work.entry = %+v, want service:frontend", w.Entry)
	}
}

// TestReasonerRejectsEmptyText 覆盖 reasoner.message 的错误路径：空 text 不得落 work。
func TestReasonerRejectsEmptyText(t *testing.T) {
	c := newChain(t)
	ctx, cancel := testContext(t)
	defer cancel()

	work := spv2.NewID()
	call, err := spv2.NewCall(work, spv2.HumanRef("portal"), spv2.ServiceRef(ref.ServiceReasoner), "message",
		ref.MessageRequest{Conversation: "web:private:owner"})
	if err != nil {
		t.Fatalf("构造 call 失败: %v", err)
	}
	if _, err := c.client.Call(ctx, call); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("空 text 错误 = %v, want invalid_argument", err)
	}
	if _, err := c.store.GetWork(ctx, work); !errors.Is(err, spv2.ErrWorkNotFound) {
		t.Fatalf("空 text 不应创建 work，GetWork err = %v", err)
	}
	select {
	case d := <-c.entry.Deliveries():
		t.Fatalf("空 text 不应 deliver，收到 %+v", d)
	default:
	}
}

// TestReasonerRejectsHumanWithoutOwnerAuthority 覆盖 human 必须是 owner 的校验。
func TestReasonerRejectsHumanWithoutOwnerAuthority(t *testing.T) {
	c := newChain(t)
	ctx, cancel := testContext(t)
	defer cancel()

	work := spv2.NewID()
	from := spv2.HumanRef("portal").WithAuthority(spv2.AuthorityNone)
	call, err := spv2.NewCall(work, from, spv2.ServiceRef(ref.ServiceReasoner), "message",
		ref.MessageRequest{Text: "hi", Conversation: "web:private:owner"})
	if err != nil {
		t.Fatalf("构造 call 失败: %v", err)
	}
	if _, err := c.client.Call(ctx, call); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("authority=none 错误 = %v, want permission_denied", err)
	}
	if _, err := c.store.GetWork(ctx, work); !errors.Is(err, spv2.ErrWorkNotFound) {
		t.Fatalf("未授权 message 不应创建 work，GetWork err = %v", err)
	}
}

// TestReasonerRejectsAgentRuntimeMessage 覆盖 reasoner.message 的 authority：
// agent 消息必须是 none，runtime 只用于执行动作。
func TestReasonerRejectsAgentRuntimeMessage(t *testing.T) {
	c := newChain(t)
	ctx, cancel := testContext(t)
	defer cancel()

	work := spv2.NewID()
	from := spv2.AgentRef(ref.ServiceReasoner).WithAuthority(spv2.AuthorityRuntime)
	call, err := spv2.NewCall(work, from, spv2.ServiceRef(ref.ServiceReasoner), "message",
		ref.MessageRequest{Text: "hi", Conversation: "web:private:owner"})
	if err != nil {
		t.Fatalf("构造 call 失败: %v", err)
	}
	if _, err := c.client.Call(ctx, call); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("agent runtime message 错误 = %v, want permission_denied", err)
	}
	if _, err := c.store.GetWork(ctx, work); !errors.Is(err, spv2.ErrWorkNotFound) {
		t.Fatalf("agent runtime message 不应创建 work，GetWork err = %v", err)
	}
}

// TestEntryDeliverAuthority 覆盖 entry.deliver 的 authority 矩阵：
// 必须是 work.agent 且 authority=runtime。
func TestEntryDeliverAuthority(t *testing.T) {
	c := newChain(t)
	ctx, cancel := testContext(t)
	defer cancel()

	work := spv2.NewID()
	memoryClient := dial(t, c.memoryURL)
	contextCall, err := spv2.NewCall(work, spv2.AgentRef(ref.ServiceReasoner), spv2.ServiceRef(ref.ServiceMemory), "context",
		ref.ContextRequest{Owner: spv2.HumanRef("portal"), Conversation: "web:private:owner"})
	if err != nil {
		t.Fatalf("构造 context call 失败: %v", err)
	}
	var contextResp ref.ContextResponse
	if err := memoryClient.CallDecode(ctx, contextCall, &contextResp); err != nil {
		t.Fatalf("memory.context 失败: %v", err)
	}

	entryClient := dial(t, c.entryURL)
	cases := []struct {
		name   string
		from   spv2.Ref
		wantOK bool
	}{
		{"work.agent 缺 runtime", spv2.AgentRef(ref.ServiceReasoner), false},
		{"非 work.agent 带 runtime", spv2.AgentRef("mallory").WithAuthority(spv2.AuthorityRuntime), false},
		{"work.agent 带 runtime", spv2.AgentRef(ref.ServiceReasoner).WithAuthority(spv2.AuthorityRuntime), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deliverCall, err := spv2.NewCall(work, tc.from, spv2.ServiceRef(ref.ServiceEntry), "deliver",
				ref.DeliverRequest{Conversation: "web:private:owner", Text: "投递-" + tc.name, Final: true})
			if err != nil {
				t.Fatalf("构造 deliver call 失败: %v", err)
			}
			if !tc.wantOK {
				if _, err := entryClient.Call(ctx, deliverCall); !spv2.IsCode(err, spv2.CodePermissionDenied) {
					t.Fatalf("deliver 错误 = %v, want permission_denied", err)
				}
				select {
				case d := <-c.entry.Deliveries():
					t.Fatalf("未授权 deliver 不应投递，收到 %+v", d)
				default:
				}
				return
			}
			if _, err := entryClient.Call(ctx, deliverCall); err != nil {
				t.Fatalf("授权 deliver 失败: %v", err)
			}
			select {
			case d := <-c.entry.Deliveries():
				if d.Text != "投递-"+tc.name {
					t.Fatalf("deliver.text = %q", d.Text)
				}
			case <-ctx.Done():
				t.Fatal("等待 entry.deliver 超时")
			}
		})
	}
}

// TestMemoryCancelAuthorizesOwner 覆盖 memory.cancel + AuthorizeControl。
func TestMemoryCancelAuthorizesOwner(t *testing.T) {
	c := newChain(t)
	ctx, cancel := testContext(t)
	defer cancel()

	work := spv2.NewID()
	memoryClient := dial(t, c.memoryURL)
	contextCall, err := spv2.NewCall(work, spv2.AgentRef(ref.ServiceReasoner), spv2.ServiceRef(ref.ServiceMemory), "context",
		ref.ContextRequest{Owner: spv2.HumanRef("portal"), Conversation: "web:private:owner"})
	if err != nil {
		t.Fatalf("构造 context call 失败: %v", err)
	}
	var contextResp ref.ContextResponse
	if err := memoryClient.CallDecode(ctx, contextCall, &contextResp); err != nil {
		t.Fatalf("memory.context 失败: %v", err)
	}

	otherCancel, err := spv2.NewCall(work, spv2.HumanRef("mallory"), spv2.ServiceRef(ref.ServiceMemory), "cancel",
		ref.CancelRequest{Reason: "不是我的 work"})
	if err != nil {
		t.Fatalf("构造 cancel call 失败: %v", err)
	}
	if _, err := memoryClient.Call(ctx, otherCancel); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("非 owner cancel 错误 = %v, want permission_denied", err)
	}
	if w, err := c.store.GetWork(ctx, work); err != nil || w.Status != spv2.WorkOpen {
		t.Fatalf("非 owner cancel 不应改变 work: %+v, %v", w, err)
	}

	ownerCancel, err := spv2.NewCall(work, spv2.HumanRef("portal"), spv2.ServiceRef(ref.ServiceMemory), "cancel",
		ref.CancelRequest{Reason: "用户停手"})
	if err != nil {
		t.Fatalf("构造 cancel call 失败: %v", err)
	}
	var cancelResp ref.CancelResponse
	if err := memoryClient.CallDecode(ctx, ownerCancel, &cancelResp); err != nil {
		t.Fatalf("owner cancel 失败: %v", err)
	}
	if !cancelResp.Cancelled || cancelResp.Work.Status != spv2.WorkCancelled {
		t.Fatalf("cancel 响应 = %+v", cancelResp)
	}
	w, err := c.store.GetWork(ctx, work)
	if err != nil {
		t.Fatalf("GetWork 失败: %v", err)
	}
	if w.Status != spv2.WorkCancelled {
		t.Fatalf("work.status = %q, want cancelled", w.Status)
	}
}

// TestMemoryRenewAuthorizesOwner 覆盖 memory.renew + AuthorizeControl。
func TestMemoryRenewAuthorizesOwner(t *testing.T) {
	c := newChain(t)
	ctx, cancel := testContext(t)
	defer cancel()

	work := spv2.NewID()
	memoryClient := dial(t, c.memoryURL)
	contextCall, err := spv2.NewCall(work, spv2.AgentRef(ref.ServiceReasoner), spv2.ServiceRef(ref.ServiceMemory), "context",
		ref.ContextRequest{Owner: spv2.HumanRef("portal"), Conversation: "web:private:owner"})
	if err != nil {
		t.Fatalf("构造 context call 失败: %v", err)
	}
	var contextResp ref.ContextResponse
	if err := memoryClient.CallDecode(ctx, contextCall, &contextResp); err != nil {
		t.Fatalf("memory.context 失败: %v", err)
	}
	oldTokens := contextResp.Work.Lease.MaxTokens
	newDeadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	otherRenew, err := spv2.NewCall(work, spv2.HumanRef("mallory"), spv2.ServiceRef(ref.ServiceMemory), "renew",
		ref.RenewRequest{Lease: spv2.Lease{DeadlineAt: newDeadline}})
	if err != nil {
		t.Fatalf("构造 renew call 失败: %v", err)
	}
	if _, err := memoryClient.Call(ctx, otherRenew); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("非 owner renew 错误 = %v, want permission_denied", err)
	}
	if w, err := c.store.GetWork(ctx, work); err != nil || w.Lease.DeadlineAt != contextResp.Work.Lease.DeadlineAt {
		t.Fatalf("非 owner renew 不应改变 lease: %+v, %v", w, err)
	}

	ownerRenew, err := spv2.NewCall(work, spv2.HumanRef("portal"), spv2.ServiceRef(ref.ServiceMemory), "renew",
		ref.RenewRequest{Lease: spv2.Lease{DeadlineAt: newDeadline, MaxTokens: oldTokens + 1000}})
	if err != nil {
		t.Fatalf("构造 renew call 失败: %v", err)
	}
	var renewResp ref.RenewResponse
	if err := memoryClient.CallDecode(ctx, ownerRenew, &renewResp); err != nil {
		t.Fatalf("owner renew 失败: %v", err)
	}
	if renewResp.Work.Lease.DeadlineAt != newDeadline {
		t.Fatalf("renew deadline = %q, want %q", renewResp.Work.Lease.DeadlineAt, newDeadline)
	}
	if renewResp.Work.Lease.MaxTokens != oldTokens+1000 {
		t.Fatalf("renew max_tokens = %d, want %d", renewResp.Work.Lease.MaxTokens, oldTokens+1000)
	}
	remaining, err := c.store.Remaining(ctx, work)
	if err != nil {
		t.Fatalf("Remaining 失败: %v", err)
	}
	if remaining.Tokens != oldTokens+1000 {
		t.Fatalf("renew remaining.tokens = %d, want %d", remaining.Tokens, oldTokens+1000)
	}
}

// TestMemoryContextLeaseAndQueue 覆盖 memory 的 context/work_get/lease_charge/message_*。
func TestMemoryContextLeaseAndQueue(t *testing.T) {
	c := newChain(t)
	ctx, cancel := testContext(t)
	defer cancel()

	memoryClient := dial(t, c.memoryURL)
	work := spv2.NewID()

	contextCall, err := spv2.NewCall(work, spv2.AgentRef(ref.ServiceReasoner), spv2.ServiceRef(ref.ServiceMemory), "context",
		ref.ContextRequest{
			Messages:     []ref.Message{{Role: "user", Text: "hi"}},
			Owner:        spv2.HumanRef("portal"),
			Conversation: "web:private:owner",
		})
	if err != nil {
		t.Fatalf("构造 context call 失败: %v", err)
	}
	var contextResp ref.ContextResponse
	if err := memoryClient.CallDecode(ctx, contextCall, &contextResp); err != nil {
		t.Fatalf("memory.context 失败: %v", err)
	}
	if contextResp.Work.ID != work || contextResp.Work.Status != spv2.WorkOpen {
		t.Fatalf("context work = %+v", contextResp.Work)
	}
	if len(contextResp.Messages) != 1 || contextResp.Messages[0].Text != "hi" {
		t.Fatalf("context messages = %+v", contextResp.Messages)
	}

	workGetCall, err := spv2.NewCall(work, spv2.AgentRef(ref.ServiceReasoner), spv2.ServiceRef(ref.ServiceMemory), "work_get",
		ref.WorkGetRequest{})
	if err != nil {
		t.Fatalf("构造 work_get call 失败: %v", err)
	}
	var workGetResp ref.WorkGetResponse
	if err := memoryClient.CallDecode(ctx, workGetCall, &workGetResp); err != nil {
		t.Fatalf("memory.work_get 失败: %v", err)
	}
	if workGetResp.Work.ID != work {
		t.Fatalf("work_get = %+v", workGetResp.Work)
	}

	chargeCall, err := spv2.NewCall(work, spv2.AgentRef(ref.ServiceReasoner), spv2.ServiceRef(ref.ServiceMemory), "lease_charge",
		ref.LeaseChargeRequest{Delta: spv2.Charge{ToolCalls: 2, Tokens: 100}})
	if err != nil {
		t.Fatalf("构造 lease_charge call 失败: %v", err)
	}
	var chargeResp ref.LeaseChargeResponse
	if err := memoryClient.CallDecode(ctx, chargeCall, &chargeResp); err != nil {
		t.Fatalf("memory.lease_charge 失败: %v", err)
	}
	if chargeResp.Remaining.ToolCalls != 48 || chargeResp.Remaining.Tokens != 99900 {
		t.Fatalf("lease_charge remaining = %+v", chargeResp.Remaining)
	}

	overChargeCall, err := spv2.NewCall(work, spv2.AgentRef(ref.ServiceReasoner), spv2.ServiceRef(ref.ServiceMemory), "lease_charge",
		ref.LeaseChargeRequest{Delta: spv2.Charge{Tokens: 1 << 30}})
	if err != nil {
		t.Fatalf("构造 lease_charge call 失败: %v", err)
	}
	if _, err := memoryClient.Call(ctx, overChargeCall); !spv2.IsCode(err, spv2.CodeResourceExhausted) {
		t.Fatalf("超额 lease_charge 错误 = %v, want resource_exhausted", err)
	}

	postCall, err := spv2.NewCall(work, spv2.AgentRef(ref.ServiceReasoner), spv2.ServiceRef(ref.ServiceMemory), "message_post",
		ref.MessagePostRequest{
			From:   spv2.AgentRef("A"),
			To:     spv2.AgentRef("B"),
			Method: "message",
			Body:   []byte("hi"),
		})
	if err != nil {
		t.Fatalf("构造 message_post call 失败: %v", err)
	}
	var postResp ref.MessagePostResponse
	if err := memoryClient.CallDecode(ctx, postCall, &postResp); err != nil {
		t.Fatalf("memory.message_post 失败: %v", err)
	}
	if postResp.ID == "" {
		t.Fatal("message_post 没有返回 id")
	}

	claimCall, err := spv2.NewCall(work, spv2.AgentRef(ref.ServiceReasoner), spv2.ServiceRef(ref.ServiceMemory), "message_claim",
		ref.MessageClaimRequest{LeaseMS: 1000})
	if err != nil {
		t.Fatalf("构造 message_claim call 失败: %v", err)
	}
	var claimResp ref.MessageClaimResponse
	if err := memoryClient.CallDecode(ctx, claimCall, &claimResp); err != nil {
		t.Fatalf("memory.message_claim 失败: %v", err)
	}
	if claimResp.Message == nil || claimResp.Message.ID != postResp.ID {
		t.Fatalf("message_claim = %+v, want id %s", claimResp.Message, postResp.ID)
	}

	ackCall, err := spv2.NewCall(work, spv2.AgentRef(ref.ServiceReasoner), spv2.ServiceRef(ref.ServiceMemory), "message_ack",
		ref.MessageAckRequest{ID: postResp.ID})
	if err != nil {
		t.Fatalf("构造 message_ack call 失败: %v", err)
	}
	var ackResp ref.MessageAckResponse
	if err := memoryClient.CallDecode(ctx, ackCall, &ackResp); err != nil {
		t.Fatalf("memory.message_ack 失败: %v", err)
	}
	if !ackResp.Acked {
		t.Fatal("message_ack 未确认")
	}
}
