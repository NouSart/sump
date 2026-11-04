// shell：SUMP v2 的命令执行服务。
// 受理即回执（不等执行）→ 审批（接缝，未通过即补报结束）→ 并行执行（限时、限流、限量）→ 补报结果。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/NouSart/sump/shell/server"
)

func main() {
	name := flag.String("name", "shell", "服务名")
	addr := flag.String("addr", "127.0.0.1:9701", "监听地址（host:port）")
	center := flag.String("center", "ws://127.0.0.1:9000/ws", "设置中心 WS 地址")
	root := flag.String("root", "", "工作目录根（默认 = 进程当前目录；命令的 work_dir 必须落在其内）")
	sh := flag.String("shell", "", "命令解释器（默认自动探测：bash → pwsh → cmd）")
	timeoutMS := flag.Int("timeout-ms", 300000, "单条命令执行超时（毫秒）")
	maxKB := flag.Int("max-output-kb", 256, "单流输出上限（KB；超出即截断并标注）")
	conc := flag.Int("max-running", 64, "同时在跑的命令上限（超过即拒绝，不排队）")
	approval := flag.String("approval", "", "审批服务名（集成启动器使用 approval；留空仍不执行）")
	approvalMS := flag.Int("approval-timeout-ms", 60000, "等审批超时（毫秒；超时按未通过处理）")
	flag.Parse()

	logger := log.New(os.Stdout, "[shell] ", log.LstdFlags|log.Lmicroseconds)

	if *root != "" {
		st, err := os.Stat(*root)
		if err != nil || !st.IsDir() {
			logger.Fatalf("工作目录根不可用: %s", *root)
		}
	}

	s := server.New(server.Config{
		Name:            *name,
		Listen:          *addr,
		Center:          *center,
		Root:            *root,
		Shell:           *sh,
		Timeout:         time.Duration(*timeoutMS) * time.Millisecond,
		MaxOutputBytes:  int64(*maxKB) << 10,
		MaxRunning:      *conc,
		ApprovalService: *approval,
		ApprovalTimeout: time.Duration(*approvalMS) * time.Millisecond,
	}, logger)
	if err := s.Start(context.Background()); err != nil {
		logger.Fatalf("启动失败: %v", err)
	}
	defer s.Shutdown()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	logger.Printf("退出（在跑 %d 条随进程变孤儿）", s.RunningCount())
}
