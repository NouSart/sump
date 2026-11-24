package store

// 幂等落库记录（S-001 §3.0 / §4.3）：
//
//   - store_receipts 用 (work, client_item_id) 做主键，记下这次落库用的 payload_hash 与
//     落库记录的 item_id；同键同 hash = 重放（不回写、回既有回执），同键不同 hash = conflict。
//   - report_receipts 用 (work, source_service, report_id) 做主键，另外用 uq_report_seq
//     拦住「换 report_id 重放同一 (call_id, report_seq)」——只防 report_id 防不住换 ID 重放。
//
// 这里只做「查 + 占位/插入」，不与 messages 的写入共用一个事务（RecordV2 是另一个入口）：
// 所以 PutStoreReceipt 支持两阶段用法（itemID 为空先占位，落库后再带真实 itemID 回填），
// 让「先查后写」的调用顺序下也能做到重放不重复落消息。调用方（server/store）用一把
// 进程内互斥锁把同键请求串起来，避免两个阶段之间被同键请求插空。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrPayloadConflict 表示同一个幂等键已经用**不同**的载荷落过库。
//
// 这不是可重试的竞态，而是调用方两次说了不一样的话（S-001 §3.0 规则 3、§6.2）：
// 同 report_id 不同载荷 / 同 (call_id, report_seq) 不同 report_id 都归到这里，交人工处置。
var ErrPayloadConflict = errors.New("幂等键已存在但载荷不同")

