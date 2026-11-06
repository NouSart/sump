// Package service 是 SUMP v2 的服务骨架（Go 便捷库，非服务）：
// 注册 / 心跳 / 名册 / 断线自愈（重连 + 重新注册）/ 优雅下线（bye）/ SPv2 服务端 / 跳转调用。
// 协议本身见 protocol 包；非 Go 服务只需遵循 docs/concepts/protocol.md，不必使用本库。
package service

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// Config 是服务启动配置（引导只靠它：自己的名字/地址 + 设置中心地址）。
type Config struct {
	Name              string             // 服务名（注册用）
	Title             string             // 中文短名（面板导航用，如「审批」）
	Listen            string             // 监听地址 host:port
	Center            string             // 设置中心 WS 地址
	HeartbeatInterval time.Duration      // 心跳间隔（默认 15s；随名片声明，中心据此判定离线）
	ActionTimeout     time.Duration      // 单次动作处理超时（默认 30s）
	Description       string             // 名片：功能简介
	Provides          []protocol.Provide // 名片：对外提供的动作
	Logger            *log.Logger        // 日志（默认 log.Default）
	// MaxMessageBytes 限制接收的单条 SPv2 消息（含帧）：入站 call/event/补报、
	// 出站 Call/Fire 的响应，以及中心注册/名册事件。正数生效；零或负数用默认 1 MiB 上限。
	// 限额在读取/解码前执行，超限关闭该连接；不限制发送方向。
	MaxMessageBytes int64
	// SecretFile 是秘密项（HandleSecret）的本地持久化文件路径。
	// 留空 = <name> 的默认路径（data/<name>-secret.json）；传 "-" 表示只放内存不落盘。
	SecretFile string
}

// Service 是服务实例：一条到设置中心的连接（注册 / 心跳 / 名册）+ 一个 SPv2 服务端。
type Service struct {
	cfg    Config
	logger *log.Logger

	native map[string]NativeMethod

	settingsMu    sync.Mutex        // 设置声明与生效值
	settings      []settingEntry    // 已声明的设置项（按声明顺序）
	settingIndex  map[string]int    // 键 → settings 下标
	settingValues map[string]string // 最近一次成功应用的生效值（初始 = 预设）
	secrets       map[string]string // 秘密项的值（不进设置中心；由本服务持久化）
	// appliedSecrets 记录骨架是否成功应用过某个秘密项：落盘失败时只有这里为真才回滚。
	// 启动参数可能在骨架之外直接应用过值（如 reasoner 的 -llm-key），骨架不知道它，
	// 贸然回滚会把一个能用的值清掉。
	appliedSecrets map[string]bool
	announceSeq    int64      // 设置回传（AnnounceSetting）的单调序号（settingsMu 保护）
	applyMu        sync.Mutex // 应用串行化：多个下发不并发进回调

	mu           sync.Mutex
	roster       map[string]protocol.ServiceCard
	selfURL      string
	centerNative *spv2.Client    // 当前 SPv2 中心连接（nil = 未连接）
	spv2Srv      *spv2.Server    // /spv2 服务端
	closed       bool            // Shutdown 后为真（阻止继续重连）
	started      bool            // Start 后为真（阻止再声明设置项）
	regs         int64           // 注册成功次数（日志用）
	lifeCtx      context.Context // Start 创建、Shutdown 取消；后台循环据此退出
	lifeCancel   context.CancelFunc
	bg           sync.WaitGroup // 生命周期后台循环（只含可取消、可快速退出的）

	repMu       sync.Mutex     // 错误上报退避锁
	repThrottle reportThrottle // 错误上报防刷（见 report.go）

	// writeSecretsHook 仅供测试：非 nil 时替代 writeSecrets，用来稳定复现落盘失败。
	// 必须在 Start 前设置；运行期只读。
	writeSecretsHook func(map[string]string) error

	httpSrv *http.Server
}

