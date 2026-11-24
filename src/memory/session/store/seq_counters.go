package store

// 会话内序号（S-001 §3.0 规则 2、§4.7）：seq 由 seq_counters 在事务内分配，
// 不用时间戳——时间戳在并发/时钟回拨下会重号，序号不会。

import (
	"context"
	"fmt"
)

// allocSeqVia 在同一句柄内取该会话的下一个序号（ex 可以是库句柄或事务句柄）。
//
// 两条边界：
//   - 计数器行不存在时，按 messages 里已有的最大 seq 播种（老写入路径不占号，不能从 1 重号）；
//   - 取号取「计数器 +1」与「表内最大 seq +1」的较大值，保证不早于任何已落库的事实。
//
// 唯一真相在 seq_counters（S-001 §4.7）：调用方传来的序号不参与分配，
// 否则两个写入方可以约定同一个号。
func allocSeqVia(ctx context.Context, ex execer, conversationID string) (int64, error) {
	if _, err := ex.ExecContext(ctx,
		`INSERT OR IGNORE INTO seq_counters(conversation_id, last_seq)
		 SELECT ?, COALESCE(MAX(seq), 0) FROM messages WHERE conversation_id = ?`,
		conversationID, conversationID); err != nil {
		return 0, fmt.Errorf("播种会话 %s 的 seq 计数器: %w", conversationID, err)
	}
	if _, err := ex.ExecContext(ctx,
		`UPDATE seq_counters SET last_seq = max(last_seq + 1,
			COALESCE((SELECT MAX(seq) FROM messages WHERE conversation_id = seq_counters.conversation_id), 0) + 1)
		 WHERE conversation_id = ?`, conversationID); err != nil {
		return 0, fmt.Errorf("推进会话 %s 的 seq 计数器: %w", conversationID, err)
	}
	var seq int64
	if err := ex.QueryRowContext(ctx,
		`SELECT last_seq FROM seq_counters WHERE conversation_id = ?`, conversationID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("读回会话 %s 的 seq: %w", conversationID, err)
	}
	return seq, nil
}
