// Package runner 负责把一次 harness 评测真正跑起来：
// 起子集服务（真进程 + 真 WS）→ 驱动一条任务 → 采集事实 → 判定终态。
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sump/protocol"
)

// BuildBinaries 把需要的服务编译到 outDir，返回 服务名 → 二进制路径。
// 一次 run 只编译一次，多个 trial 复用——否则每个 trial 都要等一遍 go build。
func BuildBinaries(repoRoot string, outDir string, names []string) (map[string]string, error) {
	return BuildBinariesCached(repoRoot, outDir, names, false)
}

// BuildBinariesCached 与 BuildBinaries 相同，但当二进制比源码新时跳过编译。
//
// 为什么要缓存：这台机器同时开着一堆进程，反复 go build 五个服务会把换页文件顶满
// （真实报错：`The paging file is too small for this operation to complete`）。
// 判据用**源码内容指纹**而不是时间戳：git checkout / 分支切换会改时间戳却不改内容，
// 只看时间会白编；只看内容才不会拿旧二进制去测新代码。
func BuildBinariesCached(repoRoot string, outDir string, names []string, force bool) (map[string]string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		bin := filepath.Join(outDir, binaryName(name))
		stamp := bin + ".srcstamp"

		srcHash, err := sourceFingerprint(repoRoot, name)
		if err != nil {
			return nil, err
		}
		if !force {
			if prev, err := os.ReadFile(stamp); err == nil && string(prev) == srcHash {
				if _, err := os.Stat(bin); err == nil {
					out[name] = bin
					continue
				}
			}
		}

		pkg := "github.com/NouSart/sump/" + name
		cmd := exec.Command("go", "build", "-o", bin, pkg)
		cmd.Dir = repoRoot
		if b, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("构建 %s 失败: %w\n%s", name, err, b)
		}
		if err := os.WriteFile(stamp, []byte(srcHash), 0o644); err != nil {
			return nil, err
		}
		out[name] = bin
	}
	return out, nil
}

// sourceFingerprint 把"这个服务可能依赖到的 Go 源码"折成一个哈希：
// 服务目录 + protocol + service 三个目录下的全部 .go 文件（内容 + 相对路径）。
func sourceFingerprint(repoRoot, name string) (string, error) {
	dirs := []string{
		filepath.Join(repoRoot, name),
		filepath.Join(repoRoot, "protocol"),
		filepath.Join(repoRoot, "service"),
	}
	h := sha256.New()
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		var files []string
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(p, ".go") || filepath.Base(p) == "go.mod" {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		sort.Strings(files)
		for _, f := range files {
			rel, _ := filepath.Rel(repoRoot, f)
			b, err := os.ReadFile(f)
			if err != nil {
				return "", err
			}
			h.Write([]byte(rel))
			h.Write([]byte{0})
			h.Write(b)
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:32], nil
}

func binaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// 端口分配：探测后立刻关闭，操作系统可能把同一端口再发一次；
// 进程内记住已发出的端口，避免两个服务抢一个（tests/ 里被这个坑咬过一次）。
var (
	portMu    sync.Mutex
	portTaken = map[int]bool{}
)

// FreePort 返回一个本次 run 未用过的空闲端口。
func FreePort() (int, error) {
	for attempt := 0; attempt < 200; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		if claimPort(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("连续 200 次都拿到已发过的端口")
}

// claimPort 抢占一个端口号；已在本次 run 发过就返回 false。
// 单独拆出来是为了能被确定性地测到：操作系统很少连续两次给同一个端口，
// 所以"重复发放"这种错误在 FreePort 的循环里几乎撞不出来。
func claimPort(port int) bool {
	portMu.Lock()
	defer portMu.Unlock()
	if portTaken[port] {
		return false
	}
	portTaken[port] = true
	return true
}

// proc 是一个被托管的服务进程。
type proc struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}
	log  string
}

