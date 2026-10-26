package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func postJobReport(t *testing.T, s *Server, original protocol.Envelope, item, phase string, final bool) {
	t.Helper()
	c, err := protocol.Dial(s.WsURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw, _ := protocol.EncodePayload(protocol.AsyncResultPayload{ItemID: item, Phase: phase, Text: "timer " + phase, Final: final})
	env, _ := protocol.NewEnvelope(protocol.TypeJump, "timer", "mcp", original.Trace, protocol.JumpPayload{Action: "report_result", Input: raw})
	env.Boss = "mcp"
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	res, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var reply protocol.ResponsePayload
	if res.DecodePayload(&reply) != nil || !reply.OK {
		t.Fatalf("report failed: %+v", reply)
	}
}

func jobCallIDs(t *testing.T, result *mcp.CallToolResult) (string, string) {
	t.Helper()
	if result.IsError {
		t.Fatalf("job rejected: %+v", result)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var out struct {
		ID  string `json:"job_id"`
		URI string `json:"resource_uri"`
	}
	if json.Unmarshal(raw, &out) != nil || out.ID == "" || out.URI != jobPrefix+out.ID {
		t.Fatalf("invalid observation receipt: %s", raw)
	}
	return out.ID, out.URI
}

func readJob(t *testing.T, c *mcp.ClientSession, uri string) JobSnapshot {
	t.Helper()
	r, err := c.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri})
	if err != nil || r == nil || len(r.Contents) != 1 {
		t.Fatalf("read observation: %v %+v", err, r)
	}
	var out JobSnapshot
	if err := json.Unmarshal([]byte(r.Contents[0].Text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func timerCard(peer string) protocol.ServiceCard {
	return protocol.ServiceCard{Name: "timer", Addr: peer, Provides: []protocol.Provide{{Action: "sleep", Tool: true, Async: true}, {Action: "cancel", Tool: true}}}
}

func TestExportEarlyFinalAndCrossRequestRead(t *testing.T) {
	var s *Server
	peer := gatewayPeer(t, func(env protocol.Envelope) (any, error) {
		// Deliver the final report and wait for its acknowledgment BEFORE the
		// original request gets a receipt. No scheduling luck is involved.
		postJobReport(t, s, env, "early-item", "done", true)
		return protocol.TimerSleepResult{ItemID: "early-item", Phase: "queued"}, nil
	})
	s = startGateway(t, Config{MaxCalls: 1, MaxCallsPerPrincipal: 1}, []protocol.ServiceCard{timerCard(peer)}, `{"exports":["timer__sleep"]}`)
	c := gatewayClient(t, s, gatewayToken)
	_, uri := jobCallIDs(t, gatewayCall(t, c, "timer__sleep", map[string]any{"ms": 100}))
	otherConnection := gatewayClient(t, s, gatewayToken)
	out := readJob(t, otherConnection, uri)
	if !out.Final || out.Phase != "done" {
		t.Fatalf("early result lost: %+v", out)
	}
	// The execution slot must be released once, even though final preceded ack.
	s.mu.Lock()
	calls := s.calls
	s.mu.Unlock()
	if calls != 0 {
		t.Fatalf("slot leak/underflow: %d", calls)
	}
	if _, err := otherConnection.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri + "?owner=x"}); err == nil {
		t.Fatal("malformed URI accepted")
	}
}

func TestExportHeldCapacityAndCancellation(t *testing.T) {
	var sequence atomic.Int32
	peer := gatewayPeer(t, func(env protocol.Envelope) (any, error) {
		var jump protocol.JumpPayload
		_ = env.DecodePayload(&jump)
		if jump.Action == "cancel" {
			var cancel protocol.TimerCancelPayload
			_ = protocol.DecodeRaw(jump.Input, &cancel)
			return protocol.TimerCancelResult{ItemID: cancel.ItemID, Status: "canceled"}, nil
		}
		sequence.Add(1)
		return protocol.TimerSleepResult{ItemID: protocol.NewID(), Phase: "queued"}, nil
	})
	s := startGateway(t, Config{MaxCalls: 1, MaxCallsPerPrincipal: 1}, []protocol.ServiceCard{timerCard(peer)}, `{"exports":["timer__sleep","timer__cancel"]}`)
	c := gatewayClient(t, s, gatewayToken)
	id, uri := jobCallIDs(t, gatewayCall(t, c, "timer__sleep", map[string]any{"ms": 1000}))
	if r := gatewayCall(t, c, "timer__sleep", map[string]any{"ms": 1000}); !r.IsError {
		t.Fatal("receipt released active budget")
	}
	if readJob(t, c, uri).Final {
		t.Fatal("receipt treated as final")
	}
	if r := gatewayCall(t, c, "timer__cancel", map[string]any{"job_id": id}); r.IsError {
		t.Fatal("full work budget prevented control")
	}
	if out := readJob(t, c, uri); !out.Final || out.Phase != "canceled" {
		t.Fatalf("cancellation evidence lost: %+v", out)
	}
	jobCallIDs(t, gatewayCall(t, c, "timer__sleep", map[string]any{"ms": 1000}))
	if sequence.Load() != 2 {
		t.Fatalf("unexpected native work count: %d", sequence.Load())
	}
}

func TestExportUnknownReceiptCanAcceptLateFinal(t *testing.T) {
	calls := make(chan protocol.Envelope, 1)
	peer := gatewayPeer(t, func(env protocol.Envelope) (any, error) {
		calls <- env
		return nil, errors.New("receipt unavailable")
	})
	s := startGateway(t, Config{}, []protocol.ServiceCard{timerCard(peer)}, `{"exports":["timer__sleep"]}`)
	c := gatewayClient(t, s, gatewayToken)
	_, uri := jobCallIDs(t, gatewayCall(t, c, "timer__sleep", map[string]any{"ms": 100}))
	if out := readJob(t, c, uri); out.State != "unknown" || out.Final {
		t.Fatalf("false completion: %+v", out)
	}
	original := <-calls
	postJobReport(t, s, original, "late-item", "done", true)
	postJobReport(t, s, original, "late-item", "running", false)
	out := readJob(t, c, uri)
	if !out.Final || out.Phase != "done" || strings.Contains(out.Text, "running") {
		t.Fatalf("late result lost/overwritten: %+v", out)
	}
}

func TestExportReadOwnershipAndControlRevocation(t *testing.T) {
	var cancelCalls atomic.Int32
	peer := gatewayPeer(t, func(env protocol.Envelope) (any, error) {
		var jump protocol.JumpPayload
		_ = env.DecodePayload(&jump)
		if jump.Action == "cancel" {
			cancelCalls.Add(1)
		}
		return protocol.TimerSleepResult{ItemID: "retained-item", Phase: "queued"}, nil
	})
	s := startGateway(t, Config{}, []protocol.ServiceCard{timerCard(peer)}, `{"exports":["timer__sleep","timer__cancel"]}`)
	c := gatewayClient(t, s, gatewayToken)
	id, uri := jobCallIDs(t, gatewayCall(t, c, "timer__sleep", map[string]any{"ms": 1000}))
	if err := os.WriteFile(s.cfg.ConfigPath, []byte(`{"exports":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.ReloadPolicy(); err != nil {
		t.Fatal(err)
	}
	// Removing permission stops new work/control but does not erase the
	// authenticated principal's evidence of an already accepted invocation.
	readJob(t, c, uri)
	if r := gatewayCall(t, c, "timer__cancel", map[string]any{"job_id": id}); !r.IsError || cancelCalls.Load() != 0 {
		t.Fatal("revoked control reached native timer")
	}
	const replacement = "new-owner-secret-01234567890123456789"
	if err := s.svc.ApplySetting("http.token", replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri}); err == nil {
		t.Fatal("revoked token read job")
	}
	newOwner := gatewayClient(t, s, replacement)
	if _, err := newOwner.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri}); err == nil {
		t.Fatal("replacement owner inherited private job")
	}
}

func TestExportFreshServerDoesNotRecoverOldObservation(t *testing.T) {
	peer := gatewayPeer(t, func(protocol.Envelope) (any, error) {
		return protocol.TimerSleepResult{ItemID: "unconfirmed-after-restart", Phase: "queued"}, nil
	})
	old := startGateway(t, Config{}, []protocol.ServiceCard{timerCard(peer)}, `{"exports":["timer__sleep"]}`)
	client := gatewayClient(t, old, gatewayToken)
	_, uri := jobCallIDs(t, gatewayCall(t, client, "timer__sleep", map[string]any{"ms": 1000}))
	old.Close()
	current := startGateway(t, Config{}, []protocol.ServiceCard{timerCard(peer)}, `{"exports":["timer__sleep"]}`)
	if _, err := gatewayClient(t, current, gatewayToken).ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri}); err == nil {
		t.Fatal("fresh server invented recovery of a volatile observation")
	}
}
