// memory：SUMP v2 的记忆服务（第一批）。
// 会话历史存储 + 上下文组装：recall 收到调用方托管的任务上下文 →
// 注入记忆后以 resume 发回（触发其“完全启动”）。
//
// 启动顺序（S-001 §11.1）：先做只读预检（版本闸门），任何 Open/DDL 都在预检之后；
// 版本不匹配只提示删库或显式重置，不迁移、不自动删除。
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
	center := flag.String("center", "ws://127.0.0.1:9000/spv2", "设置中心 WS 地址")
	dbPath := flag.String("db", "data/memory.db", "会话库（私有）路径")
	// 共享库默认与会话库同目录（data/experience.db）：换 -db 时不必记得同时换它，
	// 测试也就不会把共享数据写进仓库的 data/。
	sharedFlag := flag.String("shared-db", "", "共享经验库路径（跨会话只放提炼物；空 = 与会话库同目录的 experience.db）")
	history := flag.Int("history", 12, "recall 返回的最近消息条数")
	embedding := flag.String("embedding", "embedding", "embedding 服务名（语义召回；不可用时静默回落 bigram）")
	// 显式重置（S-001 §11.2）：默认拒绝启动旧库；确需重置时必须二次确认，且只改名备份、不删除。
	resetData := flag.Bool("reset-data", false, "显式重置：把会话库与共享库改名为 *.bak-<UTC 时间戳> 后按新库启动（需同时给 -reset-data-confirm=YES）")
	resetConfirm := flag.String("reset-data-confirm", "", "重置确认串：必须为 YES 才生效")
	flag.Parse()

	logger := log.New(os.Stdout, "[memory] ", log.LstdFlags|log.Lmicroseconds)

	sharedPath := *sharedFlag
	if sharedPath == "" {
		sharedPath = filepath.Join(filepath.Dir(*dbPath), "experience.db")
	}

	// 显式重置排在预检之前：预检会拒绝旧库，而重置的目的就是把旧库挪开。
	if err := checkResetFlags(*resetData, *resetConfirm); err != nil {
		logger.Fatalf("%v", err)
	}
	if *resetData {
		if err := resetWithBackup(*dbPath, sharedPath, time.Now()); err != nil {
			logger.Fatalf("重置失败: %v", err)
		}
		logger.Printf("已按 -reset-data 重置：%s 与 %s 已改名为 *.bak-<UTC 时间戳>（仅对原文件存在的库），按新库启动",
			*dbPath, sharedPath)
	}

	// 只读预检（S-001 §11.1）：必须在任何 Open/DDL 之前，且预检阶段绝不写库。
	if err := precheck(*dbPath, sharedPath); err != nil {
		logger.Fatalf("%v", err)
	}

	// 迁移备份只针对**启动前**就存在的会话库：重置 / 全新启动没有旧数据可备份，
	// 也避免同一秒内生成的备份文件覆盖掉重置留下的 .bak。
	memoryExisted := false
	if _, statErr := os.Stat(*dbPath); statErr == nil {
		memoryExisted = true
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		logger.Fatalf("打开会话库失败: %v", err)
	}
	defer st.Close()

	// 共享经验库是**另一个文件、另一个句柄**：这里没有 messages，只有偏好 / 经验 / 会话摘要。
	// 首次从旧版 memory.db 迁移前先复制一份备份——迁移只改名不删表，但备份更省心。
	if _, err := os.Stat(sharedPath); errors.Is(err, os.ErrNotExist) {
		if memoryExisted {
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

// precheck 是启动前的只读预检（S-001 §11.1）：只做 os.Stat 与只读读取，绝不写库。
//   - 会话库：store.CheckPath 读 user_version。不存在 = 新库（允许新建）；
//     存在且版本 == store.SchemaVersion 才允许；其余（version 0 的旧库、版本号更高、
//     无法读取）一律拒绝，并给出当前版本、期望版本、两个库路径与两条可选操作。
//   - 共享库：只做 os.Stat（共享库暂未引入版本号）。不存在 = 新库；存在 = 继续；
//     其它 stat 错误 = 直接失败。
func precheck(memoryPath, sharedPath string) error {
	exists, version, err := store.CheckPath(memoryPath)
	if err != nil {
		return fmt.Errorf("启动预检失败：%w", err)
	}
	if exists && version != store.SchemaVersion {
		return fmt.Errorf(
			"启动预检失败：会话库 %s 的 schema 版本不匹配（当前版本 %d，期望版本 %d）。\n"+
				"共享库：%s（暂无版本号，只检查文件是否存在）。\n"+
				"已拒绝启动，预检阶段未写入任何库。可选操作：\n"+
				"  1) 删除两个库文件后重启：%s 与 %s；\n"+
				"  2) 显式重置（改名为 *.bak-<UTC 时间戳> 备份，不删除原文）：-reset-data -reset-data-confirm=YES",
			memoryPath, version, store.SchemaVersion, sharedPath, memoryPath, sharedPath)
	}
	if _, statErr := os.Stat(sharedPath); statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("启动预检失败：检查共享库 %s: %w", sharedPath, statErr)
	}
	return nil
}

// checkResetFlags 校验重置参数组合（S-001 §11.2：必须二次显式确认）。
// 生效条件是 -reset-data 与 -reset-data-confirm=YES 同时给出；缺任何一个都拒绝启动。
func checkResetFlags(resetData bool, confirm string) error {
	if !resetData {
		if confirm != "" {
			return fmt.Errorf("检测到 -reset-data-confirm 但没有 -reset-data：确认串只能与 -reset-data 一起使用")
		}
		return nil
	}
	if confirm != "YES" {
		return fmt.Errorf("-reset-data 需要显式确认：请同时给 -reset-data-confirm=YES（当前值 %q）；未做任何改动", confirm)
	}
	return nil
}

// resetWithBackup 是显式重置的实现（S-001 §11.2）：把两个库文件**改名**为
// `<原名>.bak-<UTC 时间戳>`，不删除原文、不覆盖已有备份；文件本来不存在则跳过（按新库处理）。
// 先检查全部备份目标再改名，避免「备份目标冲突时只改了一半」。
func resetWithBackup(memoryPath, sharedPath string, now time.Time) error {
	suffix := ".bak-" + now.UTC().Format("20060102-150405")
	type renameOp struct{ src, dst string }
	var plan []renameOp
	seen := make(map[string]bool, 2)
	for _, path := range []string{memoryPath, sharedPath} {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // 不存在 = 新库，无需备份
			}
			return fmt.Errorf("重置前检查 %s: %w", path, err)
		}
		dst := path + suffix
		if _, err := os.Stat(dst); err == nil {
			return fmt.Errorf("备份目标已存在，拒绝覆盖：%s", dst)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("重置前检查备份目标 %s: %w", dst, err)
		}
		plan = append(plan, renameOp{src: path, dst: dst})
	}
	for _, op := range plan {
		if err := os.Rename(op.src, op.dst); err != nil {
			return fmt.Errorf("把 %s 改名为 %s: %w", op.src, op.dst, err)
		}
	}
	return nil
}

// copyFile 是迁移前的一次性文件备份（只在共享库不存在、且会话库启动前已存在时做一次）。
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
