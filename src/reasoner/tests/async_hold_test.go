package tests

import (
	"context"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

// The classification must come through the actual SPv2 receipt. A
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
			var original spv2.Frame
			select {
			case original = <-async.calls:
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
			call, err := spv2.NewCall(original.Work, spv2.ServiceRef("stub-boss"), spv2.ServiceRef("reasoner"), "user_hold",
				protocol.UserHoldPayload{ConversationID: "default", Hold: protocol.UserHoldSecondary, Notes: []string{"hold integration note"}})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.Call(ctx, call)
			if err != nil {
				t.Fatalf("hold rejected: %v", err)
			}
			var result struct {
				Status string `msgpack:"status"`
			}
			if err := protocol.DecodeRaw(resp.Body, &result); err != nil || result.Status != "accepted" {
				t.Fatalf("hold rejected: %v %+v", err, result)
			}
			if kind == protocol.BatchKindOnce {
				if req := fake.LastRequest(); req != nil && requestHas(*req, "hold integration note") {
					t.Fatal("once-only tool released hold before its final result")
				}
				fireReport(t, s, original.Work, "hold-item", "done", "hold tool finished", true)
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
			case <-async.calls:
				t.Fatal("hold replayed the tool request")
			default:
			}
		})
	}
}
