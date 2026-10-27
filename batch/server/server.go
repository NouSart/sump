// Package server 是 batch 服务：把"一次做 N 项工作"压成一次调用，并让调用方决定什么时候被叫醒。
//
// 边界（与 shell 的"无队列"同一套哲学）：
//   - 受理即回执：校验通过、任务入库、开始派发后立即返回，不等任何一项完成；
//   - 任务级不排队：全局在飞槽位不够就同步拒绝（BATCH_BUSY）；
//   - 任务内可以等：第 17 项等前 16 项，那是调用方自己声明的 50 项在等自己的槽位；
//   - 不重试、不去重、不解释 input。
package server

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/QAQ-awa-QAQ/sump/batch/store"
	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/service"
)

// Config 是 batch 的启动配置（引导只靠它：自己的名字/地址 + 设置中心地址 + 上限 + 落盘）。
type Config struct {
	Name              string        // 服务名（注册用）
	Listen            string        // 监听地址 host:port
	Center            string        // 设置中心 WS 地址
	HeartbeatInterval time.Duration // 心跳间隔（默认 15s）

	DBPath string // 任务账本路径（空 = data/batch.db）

	DefaultConcurrency int           // 单任务并发默认值（默认 8）
	MaxConcurrency     int           // 单任务并发上限（默认 64）
	MaxInflight        int           // 全局在飞槽位上限（默认 64）
	MaxItems           int           // 单任务最多项数（默认 1000）
	ItemTimeout        time.Duration // 单项超时（默认 60s）
	PreviewBytes       int           // 每项摘要上限（默认 2048）
	JobTTL             time.Duration // 终态任务可查询时长（默认 1h）
	ResumeOnStart      bool          // 重启后是否自动恢复被转暂停的任务（默认 false）
}

// Server 是 batch 服务实例（服务骨架 + 内存里的活动任务 + 落盘账本）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger
	store  *store.Store

	setMu        sync.Mutex
	defConc      int
	maxConc      int
	maxInflight  int
	maxItems     int
	itemTimeout  time.Duration
	previewBytes int
	jobTTL       time.Duration
	resumeOnBoot bool

	mu       sync.Mutex
	jobs     map[string]*runningJob // 内存里的活动任务（终态即删）
	inflight int                    // 已预留的槽位总数（跨任务）

	stopOnce sync.Once
	stopped  chan struct{}

	// 生命周期：Shutdown 先取消，再等后台调度/清理循环退出，最后关账本。
	// runMu 保护 closing 与 runWG.Add：关闭开始后不再 Add（避免 Add/Wait 并发）。
	lifeCtx    context.Context
	lifeCancel context.CancelFunc
	runMu      sync.Mutex
	runWG      sync.WaitGroup
	closing    bool
}

// New 创建实例并注册动作。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.DefaultConcurrency <= 0 {
		cfg.DefaultConcurrency = 8
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 64
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 64
	}
	if cfg.MaxItems <= 0 {
		cfg.MaxItems = 1000
	}
	if cfg.ItemTimeout <= 0 {
		cfg.ItemTimeout = time.Minute
	}
	if cfg.PreviewBytes <= 0 {
		cfg.PreviewBytes = 2048
	}
	if cfg.JobTTL <= 0 {
		cfg.JobTTL = time.Hour
	}
	s := &Server{
		cfg:          cfg,
		logger:       logger,
		defConc:      cfg.DefaultConcurrency,
		maxConc:      cfg.MaxConcurrency,
		maxInflight:  cfg.MaxInflight,
		maxItems:     cfg.MaxItems,
		itemTimeout:  cfg.ItemTimeout,
		previewBytes: cfg.PreviewBytes,
		jobTTL:       cfg.JobTTL,
		resumeOnBoot: cfg.ResumeOnStart,
		jobs:         map[string]*runningJob{},
		stopped:      make(chan struct{}),
	}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Title:             "批处理",
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Description:       "批处理：一次声明 N 项工作 + 并发度 + 唤醒时机；受理即回执，跑完推一条汇总",
		Provides: []protocol.Provide{
			{
				Action: "run",
				Tool:   true,
				Async:  true,
				Input: "{service, action, items:[{input, key?, tag?}], concurrency?, on_error?, timeout_ms?, kind?, wake?}——" +
					"对目标服务的某个动作批量执行；key 标记关键项，wake.mode=key 时关键项全部结束会提前叫你",
				Output: "受理回执 {item_id, total, phase, kind}；之后最多三条补报（key 唤醒 / 首次失败 / 终态）",
			},
			{
				Action: "status",
				Tool:   true,
				Input:  "{item_id}——run 回执里的句柄",
				Output: "{phase, total, ok, failed, skipped, pending, key_total, key_done, woken, elapsed_ms}",
			},
			{
				Action: "result",
				Tool:   true,
				Input:  "{item_id, offset?, limit?, status?}——分页取明细；status 可只看 ok / failed / skipped / pending",
				Output: "{matched, items:[{seq, tag, key, status, error, preview, size, ms}]}",
			},
			{
				Action: "list",
				Tool:   true,
				Input:  "{phase?, limit?, caller?}——查任务队列（模型与前端共用）",
				Output: "{jobs:[...], counts:{running, paused, done, failed}}",
			},
			{
				Action: "control",
				Tool:   true,
				Input:  "{op: pause|resume|cancel, scope?: one|all, item_id?}——暂停可恢复，停止是终态",
				Output: "{op, scope, affected, paused, running}",
			},
		},
		Logger: logger,
	})
	s.declareSettings()
	s.Handle("run", s.actionRun)
	s.Handle("status", s.actionStatus)
	s.Handle("result", s.actionResult)
	s.Handle("list", s.actionList)
	s.Handle("control", s.actionControl)
	return s
}