// Stack 是一套跑起来的服务栈（一次 trial 一个）。
type Stack struct {
	dir     string
	workDir string
	policy  string
	logf    func(string, ...any)
	procs   []*proc
	clients []*protocol.Client

	CenterURL   string
	MemoryURL   string
	ReasonerURL string
	Boss        *bossServer

	bin map[string]string
}

// StackOptions 控制起哪些服务。
type StackOptions struct {
	// Dir 是本次 trial 的临时工作目录（含数据库与日志）。
	Dir string
	// Bin 是 BuildBinaries 的产物。
	Bin map[string]string
	// LLMBase 指向假 OpenAI 服务。
	LLMBase string
	// ExtraServices 是除 settings-center / memory / reasoner 之外要起的服务名（如 shell、approval）。
	ExtraServices []string
	// WorkDir 是 shell/files 的根目录；为空时用 Dir/work。
	WorkDir string
	// PolicyPath 是 approval 的策略文件（评测要固定审批策略，否则结果不可比）。
	PolicyPath string
	// SkipMemory 故意不启动记忆服务：这是最真实的一种故障（依赖缺席），不该只靠 stub 模拟。
	SkipMemory bool
	// Settings 是启动后要下发到设置中心的键值（用于消融）。
	Settings map[string]string
	Logf     func(string, ...any)
}

