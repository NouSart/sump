package store

// 上下文快照与触发去重（S-001 §3.1 / §3.1.1 / §4.4）。
//
// 两个事实必须分开存，否则「重复触发」与「过期清理」会互相污染：
//   - context_snapshots：一次组装结果的**不可变投影**（压缩后的载荷），有保留期；
//   - trigger_states：这轮触发走到哪一步（pending → assembled → completed），
//     `completed` 记录**不随快照清理删除**——快照没了也不能把「已经做完」退化成「没做」。
//
// 过期语义只在这里判定一次：读取方拿到 ErrSnapshotExpired 后必须显式回 not_found，
// 不允许静默重建（§3.1 规则 2 的 expired 行）。
//
// 读路径不带 `work_id=''` 兜底（R-014 T4）：版本闸门是「不迁移、重置」，重置后的库里
// 不存在 v2 之前的行；为终态里不该存在的数据形态写永久兼容逻辑，就是把兼容层固化成设计。

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// 快照 / 触发状态相关的哨兵错误：用 errors.Is 判定，调用方据此映射线上错误码。
var (
	// ErrWorkNotFound 表示 work 行不存在（TaskIDOfWork 等读 work 的入口共用）。
	// 同一个概念只留一个名字：P3 的 call 账本也复用它，不另立别名。
	ErrWorkNotFound = errors.New("work 不存在")
	// ErrSnapshotNotFound 表示该 (work, trigger) 没有快照。
	ErrSnapshotNotFound = errors.New("上下文快照不存在")
	// ErrSnapshotExpired 表示快照存在但已过保留期；不得当成「没生成过」而重建。
	ErrSnapshotExpired = errors.New("上下文快照已过期")
	// ErrSnapshotConflict 表示同一 (work, trigger_kind, trigger_item) 已有快照（首次并发触发）。
	ErrSnapshotConflict = errors.New("同一触发的上下文快照已存在")
	// ErrSnapshotTooLarge 表示单份投影或已存总量超过容量上限（S-001 §4.4）。
	// 调用方必须显式报 invalid_argument：不截断、不静默降级。
	ErrSnapshotTooLarge = errors.New("上下文快照超过容量上限")
	// ErrTriggerConflict 表示当前触发状态不允许该操作（如对 pending 调 trigger_complete）。
	ErrTriggerConflict = errors.New("触发状态不允许该操作")
)

// 快照容量（S-001 §4.4）：单份投影上限 1 MiB；已存总量默认 2 GiB。
const (
	// SnapshotMaxPayloadBytes 是单份快照投影（**未压缩** payload）的上限：1 MiB。
	// 判定在压缩之前；超限直接拒绝，不截断。
	SnapshotMaxPayloadBytes = 1 << 20
	// DefaultSnapshotTotalLimit 是已存快照总量默认上限：2 GiB。
	// 计量口径是落盘的 gzip 后 payload（length(payload) 之和），不是明文投影。
	DefaultSnapshotTotalLimit int64 = 2 << 30
)

// triggerState 是 trigger_states.state 的取值（S-001 §3.9：pending / assembled / completed / expired）。
const (
	TriggerStatePending   = "pending"
	TriggerStateAssembled = "assembled"
	TriggerStateCompleted = "completed"
	TriggerStateExpired   = "expired"
)

// MessageRow 是 messages 表的一行投影（按 work / task 查上下文用）。
// ItemID 是 messages.id 的十进制字符串：外部只会看到这个稳定标识（S-001 §3.1 的 trigger.item）。
type MessageRow struct {
	ItemID     string
	Role       string
	Kind       string
	Content    string
	Meta       string
	WorkID     string
	Visibility string
	Seq        int64
}

// ImageRow / ListImageIndex 不搬：新架构没有 image_index 表（R-023 D7），
// 图片通道归 images 服务；在没有消费者的阶段建表 = 第二份真相。