// New 创建服务实例（尚未启动）。
func New(cfg Config) *Service {
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 15 * time.Second
	}
	if cfg.ActionTimeout <= 0 {
		cfg.ActionTimeout = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.SecretFile == "" {
		cfg.SecretFile = filepath.Join("data", cfg.Name+"-secret.json")
	}
	return &Service{
		cfg:            cfg,
		logger:         cfg.Logger,
		native:         map[string]NativeMethod{},
		settingIndex:   map[string]int{},
		settingValues:  map[string]string{},
		secrets:        map[string]string{},
		appliedSecrets: map[string]bool{},
		roster:         map[string]protocol.ServiceCard{},
	}
}

// NativeHandler 是原生 SPv2.1 method：直接读 call.Frame.Body，自己 Reply/ReplyError。
type NativeHandler func(ctx context.Context, call *spv2.Call) error

// NativeMethod 把一个原生 handler 与其服务卡声明绑在一起。
type NativeMethod struct {
	Method  spv2.Method
	Handler NativeHandler
}

// HandleNative 注册原生 SPv2.1 method。
func (s *Service) HandleNative(m spv2.Method, h NativeHandler) {
	if m.Name == "" || h == nil {
		s.logger.Printf("忽略非法原生 method 注册: name=%q", m.Name)
		return
	}
	if m.Version == 0 {
		m.Version = 1
	}
	if m.Shape == "" {
		m.Shape = spv2.ShapeCall
	}
	if m.Input == nil {
		m.Input = genericSchema()
	}
	if m.Output == nil {
		m.Output = genericSchema()
	}
	s.native[m.Name] = NativeMethod{Method: m, Handler: h}
}

// WsURL 返回自己的对外地址（Start 成功后有效）。
func (s *Service) WsURL() string { return s.selfURL }

// Roster 返回当前名册快照（按服务名排序）。
func (s *Service) Roster() []protocol.ServiceCard {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.ServiceCard, 0, len(s.roster))
	for _, c := range s.roster {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Start 启动 SPv2 服务端并**同步**接入设置中心（首次注册失败即返回错误——
// 引导配置错误要立刻可见）；之后由后台循环负责断线自愈（重连 + 重新注册）。
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()

	// 生命周期 ctx：与启动 ctx 解耦——启动 ctx 只约束"注册这一段"，取消它
	// 不该让已经在跑的服务停止收发；真正的退出由 Shutdown 取消。
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.lifeCtx, s.lifeCancel = lifeCtx, lifeCancel
	s.mu.Unlock()

	// 秘密项先落地：它们不在中心那边（中心没有值可下发），服务得自己带起来。
	s.loadSecrets()

	if err := s.buildSPv2(); err != nil {
		lifeCancel()
		return fmt.Errorf("构建 SPv2 服务卡失败: %w", err)
	}

	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		lifeCancel()
		return err
	}
	s.selfURL = "ws://" + ln.Addr().String() + "/spv2"

	mux := http.NewServeMux()
	mux.Handle("/spv2", s.spv2Srv.Handler())
	s.httpSrv = &http.Server{Handler: mux}
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("%s 服务端退出: %v", s.cfg.Name, err)
		}
	}()
	s.logger.Printf("%s 监听 %s", s.cfg.Name, s.selfURL)
	// 内部 WS 不做身份认证：信封里的 from 是发送方自称的字段，只在敏感动作上比对
	// （见 docs/concepts/protocol.md 的「信任边界」）。这条日志是部署前提的显式回声——
	// 把端口暴露到不可信网络之前，看到它的那个人应该先停一下。
	if addr := ln.Addr().String(); addr != "" {
		host, _, _ := net.SplitHostPort(addr)
		if host != "127.0.0.1" && host != "::1" && host != "localhost" {
			s.logger.Printf("%s 监听在非回环地址 %s：内部协议无身份认证，请确认仅可信内网可达", s.cfg.Name, host)
		}
	}

	if err := s.connectAndRegister(ctx); err != nil {
		lifeCancel()
		_ = s.httpSrv.Close() // 注册失败即放弃启动：回收监听，不留“半个服务”
		return fmt.Errorf("注册失败: %w", err)
	}
	s.goBackground(func() { s.watchCenter(lifeCtx) })
	return nil
}

// goBackground 启动一个生命周期后台循环；Shutdown 开始后不再接受新任务。
// 循环必须监听 lifeCtx 并在取消后尽快退出（不等待网络超时）。
func (s *Service) goBackground(fn func()) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.bg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.bg.Done()
		fn()
	}()
}