// StartStack 起 settings-center + memory + reasoner（+ extras），并注册一个 boss 替身。
func StartStack(opts StackOptions) (*Stack, error) {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}
	s := &Stack{dir: opts.Dir, logf: opts.Logf, bin: opts.Bin, policy: opts.PolicyPath}
	s.workDir = opts.WorkDir
	if s.workDir == "" {
		s.workDir = filepath.Join(opts.Dir, "work")
	}
	if err := os.MkdirAll(s.workDir, 0o755); err != nil {
		return nil, err
	}

	centerPort, err := FreePort()
	if err != nil {
		return nil, err
	}
	s.CenterURL = fmt.Sprintf("ws://127.0.0.1:%d/ws", centerPort)
	if _, err := s.start("settings-center", "-addr", fmt.Sprintf("127.0.0.1:%d", centerPort), "-settings", ""); err != nil {
		return nil, err
	}
	if err := waitWS(s.CenterURL, 30*time.Second); err != nil {
		s.Close()
		return nil, fmt.Errorf("settings-center 未就绪: %w", err)
	}

	memPort, err := FreePort()
	if err != nil {
		s.Close()
		return nil, err
	}
	if opts.SkipMemory {
		// 故意的"依赖缺席"：不起 memory，reasoner 会在入口就停住。
		s.logf("按配置不启动 memory（依赖缺席故障）")
	} else {
		s.MemoryURL = fmt.Sprintf("ws://127.0.0.1:%d/ws", memPort)
		if _, err := s.start("memory",
			"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
			"-center", s.CenterURL,
			"-db", filepath.Join(s.dir, "memory.db"),
		); err != nil {
			s.Close()
			return nil, err
		}
		if err := waitWS(s.MemoryURL, 30*time.Second); err != nil {
			s.Close()
			return nil, fmt.Errorf("memory 未就绪: %w", err)
		}
	}

	boss, err := startBoss()
	if err != nil {
		s.Close()
		return nil, err
	}
	s.Boss = boss
	if err := registerBoss(s.CenterURL, boss.URL()); err != nil {
		s.Close()
		return nil, err
	}

	rsPort, err := FreePort()
	if err != nil {
		s.Close()
		return nil, err
	}
	s.ReasonerURL = fmt.Sprintf("ws://127.0.0.1:%d/ws", rsPort)
	if _, err := s.start("reasoner",
		"-addr", fmt.Sprintf("127.0.0.1:%d", rsPort),
		"-center", s.CenterURL,
		"-llm-base", opts.LLMBase,
		"-llm-key", "eval-key",
		"-llm-model", "frozen-model",
		"-reflect=false",
	); err != nil {
		s.Close()
		return nil, err
	}
	if err := waitWS(s.ReasonerURL, 30*time.Second); err != nil {
		s.Close()
		return nil, fmt.Errorf("reasoner 未就绪: %w", err)
	}

	// 附加服务：起来后统一等它们进名册（否则下发设置会被拒）。
	for _, name := range opts.ExtraServices {
		if err := s.startExtra(name); err != nil {
			s.Close()
			return nil, err
		}
	}

	obs, err := protocol.Dial(s.CenterURL, nil)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.clients = append(s.clients, obs)
	if err := waitRoster(obs, "reasoner", 30*time.Second); err != nil {
		s.Close()
		return nil, err
	}
	// 附加服务是异步注册的：必须等它们进名册再下发设置，
	// 否则 set_setting 会以"未注册的服务"被拒（这一步曾经时绿时红）。
	for _, name := range opts.ExtraServices {
		if err := waitRoster(obs, name, 30*time.Second); err != nil {
			s.Close()
			return nil, err
		}
	}
	if !opts.SkipMemory {
		if err := waitRoster(obs, "memory", 10*time.Second); err != nil {
			s.Close()
			return nil, err
		}
	}

	if len(opts.Settings) > 0 {
		if err := s.applySettings(opts.Settings); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// startExtra 起一个附加服务（只覆盖当前用得到的几个）。
func (s *Stack) startExtra(name string) error {
	addr, err := FreePort()
	if err != nil {
		return err
	}
	host := fmt.Sprintf("127.0.0.1:%d", addr)
	args := []string{"-addr", host, "-center", s.CenterURL}
	switch name {
	case "shell":
		args = append(args, "-root", s.workDir, "-shell", defaultShell(), "-approval", "approval")
	case "approval":
		// approval 用子命令 serve；第二层关闭（评测里没有人裁决）。
		args := []string{"serve", "-addr", host, "-center", s.CenterURL, "-portal", ""}
		if s.policy != "" {
			args = append(args, "-policy", s.policy)
		}
		_, err := s.startWithArgs("approval", args)
		return err
	case "files":
		args = append(args, "-db", filepath.Join(s.dir, "files.db"), "-root", filepath.Join(s.dir, "files"))
		_ = os.MkdirAll(filepath.Join(s.dir, "files"), 0o755)
	}
	_, err = s.startWithArgs(name, args)
	return err
}

// defaultShell 固定解释器：跨机对比时不能让它自动探测（bash/pwsh/cmd 行为不同）。
// Windows 上用 powershell（系统自带，必然存在）；pwsh 是可选安装，不能当默认。
func defaultShell() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "bash"
}

func (s *Stack) start(name string, args ...string) (*proc, error) {
	return s.startWithArgs(name, args)
}

func (s *Stack) startWithArgs(name string, args []string) (*proc, error) {
	bin, ok := s.bin[name]
	if !ok {
		return nil, fmt.Errorf("缺少二进制 %s（构建列表里没有）", name)
	}
	logPath := filepath.Join(s.dir, name+".log")
	f, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	// 子进程的工作目录固定在本次 trial 的临时目录：服务若用了相对默认路径（比如记忆的
	// 共享库 data/experience.db），产物会落在这块可丢弃的地方，而不是仓库模块目录里。
	cmd.Dir = s.dir
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("启动 %s 失败: %w", name, err)
	}
	p := &proc{name: name, cmd: cmd, done: make(chan struct{}), log: logPath}
	s.procs = append(s.procs, p)
	go func() {
		_ = cmd.Wait()
		_ = f.Close()
		close(p.done)
	}()
	s.logf("起 %s pid=%d", name, cmd.Process.Pid)
	return p, nil
}