// TriggerItemExists 判断 trigger.item 是否已作为消息落库、且属于本 work。
//
// item 是 messages.id 的十进制字符串（INTEGER 自增）。归属判定只有一条：消息的
// work_id 是本 work。work_id 为空字符串的老记录不再兜底（R-014 T4）：版本闸门是重置，
// 重置后的库里不该再有这种行，放宽读路径等于给兼容层留下永久入口。
// 非十进制 / 负数一律视为「不存在」，不把解析失败升级成服务端错误。
func (s *Store) TriggerItemExists(ctx context.Context, work, item string) (bool, error) {
	id, err := strconv.ParseInt(item, 10, 64)
	if err != nil || id <= 0 {
		return false, nil
	}
	var n int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE id = ? AND work_id = ?`,
		id, work).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("检查 trigger.item %s 是否属于 work %s: %w", item, work, err)
	}
	return n > 0, nil
}

// GetTriggerState 读触发状态；没有记录时返回 ("", "", nil)（调用方把空串按 pending 处理）。
func (s *Store) GetTriggerState(ctx context.Context, work, kind, item string) (state, snapshotID string, err error) {
	var snapshot sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT state, snapshot_id FROM trigger_states
		 WHERE work = ? AND trigger_kind = ? AND trigger_item = ?`,
		work, kind, item).Scan(&state, &snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("读取触发状态（work=%s kind=%s item=%s）: %w", work, kind, item, err)
	}
	return state, snapshot.String, nil
}