func (s *Service) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// waitBackground 等待后台循环退出；只用于"已经取消并断开连接"之后，且必须有上限。
func (s *Service) waitBackground() {
	done := make(chan struct{})
	go func() {
		s.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.logger.Printf("%s 关闭等待超时：仍有后台循环未退出", s.cfg.Name)
	}
}

// Shutdown 尽力告别（bye，中心立即摘除名册）并关闭服务端与中心连接。
// 顺序：先取消/断开让阻塞的读写立刻失败，再等后台循环退出（有上限），绝不持锁等待。
func (s *Service) Shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.waitBackground()
		return
	}
	s.closed = true
	n := s.centerNative
	s.centerNative = nil
	cancel := s.lifeCancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if n != nil {
		if ev, err := spv2.NewEvent(spv2.NewID(), spv2.ServiceRef(s.cfg.Name), spv2.ServiceRef("settings-center"), "heartbeat",
			protocol.HeartbeatPayload{Status: "bye"}); err == nil {
			sent := make(chan struct{})
			go func() {
				_ = n.Send(ev)
				close(sent)
			}()
			select {
			case <-sent:
			case <-time.After(time.Second):
			}
		}
		_ = n.Close()
	}
	if s.httpSrv != nil {
		_ = s.httpSrv.Close()
	}
	s.waitBackground()
}

// ---------- 到设置中心的连接：注册 / 心跳 / 断线自愈 ----------

func (s *Service) setRoster(r protocol.RosterPayload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roster = make(map[string]protocol.ServiceCard, len(r.Services))
	for _, c := range r.Services {
		s.roster[c.Name] = c
	}
}

// ---------- 出站：跳转调用 ----------

// Call 访问目标服务：发一跳并等响应（短连接）。to 支持 "self" 与服务名（查名册）。
// 送达后的失败不重试——可能已被处理，重试需要幂等键（待定）。
func (s *Service) Call(ctx context.Context, to, action string, payload any, trace, boss string) (msgpack.RawMessage, error) {
	name, addr, err := s.Resolve(to)
	if err != nil {
		return nil, err
	}
	raw, err := encodeInput(payload)
	if err != nil {
		return nil, err
	}
	return s.callSPv2(ctx, name, addr, action, raw, trace)
}

// Fire 后台发起一跳（发出即完；完成与否仅记日志）。
// 失败时向设置中心上报一条（kind=fire）——链路跳转未送达属于值得集中可见的错误。
func (s *Service) Fire(to, action string, payload any, trace, boss string) {
	s.mu.Lock()
	base := s.lifeCtx
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return // 关闭后不再发起新跳转（避免测试结束后还在打日志）
	}
	if base == nil {
		base = context.Background()
	}
	go func() {
		ctx, cancel := context.WithTimeout(base, 60*time.Second)
		defer cancel()
		if _, err := s.Call(ctx, to, action, payload, trace, boss); err != nil {
			if ctx.Err() != nil || s.isClosed() {
				return // 关闭导致的中断不是"未送达"，也不该在 Shutdown 之后写日志
			}
			s.logger.Printf("跳转 %s(%s) 未确认送达: %v", to, action, err)
			if to != "settings-center" {
				s.Report("fire", fmt.Sprintf("跳转 %s(%s) 未送达: %v", to, action, err))
			}
		}
	}()
}

// Resolve 解析跳转目标：支持 "self" 与服务名（查名册）。
func (s *Service) Resolve(to string) (name, addr string, err error) {
	if to == "self" {
		return s.cfg.Name, s.selfURL, nil
	}
	s.mu.Lock()
	card, ok := s.roster[to]
	s.mu.Unlock()
	if !ok {
		return "", "", fmt.Errorf("未知目标服务: %s", to)
	}
	return card.Name, card.Addr, nil
}

// encodeInput 把任意载荷编码为 msgpack；已是 RawMessage 的原样透传。
func encodeInput(payload any) (msgpack.RawMessage, error) {
	if payload == nil {
		return nil, nil
	}
	if raw, ok := payload.(msgpack.RawMessage); ok {
		return raw, nil
	}
	return protocol.EncodePayload(payload)
}
