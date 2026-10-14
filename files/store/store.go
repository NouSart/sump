// Package store 是文件服务的存储层：二进制落盘 + SQLite 元数据（与图片的差异：文件有名字）。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// ErrNotFound 表示文件不存在。
var ErrNotFound = errors.New("文件不存在")

// Meta 是文件的元数据（台账一行，不含二进制本体）。
type Meta struct {
	Filename string
	Mime     string
	Size     int64
}

// Store 是文件库。
type Store struct {
	db  *sql.DB
	dir string
}

// Open 打开（或创建）文件库：元数据在 dbPath，二进制落在 dir。
func Open(dbPath, dir string) (*Store, error) {
	if parent := filepath.Dir(dbPath); parent != "" {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	// SQLite 单写者：串行化连接，避免并发写锁冲突。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, dir: dir}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS files (
	id         TEXT PRIMARY KEY,
	filename   TEXT NOT NULL,
	mime       TEXT NOT NULL,
	path       TEXT NOT NULL,
	size       INTEGER NOT NULL,
	created_at TEXT NOT NULL
);
`

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Save 保存一个文件，返回 id（ULID 一证三用：数据库主键 / 磁盘文件名 / 链上引用）。
func (s *Store) Save(data []byte, filename, mime string) (string, error) {
	id := protocol.NewID()
	path := filepath.Join(s.dir, id)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	_, err := s.db.Exec(
		`INSERT INTO files(id, filename, mime, path, size, created_at) VALUES(?,?,?,?,?,?)`,
		id, filename, mime, path, len(data), time.Now().Format(time.RFC3339Nano),
	)
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return id, nil
}

// Stat 只读元数据：read 动作先判定类型，避免把二进制大文件整个读进内存再拒绝。
func (s *Store) Stat(id string) (Meta, error) {
	var m Meta
	err := s.db.QueryRow(`SELECT filename, mime, size FROM files WHERE id=?`, id).
		Scan(&m.Filename, &m.Mime, &m.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return Meta{}, ErrNotFound
	}
	if err != nil {
		return Meta{}, err
	}
	return m, nil
}

// Load 读取一个文件（二进制 + 元数据）。
func (s *Store) Load(id string) ([]byte, Meta, error) {
	var m Meta
	var path string
	err := s.db.QueryRow(`SELECT filename, mime, size, path FROM files WHERE id=?`, id).
		Scan(&m.Filename, &m.Mime, &m.Size, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Meta{}, ErrNotFound
	}
	if err != nil {
		return nil, Meta{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("读取文件失败: %w", err)
	}
	return data, m, nil
}
