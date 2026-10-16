// images：SUMP v2 的图片服务（第一批）。
// 保存图片（URL / base64），按需转成 base64 发回调用方（llm 取图内联给模型）。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/QAQ-awa-QAQ/sump/images/server"
	"github.com/QAQ-awa-QAQ/sump/images/store"
)

func main() {
	name := flag.String("name", "images", "服务名")
	addr := flag.String("addr", "127.0.0.1:9401", "监听地址（host:port）")
	center := flag.String("center", "ws://127.0.0.1:9000/ws", "设置中心 WS 地址")
	dbPath := flag.String("db", "data/images.db", "SQLite 元数据文件路径")
	dir := flag.String("dir", "data/images", "图片二进制存放目录")
	maxMB := flag.Int("max-mb", 32, "单图大小上限（MB）")
	flag.Parse()

	logger := log.New(os.Stdout, "[images] ", log.LstdFlags|log.Lmicroseconds)

	st, err := store.Open(*dbPath, *dir)
	if err != nil {
		logger.Fatalf("打开图库失败: %v", err)
	}
	defer st.Close()

	s := server.New(server.Config{
		Name:     *name,
		Listen:   *addr,
		Center:   *center,
		Store:    st,
		MaxBytes: int64(*maxMB) << 20,
		DBPath:   *dbPath,
		Dir:      *dir,
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
