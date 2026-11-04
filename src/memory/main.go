// memory：SUMP v2 的记忆服务（第一批）。
// 会话历史存储 + 上下文组装：recall 收到调用方托管的任务上下文 →
// 注入记忆后以 resume 发回（触发其“完全启动”）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/memory/shared"
	"github.com/NouSart/sump/memory/store"
)

func main() {
	name := flag.String("name", "memory", "服务名")
	addr := flag.String("addr", "127.0.0.1:9201", "监听地址（host:port）")
	center := flag.String("center", "ws://127.0.0.1:9000/ws", "设置中心 WS 地址")
	dbPath := flag.String("db", "data/memory.db", "会话库（私有）路径")
	// 共享库默认与会话库同目录（data/experience.db）：换 -db 时不必记得同时换它，
	// 测试也就不会把共享数据写进仓库的 data/。
	sharedFlag := flag.String("shared-db", "", "共享经验库路径（跨会话只放提炼物；空 = 与会话库同目录的 experience.db）")
	history := flag.Int("history", 12, "recall 返回的最近消息条数")
	embedding := flag.String("embedding", "embedding", "embedding 服务名（语义召回；不可用时静默回落 bigram）")
	flag.Parse()

	logger := log.New(os.Stdout, "[memory] ", log.LstdFlags|log.Lmicroseconds)

	sharedPath := *sharedFlag
	if sharedPath == "" {
		sharedPath = filepath.Join(filepath.Dir(*dbPath), "experience.db")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		logger.Fatalf("打开会话库失败: %v", err)
	}
	defer st.Close()

	// 共享经验库是**另一个文件、另一个句柄**：这里没有 messages，只有偏好 / 经验 / 会话摘要。
	// 首次从旧版 memory.db 迁移前先复制一份备份——迁移只改名不删表，但备份更省心。
	if _, err := os.Stat(sharedPath); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(*dbPath); err == nil {
			bak := fmt.Sprintf("%s.bak-%s", *dbPath, time.Now().Format("20060102-150405"))
			if err := copyFile(*dbPath, bak); err != nil {
				logger.Printf("备份会话库失败（继续迁移）: %v", err)
			} else {
				logger.Printf("迁移前已备份会话库: %s", bak)
			}
		}
	}
	sh, err := shared.Open(sharedPath, time.Now())
	if err != nil {
		logger.Fatalf("打开共享经验库失败: %v", err)
	}
	defer sh.Close()
	if migrated, err := sh.MigrateFrom(st.DB()); err != nil {
		logger.Printf("共享数据迁移未完成（共享库保持现状）: %v", err)
	} else if migrated {
		logger.Printf("共享数据已迁移到 %s（旧表改名为 legacy_*）", sharedPath)
	}

	s := server.New(server.Config{
		Name:         *name,
		Listen:       *addr,
		Center:       *center,
		Store:        st,
		Shared:       sh,
		HistoryLimit: *history,
		Embedding:    *embedding,
		DBPath:       *dbPath,
		SharedPath:   sharedPath,
	}, logger)
	if err := s.Start(context.Background()); err != nil {
		logger.Fatalf("启动失败: %v", err)
	}
	defer s.Shutdown()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	logger.Println("退出")
}

// copyFile 是迁移前的一次性文件备份（只在共享库不存在时做一次）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
