// files：SUMP v2 的文件服务（M7）。
// 保存文件（URL / base64），按需取回（fetch 全量 base64）或读取文本（read 截断文本，LLM 工具）。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/NouSart/sump/files/server"
	"github.com/NouSart/sump/files/store"
)

func main() {
	name := flag.String("name", "files", "服务名")
	addr := flag.String("addr", "127.0.0.1:9802", "监听地址（host:port）")
	center := flag.String("center", "ws://127.0.0.1:9000/ws", "设置中心 WS 地址")
	dbPath := flag.String("db", "data/files.db", "SQLite 元数据文件路径")
	dir := flag.String("dir", "data/files", "文件二进制存放目录")
	maxMB := flag.Int("max-mb", 100, "单文件大小上限（MB）")
	readMaxChars := flag.Int("read-max-chars", 4000, "read 返回的最大字符数（截断阈值）")
	flag.Parse()

	logger := log.New(os.Stdout, "[files] ", log.LstdFlags|log.Lmicroseconds)

	st, err := store.Open(*dbPath, *dir)
	if err != nil {
		logger.Fatalf("打开文件库失败: %v", err)
	}
	defer st.Close()

	s := server.New(server.Config{
		Name:         *name,
		Listen:       *addr,
		Center:       *center,
		Store:        st,
		MaxBytes:     int64(*maxMB) << 20,
		ReadMaxChars: *readMaxChars,
		DBPath:       *dbPath,
		Dir:          *dir,
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
