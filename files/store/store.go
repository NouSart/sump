// Package store 是文件服务的存储层：二进制落盘 + SQLite 元数据（与图片的差异：文件有名字）。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/NouSart/sump/protocol"
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
//
// mu 保证"元数据 + 二进制"对调用方是一个快照：Load/Stat 持读锁，Save/Overwrite/
// Close 持写锁。没有它，Overwrite 的 O_TRUNC 窗口会被并发 Load 读到空文件，
// 元数据也可能来自改动前后两个版本。
type Store struct {
	db  *sql.DB
	dir string

	mu sync.RWMutex
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
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

// Save 保存一个文件，返回 id（ULID 一证三用：数据库主键 / 磁盘文件名 / 链上引用）。
func (s *Store) Save(data []byte, filename, mime string) (string, error) {
	id := protocol.NewID()
	path := filepath.Join(s.dir, id)
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.RLock()
	defer s.mu.RUnlock()
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

// Overwrite 用新内容就地覆盖已存在的文件（权威副本保持唯一；新尺寸入台账）。
//
// 写盘使用临时文件 + 原子替换，避免读者看到 O_TRUNC 后的空文件；若 UPDATE 失败，
// 用写前备份把二进制恢复，保证"返回错误"时原文和元数据都还是原样。
func (s *Store) Overwrite(id string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var path string
	err := s.db.QueryRow(`SELECT path FROM files WHERE id=?`, id).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	backup, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取覆盖前的原文件 %s 失败: %w", id, err)
	}
	if err := replaceFile(path, data); err != nil {
		return fmt.Errorf("覆盖文件 %s 失败: %w", id, err)
	}
	if _, err := s.db.Exec(`UPDATE files SET size=? WHERE id=?`, len(data), id); err != nil {
		if rerr := replaceFile(path, backup); rerr != nil {
			return fmt.Errorf("更新文件 %s 的元数据失败: %v；且回滚原文件失败: %w", id, err, rerr)
		}
		return fmt.Errorf("更新文件 %s 的元数据失败（原文件已回滚）: %w", id, err)
	}
	return nil
}

// Load 读取一个文件（二进制 + 元数据）。
// 整个读取持读锁：返回的字节与尺寸必须来自同一个快照。
func (s *Store) Load(id string) ([]byte, Meta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	if int64(len(data)) != m.Size {
		return nil, Meta{}, fmt.Errorf(
			"文件 %s 的元数据尺寸是 %d，实际读到 %d 字节（权威副本与台账不一致）",
			id, m.Size, len(data))
	}
	return data, m, nil
}

// replaceFile 在同一目录写临时文件再原子替换目标，避免读者看到半份内容。
func replaceFile(path string, data []byte) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(st.Mode().Perm()); err != nil {
		return err
	}
	if n, err := f.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return fmt.Errorf("短写：写入了 %d/%d 字节", n, len(data))
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
