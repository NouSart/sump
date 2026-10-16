// qq：SUMP v2 的 QQ 接入服务（第一批，仅私聊）。
// NapCat（OneBot 11）正向 WS → user_message 任务链；deliver → 发回 QQ。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/QAQ-awa-QAQ/sump/qq/server"
)

func main() {
	name := flag.String("name", "qq", "服务名")
	addr := flag.String("addr", "127.0.0.1:9301", "监听地址（host:port）")
	center := flag.String("center", "ws://127.0.0.1:9000/ws", "设置中心 WS 地址")
	napcatURL := flag.String("napcat", "ws://127.0.0.1:3001", "NapCat 正向 WS 地址")
	napcatToken := flag.String("napcat-token", os.Getenv("SUMP_NAPCAT_TOKEN"), "NapCat access_token（默认取环境变量 SUMP_NAPCAT_TOKEN）")
	owner := flag.String("owner", "", "主人 QQ（唯一授权私聊用户；留空则拒绝所有私聊）")
	reasoner := flag.String("reasoner", "reasoner", "任务跳转目标（reasoner 服务名）")
	images := flag.String("images", "images", "图片服务名")
	secretFile := flag.String("secret-file", "", "秘密文件路径（空 = data/<服务名>-secret.json；\"-\" = 不落盘）")
	flag.Parse()

	logger := log.New(os.Stdout, "[qq] ", log.LstdFlags|log.Lmicroseconds)

	s := server.New(server.Config{
		Name:        *name,
		Listen:      *addr,
		Center:      *center,
		NapCatURL:   *napcatURL,
		NapCatToken: *napcatToken,
		Owner:       *owner,
		Reasoner:    *reasoner,
		Images:      *images,
		SecretFile:  *secretFile,
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
