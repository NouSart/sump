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
// 「取号 + 递增」在同一个事务里完成：插入或原子自增后读回，同一会话的并发调用
// 不会拿到同一个号。不同会话各有一行计数器，互不影响。
func (s *Store) NextSeq(ctx context.Context, conversationID string) (int64, error) {
	if conversationID == "" {
		return 0, errors.New("NextSeq: conversation_id 必填")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("取会话 %s 的 seq（开启事务）: %w", conversationID, err)
	}
	defer func() { _ = tx.Rollback() }()

	// 首次调用插入 1；已有计数器的会话原子 +1；RETURNING 在同一条语句里读回新值。
	var seq int64
	err = tx.QueryRowContext(ctx, `INSERT INTO seq_counters(conversation_id, last_seq) VALUES(?, 1)
		ON CONFLICT(conversation_id) DO UPDATE SET last_seq = last_seq + 1
		RETURNING last_seq`, conversationID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("取会话 %s 的 seq: %w", conversationID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交会话 %s 的 seq: %w", conversationID, err)
	}
	return seq, nil
}
