package server

// 整理池属于会话私有库，但整理动作归 experience。
// 跨进程时 experience 通过 SPv2 向 session 取候选 / 回写状态，不直接打开会话库。

import (
	"context"
	"fmt"

	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

type stagedStore interface {
	PendingStaged(context.Context, int) ([]store.StagedItem, error)
	MarkStaged(context.Context, int64, string, string) error
	RetryStaged(context.Context, int64, string) error
}

type localStaged struct{ st *store.Store }

func (l localStaged) PendingStaged(_ context.Context, limit int) ([]store.StagedItem, error) {
	return l.st.PendingStaged(limit)
}
func (l localStaged) MarkStaged(_ context.Context, id int64, status, lastErr string) error {
	return l.st.MarkStaged(id, status, lastErr)
}
func (l localStaged) RetryStaged(_ context.Context, id int64, lastErr string) error {
	return l.st.RetryStaged(id, lastErr)
}

type remoteStaged struct{ s *Server }

func (r remoteStaged) PendingStaged(ctx context.Context, limit int) ([]store.StagedItem, error) {
	raw, err := r.s.Call(ctx, "session", "staged_pending", map[string]any{"limit": limit}, "")
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []store.StagedItem `msgpack:"items"`
	}
	if err := protocol.DecodeRaw(raw, &out); err != nil {
		return nil, fmt.Errorf("staged_pending 回执无法解码: %w", err)
	}
	return out.Items, nil
}

func (r remoteStaged) MarkStaged(ctx context.Context, id int64, status, lastErr string) error {
	_, err := r.s.Call(ctx, "session", "staged_mark", map[string]any{
		"id": id, "status": status, "last_error": lastErr,
	}, "")
	return err
}

func (r remoteStaged) RetryStaged(ctx context.Context, id int64, lastErr string) error {
	_, err := r.s.Call(ctx, "session", "staged_retry", map[string]any{
		"id": id, "last_error": lastErr,
	}, "")
	return err
}

// staged 返回整理池访问器：本进程有会话库就本地直连，否则走 session。
func (s *Server) stagedStore() stagedStore {
	if s.cfg.Store != nil {
		return localStaged{st: s.cfg.Store}
	}
	return remoteStaged{s: s}
}

// ---------- session 侧内部 method ----------

func (s *Server) nativeStagedPending(ctx context.Context, call *spv2.Call) error {
	var p struct {
		Limit int `msgpack:"limit"`
	}
	if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "staged_pending body 无效: "+err.Error(), false, nil)
	}
	if s.cfg.Store == nil {
		return call.ReplyError(spv2.CodeNotFound, "会话库未启用", false, nil)
	}
	items, err := s.cfg.Store.PendingStaged(p.Limit)
	if err != nil {
		return call.ReplyError(spv2.CodeInternal, "读取整理池失败: "+err.Error(), true, nil)
	}
	return call.Reply(map[string]any{"items": items})
}

func (s *Server) nativeStagedMark(ctx context.Context, call *spv2.Call) error {
	var p struct {
		ID      int64  `msgpack:"id"`
		Status  string `msgpack:"status"`
		LastErr string `msgpack:"last_error"`
	}
	if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "staged_mark body 无效: "+err.Error(), false, nil)
	}
	if s.cfg.Store == nil {
		return call.ReplyError(spv2.CodeNotFound, "会话库未启用", false, nil)
	}
	if err := s.cfg.Store.MarkStaged(p.ID, p.Status, p.LastErr); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "标记整理池失败: "+err.Error(), false, nil)
	}
	return call.Reply(map[string]any{"ok": true})
}

func (s *Server) nativeStagedRetry(ctx context.Context, call *spv2.Call) error {
	var p struct {
		ID      int64  `msgpack:"id"`
		LastErr string `msgpack:"last_error"`
	}
	if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "staged_retry body 无效: "+err.Error(), false, nil)
	}
	if s.cfg.Store == nil {
		return call.ReplyError(spv2.CodeNotFound, "会话库未启用", false, nil)
	}
	if err := s.cfg.Store.RetryStaged(p.ID, p.LastErr); err != nil {
		return call.ReplyError(spv2.CodeInternal, "重试整理池失败: "+err.Error(), true, nil)
	}
	return call.Reply(map[string]any{"ok": true})
}
