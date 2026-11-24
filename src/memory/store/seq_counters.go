package store

// 会话内序号（S-001 §3.0 规则 2、§4.7）：seq 由 seq_counters 在事务内分配，
// 不用时间戳——时间戳在并发/时钟回拨下会重号，序号不会。

import (
	"context"
	"errors"
	"fmt"
)

// NextSeq 为会话分配下一个单调递增的序号（从 1 开始）。
//
// 「播种 + 推进 + 读回」走 allocSeqVia（与 v2 原子落库同一个实现，S-001 §4.7）：
// 同一会话不会拿到同一个号，也不会落后于 messages 里已有的最大 seq。
func (s *Store) NextSeq(ctx context.Context, conversationID string) (int64, error) {
	if conversationID == "" {
		return 0, errors.New("NextSeq: conversation_id 必填")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("取会话 %s 的 seq（开启事务）: %w", conversationID, err)
	}
	defer func() { _ = tx.Rollback() }()

	seq, err := allocSeqVia(ctx, tx, conversationID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交会话 %s 的 seq: %w", conversationID, err)
	}
	return seq, nil
}