// PutStoreReceipt 写入 / 查询一条 store 幂等记录（S-001 §3.0 规则 3）。
//
// 返回：
//   - existing：已存在且同 hash 时，先前落库记录的 item_id；
//   - deduped：true 表示同 (work, client_item_id) 同 payload_hash 的重放，调用方直接回既有回执；
//   - ErrPayloadConflict：同键但 hash 不同。
//
// 两阶段用法（server/store 的新路径）：
//  1. 落消息前用 itemID="" 调一次：不存在就插入一条 item_id 为空的占位行（返回 "", false）；
//     若上次在「占位 → 落消息」之间崩过，会看到 item_id 为空的同 hash 行，返回 "", false
//     让调用方继续落消息（崩溃重放的恢复路径）；
//  2. 消息落库后用真实 itemID 再调一次：把占位行的 item_id 回填（返回 "", false）。
//
// 常规单次调用（itemID 非空、行不存在）就是「查不到 → 插入」。
func (s *Store) PutStoreReceipt(ctx context.Context, work, clientItemID, payloadHash, itemID string) (existing string, deduped bool, err error) {
	if strings.TrimSpace(work) == "" || strings.TrimSpace(clientItemID) == "" {
		return "", false, errors.New("PutStoreReceipt: work 与 client_item_id 必填")
	}
	if strings.TrimSpace(payloadHash) == "" {
		return "", false, errors.New("PutStoreReceipt: payload_hash 必填")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("store 幂等记录（开启事务）: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		storedHash string
		storedItem string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT payload_hash, item_id FROM store_receipts WHERE work = ? AND client_item_id = ?`,
		work, clientItemID,
	).Scan(&storedHash, &storedItem)
	switch {
	case err == nil:
		if storedHash != payloadHash {
			return "", false, ErrPayloadConflict
		}
		if storedItem == "" {
			// 上次只完成了占位：还没落消息就继续，落了消息就把 item_id 回填。
			if itemID == "" {
				return "", false, nil
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE store_receipts SET item_id = ? WHERE work = ? AND client_item_id = ? AND item_id = ''`,
				itemID, work, clientItemID); err != nil {
				return "", false, fmt.Errorf("回填 store 幂等记录的 item_id: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return "", false, fmt.Errorf("提交 store 幂等记录: %w", err)
			}
			return "", false, nil
		}
		// 同键同 hash 的重放：既有回执就是答案（item_id 以先落库的那条为准）。
		if err := tx.Commit(); err != nil {
			return "", false, fmt.Errorf("提交 store 幂等记录: %w", err)
		}
		return storedItem, true, nil
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO store_receipts(work, client_item_id, payload_hash, item_id, created_at) VALUES(?,?,?,?,?)`,
			work, clientItemID, payloadHash, itemID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return "", false, fmt.Errorf("写入 store 幂等记录: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return "", false, fmt.Errorf("提交 store 幂等记录: %w", err)
		}
		return "", false, nil
	default:
		return "", false, fmt.Errorf("查询 store 幂等记录: %w", err)
	}
}

// PutReportReceipt 写入 / 查询一条补报幂等记录（S-001 §3.0 规则 6 / §4.3）。
//
// 返回 deduped=true 表示同 (work, source_service, report_id) 同 payload_hash 的重放，未重复入账。
// 两种冲突都回 ErrPayloadConflict：
//   - 同 report_id 但 payload_hash 不同（同一次补报换了内容）；
//   - 同 (work, source_service, call_id, report_seq) 但 report_id 不同（换 ID 重放，被 uq_report_seq 拦住）。
func (s *Store) PutReportReceipt(ctx context.Context, work, source, reportID, payloadHash, itemID, callID string, seq int64) (deduped bool, err error) {
	if strings.TrimSpace(work) == "" || strings.TrimSpace(source) == "" || strings.TrimSpace(reportID) == "" {
		return false, errors.New("PutReportReceipt: work / source_service / report_id 必填")
	}
	if strings.TrimSpace(payloadHash) == "" {
		return false, errors.New("PutReportReceipt: payload_hash 必填")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("补报幂等记录（开启事务）: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// ON CONFLICT DO NOTHING 一次盖住主键与 uq_report_seq 两种唯一冲突；插没插进去再看原因。
	res, err := tx.ExecContext(ctx,
		`INSERT INTO report_receipts(work, source_service, report_id, payload_hash, item_id, call_id, report_seq, created_at)
		 VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		work, source, reportID, payloadHash, itemID, callID, seq, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, fmt.Errorf("写入补报幂等记录: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("补报幂等记录写入结果: %w", err)
	}
	if n == 1 {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("提交补报幂等记录: %w", err)
		}
		return false, nil
	}
	// 没插进去：同 report_id 的重放（同 hash → deduped；不同 hash → conflict），
	// 或者主键没撞、是 uq_report_seq 被别的 report_id 占了（换 ID 重放 → conflict）。
	var storedHash string
	err = tx.QueryRowContext(ctx,
		`SELECT payload_hash FROM report_receipts WHERE work = ? AND source_service = ? AND report_id = ?`,
		work, source, reportID,
	).Scan(&storedHash)
	switch {
	case err == nil:
		if storedHash != payloadHash {
			return false, ErrPayloadConflict
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("提交补报幂等记录: %w", err)
		}
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrPayloadConflict
	default:
		return false, fmt.Errorf("查询补报幂等记录: %w", err)
	}
}

// MessageByItemID 按 store 回执里的 item_id 取该记录的会话内 seq。
//
// item_id 是 messages.id 的十进制字符串（见 RecordV2 的说明）。用于 deduped 重放时
// 把既有回执里的 seq 原样还给调用方——不能拿本次请求的 seq 顶替，否则回执不可信。
func (s *Store) MessageByItemID(ctx context.Context, itemID string) (seq int64, err error) {
	if strings.TrimSpace(itemID) == "" {
		return 0, errors.New("MessageByItemID: item_id 必填")
	}
	id, err := strconv.ParseInt(itemID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("MessageByItemID: item_id %q 不是消息 id: %w", itemID, err)
	}
	err = s.db.QueryRowContext(ctx, `SELECT seq FROM messages WHERE id = ?`, id).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("MessageByItemID: 消息 %s 不存在: %w", itemID, sql.ErrNoRows)
	}
	if err != nil {
		return 0, fmt.Errorf("读取消息 %s 的 seq: %w", itemID, err)
	}
	return seq, nil
}