// splitSettingKey 把 "<服务名>.<设置项>" 拆开。
//
// 必须带服务名：设置项归服务所有（例如 reasoner.tool.notify_mode），
// 少了服务名就不知道该推给谁——曾经把 tool.notify_mode 当成服务名下发，被中心拒了。
func splitSettingKey(k string) (service, key string, err error) {
	dot := strings.Index(k, ".")
	if dot <= 0 || dot == len(k)-1 {
		return "", "", fmt.Errorf("设置键 %q 不符合 <服务名>.<设置项> 的格式（例如 reasoner.tool.notify_mode）", k)
	}
	return k[:dot], k[dot+1:], nil
}

// applySettings 通过设置中心把消融项下发到各服务。
// 键的格式是 `<服务名>.<设置项>`（例如 `approval.auto_approve`）——
// 设置项归服务所有，不给服务名就不知道该推给谁。
func (s *Stack) applySettings(kv map[string]string) error {
	cli, err := protocol.Dial(s.CenterURL, nil)
	if err != nil {
		return err
	}
	defer cli.Close()
	for k, v := range kv {
		svc, key, err := splitSettingKey(k)
		if err != nil {
			return err
		}
		raw, err := protocol.EncodePayload(protocol.SetSettingPayload{Service: svc, Key: key, Value: v})
		if err != nil {
			return err
		}
		env, err := protocol.NewEnvelope(protocol.TypeJump, "eval", "settings-center", "", protocol.JumpPayload{Action: "set_setting", Input: raw})
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := cli.Call(ctx, env)
		cancel()
		if err != nil {
			return fmt.Errorf("下发设置 %s 失败: %w", k, err)
		}
		var rp protocol.ResponsePayload
		if err := resp.DecodePayload(&rp); err != nil {
			return err
		}
		if !rp.OK {
			return fmt.Errorf("下发设置 %s 被拒: %s", k, rp.Error)
		}
		s.logf("设置 %s=%s 已下发", k, v)
	}
	// 设置是异步推给服务的，给一拍时间生效。
	time.Sleep(300 * time.Millisecond)
	return nil
}

// Close 收摊：先关客户端，再杀进程。
func (s *Stack) Close() {
	for _, c := range s.clients {
		_ = c.Close()
	}
	if s.Boss != nil {
		s.Boss.Close()
	}
	for _, p := range s.procs {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
	}
}

// waitWS 重试拨号直到可连（或进程已退出）。
func waitWS(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		c, err := protocol.Dial(url, nil)
		if err == nil {
			_ = c.Close()
			return nil
		}
		last = err
		time.Sleep(150 * time.Millisecond)
	}
	return last
}

func waitRoster(obs *protocol.Client, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		raw, _ := protocol.EncodePayload(nil)
		env, err := protocol.NewEnvelope(protocol.TypeJump, "eval", "settings-center", "", protocol.JumpPayload{Action: "roster", Input: raw})
		if err == nil {
			if resp, err := obs.Call(ctx, env); err == nil {
				var rp protocol.ResponsePayload
				if err := resp.DecodePayload(&rp); err == nil && rp.OK {
					var r protocol.RosterPayload
					if err := protocol.DecodeRaw(rp.Data, &r); err == nil {
						for _, svc := range r.Services {
							if svc.Name == name {
								cancel()
								return nil
							}
						}
					}
				}
			}
		}
		cancel()
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("名册里等不到 %s", name)
}

// ---------- boss 替身 ----------

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// bossServer 扮演「入口方」：接收 deliver，把文本交给评测器。
type bossServer struct {
	ln        net.Listener
	srv       *http.Server
	mu        sync.Mutex
	delivered []string
}

func startBoss() (*bossServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &bossServer{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", b.handleWS)
	b.srv = &http.Server{Handler: mux}
	go func() { _ = b.srv.Serve(ln) }()
	return b, nil
}

func (b *bossServer) URL() string { return "ws://" + b.ln.Addr().String() + "/ws" }

func (b *bossServer) Close() { _ = b.srv.Close() }

// Delivered 返回收到的交付文本。
func (b *bossServer) Delivered() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.delivered))
	copy(out, b.delivered)
	return out
}