// declareSettings 声明全部设置项（必须在 Start 前调用）。
func (s *Server) declareSettings() {
	s.HandleSetting("concurrency", strconv.Itoa(s.cfg.DefaultConcurrency), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("需要 ≥1 的整数，收到 %q", v)
		}
		s.setMu.Lock()
		s.defConc = n
		s.setMu.Unlock()
		return nil
	}, service.Label("单任务默认并发"),
		service.Hint("调用方不写 concurrency 时用这个值；要 50 路并发就在 JSON 里显式写"),
		service.As("number"), service.Unit("count"))

	s.HandleSetting("max_concurrency", strconv.Itoa(s.cfg.MaxConcurrency), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("需要 ≥1 的整数，收到 %q", v)
		}
		s.setMu.Lock()
		s.maxConc = n
		s.setMu.Unlock()
		return nil
	}, service.Label("单任务并发上限"),
		service.Hint("单个任务最多能开多少路；超过就拒绝这个任务"),
		service.As("number"), service.Unit("count"))

	s.HandleSetting("max_inflight", strconv.Itoa(s.cfg.MaxInflight), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("需要 ≥1 的整数，收到 %q", v)
		}
		s.setMu.Lock()
		s.maxInflight = n
		s.setMu.Unlock()
		return nil
	}, service.Label("全局在飞槽位上限"),
		service.Hint("所有任务加起来最多同时跑多少项；满了新任务会被同步拒绝（不排队）"),
		service.As("number"), service.Unit("count"))

	s.HandleSetting("max_items", strconv.Itoa(s.cfg.MaxItems), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("需要 ≥1 的整数，收到 %q", v)
		}
		s.setMu.Lock()
		s.maxItems = n
		s.setMu.Unlock()
		return nil
	}, service.Label("单任务最多项数"),
		service.Hint("一次最多处理多少项；几百项以上建议等 source 扇出（后续版本）"),
		service.As("number"), service.Unit("count"))

	s.HandleSetting("item_timeout_ms", strconv.FormatInt(s.cfg.ItemTimeout.Milliseconds(), 10), func(v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1000 {
			return fmt.Errorf("需要 ≥1000 的整数（毫秒），收到 %q", v)
		}
		s.setMu.Lock()
		s.itemTimeout = time.Duration(n) * time.Millisecond
		s.setMu.Unlock()
		return nil
	}, service.Label("单项超时"),
		service.Hint("每一项最多等多久，超时算这一项失败"),
		service.As("number"), service.Unit("ms"))

	s.HandleSetting("preview_bytes", strconv.Itoa(s.cfg.PreviewBytes), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 64 {
			return fmt.Errorf("需要 ≥64 的整数，收到 %q", v)
		}
		s.setMu.Lock()
		s.previewBytes = n
		s.setMu.Unlock()
		return nil
	}, service.Label("每项结果摘要上限"),
		service.Hint("只存摘要不存完整响应；要看完整结果就再用原参数调一次目标动作"),
		service.As("number"), service.Unit("bytes"))

	s.HandleSetting("job_ttl_ms", strconv.FormatInt(s.cfg.JobTTL.Milliseconds(), 10), func(v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 60_000 {
			return fmt.Errorf("需要 ≥60000 的整数（毫秒），收到 %q", v)
		}
		s.setMu.Lock()
		s.jobTTL = time.Duration(n) * time.Millisecond
		s.setMu.Unlock()
		return nil
	}, service.Label("任务可查询时长"),
		service.Hint("任务结束后还能查多久；过期就查不到了（暂停中的任务不受影响）"),
		service.As("number"), service.Unit("ms"))

	s.HandleSetting("resume_on_start", strconv.FormatBool(s.cfg.ResumeOnStart), func(v string) error {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("需要 true 或 false，收到 %q", v)
		}
		s.setMu.Lock()
		s.resumeOnBoot = b
		s.setMu.Unlock()
		return nil
	}, service.Label("重启后自动恢复"),
		service.Hint("开：服务重启后自动接着跑被转暂停的任务。默认关——一次放起 50 个任务比手动点一下更危险"))

	s.HandleReadOnly("db", s.cfg.DBPath)
	s.HandleReadOnly("name", s.cfg.Name)
	s.HandleReadOnly("addr", s.cfg.Listen)
	s.HandleReadOnly("center", s.cfg.Center)
}

