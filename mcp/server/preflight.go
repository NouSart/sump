package server

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// ImportCheck describes connectivity and the reviewed contract only. Success
// does not prove a tool's business behavior or authorize any future invocation.
type ImportCheck struct {
	Remote  string `json:"remote"`
	Action  string `json:"action"`
	Tool    string `json:"tool"`
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Check contacts configured remotes using the exact runtime trust, credentials
// and schema checks. It never sends tools/call. The complete check has one
// timeout budget; at most four discoveries run together, and each also consumes
// a target's normal slot. Neither capacity pressure nor network failure retries
// a check. Results remain in action order, independent of completion order.
func (i *Imports) Check(ctx context.Context) []ImportCheck {
	ctx, cancel := context.WithTimeout(ctx, i.timeout)
	stop := context.AfterFunc(i.lifetime, cancel)
	defer func() { stop(); cancel() }()
	actions := make([]string, 0, len(i.tools))
	for action := range i.tools {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	out := make([]ImportCheck, len(actions))
	queue := make(chan int, len(actions))
	for n := range actions {
		queue <- n
	}
	close(queue)
	var wg sync.WaitGroup
	for range min(4, len(actions)) {
		wg.Go(func() {
			for n := range queue {
				out[n] = i.checkImport(ctx, i.tools[actions[n]])
			}
		})
	}
	wg.Wait()
	return out
}

func (i *Imports) checkImport(ctx context.Context, tool importedTool) ImportCheck {
	result := ImportCheck{Remote: tool.config.Remote, Action: tool.config.Action, Tool: tool.config.Tool}
	fail := func(err error) ImportCheck {
		// All errors below are locally constructed messages, never SDK response
		// text, URLs or credentials supplied by the remote.
		result.Code, result.Message, _ = strings.Cut(err.Error(), ": ")
		return result
	}
	if ctx.Err() != nil {
		result.Code, result.Message = "MCP_CHECK_CANCELLED", "preflight deadline or cancellation; no tool submitted"
		return result
	}
	target, token, err := i.reserveTarget(tool)
	if err != nil {
		return fail(err)
	}
	defer func() { <-target.slots }()
	session, err := i.connectChecked(ctx, target, tool, token)
	if err != nil {
		return fail(err)
	}
	defer session.Close()
	if ctx.Err() != nil || i.closed.Load() || token != i.token(target.config.SecretKey()) {
		result.Code, result.Message = "MCP_CHECK_CANCELLED", "preflight cancelled or credential changed; no tool submitted"
		return result
	}
	result.OK, result.Code, result.Message = true, "MCP_CHECK_OK", "connection and reviewed contract verified; no tool submitted"
	return result
}
