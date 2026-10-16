// Package server 是 shell 服务的核心：把命令执行动作注册到服务骨架
// （注册 / 心跳 / 名册 / 断线自愈 / 告别见 service 包）。
//
// 形状（代理商：代客执行，自己不设判断）：
//
//	run 受理（立即回执，带 item 句柄）
//	  → 审批（接缝，见 approval.go；未通过即补报 rejected 结束，不执行）
//	  → 执行（并行上限 + 待执行队列上限；满即拒绝，不排队）
//	  → report_result 补报（approved / rejected / done / failed；结果可能晚于调用方收尾）
//
// 执行不在受理路径上：调用方拿到回执即走人，后续每条进展各自一跳送达。
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/service"
)

// 默认值（引导值；可被设置中心的覆盖值改写）。
const (
	defaultTimeout      = 5 * time.Minute
	defaultMaxOutput    = 256 << 10 // 单流输出上限 256KiB
	defaultConcurrency  = 8
	defaultQueueLimit   = 32
	defaultApprovalWait = time.Minute
	maxPendingApprovals = 64
)

var serviceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Config 是 shell 服务的启动配置（引导只靠它：自己的名字/地址 + 设置中心地址）。
type Config struct {
	Name              string        // 服务名（注册用）
	Listen            string        // 监听地址 host:port
	Center            string        // 设置中心 WS 地址
	HeartbeatInterval time.Duration // 心跳间隔（默认 15s）
	Root              string        // 工作目录根（默认 = 进程当前目录；命令的 work_dir 必须落在其内）
	Shell             string        // 命令解释器（默认自动探测：bash → pwsh → cmd / sh）
	Timeout           time.Duration // 单条命令执行超时（默认 5 分钟）
	MaxOutputBytes    int64         // 单流输出上限（默认 256KiB）
	Concurrency       int           // 并行执行上限（默认 8）
	QueueLimit        int           // 待执行队列上限（默认 32；满即拒绝执行）
	ApprovalService   string        // 审批服务名（默认空 = 未接入，一律未通过）
	ApprovalTimeout   time.Duration // 等审批超时（默认 1 分钟；超时按未通过处理）
	Approver          Approver      // 裁决实现（默认 nil = fail-closed，见 approval.go）
}

// Server 是 shell 服务实例（服务骨架 + 执行调度）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger

	setMu           sync.Mutex // 运行时可设项锁（设置下发 / 拉取即时生效）
	root            string     // 工作目录根（绝对路径、已解析链接）
	shell           string     // 命令解释器（空 = 自动探测）
	timeout         time.Duration
	maxOutput       int64
	conc            int
	queueLimit      int
	approvalService string
	approvalWait    time.Duration

	schedMu          sync.Mutex // 调度锁：running / waiting 只在此锁下读写
	running          int
	waiting          []*job
	pendingApprovals chan struct{}
}

// job 是一条已受理的命令：审批通过后进队列 / 执行，全程带着补报所需的信息。
type job struct {
	item          string // 受理句柄（补报原样带回）
	caller        string // 受理时的调用方（补报目标）
	trace         string // 链路 id（原样沿用）
	boss          string // 受理时的 boss（原样沿用）
	command       string
	workDir       string
	plan          *protocol.ExecutionPlan
	environment   []string
	approval      *protocol.CommandApprovalResult
	approvedUntil time.Time
	progressReady chan struct{} // 先完成 approved 的投递尝试，再允许执行结果发出。
}