// Start 打开账本、启动服务骨架，然后做一次重启恢复。
func (s *Server) Start(ctx context.Context) error {
	st, err := store.Open(s.cfg.DBPath)
	if err != nil {
		return fmt.Errorf("打开任务账本失败: %w", err)
	}
	s.store = st
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	s.runMu.Lock()
	s.lifeCtx, s.lifeCancel = lifeCtx, lifeCancel
	s.runMu.Unlock()
	if err := s.Service.Start(ctx); err != nil {
		lifeCancel()
		_ = st.Close()
		return err
	}
	s.recoverAfterRestart()
	s.startBackground(func() { s.pruneLoop() })
	return nil
}

// startBackground 启动一个随服务关闭而退出的后台循环；关闭开始后拒绝新任务。
func (s *Server) startBackground(fn func()) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.closing {
		return false
	}
	s.runWG.Add(1)
	go func() {
		defer s.runWG.Done()
		fn()
	}()
	return true
}

func (s *Server) isClosing() bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.closing
}

// waitRunBackground 等后台调度退出；有上限，避免一个卡住的项把关闭挂死。
func (s *Server) waitRunBackground() {
	done := make(chan struct{})
	go func() {
		s.runWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.logger.Printf("batch 关闭等待超时：仍有后台任务未退出")
	}
}

// Shutdown 停服务并关账本：先取消/停止，再等调度循环退出，最后关账本。
func (s *Server) Shutdown() {
	s.stopOnce.Do(func() { close(s.stopped) })
	s.runMu.Lock()
	s.closing = true
	cancel := s.lifeCancel
	s.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.Service.Shutdown()
	s.waitRunBackground()
	if s.store != nil {
		_ = s.store.Close()
	}
}

// pruneLoop 定期清理过期终态任务。
func (s *Server) pruneLoop() {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-s.stopped:
			return
		case <-tick.C:
			s.setMu.Lock()
			ttl := s.jobTTL
			s.setMu.Unlock()
			if n, err := s.store.Prune(time.Now(), ttl); err != nil {
				s.logger.Printf("清理过期任务失败: %v", err)
			} else if n > 0 {
				s.logger.Printf("清理过期任务 %d 个", n)
			}
		}
	}
}

