package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const jobPrefix = "sump://jobs/"

// Each exported invocation has its own trace, allocated before dispatch. The
// job is an observation of native work, not a new scheduler or MCP Task.
func (s *Server) callAsyncTool(ctx context.Context, owner string, b Binding, input any) *mcp.CallToolResult {
	trace := protocol.NewID()
	id, err := s.jobs.Reserve(owner, b.Service, b.Name, trace, func() { s.release(owner) })
	if err != nil {
		s.release(owner)
		return s.failure("rejected", err.Error())
	}
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	stop := context.AfterFunc(s.lifetime, cancel)
	defer func() { stop(); cancel() }()
	raw, err := s.svc.Call(callCtx, b.Service, b.Action, input, trace, "mcp")
	if err != nil {
		// Service.Call does not replay after sending. Preserve this observation
		// so a late report can still establish the outcome after receipt loss.
		s.jobs.MarkUnknown(id, "native receipt not confirmed; not retried")
	} else {
		var receipt protocol.TimerSleepResult
		if decodeMessagePack(raw, &receipt) != nil || receipt.Phase != "queued" || boundedText("item_id", receipt.ItemID, 256, true) != nil {
			s.jobs.MarkUnknown(id, "invalid native receipt; execution may have occurred")
			s.svc.Report("internal", "MCP timer adapter received an incompatible receipt")
		} else if err := s.jobs.BindReceipt(id, receipt.ItemID); err != nil {
			s.jobs.MarkUnknown(id, "native receipt could not be associated")
		}
	}
	snapshot, err := s.jobs.Read(owner, id)
	if err != nil {
		return s.failure("unknown", "observation unavailable; native execution may continue")
	}
	// Always label this as an observation, even if a very fast final report
	// arrived first. The resource snapshot carries the authoritative state.
	result := s.result(map[string]any{"job_id": id, "resource_uri": jobPrefix + id, "observation": snapshot}, false)
	if !result.IsError {
		result.Content = append(result.Content, &mcp.ResourceLink{URI: jobPrefix + id, Name: "job", MIMEType: "application/json"})
	}
	return result
}

func (s *Server) handleReport(_ context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	if env.To != "mcp" || env.Boss != "mcp" {
		return nil, errors.New("report target does not match an MCP observation")
	}
	var report protocol.AsyncResultPayload
	if err := decodeMessagePack(in.Input, &report); err != nil {
		return nil, err
	}
	accepted, err := s.jobs.Report(env, report)
	if err != nil {
		return nil, err
	}
	return map[string]any{"accepted": accepted}, nil
}

func (s *Server) currentOwner(owner string) bool {
	c := s.token.Load()
	return !s.closed && c != nil && c.owner == owner && s.svc.SettingValue("http.token") == c.value
}

func (s *Server) configureJobResources(ms *mcp.Server) {
	ms.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: jobPrefix + "{job_id}", Name: "job", MIMEType: "application/json",
		Description: "Private, volatile observation of an explicitly exported asynchronous SUMP call. Not a durable task."},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			if req.Extra == nil || req.Extra.TokenInfo == nil {
				return nil, errors.New("authenticated principal required")
			}
			owner := req.Extra.TokenInfo.UserID
			uri := req.Params.URI
			id := strings.TrimPrefix(uri, jobPrefix)
			if id == uri || id == "" || len(id) > 128 || strings.ContainsAny(id, "/?#%\\") {
				return nil, errors.New("invalid job URI")
			}
			s.mu.Lock()
			valid := s.currentOwner(owner)
			s.mu.Unlock()
			if !valid {
				return nil, errors.New("credential revoked")
			}
			snapshot, err := s.jobs.Read(owner, id)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(snapshot)
			if err != nil {
				return nil, errors.New("cannot encode observation")
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(raw)}}}, nil
		})
}

// Control requests use a small independent budget. A principal with its two
// running timers must still be able to read or cancel them.
func (s *Server) cancelTimer(ctx context.Context, owner string, b Binding, id string) *mcp.CallToolResult {
	s.mu.Lock()
	if !s.currentOwner(owner) || !s.available(b) || s.controls >= 4 {
		s.mu.Unlock()
		return s.failure("rejected", "cancellation unavailable, unauthorized or busy")
	}
	s.controls++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.controls--; s.mu.Unlock() }()
	target, err := s.jobs.LookupCancel(owner, id)
	if err != nil {
		return s.failure("rejected", err.Error())
	}
	if target.Service != "timer" || target.Tool != "timer__sleep" || target.ItemID == "" {
		return s.failure("unsupported", "this observation has no cancellable timer receipt")
	}
	callCtx, cancel := context.WithTimeout(ctx, min(s.cfg.CallTimeout, 5*time.Second))
	stop := context.AfterFunc(s.lifetime, cancel)
	defer func() { stop(); cancel() }()
	raw, err := s.svc.Call(callCtx, "timer", "cancel", protocol.TimerCancelPayload{ItemID: target.ItemID}, target.Trace, "mcp")
	if err != nil {
		return s.failure("unknown", "cancellation not confirmed; native timer may continue")
	}
	var result protocol.TimerCancelResult
	if decodeMessagePack(raw, &result) != nil || result.ItemID != target.ItemID || (result.Status != "canceled" && result.Status != "unknown") {
		s.svc.Report("internal", "MCP timer adapter received an incompatible cancellation result")
		return s.failure("unknown", "invalid cancellation receipt; native outcome unknown")
	}
	if result.Status == "canceled" {
		_ = s.jobs.ConfirmCanceled(owner, id)
	}
	return s.result(map[string]any{"job_id": id, "status": result.Status}, false)
}