// New 创建实例并注册动作与设置项。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = defaultMaxOutput
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.QueueLimit <= 0 {
		cfg.QueueLimit = defaultQueueLimit
	}
	if cfg.ApprovalTimeout <= 0 {
		cfg.ApprovalTimeout = defaultApprovalWait
	}
	if cfg.Root == "" {
		wd, err := os.Getwd()
		if err != nil {
			wd = "."
		}
		cfg.Root = wd
	}
	if logger == nil {
		logger = log.Default()
	}
	root, err := resolveRoot(cfg.Root)
	if err != nil {
		logger.Printf("工作目录根不可用（沿用原值，命令会被拒）: %v", err)
		root = cfg.Root
	}

	s := &Server{
		cfg: cfg, logger: logger,
		root:             root,
		shell:            strings.TrimSpace(cfg.Shell),
		timeout:          cfg.Timeout,
		maxOutput:        cfg.MaxOutputBytes,
		conc:             cfg.Concurrency,
		queueLimit:       cfg.QueueLimit,
		approvalService:  cfg.ApprovalService,
		approvalWait:     cfg.ApprovalTimeout,
		pendingApprovals: make(chan struct{}, maxPendingApprovals),
	}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		ActionTimeout:     15 * time.Second, // 受理/校验是短动作（执行不在请求路径上，不占这里的超时）
		Description:       "命令执行：受理即回执（审批通过后并行执行），结果由 report_result 补报送达",
		Provides: []protocol.Provide{
			{
				Action: "run",
				Tool:   true,
				Async:  true,
				Input:  "{command: 要执行的命令行（交给服务的 shell 解释）; work_dir?: 工作目录（须在服务根目录内，可省略）}",
				Output: "受理回执 {item_id, phase}；执行结果稍后由 report_result 补报送达（可能在你收尾之后才到）",
			},
		},
		Logger: logger,
	})

	// 可设置项：预设 = 本服务的启动引导值（来源由服务自己决定，见 main.go 的 flag）。
	s.HandleSetting("exec.root", root, func(v string) error {
		p, err := resolveRoot(v)
		if err != nil {
			return err
		}
		s.setMu.Lock()
		s.root = p
		s.setMu.Unlock()
		return nil
	})
	s.HandleSetting("exec.shell", s.shell, func(v string) error {
		v = strings.TrimSpace(v)
		if v != "" {
			if _, err := lookShell(v); err != nil {
				return fmt.Errorf("不可用的解释器 %q: %v", v, err)
			}
		}
		s.setMu.Lock()
		s.shell = v
		s.setMu.Unlock()
		return nil
	})
	s.HandleSetting("exec.timeout_ms", strconv.FormatInt(cfg.Timeout.Milliseconds(), 10), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1000 {
			return fmt.Errorf("需要 ≥1000 的整数（毫秒），收到 %q", v)
		}
		s.setMu.Lock()
		s.timeout = time.Duration(n) * time.Millisecond
		s.setMu.Unlock()
		return nil
	})
	s.HandleSetting("exec.max_output_kb", strconv.FormatInt(cfg.MaxOutputBytes>>10, 10), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("需要 ≥1 的整数（KB），收到 %q", v)
		}
		s.setMu.Lock()
		s.maxOutput = int64(n) << 10
		s.setMu.Unlock()
		return nil
	})
	s.HandleSetting("exec.concurrency", strconv.Itoa(cfg.Concurrency), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("需要 ≥1 的整数，收到 %q", v)
		}
		s.setMu.Lock()
		s.conc = n
		s.setMu.Unlock()
		return nil
	})
	s.HandleSetting("exec.queue_limit", strconv.Itoa(cfg.QueueLimit), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("需要 ≥1 的整数，收到 %q", v)
		}
		s.setMu.Lock()
		s.queueLimit = n
		s.setMu.Unlock()
		return nil
	})
	s.HandleSetting("approval.service", cfg.ApprovalService, func(v string) error {
		v = strings.TrimSpace(v)
		if v != "" && !serviceNamePattern.MatchString(v) {
			return errors.New("审批服务名无效")
		}
		s.setMu.Lock()
		s.approvalService = v
		s.setMu.Unlock()
		return nil
	})
	s.HandleSetting("approval.timeout_ms", strconv.FormatInt(cfg.ApprovalTimeout.Milliseconds(), 10), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1000 {
			return fmt.Errorf("需要 ≥1000 的整数（毫秒），收到 %q", v)
		}
		s.setMu.Lock()
		s.approvalWait = time.Duration(n) * time.Millisecond
		s.setMu.Unlock()
		return nil
	})

	s.Handle("run", s.actionRun)
	return s
}

// Start 启动服务骨架；设置同步由骨架负责（注册后全量下发 + 拉取兜底）。
func (s *Server) Start(ctx context.Context) error {
	return s.Service.Start(ctx)
}

// RunningCount 返回正在执行的命令数（退出时记日志用）。
func (s *Server) RunningCount() int {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.running
}

// QueueCount 返回队列中等待执行的命令数。
func (s *Server) QueueCount() int {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return len(s.waiting)
}

// ---------- 受理 ----------