// Reset 清空——每次 trial 开头调用。
func (b *bossServer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.delivered = nil
}

func (b *bossServer) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			continue
		}
		if env.Type != protocol.TypeJump {
			continue
		}
		var jp protocol.JumpPayload
		if err := env.DecodePayload(&jp); err != nil {
			continue
		}
		if jp.Action != "deliver" {
			continue
		}
		var p struct {
			Text string `msgpack:"text"`
		}
		_ = protocol.DecodeRaw(jp.Input, &p)
		b.mu.Lock()
		b.delivered = append(b.delivered, p.Text)
		b.mu.Unlock()

		resp, _ := protocol.NewResponse(env, "eval-boss", protocol.ResponsePayload{OK: true})
		if raw, err := protocol.Marshal(resp); err == nil {
			_ = ws.WriteMessage(websocket.BinaryMessage, raw)
		}
	}
}

func registerBoss(centerURL, bossURL string) error {
	c, err := protocol.Dial(centerURL, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	env, err := protocol.NewEnvelope(protocol.TypeRegister, "eval-boss", "settings-center", "", protocol.RegisterPayload{
		Name:        "eval-boss",
		Addr:        bossURL,
		Title:       "评测入口",
		Description: "评测器扮演的入口方",
		Provides:    []protocol.Provide{{Action: "deliver", Input: "{text}", Output: "回执"}},
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, env)
	if err != nil {
		return err
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		return err
	}
	if !rp.OK {
		return fmt.Errorf("boss 注册失败: %s", rp.Error)
	}
	return nil
}

// HistoryEntry 是记忆里的一条记录（只取判定需要的最小字段）。
type HistoryEntry struct {
	Seq     int64  `msgpack:"seq" json:"seq"`
	Role    string `msgpack:"role" json:"role"`
	Kind    string `msgpack:"kind" json:"kind"`
	Content string `msgpack:"content" json:"content"`
	At      string `msgpack:"at" json:"at"`
}

// WorkDir 是 shell/files 的根目录（文件类断言相对它解析）。
func (s *Stack) WorkDir() string { return s.workDir }

// HasMemory 表示记忆服务在本次 trial 里起着。
// 不起时（依赖缺席场景）拿不到轨迹——这是被测环境的事实，不是契约违规。
func (s *Stack) HasMemory() bool { return s.MemoryURL != "" }

// History 拉取整个会话的记录（含轨迹）。
func (s *Stack) History(conversationID string) ([]HistoryEntry, error) {
	c, err := protocol.Dial(s.MemoryURL, nil)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	raw, err := protocol.EncodePayload(protocol.HistoryPayload{ConversationID: conversationID, AllTasks: true, Limit: 500})
	if err != nil {
		return nil, err
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "eval", "memory", "", protocol.JumpPayload{Action: "history", Input: raw})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, env)
	if err != nil {
		return nil, err
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		return nil, err
	}
	if !rp.OK {
		return nil, fmt.Errorf("history 失败: %s", rp.Error)
	}
	var out struct {
		Messages []HistoryEntry `msgpack:"messages"`
	}
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		return nil, err
	}
	return out.Messages, nil
}

// Send 向 reasoner 发一条用户消息（评测的入口动作）。
func (s *Stack) Send(conversationID, text string) error {
	c, err := protocol.Dial(s.ReasonerURL, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	raw, err := protocol.EncodePayload(map[string]any{"text": text, "conversation_id": conversationID})
	if err != nil {
		return err
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "eval", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	if err != nil {
		return err
	}
	env.Boss = "eval-boss"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, env)
	if err != nil {
		return err
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		return err
	}
	if !rp.OK {
		return fmt.Errorf("user_message 未被受理: %s", rp.Error)
	}
	return nil
}
