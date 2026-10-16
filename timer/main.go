// timer：SUMP v2 的计时器服务。
// 受理「N 毫秒后唤醒我」→ 立即回执（受理即回执，调用方不被占住）→ 到点把一条补报跳回调用方。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/QAQ-awa-QAQ/sump/timer/server"
)

func main() {
	name := flag.String("name", "timer", "服务名")
	addr := flag.String("addr", "127.0.0.1:9801", "监听地址（host:port）")
	center := flag.String("center", "ws://127.0.0.1:9000/ws", "设置中心 WS 地址")
	portal := flag.String("portal", "frontend", "门户服务名（在计时器推给它展示倒计时；留空 = 不推）")
	maxMS := flag.Int64("max-ms", 3600000, "单次计时上限（毫秒；默认 1 小时，天花板 23 小时）")
	maxPending := flag.Int("max-pending", 64, "在计时器上限（满即拒，不排队）")
	flag.Parse()

	logger := log.New(os.Stdout, "[timer] ", log.LstdFlags|log.Lmicroseconds)

	s := server.New(server.Config{
		Name:        *name,
		Listen:      *addr,
		Center:      *center,
		MaxDuration: time.Duration(*maxMS) * time.Millisecond,
		MaxPending:  *maxPending,
		Portal:      *portal,
	}, logger)
	if err := s.Start(context.Background()); err != nil {
		logger.Fatalf("启动失败: %v", err)
	}
	defer s.Shutdown()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	n := s.PendingCount()
	s.StopAll()
	logger.Printf("退出（在计时 %d 条随进程丢弃；这些计时的调用方不会再收到补报）", n)
}
