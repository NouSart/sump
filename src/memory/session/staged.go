package session

// 整理池属于会话私有库：experience 通过 SPv2 向本服务取候选 / 回写状态，
// 不直接打开会话库。回执用 protocol.StagedItem（跨服务视图），不回传 store 类型。

import (
	"context"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func (s *Server) nativeStagedPending(_ context.Context, call *spv2.Call) error {
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
	var out protocol.StagedPendingResult
	for _, it := range items {
		out.Items = append(out.Items, protocol.StagedItem{
			ID:             it.ID,
			ConversationID: it.ConversationID,
			Role:           it.Role,
			Content:        it.Content,
			Status:         it.Status,
			Attempts:       it.Attempts,
			LastError:      it.LastError,
			CreatedAt:      it.CreatedAt,
		})
	}
	return call.Reply(out)
}

func (s *Server) nativeStagedMark(_ context.Context, call *spv2.Call) error {
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

func (s *Server) nativeStagedRetry(_ context.Context, call *spv2.Call) error {
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
