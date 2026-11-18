package experience

// 整理池的远程访问：整理池属于会话私有库，本服务不持有会话库。
// 跨进程时通过 SPv2 向 session 取候选 / 回写状态，item 用 protocol.StagedItem。

import (
	"context"
	"fmt"

	"github.com/NouSart/sump/protocol"
)

type stagedStore interface {
	PendingStaged(context.Context, int) ([]protocol.StagedItem, error)
	MarkStaged(context.Context, int64, string, string) error
	RetryStaged(context.Context, int64, string) error
}

type remoteStaged struct{ s *Server }

func (r remoteStaged) PendingStaged(ctx context.Context, limit int) ([]protocol.StagedItem, error) {
	raw, err := r.s.Call(ctx, "session", "staged_pending", map[string]any{"limit": limit}, "")
	if err != nil {
		return nil, err
	}
	var out protocol.StagedPendingResult
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
