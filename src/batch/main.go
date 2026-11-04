// batch：SUMP v2 的批处理服务。
// 一次声明 N 项工作 + 并发度 + 唤醒时机；受理即回执，跑完（或关键项跑完）推一条汇总。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/NouSart/sump/batch/server"
)

func main() {
	name := flag.String("name", "batch", "服务名")
	addr := flag.String("addr", "127.0.0.1:9951", "监听地址（host:port）")
	center := flag.String("center", "ws://127.0.0.1:9000/ws", "设置中心 WS 地址")
	db := flag.String("db", "data/batch.db", "任务账本路径")
	conc := flag.Int("concurrency", 8, "单任务默认并发")
	maxConc := flag.Int("max-concurrency", 64, "单任务并发上限")
	maxInflight := flag.Int("max-inflight", 64, "全局在飞槽位上限（满了同步拒绝，不排队）")
	maxItems := flag.Int("max-items", 1000, "单任务最多项数")
	itemTimeout := flag.Duration("item-timeout", 60*time.Second, "单项超时")
	previewBytes := flag.Int("preview-bytes", 2048, "每项结果摘要上限（字节）")
	jobTTL := flag.Duration("job-ttl", time.Hour, "终态任务可查询时长")
	resumeOnStart := flag.Bool("resume-on-start", false, "重启后自动恢复被转暂停的任务")
	flag.Parse()

	logger := log.New(os.Stdout, "[batch] ", log.LstdFlags|log.Lmicroseconds)

	s := server.New(server.Config{
		Name:               *name,
		Listen:             *addr,
		Center:             *center,
		DBPath:             *db,
		DefaultConcurrency: *conc,
		MaxConcurrency:     *maxConc,
		MaxInflight:        *maxInflight,
		MaxItems:           *maxItems,
		ItemTimeout:        *itemTimeout,
		PreviewBytes:       *previewBytes,
		JobTTL:             *jobTTL,
		ResumeOnStart:      *resumeOnStart,
	}, logger)
	if err := s.Start(context.Background()); err != nil {
		logger.Fatalf("启动失败: %v", err)
	}
	defer s.Shutdown()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	logger.Println("退出（未完成的任务会转暂停，重启后 resume 可继续）")
}
