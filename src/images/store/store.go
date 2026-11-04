// Package store 是图片服务的存储层：二进制落盘 + SQLite 元数据。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/NouSart/sump/protocol"
)

// ErrNotFound 表示图片不存在。
var ErrNotFound = errors.New("图片不存在")

// Store 是图片库。
type Store struct {
	db  *sql.DB
	dir string
}

// Open 打开（或创建）图库：元数据在 dbPath，二进制落在 dir。
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
CREATE TABLE IF NOT EXISTS images (
	id         TEXT PRIMARY KEY,
	mime       TEXT NOT NULL,
	path       TEXT NOT NULL,
	size       INTEGER NOT NULL,
	created_at TEXT NOT NULL
);
`

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Save 保存一张图片，返回 id。
func (s *Store) Save(data []byte, mime string) (string, error) {
	id := protocol.NewID()
	path := filepath.Join(s.dir, id)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	_, err := s.db.Exec(
		`INSERT INTO images(id, mime, path, size, created_at) VALUES(?,?,?,?,?)`,
		id, mime, path, len(data), time.Now().Format(time.RFC3339Nano),
	)
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return id, nil
}

// Load 读取一张图片（二进制 + mime）。
func (s *Store) Load(id string) ([]byte, string, error) {
	var mime, path string
	err := s.db.QueryRow(`SELECT mime, path FROM images WHERE id=?`, id).Scan(&mime, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("读取文件失败: %w", err)
	}
	return data, mime, nil
}
