package shared

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/longterm"
	"github.com/NouSart/sump/memory/memnet"
	"github.com/NouSart/sump/memory/store"
)

// TestMigrateFromLegacyMemoryDB 验证旧库（共享数据与会话流水同库）能整体搬过来：
// 数据在、向量字节没坏、旧表改名 legacy_*、重复调用不再搬。
func TestMigrateFromLegacyMemoryDB(t *testing.T) {
	dir := t.TempDir()
	priv, err := store.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = priv.Close() })

	lt, err := longterm.Open(priv.DB())
	if err != nil {
		t.Fatal(err)
	}
	memID, _, err := lt.Add("偏好", "配色跟随本地时间", 1, "web:private:owner", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := lt.SetEmbedding(memID, []float32{1, 0, 0, 0}, "fake"); err != nil {
		t.Fatal(err)
	}
	if _, err := memnet.Open(priv.DB(), time.Now()); err != nil {
		t.Fatal(err)
	}
	// 直接写一行旧经验（Attach 需要大类向量，迁移测试不需要那份几何）。
	if _, err := priv.DB().Exec(
		`INSERT INTO mem_entries(id, content, conversation_id, task_id, source, durability, hits, status,
		   last_hit_at, created_at, embedding, embedding_model)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		"e-legacy", "进程内计数器做 seq，重启会倒流", "web:private:owner", "t-legacy", "agent", "durable",
		1, "active", time.Now().Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano),
		[]byte{0, 0, 128, 63}, "fake",
	); err != nil {
		t.Fatal(err)
	}

	sh, err := Open(filepath.Join(dir, "experience.db"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Close() })

	migrated, err := sh.MigrateFrom(priv.DB())
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !migrated {
		t.Fatal("首次调用应真的搬了")
	}

	entries, err := sh.Longterm.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Content != "配色跟随本地时间" {
		t.Fatalf("长期记忆没搬过来: %+v", entries)
	}
	if len(entries[0].Embedding) == 0 {
		t.Fatal("向量字节丢了")
	}

	var notes int
	if err := sh.DB().QueryRow(`SELECT COUNT(*) FROM mem_entries WHERE conversation_id = ?`, "web:private:owner").Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if notes != 1 {
		t.Fatalf("经验条目数 = %d", notes)
	}

	for _, name := range []string{"memories", "mem_nodes", "mem_entries", "mem_links"} {
		if ok, err := tableExists(priv.DB(), name); err != nil || ok {
			t.Fatalf("旧表 %s 应已改名（ok=%v err=%v）", name, ok, err)
		}
		if ok, err := tableExists(priv.DB(), "legacy_"+name); err != nil || !ok {
			t.Fatalf("legacy_%s 应存在（ok=%v err=%v）", name, ok, err)
		}
	}

	migrated, err = sh.MigrateFrom(priv.DB())
	if err != nil || migrated {
		t.Fatalf("重复迁移 = %v, %v（应跳过）", migrated, err)
	}
}

// TestMigrateWithoutLegacyTables 的全新部署：只落标记，不报错。
func TestMigrateWithoutLegacyTables(t *testing.T) {
	dir := t.TempDir()
	priv, err := store.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = priv.Close() })
	sh, err := Open(filepath.Join(dir, "experience.db"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Close() })

	migrated, err := sh.MigrateFrom(priv.DB())
	if err != nil {
		t.Fatal(err)
	}
	if migrated {
		t.Fatal("没有旧表时不该算迁移")
	}
	if v, err := sh.metaGet(migrateKey); err != nil || v == "" {
		t.Fatalf("应留下标记（v=%q err=%v）", v, err)
	}
}

// TestMigrateFromNilSource 允许没有会话库句柄（测试 / 只跑共享库时）。
func TestMigrateFromNilSource(t *testing.T) {
	sh := openTest(t)
	migrated, err := sh.MigrateFrom(nil)
	if err != nil || migrated {
		t.Fatalf("nil 源 = %v, %v", migrated, err)
	}
}