// actionRun 受理一条命令：立即回执（不等审批、不等执行，后续由 report_result 补报）。
// 同步拒绝的只有“请求本身不合法”：命令为空、工作目录越界或不可访问。
func (s *Server) actionRun(_ context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	if len(in.Input) > 64<<10 {
		return nil, errors.New("run: 请求超过 64 KiB")
	}
	var p protocol.ShellRunPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Command) == "" {
		return nil, errors.New("run: command 必填")
	}
	if len(p.Command) > 16<<10 {
		return nil, errors.New("run: 命令超过 16 KiB")
	}
	dir, err := s.resolveWorkDir(p.WorkDir)
	if err != nil {
		return nil, err
	}
	select {
	case s.pendingApprovals <- struct{}{}:
	default:
		return nil, errors.New("run: 待审批队列已满，请稍后重试")
	}
	j := &job{
		item:          protocol.NewID(),
		caller:        env.From,
		trace:         env.Trace,
		boss:          env.Boss,
		command:       p.Command,
		workDir:       dir,
		progressReady: make(chan struct{}),
	}
	s.logger.Printf("受理命令 %s（来自 %s，命令 %d 字节）", j.item, j.caller, len(j.command))
	go s.process(j)
	return protocol.ShellRunResult{ItemID: j.item, Phase: "queued"}, nil
}

// process 走完一条命令的全程：审批 → 排队 / 执行 → 补报。
func (s *Server) process(j *job) {
	defer func() { <-s.pendingApprovals }()
	svc := s.approvalServiceNow()
	actx, cancel := context.WithTimeout(context.Background(), s.approvalWaitNow())
	verr := s.approveJob(actx, j, svc)
	cancel()
	if verr != nil {
		s.logger.Printf("命令 %s 未通过审批: %v", j.item, verr)
		s.report(j, "rejected", fmt.Sprintf("未执行：审批未通过——%v\n$ %s", verr, j.command),
			protocol.ShellEventData{Command: j.command, WorkDir: j.workDir, Error: verr.Error()})
		return
	}
	state, err := s.enqueue(j)
	if err != nil {
		s.logger.Printf("命令 %s 被拒: %v", j.item, err)
		s.report(j, "rejected", fmt.Sprintf("未执行：%v\n$ %s", err, j.command),
			protocol.ShellEventData{Command: j.command, WorkDir: j.workDir, Error: err.Error()})
		return
	}
	s.report(j, "approved", fmt.Sprintf("审批通过，%s\n$ %s", state, j.command),
		protocol.ShellEventData{Command: j.command, WorkDir: j.workDir})
	close(j.progressReady)
}

// ---------- 调度：并行上限 + 待执行队列 ----------

// enqueue 把已通过审批的命令交给调度：有空闲槽立即执行；否则进等待队列（满即拒绝）。
// 返回的字符串是给补报用的状态描述（已开始执行 / 已排队）。
func (s *Server) enqueue(j *job) (string, error) {
	s.schedMu.Lock()
	if s.running < s.concNow() {
		s.running++
		s.schedMu.Unlock()
		go s.execute(j)
		return "准备执行", nil
	}
	if len(s.waiting) >= s.queueLimitNow() {
		n, running := len(s.waiting), s.running
		s.schedMu.Unlock()
		return "", fmt.Errorf("执行队列已满（上限 %d：队列中 %d 条，正在执行 %d 条）", s.queueLimitNow(), n, running)
	}
	s.waiting = append(s.waiting, j)
	ahead := len(s.waiting) - 1 // 不含自己：真正排在自己前面的条数
	s.schedMu.Unlock()
	return fmt.Sprintf("已排队（前方 %d 条）", ahead), nil
}

// execute 执行一条命令并补报结果；结束后让出槽位、拉起下一条等待中的命令。
func (s *Server) execute(j *job) {
	if j.progressReady != nil {
		<-j.progressReady
	}
	if err := j.validateBeforeExecution(); err != nil {
		s.report(j, "rejected", err.Error(), protocol.ShellEventData{Command: j.command, WorkDir: j.workDir, Error: err.Error()})
		s.next()
		return
	}
	start := time.Now()
	limit := s.maxOutputNow() // 每次执行只读一次上限：截断与补报文案共用，杜绝两次读数不一致
	if j.plan != nil {
		limit = j.plan.MaxOutputBytes
	}
	res, err := s.runCommand(j, limit)
	res.DurationMS = time.Since(start).Milliseconds()
	switch {
	case err != nil:
		if res.Error == "" {
			res.Error = err.Error()
		}
		s.report(j, "failed", fmt.Sprintf("未能执行：%v\n$ %s", err, j.command), res)
	default:
		s.report(j, "done", renderDone(j, res, limit), res)
	}
	s.next()
}