// recoverAfterRestart 给上次没跑完的任务一个明确归宿。
//
// 默认转 paused（不替调用方决定"把 50 个任务同时放起来"）；
// 开着 resume_on_start 就直接接着跑。重启时正在跑的项一律标失败——结果未知，不能假装成功、
// 也不能擅自重跑（可能有副作用）。
func (s *Server) recoverAfterRestart() {
	jobs, err := s.store.Running()
	if err != nil {
		s.logger.Printf("读取未完成任务失败: %v", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	s.setMu.Lock()
	autoResume := s.resumeOnBoot
	s.setMu.Unlock()
	for _, j := range jobs {
		if n, err := s.store.MarkInterrupted(j.ItemID, "服务重启中断，执行结果未知"); err != nil {
			s.logger.Printf("标记中断项失败（%s）: %v", j.ItemID, err)
		} else if n > 0 {
			s.logger.Printf("任务 %s：%d 项因服务重启中断（结果未知）", j.ItemID, n)
			// 计数按项的真实状态重算——只改 items 不改 jobs 会让面板显示成 0 失败。
			if counts, err := s.store.CountByStatus(j.ItemID); err == nil {
				_ = s.store.SetCounts(j.ItemID, counts[protocol.BatchItemOK],
					counts[protocol.BatchItemFailed], counts[protocol.BatchItemSkipped])
			}
			// 重算过的计数必须重新读回来：拿重启前的旧快照去重建，收尾时会把中断项
			// 从总数里蒸发掉（3+0+0 对不上 total=4）。
			if fresh, ok, err := s.store.Job(j.ItemID); err != nil {
				s.logger.Printf("重读任务 %s 失败: %v", j.ItemID, err)
			} else if ok {
				j = fresh
			}
		}
		if autoResume {
			if rj, err := s.rebuildJob(j); err != nil {
				s.logger.Printf("恢复任务 %s 失败: %v", j.ItemID, err)
				_ = s.store.SetPhase(j.ItemID, protocol.BatchPhasePaused)
			} else if s.reserve(rj.reserved) {
				s.mu.Lock()
				s.jobs[rj.id] = rj
				s.mu.Unlock()
				if !s.startBackground(func() { s.runJob(rj) }) {
					s.release(rj.reserved)
					s.mu.Lock()
					delete(s.jobs, rj.id)
					s.mu.Unlock()
				}
			} else {
				s.logger.Printf("恢复任务 %s 失败：全局槽位不足，转暂停", j.ItemID)
				_ = s.store.SetPhase(j.ItemID, protocol.BatchPhasePaused)
			}
			continue
		}
		if err := s.store.SetPhase(j.ItemID, protocol.BatchPhasePaused); err != nil {
			s.logger.Printf("任务 %s 转暂停失败: %v", j.ItemID, err)
			continue
		}
		s.logger.Printf("任务 %s 因服务重启转暂停（resume 才会继续）", j.ItemID)
	}
}

// rebuildJob 从账本重建一个可继续跑的任务（重启自动恢复 / 恢复暂停任务用）。
//
// 策略（并发、单项超时、on_error、唤醒档位）必须从账本读回来：否则恢复出来的任务和
// 调用方当初声明的不是一回事。并发还要按当前上限与剩余项数夹一下。
func (s *Server) rebuildJob(j store.Job) (*runningJob, error) {
	items, err := s.store.PendingItems(j.ItemID)
	if err != nil {
		return nil, err
	}
	conc := j.Concurrency
	if conc < 1 {
		conc = s.concValue()
	}
	if max := s.maxConcValue(); conc > max {
		conc = max
	}
	if len(items) > 0 && conc > len(items) {
		conc = len(items)
	}
	timeout := s.timeoutValue()
	if j.TimeoutMS > 0 {
		timeout = time.Duration(j.TimeoutMS) * time.Millisecond
	}
	onError := j.OnError
	switch onError {
	case protocol.BatchOnErrorStop:
	default:
		onError = protocol.BatchOnErrorContinue
	}
	wakeMode := j.WakeMode
	switch wakeMode {
	case protocol.BatchWakeKey:
	default:
		wakeMode = protocol.BatchWakeAll
	}
	rj := &runningJob{
		id: j.ItemID, service: j.Service, action: j.Action,
		caller: j.Caller, boss: j.Boss, trace: j.Trace, kind: kindOf(j.Kind),
		wakeMode: wakeMode, onError: onError,
		timeout: timeout, concurrency: conc, reserved: conc,
		ok: j.Ok, failed: j.Failed, skipped: j.Skipped,
		keyTotal: j.KeyTotal, keyDone: j.KeyDone, woken: j.Woken,
		failures: map[string][]string{},
	}
	for _, it := range items {
		if len(it.Input) == 0 {
			return nil, fmt.Errorf("第 %d 项缺少原始输入，无法恢复", it.Seq+1)
		}
		rj.items = append(rj.items, queued{seq: it.Seq, key: it.Key, tag: it.Tag, input: msgpack.RawMessage(it.Input)})
	}
	return rj, nil
}

// ---------- 设置读取 ----------

func (s *Server) concValue() int {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.defConc
}

func (s *Server) maxConcValue() int {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.maxConc
}

func (s *Server) maxInflightValue() int {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.maxInflight
}

func (s *Server) maxItemsValue() int {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.maxItems
}

func (s *Server) timeoutValue() time.Duration {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.itemTimeout
}

func (s *Server) previewValue() int {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.previewBytes
}

// kindOf 归一化任务类别。
func kindOf(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), protocol.BatchKindResident) {
		return protocol.BatchKindResident
	}
	return protocol.BatchKindOnce
}
