package shared

// 一次性迁移：把旧版 memory.db 里的共享数据（长期记忆 + 经验网）搬进共享库。
//
// 为什么搬：这两个东西本来就是**跨会话**的（召回不分会话），以前只是与会话流水同库。
// 搬完以后，"共享"由库文件保证，而不是由读代码的人记得。
//
// 安全约定：
//   - 幂等：靠 shared_meta 的标记，只搬一次；
//   - 不删数据：旧表只**改名**为 legacy_*，字节还在；
//   - 失败不动旧表：拷贝在事务里，任何一步失败就整体回滚，服务照常启动（共享库留空）。

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const migrateKey = "migrated_from_memory_db"

// legacyTables 是旧库里要搬走的表（搬完改名 legacy_*）。
var legacyTables = []struct {
	name string
	cols []string
}{
	{"memories", []string{"id", "kind", "content", "priority", "source", "conversation_id", "status", "created_at", "updated_at", "embedding", "embedding_model"}},
	{"mem_nodes", []string{"id", "parent_id", "level", "anchor", "entries", "centroid", "centroid_model", "created_at", "updated_at"}},
	{"mem_entries", []string{"id", "content", "conversation_id", "task_id", "source", "durability", "hits", "status", "last_hit_at", "created_at", "embedding", "embedding_model"}},
	{"mem_links", []string{"entry_id", "node_id", "conductance", "last_flow_at"}},
}

// MigrateFrom 把 src（会话库句柄）里的旧表搬进共享库。返回 true 表示这次真的搬了。
// src 为空、或旧库里根本没有这些表时，只写标记并返回 false。
func (s *Store) MigrateFrom(src *sql.DB) (bool, error) {
	done, err := s.metaGet(migrateKey)
	if err != nil {
		return false, err
	}
	if done != "" {
		return false, nil
	}
	if src == nil {
		return false, s.metaSet(migrateKey, "no-source")
	}

	present := make([]string, 0, len(legacyTables))
	for _, t := range legacyTables {
		ok, err := tableExists(src, t.name)
		if err != nil {
			return false, err
		}
		if ok {
			present = append(present, t.name)
		}
	}
	if len(present) == 0 {
		return false, s.metaSet(migrateKey, "no-tables")
	}

	counts := map[string]int{}
	err = func() error {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		for _, t := range legacyTables {
			ok, err := tableExists(src, t.name)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			n, err := copyTable(tx, src, t.name, t.cols)
			if err != nil {
				return fmt.Errorf("搬运 %s 失败: %w", t.name, err)
			}
			counts[t.name] = n
		}
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO shared_meta(key, value) VALUES(?,?)`,
			migrateKey, time.Now().Format(time.RFC3339),
		); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		return false, err
	}

	// 改名失败不影响正确性（新代码不再读旧表），但要留下痕迹。
	for _, name := range present {
		if err := renameLegacy(src, name); err != nil {
			return true, err
		}
	}
	return true, nil
}

// copyTable 把 src 的一张表逐行 INSERT OR REPLACE 到共享库（列取交集，缺的列用默认值）。
func copyTable(tx *sql.Tx, src *sql.DB, table string, expected []string) (int, error) {
	have, err := tableColumns(src, table)
	if err != nil {
		return 0, err
	}
	cols := make([]string, 0, len(expected))
	for _, c := range expected {
		if have[c] {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return 0, fmt.Errorf("表 %s 没有可用的列", table)
	}

	rows, err := src.Query(`SELECT ` + strings.Join(cols, ",") + ` FROM ` + table)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	marks := make([]string, len(cols))
	for i := range marks {
		marks[i] = "?"
	}
	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO ` + table + `(` + strings.Join(cols, ",") + `) VALUES(` + strings.Join(marks, ",") + `)`,
	)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		if _, err := stmt.Exec(vals...); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// renameLegacy 把旧表改名（幂等：上一次已经改过就直接丢掉残留的旧表）。
func renameLegacy(db *sql.DB, name string) error {
	if ok, err := tableExists(db, "legacy_"+name); err != nil {
		return err
	} else if ok {
		_, err := db.Exec(`DROP TABLE IF EXISTS ` + name)
		return err
	}
	if _, err := db.Exec(`ALTER TABLE ` + name + ` RENAME TO legacy_` + name); err != nil {
		return fmt.Errorf("旧表 %s 改名失败: %w", name, err)
	}
	return nil
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var got string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

func (s *Store) metaGet(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM shared_meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) metaSet(key, value string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO shared_meta(key, value) VALUES(?,?)`, key, value)
	return err
}
