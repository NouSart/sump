package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/NouSart/sump/protocol"
)

// asyncImports owns only local in-flight translations, not Reasoner tasks or a
// durable job queue. Admission limits remain held through bounded delivery.
type asyncImports struct {
	mu     sync.Mutex
	active map[string]*asyncImport
	closed bool
	wg     sync.WaitGroup
}

type asyncImport struct {
	owner  string
	cancel context.CancelFunc // nil once local execution/observation has ended
}

type importReceipt struct {
	ItemID string `msgpack:"item_id"`
	Phase  string `msgpack:"phase"`
}

func newAsyncImports() *asyncImports {
	return &asyncImports{active: make(map[string]*asyncImport)}
}

func (a *asyncImports) add(id, owner string, cancel context.CancelFunc) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return false
	}
	a.active[id] = &asyncImport{owner: owner, cancel: cancel}
	a.wg.Add(1)
	return true
}

func (a *asyncImports) observed(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if call := a.active[id]; call != nil {
		call.cancel = nil
	}
}

func (a *asyncImports) finish(id string) {
	a.mu.Lock()
	delete(a.active, id)
	a.mu.Unlock()
	a.wg.Done()
}

// Cancel affects this invocation's local wait only. A non-cooperating remote
// may continue executing after the HTTP connection has been canceled.
func (a *asyncImports) cancelOwned(id, owner string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	call := a.active[id]
	if call == nil || call.owner != owner || call.cancel == nil {
		return false
	}
	call.cancel()
	return true
}

// Close rejects new work, cancels existing local waits and drains workers while
// the native service is still alive to deliver their final observations.
func (a *asyncImports) Close(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	for _, call := range a.active {
		if call.cancel != nil {
			call.cancel()
		}
	}
	a.mu.Unlock()
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) handleAsyncImport(ctx context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	if env.From == "" || env.Trace == "" || ctx.Err() != nil {
		return nil, errors.New("MCP_IMPORT_REJECTED: live caller and trace required; remote tool not submitted")
	}
	principal := "sump:" + env.From
	if err := s.acquire(principal, nil); err != nil {
		return nil, err
	}
	prepared, err := s.imports.Prepare(in)
	if err != nil {
		s.release(principal)
		return nil, err
	}
	// The inbound handler context ends when its receipt returns. The worker must
	// instead follow service lifetime and the configured async observation limit.
	workCtx, cancel := context.WithTimeout(s.lifetime, s.cfg.AsyncTimeout)
	id := protocol.NewID()
	if ctx.Err() != nil || !s.asyncImports.add(id, env.From, cancel) {
		cancel()
		prepared.Close()
		s.release(principal)
		return nil, errors.New("MCP_IMPORT_REJECTED: service closing or request cancelled; remote tool not submitted")
	}
	go func() {
		defer s.asyncImports.finish(id)
		defer s.release(principal)
		defer prepared.Close()
		defer cancel()
		value, runErr := prepared.Run(workCtx)
		s.asyncImports.observed(id)
		report := importReport(id, value, runErr, prepared.submitted.Load())
		// Delivery gets its own bounded context, including during shutdown. A lost
		// acknowledgment never triggers replay of the remote business operation.
		deliveryCtx, stop := context.WithTimeout(context.Background(), s.cfg.DeliveryTimeout)
		defer stop()
		ack, err := s.svc.Call(deliveryCtx, env.From, "report_result", report, env.Trace, env.Boss)
		if err != nil || importDeliveryUnconfirmed(ack) {
			s.svc.Report("fire", fmt.Sprintf("MCP result delivery unconfirmed: item_id=%s phase=%s; remote operation not replayed", id, report.Phase))
		}
	}()
	return importReceipt{ItemID: id, Phase: "accepted"}, nil
}

// 兼容现有接收方的成功回执，不另造强制 ACK 协议；但明确的忽略或拒收
// 不能被 response.ok=true 掩盖。此处只判断交付，不改变已观察到的执行结果。
func importDeliveryUnconfirmed(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	if len(raw) > 4096 {
		return true
	}
	value, err := decodeJobData(raw)
	if err != nil {
		return true
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return fields["status"] == "ignored" || fields["accepted"] == false || fields["received"] == false
}

func (s *Server) handleCancelImport(_ context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	if len(in.Input) > 256 {
		return nil, errors.New("cancel_import input exceeds byte limit")
	}
	value, err := decodeJobData(in.Input)
	fields, valid := value.(map[string]any)
	if err != nil || !valid || len(fields) != 1 {
		return nil, errors.New("cancel_import requires only item_id")
	}
	id, ok := fields["item_id"].(string)
	if !ok || boundedText("item_id", id, 128, true) != nil || env.From == "" {
		return nil, errors.New("cancel_import requires a valid item_id and caller")
	}
	status := "unknown"
	if s.asyncImports.cancelOwned(id, env.From) {
		status = "observation_cancelled"
	}
	return map[string]any{"item_id": id, "status": status, "remote_outcome": "unknown"}, nil
}

func importReport(id string, value any, err error, submitted bool) protocol.AsyncResultPayload {
	phase := "completed"
	if err != nil {
		phase = "failed"
		outcome := "not_submitted"
		if submitted {
			phase, outcome = "outcome_unknown", "unknown"
		}
		value = map[string]any{"phase": phase, "reason": err.Error(), "execution_outcome": outcome}
	} else if data, ok := value.(map[string]any); ok && data["is_error"] == true {
		phase = "failed"
	}
	raw, encodeErr := protocol.EncodePayload(value)
	if encodeErr != nil {
		phase = "outcome_unknown"
		value = map[string]any{"phase": phase, "reason": "MCP result encoding failed; completion cannot be delivered"}
		raw, _ = protocol.EncodePayload(value)
	}
	text := importResultText(value)
	return protocol.AsyncResultPayload{ItemID: id, Phase: phase, Text: "[MCP 工具结果 · " + phase + "]\n" + text, Data: raw, Final: true}
}

// Reasoner consumes Text while Data preserves the structured result. Truncation
// here affects only the human-readable projection and is explicitly marked.
func importResultText(value any) string {
	var text strings.Builder
	if data, ok := value.(map[string]any); ok {
		if content, ok := data["content"].([]any); ok {
			for _, entry := range content {
				if part, ok := entry.(map[string]any); ok && part["type"] == "text" {
					if v, ok := part["text"].(string); ok {
						text.WriteString(v)
						text.WriteByte('\n')
					}
				}
			}
		}
	}
	if text.Len() == 0 {
		encoded, _ := json.Marshal(value)
		text.Write(encoded)
	}
	const maxTextBytes = 8192
	if text.Len() > maxTextBytes {
		return utf8Prefix(text.String(), maxTextBytes) + "\n[文本摘要已截断；结构化结果保留在 data]"
	}
	return text.String()
}
