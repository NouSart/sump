// memory 域进程入口。
// 生产按角色拆成三进程：session（会话库）/ experience（共享库）/ facade（唯一对外）。
// -role memory 保留旧单进程实现，供尚未迁移的本地脚本与端到端测试使用。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/NouSart/sump/memory/experience"
	"github.com/NouSart/sump/memory/experience/shared"
	"github.com/NouSart/sump/memory/facade"
	"github.com/NouSart/sump/memory/session"
	"github.com/NouSart/sump/memory/session/store"
)

type role string

const (
	roleFacade     role = "facade"
	roleSession    role = "session"
	roleExperience role = "experience"
)

func main() {
	roleFlag := flag.String("role", "", "进程角色（必填）：facade / session / experience")
	peers := flag.String("peers", "", "静态内部对端，逗号分隔：embedding=ws://127.0.0.1:9202/spv2,session=...")
	name := flag.String("name", "", "服务名（空 = 按角色默认）")
	addr := flag.String("addr", "", "监听地址（空 = 按角色默认）")
	center := flag.String("center", "ws://127.0.0.1:9000/spv2", "设置中心 WS 地址（session/experience 独立模式不连接）")
	dbPath := flag.String("db", "data/memory.db", "会话库（私有）路径")
	sharedFlag := flag.String("shared-db", "", "共享经验库路径（空 = 与会话库同目录的 experience.db）")
	history := flag.Int("history", 12, "context_base 返回的最近消息条数")
	embedding := flag.String("embedding", "embedding", "embedding 服务名")
	flag.Parse()

	r := parseRole(*roleFlag)
	nameDefault, addrDefault := "memory", "127.0.0.1:9201"
	switch r {
	case roleSession:
		nameDefault, addrDefault = "session", "127.0.0.1:9311"
	case roleExperience:
		nameDefault, addrDefault = "experience", "127.0.0.1:9312"
	case roleFacade:
	default:
		log.Fatalf("必须指定 -role facade|session|experience，收到 %q", *roleFlag)
	}
	if *name == "" {
		*name = nameDefault
	}
	if *addr == "" {
		*addr = addrDefault
	}

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
	default:
		runFacade(cfg)
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
	// owner 客户端要拿本服务骨架做出站一跳（按服务名解析），所以在 New 之后注入。
	// session 角色是调用账本的 owner：pause/resume/cancel 与 call_status 都要问执行服务。
	s.SetOwnerClient(session.NewSPv2OwnerClient(s.Service))
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
		return ""
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
