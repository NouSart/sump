package tests

import (
	"context"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
)

// The classification must come through the actual WebSocket receipt. A
// resident tool may stay alive while secondary hold resumes; a once-only tool
// must finish first. Neither case may replay the original tool request.
func TestAsyncReceiptKindControlsSecondaryHold(t *testing.T) {
	for _, kind := range []string{protocol.BatchKindOnce, protocol.BatchKindResident} {
		t.Run(kind, func(t *testing.T) {
			sc := startStubCenter(t)
			async := startStubAsync(t, sc, "hold-item", kind)
			fake := &llm.Fake{Replies: []llm.Message{
				{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call", Type: "function",
					Function: llm.FunctionCall{Name: "stub-async__run", Arguments: `{"command":"echo hi"}`}}}},
				{Role: "assistant"},
			}}
			s, _, _ := startReportEnv(t, sc, fake)
			sendUser(t, s, "hold receipt test")
			var original protocol.Envelope
			select {
			case original = <-async.envelope:
			case <-time.After(5 * time.Second):
				t.Fatal("tool was not dispatched")
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if req := fake.LastRequest(); req != nil && requestHas(*req, "hold-item") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("receipt was not consumed")
				}
				time.Sleep(time.Millisecond)
			}
			c := dialReasoner(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			raw, _ := protocol.EncodePayload(protocol.UserHoldPayload{
				ConversationID: "default", Hold: protocol.UserHoldSecondary, Notes: []string{"hold integration note"},
			})
			env, _ := protocol.NewEnvelope(protocol.TypeJump, "stub-boss", "reasoner", original.Trace,
				protocol.JumpPayload{Action: "user_hold", Input: raw})
			resp, err := c.Call(ctx, env)
			if err != nil {
				t.Fatal(err)
			}
			var result protocol.ResponsePayload
			if err := resp.DecodePayload(&result); err != nil || !result.OK {
				t.Fatalf("hold rejected: %v %+v", err, result)
			}
			if kind == protocol.BatchKindOnce {
				if req := fake.LastRequest(); req != nil && requestHas(*req, "hold integration note") {
					t.Fatal("once-only tool released hold before its final result")
				}
				fireReport(t, s, original.Trace, "hold-item", "done", "hold tool finished", true)
			}
			deadline = time.Now().Add(5 * time.Second)
			for {
				if req := fake.LastRequest(); req != nil && requestHas(*req, "hold integration note") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("secondary hold did not resume with its note")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case <-async.envelope:
				t.Fatal("hold replayed the tool request")
			default:
			}
		})
	}
}