// next 结束一条命令的执行：让出槽位；有等待中的命令就立即补位（保持并行度）。
func (s *Server) next() {
	s.schedMu.Lock()
	if len(s.waiting) == 0 {
		s.running--
		s.schedMu.Unlock()
		return
	}
	j := s.waiting[0]
	s.waiting = s.waiting[1:]
	s.schedMu.Unlock()
	go s.execute(j)
}

// ---------- 补报 ----------

// report 向调用方补报一条进展（受理回执已带走 item 句柄，这里只送后续消息）。
// approved 不是终态（其后还跟着执行结果），其余阶段都是终态。
func (s *Server) report(j *job, phase, text string, data protocol.ShellEventData) {
	if j.caller == "" {
		s.logger.Printf("命令 %s 无调用方，补报丢弃（phase=%s）", j.item, phase)
		return
	}
	data.Approval = j.approval
	if j.approval != nil && (phase == "approved" || phase == "rejected") {
		text = fmt.Sprintf("[审批 %s / %s / 规则 %s] %s\n%s", j.approval.Decision, j.approval.ReasonCode, j.approval.PolicyVersion, j.approval.Feedback.Summary, text)
		if len(j.approval.Feedback.RuleIDs) > 0 {
			text += "\n命中规则：" + strings.Join(j.approval.Feedback.RuleIDs, ", ")
		}
		if len(j.approval.Feedback.Unknowns) > 0 {
			text += "\n未覆盖结构：" + strings.Join(j.approval.Feedback.Unknowns, ", ")
		}
	}
	p := protocol.AsyncResultPayload{ItemID: j.item, Phase: phase, Text: text, Final: phase != "approved"}
	if raw, err := protocol.EncodePayload(data); err == nil {
		p.Data = raw
	} else {
		s.logger.Printf("补报载荷编码失败（结构化补充丢弃）: %v", err)
	}
	s.logger.Printf("补报 %s（phase=%s, final=%v）→ %s", j.item, phase, p.Final, j.caller)
	// 在当前后台 job 内顺序投递，避免 done 抢在 approved 之前抵达并终结模型记账。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.deliverReport(ctx, j, p); err != nil {
		s.logger.Printf("补报 %s 未确认送达: %v", j.item, err)
		s.Report("shell_feedback", "异步结果未确认送达；item="+j.item)
	}
}

// 受理回执与后台补报走不同连接，补报可能先到。只有明确 ignored
// （对端尚未登记 item）才短暂重试；网络状态不明时不重发，避免重复启动模型轮次。
func (s *Server) deliverReport(ctx context.Context, j *job, p protocol.AsyncResultPayload) error {
	for {
		raw, err := s.Call(ctx, j.caller, "report_result", p, j.trace, j.boss)
		if err != nil {
			return err
		}
		var receipt struct {
			Status string `msgpack:"status"`
		}
		if err := protocol.DecodeRaw(raw, &receipt); err != nil {
			return fmt.Errorf("反馈回执无效: %w", err)
		}
		if receipt.Status != "ignored" {
			return nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// ---------- 运行时取值（设置下发即时生效） ----------

func (s *Server) rootNow() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.root
}

func (s *Server) shellNow() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.shell
}

func (s *Server) timeoutNow() time.Duration {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.timeout
}

func (s *Server) maxOutputNow() int64 {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.maxOutput
}

func (s *Server) concNow() int {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.conc
}

func (s *Server) queueLimitNow() int {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.queueLimit
}

func (s *Server) approvalServiceNow() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.approvalService
}

func (s *Server) approvalWaitNow() time.Duration {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.approvalWait
}

// oneLine 把命令压成一行（日志用）。
func oneLine(cmd string) string {
	return strings.Join(strings.Fields(cmd), " ")
}

// resolveRoot 校验工作目录根：必须是存在的目录，返回解析链接后的绝对路径。
func resolveRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("工作目录根无效 %q: %v", dir, err)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("工作目录根不存在或不是目录: %s", abs)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}
