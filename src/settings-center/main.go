// settings-center：SUMP v2 的设置中心服务（第一批）。
// 职责：服务注册 · 全局信息分发 · 设置与调度（见 docs/concepts/architecture.md）。
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/NouSart/sump/settings-center/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "监听地址（host:port）")
	settingsPath := flag.String("settings", "data/settings.json", "设置覆盖值文件路径（空 = 不落盘）")
	hb := flag.Duration("hb", 15*time.Second, "心跳间隔预期（服务未声明心跳时用于离线判定）")
	sweep := flag.Duration("sweep", 5*time.Second, "离线扫描间隔")
	flag.Parse()

	logger := log.New(os.Stdout, "[settings-center] ", log.LstdFlags|log.Lmicroseconds)

	s, err := server.Start(server.Config{
		Addr:         *addr,
		SettingsPath: *settingsPath,
		Heartbeat:    *hb,
		Sweep:        *sweep,
	}, logger)
	if err != nil {
		logger.Fatalf("启动失败: %v", err)
	}
	defer func() { _ = s.Shutdown() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	logger.Println("退出")
}
