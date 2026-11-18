// memory 域进程入口。
// 生产按角色拆成三进程：session（会话库）/ experience（共享库）/ facade（唯一对外）。
// -role memory 保留旧单进程实现，供尚未迁移的本地脚本与端到端测试使用。
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
	"strings"
	"time"

	"github.com/NouSart/sump/memory/experience"
	"github.com/NouSart/sump/memory/facade"
	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/memory/session"
	"github.com/NouSart/sump/memory/shared"
	"github.com/NouSart/sump/memory/store"
)

type role string

const (
	roleMemory     role = "memory"
	roleFacade     role = "facade"
	roleSession    role = "session"
	roleExperience role = "experience"
)

func main() {
	roleFlag := flag.String("role", "memory", "进程角色：memory（旧单进程）/ facade / session / experience")
	peers := flag.String("peers", "", "静态内部对端，逗号分隔：embedding=ws://127.0.0.1:9202/spv2,session=...")
	r := parseRole(*roleFlag)
	nameDefault, addrDefault := "memory", "127.0.0.1:9201"
	switch r {
	case roleSession:
		nameDefault, addrDefault = "session", "127.0.0.1:9311"
	case roleExperience:
		nameDefault, addrDefault = "experience", "127.0.0.1:9312"
	}
	name := flag.String("name", nameDefault, "服务名")
	addr := flag.String("addr", addrDefault, "监听地址（host:port）")
	center := flag.String("center", "ws://127.0.0.1:9000/spv2", "设置中心 WS 地址（session/experience 独立模式不连接）")
	dbPath := flag.String("db", "data/memory.db", "会话库（私有）路径")
	sharedFlag := flag.String("shared-db", "", "共享经验库路径（空 = 与会话库同目录的 experience.db）")
	history := flag.Int("history", 12, "context_base 返回的最近消息条数")
	embedding := flag.String("embedding", "embedding", "embedding 服务名")
	flag.Parse()

	logger := log.New(os.Stdout, "[memory] ", log.LstdFlags|log.Lmicroseconds)
	sharedPath := *sharedFlag
	if sharedPath == "" {
		sharedPath = filepath.Join(filepath.Dir(*dbPath), "experience.db")
	}
	cfg := runConfig{
		name: *name, addr: *addr, center: *center, dbPath: *dbPath, sharedPath: sharedPath,
		history: *history, embedding: *embedding, peers: parsePeers(*peers), logger: logger,
	}
	switch r {
	case roleSession:
		runSession(cfg)
	case roleExperience:
		runExperience(cfg)
	case roleFacade:
		runFacade(cfg)
	default:
		runLegacy(cfg)
	}
}

type runConfig struct {
	name, addr, center, dbPath, sharedPath, embedding string
	history                                           int
	peers                                             map[string]string
	logger                                            *log.Logger
}

func runSession(c runConfig) {
	st, err := store.Open(c.dbPath)
	if err != nil {
		c.logger.Fatalf("打开会话库失败: %v", err)
	}
	defer st.Close()
	s := session.New(session.Config{
		Name: c.name, Listen: c.addr, Center: c.center, Standalone: true,
		Peers: c.peers, Store: st, HistoryLimit: c.history, DBPath: c.dbPath,
		Logger: c.logger,
	}, c.logger)
	serve(s, c.logger)
}

func runExperience(c runConfig) {
	sh, err := shared.Open(c.sharedPath, time.Now())
	if err != nil {
		c.logger.Fatalf("打开共享经验库: %v", err)
	}
	defer sh.Close()
	s := experience.New(experience.Config{
		Name: c.name, Listen: c.addr, Center: c.center, Standalone: true,
		Peers: c.peers, Shared: sh, Embedding: c.embedding, SharedPath: c.sharedPath,
		Logger: c.logger,
	}, c.logger)
	serve(s, c.logger)
}

func runFacade(c runConfig) {
	s := facade.New(facade.Config{
		Name: c.name, Listen: c.addr, Center: c.center, Peers: c.peers,
		Logger: c.logger,
	}, c.logger)
	serve(s, c.logger)
}

// runLegacy 是旧单进程形态：同时开两个库，保留到端到端脚本迁完。
func runLegacy(c runConfig) {
	st, err := store.Open(c.dbPath)
	if err != nil {
		c.logger.Fatalf("打开会话库失败: %v", err)
	}
	defer st.Close()
	if _, err := os.Stat(c.sharedPath); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(c.dbPath); err == nil {
			bak := fmt.Sprintf("%s.bak-%s", c.dbPath, time.Now().Format("20060102-150405"))
			if err := copyFile(c.dbPath, bak); err != nil {
				c.logger.Printf("备份会话库失败（继续迁移）: %v", err)
			} else {
				c.logger.Printf("迁移前已备份会话库: %s", bak)
			}
		}
	}
	sh, err := shared.Open(c.sharedPath, time.Now())
	if err != nil {
		c.logger.Fatalf("打开共享经验库: %v", err)
	}
	defer sh.Close()
	if migrated, err := sh.MigrateFrom(st.DB()); err != nil {
		c.logger.Printf("共享数据迁移未完成（共享库保持现状）: %v", err)
	} else if migrated {
		c.logger.Printf("共享数据已迁移到 %s（旧表改名为 legacy_*）", c.sharedPath)
	}
	s := server.New(server.Config{
		Name: c.name, Role: server.RoleAll, Peers: c.peers, Listen: c.addr, Center: c.center,
		Store: st, Shared: sh, HistoryLimit: c.history, Embedding: c.embedding,
		DBPath: c.dbPath, SharedPath: c.sharedPath,
	}, c.logger)
	serve(s, c.logger)
}

type serveable interface {
	Start(context.Context) error
	Shutdown()
}

func serve(s serveable, logger *log.Logger) {
	if err := s.Start(context.Background()); err != nil {
		logger.Fatalf("启动失败: %v", err)
	}
	defer s.Shutdown()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	logger.Println("退出")
}

func parseRole(v string) role {
	switch strings.TrimSpace(v) {
	case "facade", "memory-facade":
		return roleFacade
	case "session":
		return roleSession
	case "experience":
		return roleExperience
	default:
		return roleMemory
	}
}

func parsePeers(v string) map[string]string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	out := map[string]string{}
	for _, item := range strings.Split(v, ",") {
		name, addr, ok := strings.Cut(strings.TrimSpace(item), "=")
		if ok && name != "" && addr != "" {
			out[name] = addr
		}
	}
	return out
}

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
