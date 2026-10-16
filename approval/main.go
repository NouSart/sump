// approval 提供三个入口：serve 启动服务，inspect 只分析，send 通过现有 shell 执行并观察反馈。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"github.com/QAQ-awa-QAQ/sump/approval/analysis"
	"github.com/QAQ-awa-QAQ/sump/approval/engine"
	"github.com/QAQ-awa-QAQ/sump/approval/policy"
	"github.com/QAQ-awa-QAQ/sump/approval/server"
	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/service"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: sump-approval serve | inspect -command 'Get-Location' | send -command 'Get-Location'")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "inspect":
		err = inspect(ctx, os.Args[2:])
	case "send":
		err = send(ctx, os.Args[2:])
	default:
		err = errors.New("未知子命令：使用 serve、inspect 或 send")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newEngine(path string) (*engine.Engine, string, error) {
	text := policy.DefaultJSON
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
		text = string(b)
	}
	exe, err := analysis.FindPowerShell()
	if err != nil {
		return nil, "", err
	}
	e, err := engine.New(analysis.PowerShell{Executable: exe}, text)
	return e, text, err
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:9901", "监听地址")
	center := fs.String("center", "ws://127.0.0.1:9000/ws", "设置中心")
	name := fs.String("name", "approval", "服务名")
	path := fs.String("policy", "", "本地 JSON 规则包；中心 policy.json 可覆盖")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, text, err := newEngine(*path)
	if err != nil {
		return err
	}
	s, err := server.New(server.Config{Name: *name, Listen: *addr, Center: *center, Engine: e, PolicyJSON: text, Logger: log.Default()})
	if err != nil {
		return err
	}
	if err := s.Start(ctx); err != nil {
		return err
	}
	defer s.Shutdown()
	log.Printf("approval 已就绪 %s；第一层，PowerShell；第二层钩子尚未启用", s.WsURL())
	<-ctx.Done()
	return nil
}

func inspect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	command := fs.String("command", "", "只分析这条命令，绝不执行")
	dir := fs.String("work-dir", ".", "假定执行目录")
	path := fs.String("policy", "", "JSON 规则包")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, _, err := newEngine(*path)
	if err != nil {
		return err
	}
	exe, err := analysis.FindPowerShell()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	result, err := e.Evaluate(ctx, protocol.CommandApprovalRequest{Version: 1, ItemID: protocol.NewID(), Plan: protocol.ExecutionPlan{
		Version: 1, Command: *command, WorkDir: abs, ShellPath: exe, Args: protocol.PowerShellArgs(), Dialect: "powershell",
		Platform: runtime.GOOS, Profile: protocol.PowerShellProfile, TimeoutMS: 300000, MaxOutputBytes: 256 << 10,
	}})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(result)
}

func send(parent context.Context, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	command := fs.String("command", "", "提交给 shell 的命令（通过后真实执行）")
	dir := fs.String("work-dir", "", "执行目录，默认 shell 根目录")
	center := fs.String("center", "ws://127.0.0.1:9000/ws", "设置中心")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 6*time.Minute)
	defer cancel()
	results := make(chan protocol.AsyncResultPayload, 16)
	name := "approval-console-" + fmt.Sprint(time.Now().UnixNano())
	c := service.New(service.Config{Name: name, Listen: "127.0.0.1:0", Center: *center, Description: "命令提交与反馈观察客户端；不是真实 LLM"})
	c.Handle("report_result", func(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
		var r protocol.AsyncResultPayload
		if err := protocol.DecodeRaw(in.Input, &r); err != nil {
			return nil, err
		}
		select {
		case results <- r:
			return map[string]string{"status": "accepted"}, nil
		default:
			return nil, errors.New("反馈队列已满")
		}
	})
	if err := c.Start(ctx); err != nil {
		return err
	}
	defer c.Shutdown()
	raw, err := c.Call(ctx, "shell", "run", protocol.ShellRunPayload{Command: *command, WorkDir: *dir}, protocol.NewID(), name)
	if err != nil {
		return err
	}
	var accepted protocol.ShellRunResult
	if err := protocol.DecodeRaw(raw, &accepted); err != nil {
		return err
	}
	fmt.Printf("已受理 item=%s；正在等待审批和执行反馈\n", accepted.ItemID)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case r := <-results:
			if r.ItemID != accepted.ItemID {
				continue
			}
			fmt.Printf("[%s] %s\n", r.Phase, r.Text)
			if r.Final {
				return nil
			}
		}
	}
}
