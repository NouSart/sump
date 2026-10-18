// reasoner：SUMP v2 的单步推理服务。
// 每收到一次输入（任务入口 / 自跳 / 工具结果注入）调用一次 LLM，产出下一步动作
// （工具派发 / 自跳续跑 / 交付）；任务窗口内的上下文由黑板持有，循环不驻留本服务。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/QAQ-awa-QAQ/sump/reasoner/server"
)

func main() {
	name := flag.String("name", "reasoner", "服务名")
	addr := flag.String("addr", "127.0.0.1:9101", "监听地址（host:port）")
	center := flag.String("center", "ws://127.0.0.1:9000/ws", "设置中心 WS 地址")
	memorySvc := flag.String("memory", "memory", "记忆服务名（完全启动链的另一半）")
	imagesSvc := flag.String("images", "images", "图片服务名（推理前取图内联）")
	llmBase := flag.String("llm-base", "https://api.deepseek.com", "LLM API 地址（OpenAI 兼容；设置中心 llm.base 可热改）")
	llmKey := flag.String("llm-key", os.Getenv("DEEPSEEK_API_KEY"), "LLM API Key（默认取环境变量 DEEPSEEK_API_KEY；设置中心 llm.key 可热改）")
	llmModel := flag.String("llm-model", "deepseek-flash", "LLM 模型名（设置中心 llm.model 可热改）")
	secretFile := flag.String("secret-file", "", "秘密文件路径（空 = data/<服务名>-secret.json；\"-\" = 不落盘）")
	toolConc := flag.Int("tool-concurrency", 10, "单会话工具并发上限（-1 = 不限制）")
	toolTimeout := flag.Duration("tool-timeout", 60*time.Second, "单个工具调用的执行超时")
	toolNotify := flag.String("tool-notify", "each", "工具结果通知模式：each=逐条唤醒 / batch=攒批唤醒（设置中心 tool.notify_mode 覆盖此项）")
	reflect := flag.Bool("reflect", true, "交付后复盘写经验（设置中心 reflect.on 可热改）")
	flag.Parse()

	logger := log.New(os.Stdout, "[reasoner] ", log.LstdFlags|log.Lmicroseconds)
	if *llmKey == "" {
		logger.Printf("警告: 未配置 LLM Key（-llm-key 或环境变量 DEEPSEEK_API_KEY），user_message 将无法推理")
	}

	// LLM 客户端由 server.New 按启动值构建（Key 非空才建，空 = nil 轮次停住）；llm.* 可在设置中心热改。
	s := server.New(server.Config{Name: *name, Listen: *addr, Center: *center, Memory: *memorySvc, Images: *imagesSvc, LLMBase: *llmBase, LLMModel: *llmModel, LLMKey: *llmKey, ToolConcurrency: *toolConc, ToolTimeout: *toolTimeout, NotifyMode: *toolNotify, Reflect: *reflect, SecretFile: *secretFile}, logger)
	if err := s.Start(context.Background()); err != nil {
		logger.Fatalf("启动失败: %v", err)
	}
	defer s.Shutdown()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	logger.Println("退出")
}