// SetTriggerState upsert 触发状态（首次写 pending/assembled，后续推进）。
// completed_at 只由 state=completed 的写入设置；其余状态不篡改它。
func (s *Store) SetTriggerState(ctx context.Context, work, kind, item, state, snapshotID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var snap any
	if snapshotID != "" {
		snap = snapshotID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO trigger_states(work, trigger_kind, trigger_item, state, snapshot_id, completed_at, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(work, trigger_kind, trigger_item) DO UPDATE SET
		   state       = excluded.state,
		   snapshot_id = excluded.snapshot_id,
		   completed_at = CASE WHEN excluded.state = 'completed' THEN excluded.updated_at
		                       ELSE trigger_states.completed_at END,
		   updated_at  = excluded.updated_at`,
		work, kind, item, state, snap,
		completedAtFor(state, now), now, now)
	if err != nil {
		return fmt.Errorf("写入触发状态（work=%s kind=%s item=%s state=%s）: %w", work, kind, item, state, err)
	}
	return nil
}

// completedAtFor 返回插入时的 completed_at：只有 completed 状态落时间，其余为 NULL。
func completedAtFor(state, now string) any {
	if state == TriggerStateCompleted {
		return now
	}
	return nil
}

// CompleteTrigger 原子地把触发从 assembled 置为 completed（S-001 §3.1.1）。
//
// 幂等：已经是 completed 时直接成功；pending / expired / 没有记录都返回 ErrTriggerConflict。
// 原子性靠单条带 state='assembled' 条件的 UPDATE：并发的两次调用只有一次能改成功，
// 另一次在回读时看到 completed，按幂等成功返回。
func (s *Store) CompleteTrigger(ctx context.Context, work, kind, item string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx,
		`UPDATE trigger_states SET state = ?, completed_at = ?, updated_at = ?
		 WHERE work = ? AND trigger_kind = ? AND trigger_item = ? AND state = ?`,
		TriggerStateCompleted, now, now, work, kind, item, TriggerStateAssembled)
	if err != nil {
		return fmt.Errorf("完成触发（work=%s kind=%s item=%s）: %w", work, kind, item, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	// 没改到：要么已经 completed（幂等成功），要么还停在 pending/expired/不存在（冲突）。
	state, _, err := s.GetTriggerState(ctx, work, kind, item)
	if err != nil {
		return err
	}
	if state == TriggerStateCompleted {
		return nil
	}
	if state == "" {
		return fmt.Errorf("触发（work=%s kind=%s item=%s）尚未组装，不能标记完成: %w", work, kind, item, ErrTriggerConflict)
	}
	return fmt.Errorf("触发（work=%s kind=%s item=%s）当前状态 %s，不能标记完成: %w", work, kind, item, state, ErrTriggerConflict)
}

// SaveSnapshot 保存一份快照：payload 先 gzip 压缩再写 BLOB（§4.4），不可变投影。
//
// 容量语义（S-001 §4.4）：
//   - 单份投影（未压缩 payload）> SnapshotMaxPayloadBytes（1 MiB）→ ErrSnapshotTooLarge；
//   - 已存总量（gzip 后 payload 之和）> Store.snapshotTotalLimit → ErrSnapshotTooLarge；
//   - 冲突（同一触发已有快照）→ ErrSnapshotConflict，且不会留下本次写入 / 容量改动。
//
// 单条判定在压缩前；总量判定在事务内（插入之后统计），所以「先写后判、失败回滚」
// 不会出现「超限行已落库」的中间状态。并发首触发由唯一约束决定谁赢。
func (s *Store) SaveSnapshot(ctx context.Context, snapshotID, work, kind, item string, payload []byte, schemaName string, createdAt, expiresAt time.Time) error {
	if len(payload) > SnapshotMaxPayloadBytes {
		return fmt.Errorf("保存上下文快照 %s（work=%s trigger=%s/%s）: 投影 %d 字节超过单条上限 %d 字节: %w",
			snapshotID, work, kind, item, len(payload), SnapshotMaxPayloadBytes, ErrSnapshotTooLarge)
	}
	raw, err := encodeSnapshotPayload(snapshotID, payload)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启上下文快照保存事务 %s: %w", snapshotID, err)
	}
	// 成功提交后 Rollback 返回 ErrTxDone，可以直接忽略；失败路径统一由它收尾。
	defer func() { _ = tx.Rollback() }()

	inserted, err := insertSnapshotRow(ctx, tx, snapshotID, work, kind, item, raw, schemaName, createdAt, expiresAt)
	if err != nil {
		return err
	}
	if !inserted {
		return fmt.Errorf("保存上下文快照 %s（work=%s kind=%s item=%s）: %w",
			snapshotID, work, kind, item, ErrSnapshotConflict)
	}
	total, err := snapshotTotalBytes(ctx, tx)
	if err != nil {
		return err
	}
	if total > s.snapshotTotalLimit {
		return fmt.Errorf("保存上下文快照 %s（work=%s trigger=%s/%s）: 已存总量 %d 字节超过上限 %d 字节: %w",
			snapshotID, work, kind, item, total, s.snapshotTotalLimit, ErrSnapshotTooLarge)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交上下文快照 %s: %w", snapshotID, err)
	}
	return nil
}

// encodeSnapshotPayload 是快照 gzip 压缩的唯一实现（decodeSnapshotPayload 的逆操作）。
// 压缩失败一律报错：不能把「压不成」当成「没有正文」继续写空载荷。
func encodeSnapshotPayload(snapshotID string, payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		_ = zw.Close()
		return nil, fmt.Errorf("压缩上下文快照 %s: %w", snapshotID, err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("压缩上下文快照 %s: %w", snapshotID, err)
	}
	return buf.Bytes(), nil
}

// insertSnapshotRow 在事务里写一行快照；false = 同一触发已存在（唯一约束命中）。
func insertSnapshotRow(ctx context.Context, tx *sql.Tx, snapshotID, work, kind, item string, raw []byte, schemaName string, createdAt, expiresAt time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO context_snapshots(snapshot_id, work, trigger_kind, trigger_item, payload, schema, created_at, expires_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT DO NOTHING`,
		snapshotID, work, kind, item, raw, schemaName,
		createdAt.UTC().Format(time.RFC3339Nano), expiresAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, fmt.Errorf("保存上下文快照 %s: %w", snapshotID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// snapshotTotalBytes 统计已存快照的 gzip 后字节总量（S-001 §4.4 的计量口径）。
func snapshotTotalBytes(ctx context.Context, tx *sql.Tx) (int64, error) {
	var total int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(length(payload)), 0) FROM context_snapshots`).Scan(&total); err != nil {
		return 0, fmt.Errorf("统计上下文快照总量: %w", err)
	}
	return total, nil
}

// LoadSnapshot 读一份快照并解压。过期返回 ErrSnapshotExpired（不返回载荷）；
// 没有快照返回 ErrSnapshotNotFound。过期判定只比较 DB 里的 expires_at，不猜 trigger 状态。
func (s *Store) LoadSnapshot(ctx context.Context, work, kind, item string) (snapshotID string, payload []byte, expiresAt time.Time, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT snapshot_id, payload, expires_at FROM context_snapshots
		 WHERE work = ? AND trigger_kind = ? AND trigger_item = ?`,
		work, kind, item)
	return scanSnapshotRow(row, fmt.Sprintf("work=%s kind=%s item=%s", work, kind, item))
}

// LoadSnapshotByID 按主键（snapshot_id）读一份快照，且要求它属于 work。
//
// transcript 用它：文本必须对应调用方点名的那份投影，不能退化成「这个 work 的最新快照」。
// 归属条件写进同一条 SELECT（不是读出来再比）：别人的 snapshot_id 在这里与「不存在」同形，
// 不泄漏它是否存在。过期 / 缺失沿用 ErrSnapshotExpired / ErrSnapshotNotFound。
func (s *Store) LoadSnapshotByID(ctx context.Context, work, snapshotID string) (snapshotIDOut string, payload []byte, expiresAt time.Time, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT snapshot_id, payload, expires_at FROM context_snapshots
		 WHERE snapshot_id = ? AND work = ?`, snapshotID, work)
	return scanSnapshotRow(row, fmt.Sprintf("work=%s snapshot_id=%s", work, snapshotID))
}

// scanSnapshotRow 把一行快照收成 (id, 明文载荷, 过期时间)：缺失 / 过期用既有哨兵错误，
// 解压失败原样返回错误（不得当成空载荷）。rowScanner 与 calls.go 共用（同包）。
func scanSnapshotRow(row rowScanner, by string) (snapshotID string, payload []byte, expiresAt time.Time, err error) {
	var raw []byte
	var expires string
	queryErr := row.Scan(&snapshotID, &raw, &expires)
	if errors.Is(queryErr, sql.ErrNoRows) {
		return "", nil, time.Time{}, fmt.Errorf("读取上下文快照（%s）: %w", by, ErrSnapshotNotFound)
	}
	if queryErr != nil {
		return "", nil, time.Time{}, fmt.Errorf("读取上下文快照（%s）: %w", by, queryErr)
	}
	exp, parseErr := parseSnapshotTime(expires)
	if parseErr != nil {
		return snapshotID, nil, time.Time{}, fmt.Errorf("解析快照 %s 的 expires_at %q: %w", snapshotID, expires, parseErr)
	}
	if time.Now().After(exp) {
		return snapshotID, nil, exp, fmt.Errorf("上下文快照 %s 已过期（expires_at=%s）: %w",
			snapshotID, expires, ErrSnapshotExpired)
	}
	plain, decErr := decodeSnapshotPayload(snapshotID, raw)
	if decErr != nil {
		return snapshotID, nil, exp, decErr
	}
	return snapshotID, plain, exp, nil
}

// decodeSnapshotPayload 是快照 gzip 解压的唯一实现（SaveSnapshot 的逆操作）。
// 所有读快照的入口都必须经它：坏载荷一律报错，不得静默返回空文本。
func decodeSnapshotPayload(snapshotID string, raw []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("解压上下文快照 %s: %w", snapshotID, err)
	}
	defer func() { _ = zr.Close() }()
	plain, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("解压上下文快照 %s: %w", snapshotID, err)
	}
	return plain, nil
}

// parseSnapshotTime 解析快照时间：写入侧统一 RFC3339Nano，兼容只有秒精度的老值。
func parseSnapshotTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, v)
}

// RecentMessagesByWork 取某 work 的最近消息（按 seq 升序返回），供 context_base 组装。
//
// 归属规则（S-001 §4.2 / §5.2）：只取 work_id = work 的消息。不按 task 兜底
// `work_id` 为空字符串的老记录（R-014 T4）：那条路会把别的 work 的历史混进来。
// limit<=0 用默认 50：先按 seq 倒序取最近 N 条，再翻正，保证「窗口」是最近的而不是最旧的。
func (s *Store) RecentMessagesByWork(ctx context.Context, work string, limit int) ([]MessageRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, role, kind, content, meta, work_id, visibility, seq FROM messages
		 WHERE work_id = ?
		 ORDER BY seq DESC, id DESC LIMIT ?`,
		work, limit)
	if err != nil {
		return nil, fmt.Errorf("查询 work %s 的消息: %w", work, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]MessageRow, 0, limit)
	for rows.Next() {
		var id int64
		var m MessageRow
		if err := rows.Scan(&id, &m.Role, &m.Kind, &m.Content, &m.Meta, &m.WorkID, &m.Visibility, &m.Seq); err != nil {
			return nil, err
		}
		m.ItemID = strconv.FormatInt(id, 10)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 倒序 → 正序：组装侧按 Seq 升序渲染，这里先给调用方一个符合直觉的顺序。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// TaskIDOfWork 读 work 所属的任务 id（works.task，S-001 §4.1）。
// work 不存在时返回 ErrWorkNotFound；task 为空（老 work）由调用方按「找不到任务」处理。
func (s *Store) TaskIDOfWork(ctx context.Context, work string) (string, error) {
	var task string
	err := s.db.QueryRowContext(ctx, `SELECT task FROM works WHERE id = ?`, work).Scan(&task)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("work %s: %w", work, ErrWorkNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("读取 work %s 的任务: %w", work, err)
	}
	return task, nil
}
