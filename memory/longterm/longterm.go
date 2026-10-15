// Package longterm 是记忆服务的长期记忆库（SQLite）：条目写入 / 软删 / 读取。
// 与 store（会话库）**共享同一个 *sql.DB 句柄**（同进程同库——将来跨表事务才有原子性）。
// 检索（bigram 打分）见 rank.go：纯函数，不碰库。
package longterm

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Entry 是一条长期记忆条目。
type Entry struct {
	ID             int64
	Kind           string
	Content        string
	Priority       int
	ConversationID string
	CreatedAt      string
	UpdatedAt      string

	// Embedding 是派生数据（可丢弃、可重建）：条目的 L2 归一化向量；
	// 为空 = 尚未计算（embedding 服务缺席时留空，后续召回时懒补齐）。
	Embedding      []float32
	EmbeddingModel string // 向量模型标识（换模型后旧向量视为失效）
}

// Store 是长期记忆库。
type Store struct {
	db *sql.DB
}

// Open 校验 / 建立 memories 表（db 由会话库提供——共享句柄是有意为之）。
func Open(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS memories (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	kind            TEXT NOT NULL,
	content         TEXT NOT NULL,
	priority        INTEGER NOT NULL DEFAULT 0,
	source          TEXT NOT NULL DEFAULT '',
	conversation_id TEXT,
	status          TEXT NOT NULL DEFAULT 'active',
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL,
	embedding       BLOB,
	embedding_model TEXT
);
CREATE INDEX IF NOT EXISTS idx_memories_status ON memories(status, priority DESC, id DESC);
`

// migrate 建表 / 加法迁移。
// 现行形状 = 含 status 列；此后只做加法（新增向量列，老库 ALTER 补列）。
// 占位期的 memories 表形状不同（从未写入过）——空表直接重建，非空则拒绝自动迁移。
func (s *Store) migrate() error {
	var name string
	err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='memories'`).Scan(&name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec(schema)
		return err
	case err != nil:
		return err
	}

	cols, err := s.tableColumns()
	if err != nil {
		return err
	}
	if _, ok := cols["status"]; ok {
		// 现行形状：加法迁移——补向量列（派生数据；老库补齐后可正常召回）
		migrations := []struct{ col, stmt string }{
			{"embedding", `ALTER TABLE memories ADD COLUMN embedding BLOB`},
			{"embedding_model", `ALTER TABLE memories ADD COLUMN embedding_model TEXT`},
		}
		for _, m := range migrations {
			if cols[m.col] {
				continue
			}
			if _, err := s.db.Exec(m.stmt); err != nil {
				return err
			}
		}
		return nil
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("memories 表是旧占位形状且有 %d 行数据，无法自动迁移（请人工处理后重试）", n)
	}
	if _, err := s.db.Exec(`DROP TABLE memories`); err != nil {
		return err
	}
	_, err = s.db.Exec(schema)
	return err
}

// tableColumns 返回表当前列名集合。
func (s *Store) tableColumns() (map[string]bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(memories)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var cName, cType string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &cName, &cType, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[cName] = true
	}
	return cols, rows.Err()
}

// Add 写入一条条目；已有相同（kind+content）的 active 条目时幂等跳过。
// source 记录写入方（调用方服务名）。
func (s *Store) Add(kind, content string, priority int, conversationID, source string) (int64, bool, error) {
	kind = strings.TrimSpace(kind)
	content = strings.TrimSpace(content)
	if kind == "" || content == "" {
		return 0, false, errors.New("kind 与 content 必填")
	}
	if priority < 0 {
		priority = 0
	}
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM memories WHERE status='active' AND kind=? AND content=? LIMIT 1`,
		kind, content,
	).Scan(&id)
	switch {
	case err == nil:
		return id, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, err
	}

	now := time.Now().Format(time.RFC3339Nano)
	res, err := s.db.Exec(
		`INSERT INTO memories(kind, content, priority, source, conversation_id, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,'active',?,?)`,
		kind, content, priority, source, nullIfEmpty(conversationID), now, now,
	)
	if err != nil {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	return id, true, err
}

// Forget 软删一条条目（status='deleted'）；返回是否真的删到了。
func (s *Store) Forget(id int64) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE memories SET status='deleted', updated_at=? WHERE id=? AND status='active'`,
		time.Now().Format(time.RFC3339Nano), id,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListActive 返回全部 active 条目（按 id 升序；v0 个人规模全量取出，检索在进程内做）；
// 含派生向量（未计算为 nil）。
func (s *Store) ListActive() ([]Entry, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, content, priority, COALESCE(conversation_id,''), created_at, updated_at,
		        embedding, COALESCE(embedding_model,'')
		 FROM memories WHERE status='active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var blob []byte
		if err := rows.Scan(&e.ID, &e.Kind, &e.Content, &e.Priority, &e.ConversationID, &e.CreatedAt, &e.UpdatedAt, &blob, &e.EmbeddingModel); err != nil {
			return nil, err
		}
		e.Embedding = decodeVector(blob)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetEmbedding 写入条目的派生向量（可丢弃、可重建；不触碰 updated_at——内容未变）。
func (s *Store) SetEmbedding(id int64, vec []float32, model string) error {
	_, err := s.db.Exec(
		`UPDATE memories SET embedding=?, embedding_model=? WHERE id=?`,
		encodeVector(vec), model, id,
	)
	return err
}

// encodeVector / decodeVector：[]float32 ↔ 小端 BLOB（派生数据的存储格式）。
func encodeVector(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

func decodeVector(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
