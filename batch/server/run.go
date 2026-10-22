package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/QAQ-awa-QAQ/sump/batch/store"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// queued 是一项待开始的工作（还没被 worker 领走）。
type queued struct {
	seq int
	key bool
	tag string
	// input 必须是 msgpack.RawMessage（不是 []byte）：service.encodeInput 只对这个类型
	// 走"原样透传"，[]byte 会被当成 bin 再包一层。
	input msgpack.RawMessage
}

// runningJob 是内存里的活动任务。终态即从 s.jobs 删除（历史仍在账本里）。
type runningJob struct {
	id      string
	service string
	action  string
	caller  string
	boss    string
	trace   string
	kind    string

	wakeMode    string
	onError     string
	timeout     time.Duration
	concurrency int
	reserved    int // 预留给全局的槽位数（= concurrency，admission 时一次扣）

	mu             sync.Mutex
	items          []queued // 未开始的项（任务内可以等，这是调用方自己声明的）
	inflight       int
	paused         bool
	cancelled      bool
	stopped        bool // on_error=stop 触发：不再开始新项
	finished       bool
	schedRunning   bool
	ok             int
	failed         int
	skipped        int
	keyTotal       int
	keyDone        int
	woken          bool
	failedNotified bool
	failures       map[string][]string // 错误原文 → 样例 tag
	failOrder      []string            // 错误首次出现的顺序（汇总按它排）
}

// jobSnapshot 是加锁拷出来的只读快照（不能直接拷 runningJob：它带 mutex）。
type jobSnapshot struct {
	id        string
	ok        int
	failed    int
	skipped   int
	keyTotal  int
	keyDone   int
	pending   int // 未开始
	inflight  int
	failures  map[string][]string
	failOrder []string
}

// snapshotLocked 在持锁状态下取快照。
func (j *runningJob) snapshotLocked() jobSnapshot {
	failures := make(map[string][]string, len(j.failures))
	for k, v := range j.failures {
		failures[k] = append([]string(nil), v...)
	}
	return jobSnapshot{
		id: j.id, ok: j.ok, failed: j.failed, skipped: j.skipped,
		keyTotal: j.keyTotal, keyDone: j.keyDone,
		pending: len(j.items), inflight: j.inflight,
		failures: failures, failOrder: append([]string(nil), j.failOrder...),
	}
}

// total 是这项任务的总项数（已完成 + 未开始 + 在飞）。
func (s jobSnapshot) total() int { return s.ok + s.failed + s.skipped + s.pending + s.inflight }

// tagOf 给一项一个可读标识（汇总里用）。
func tagOf(q queued) string {
	if strings.TrimSpace(q.tag) != "" {
		return q.tag
	}
	return fmt.Sprintf("#%d", q.seq+1)
}

// actionRun 受理一批：校验 → 预留槽位 → 落库 → 立即回执 → 后台开跑。
func (s *Server) actionRun(ctx context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.BatchRunPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if len(p.Items) == 0 {
		return nil, fmt.Errorf("BATCH_ITEMS: items 不能为空")
	}
	if n := s.maxItemsValue(); len(p.Items) > n {
		return nil, fmt.Errorf("BATCH_ITEMS: 一次最多 %d 项，收到 %d", n, len(p.Items))
	}
	service := strings.TrimSpace(p.Service)
	action := strings.TrimSpace(p.Action)
	if service == "" || action == "" {
		return nil, fmt.Errorf("BATCH_TARGET_UNKNOWN: service 与 action 都必填")
	}
	if service == s.cfg.Name {
		return nil, fmt.Errorf("BATCH_NESTED: 不允许批量调用 batch 自己（深度固定为 1）")
	}
	if !s.batchable(service, action) {
		return nil, fmt.Errorf("BATCH_NOT_BATCHABLE: %s.%s 没有声明可批量（Batchable）", service, action)
	}

	conc := p.Concurrency
	if conc == 0 {
		conc = s.concValue()
	}
	if conc < 1 || conc > s.maxConcValue() {
		return nil, fmt.Errorf("BATCH_CONCURRENCY: concurrency 需在 1..%d，收到 %d", s.maxConcValue(), conc)
	}
	if conc > len(p.Items) {
		conc = len(p.Items)
	}

	onError := strings.ToLower(strings.TrimSpace(p.OnError))
	switch onError {
	case "", protocol.BatchOnErrorContinue:
		onError = protocol.BatchOnErrorContinue
	case protocol.BatchOnErrorStop:
	default:
		return nil, fmt.Errorf("BATCH_OP: on_error 只能是 continue 或 stop，收到 %q", p.OnError)
	}

	wakeMode := strings.ToLower(strings.TrimSpace(p.Wake.Mode))
	switch wakeMode {
	case "", protocol.BatchWakeAll:
		wakeMode = protocol.BatchWakeAll
	case protocol.BatchWakeKey:
		keyTotal := 0
		for _, it := range p.Items {
			if it.Key {
				keyTotal++
			}
		}
		if keyTotal == 0 {
			return nil, fmt.Errorf("BATCH_WAKE: wake.mode=key 但没有任何项标了 key")
		}
	default:
		return nil, fmt.Errorf("BATCH_WAKE: wake.mode 只能是 all 或 key，收到 %q", p.Wake.Mode)
	}

	timeout := s.timeoutValue()
	if p.TimeoutMS > 0 {
		timeout = time.Duration(p.TimeoutMS) * time.Millisecond
	}

	// 预留全局槽位：不够就同步拒绝（不排队）。
	if !s.reserve(conc) {
		s.setMu.Lock()
		max := s.maxInflight
		s.setMu.Unlock()
		return nil, fmt.Errorf("BATCH_BUSY: 全局在飞槽位已满（上限 %d，当前已用 %d），本次未受理", max, s.inflightNow())
	}

	id := protocol.NewID()
	job := &runningJob{
		id: id, service: service, action: action,
		caller: env.From, boss: env.Boss, trace: env.Trace,
		kind:     kindOf(p.Kind),
		wakeMode: wakeMode, onError: onError, timeout: timeout,
		concurrency: conc, reserved: conc,
		keyTotal: countKeys(p.Items),
		failures: map[string][]string{},
	}
	for i, it := range p.Items {
		job.items = append(job.items, queued{seq: i, key: it.Key, tag: it.Tag, input: it.Input})
	}

	if err := s.store.CreateJob(id, service, action, env.From, env.Boss, env.Trace, job.kind, p.Items, time.Now()); err != nil {
		s.release(conc)
		return nil, fmt.Errorf("落库失败: %w", err)
	}

	s.mu.Lock()
	s.jobs[id] = job
	s.mu.Unlock()

	go s.runJob(job)

	return protocol.BatchRunResult{ItemID: id, Total: len(p.Items), Phase: protocol.BatchPhaseAccepted, Kind: job.kind}, nil
}

// countKeys 数关键项。
func countKeys(items []protocol.BatchItem) int {
	n := 0
	for _, it := range items {
		if it.Key {
			n++
		}
	}
	return n
}

// batchable 查名册里目标动作是否声明了 Batchable。
func (s *Server) batchable(service, action string) bool {
	for _, c := range s.Roster() {
		if c.Name != service {
			continue
		}
		for _, p := range c.Provides {
			if p.Action == action {
				return p.Batchable
			}
		}
		return false
	}
	return false
}

// reserve 预留全局槽位；不够返回 false。
func (s *Server) reserve(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setMu.Lock()
	max := s.maxInflight
	s.setMu.Unlock()
	if s.inflight+n > max {
		return false
	}
	s.inflight += n
	return true
}

// release 归还槽位。
func (s *Server) release(n int) {
	s.mu.Lock()
	s.inflight -= n
	if s.inflight < 0 {
		s.inflight = 0
	}
	s.mu.Unlock()
}

// inflightNow 返回当前已预留的槽位数。
func (s *Server) inflightNow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight
}

// runJob 起一次调度循环（暂停后恢复会再起一次）。
func (s *Server) runJob(j *runningJob) {
	if err := s.store.MarkStarted(j.id, time.Now()); err != nil {
		s.logger.Printf("任务 %s 标记开始失败: %v", j.id, err)
	}
	s.runScheduler(j)
}

// runScheduler 循环领活直到队列空 / 暂停 / 取消 / 失败即停。
//
// 这里**不排队**：任务内最多 concurrency 个 worker 在跑，剩下的就是调用方自己声明的项在等
// 自己的槽位——不是执行器替别人保管意图。
func (s *Server) runScheduler(j *runningJob) {
	j.mu.Lock()
	if j.schedRunning || j.finished {
		j.mu.Unlock()
		return
	}
	j.schedRunning = true
	j.mu.Unlock()

	sem := make(chan struct{}, j.concurrency)
	var wg sync.WaitGroup
	for {
		sem <- struct{}{}
		q, ok := j.claim()
		if !ok {
			<-sem
			break
		}
		wg.Add(1)
		go func(q queued) {
			defer wg.Done()
			defer func() { <-sem }()
			s.runItem(j, q)
		}(q)
	}
	// 必须等在飞的项全部收尾，再判断要不要终结——否则会把还在跑的项算漏。
	wg.Wait()

	j.mu.Lock()
	j.schedRunning = false
	paused := j.paused
	finished := j.finished
	j.mu.Unlock()
	if paused || finished {
		return
	}
	s.finishJob(j)
}

// claim 领走下一项；false 表示不再开始新项（队列空 / 暂停 / 取消 / 失败即停）。
func (j *runningJob) claim() (queued, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.paused || j.cancelled || j.stopped || j.finished || len(j.items) == 0 {
		return queued{}, false
	}
	q := j.items[0]
	j.items = j.items[1:]
	j.inflight++
	return q, true
}

// runItem 执行一项：调目标动作，落终态，然后按需唤醒。
func (s *Server) runItem(j *runningJob, q queued) {
	_ = s.store.SetItemStatus(j.id, q.seq, protocol.BatchItemRunning)

	ctx, cancel := context.WithTimeout(context.Background(), j.timeout)
	defer cancel()
	start := time.Now()
	raw, err := s.Call(ctx, j.service, j.action, q.input, j.trace, j.boss)
	ms := time.Since(start).Milliseconds()

	it := store.Item{Seq: q.seq, Status: protocol.BatchItemOK, MS: ms}
	if err != nil {
		it.Status = protocol.BatchItemFailed
		it.Error = err.Error()
	} else {
		it.Size = len(raw)
		it.Preview = truncateBytes(raw, s.previewValue())
	}
	if err := s.store.MarkItem(j.id, it); err != nil {
		s.logger.Printf("任务 %s 第 %d 项落库失败: %v", j.id, q.seq+1, err)
	}
	s.record(j, q, it)
}

// record 更新计数，并按三个触发点决定要不要中途推一条。
func (s *Server) record(j *runningJob, q queued, it store.Item) {
	now := time.Now()

	j.mu.Lock()
	j.inflight--
	switch it.Status {
	case protocol.BatchItemOK:
		j.ok++
	case protocol.BatchItemFailed:
		j.failed++
		if _, seen := j.failures[it.Error]; !seen {
			j.failOrder = append(j.failOrder, it.Error)
		}
		if len(j.failures[it.Error]) < 3 {
			j.failures[it.Error] = append(j.failures[it.Error], tagOf(q))
		}
	}
	if q.key && it.Status != protocol.BatchItemPending && it.Status != protocol.BatchItemRunning {
		j.keyDone++
	}
	remaining := len(j.items) + j.inflight

	keyWake := j.wakeMode == protocol.BatchWakeKey && q.key &&
		j.keyDone >= j.keyTotal && j.keyTotal > 0 && !j.woken && remaining > 0
	if keyWake {
		j.woken = true
	}
	failWake := it.Status == protocol.BatchItemFailed && !j.failedNotified && remaining > 0
	if failWake {
		j.failedNotified = true
	}
	stopNow := it.Status == protocol.BatchItemFailed && j.onError == protocol.BatchOnErrorStop && !j.stopped
	var pendingDrop []queued
	if stopNow {
		j.stopped = true
		pendingDrop = j.items
		j.items = nil
		j.skipped += len(pendingDrop)
	}
	snapshot := j.snapshotLocked()
	j.mu.Unlock()

	if stopNow {
		if n, err := s.store.SkipPending(j.id); err != nil {
			s.logger.Printf("任务 %s 未开始项转跳过失败: %v", j.id, err)
		} else {
			s.logger.Printf("任务 %s：失败即停，%d 项转为跳过", j.id, n)
		}
	}
	if keyWake {
		_ = s.store.SetWoken(j.id, true)
		_ = s.store.SetKeyDone(j.id, snapshot.keyDone)
		s.pushProgress(j, "key", keyWakeText(snapshot), now)
	}
	if failWake {
		_ = s.store.SetKeyDone(j.id, snapshot.keyDone)
		s.pushProgress(j, "failed", firstFailureText(snapshot, q, it), now)
	}
}

// pushProgress 推一条非终态补报（key 唤醒 / 首次失败）。
func (s *Server) pushProgress(j *runningJob, kind, text string, _ time.Time) {
	payload := protocol.AsyncResultPayload{
		ItemID: j.id,
		Phase:  protocol.BatchPhaseProgress,
		Text:   text,
		Final:  false,
	}
	s.Fire(j.caller, "report_result", payload, j.trace, j.boss)
	s.logger.Printf("任务 %s：推送 %s 进展", j.id, kind)
}

// finishJob 落终态并推最后一条。
func (s *Server) finishJob(j *runningJob) {
	now := time.Now()

	// 取消时把未开始的项一并转跳过（在飞的已经跑完）。
	j.mu.Lock()
	drop := j.items
	if j.cancelled && len(drop) > 0 {
		j.items = nil
		j.skipped += len(drop)
	}
	phase := protocol.BatchPhaseDone
	switch {
	case j.cancelled:
		phase = protocol.BatchPhaseCancelled
	case j.failed > 0:
		phase = protocol.BatchPhaseFailed
	}
	if j.finished {
		j.mu.Unlock()
		return
	}
	j.finished = true
	snapshot := j.snapshotLocked()
	j.mu.Unlock()

	if j.cancelled && len(drop) > 0 {
		if _, err := s.store.SkipPending(j.id); err != nil {
			s.logger.Printf("任务 %s 取消时跳过未开始项失败: %v", j.id, err)
		}
	}

	summary := summaryText(snapshot)
	if err := s.store.Finish(j.id, phase, snapshot.ok, snapshot.failed, snapshot.skipped, summary, now); err != nil {
		s.logger.Printf("任务 %s 落终态失败: %v", j.id, err)
	}
	s.release(j.reserved)
	s.mu.Lock()
	delete(s.jobs, j.id)
	s.mu.Unlock()

	s.Fire(j.caller, "report_result", protocol.AsyncResultPayload{
		ItemID: j.id,
		Phase:  phase,
		Text:   summary,
		Final:  true,
	}, j.trace, j.boss)
	s.logger.Printf("任务 %s 结束：%s（成功 %d / 失败 %d / 跳过 %d）", j.id, phase, snapshot.ok, snapshot.failed, snapshot.skipped)
}

// keyWakeText 是 key 唤醒的文案。
func keyWakeText(j jobSnapshot) string {
	return fmt.Sprintf("关键项已全部完成（%d/%d）。其余 %d 项仍在跑，全部完成会再通知一次（item_id=%s）",
		j.keyDone, j.keyTotal, j.pending+j.inflight, j.id)
}

// firstFailureText 是首次失败通知的文案（带四个计数，供模型决定继续还是取消）。
func firstFailureText(j jobSnapshot, q queued, it store.Item) string {
	total := j.total()
	return fmt.Sprintf("批量进度：%d 项，完成 %d，失败 %d，在跑 %d，待跑 %d（item_id=%s）\n首次失败：%s — %s\n继续就等终态；要停：batch__control {\"op\":\"cancel\",\"item_id\":\"%s\"}",
		total, j.ok, j.failed, j.inflight, j.pending, j.id, tagOf(q), oneLine(it.Error), j.id)
}

// summaryText 是终态补报：失败按原因归组，文本硬上限 2 KiB。
func summaryText(j jobSnapshot) string {
	total := j.total()
	var b strings.Builder
	fmt.Fprintf(&b, "批量结束：%d 项，成功 %d，失败 %d，跳过 %d（item_id=%s）",
		total, j.ok, j.failed, j.skipped, j.id)
	if j.failed > 0 && len(j.failOrder) > 0 {
		fmt.Fprintf(&b, "\n失败集中在 %d 类：", len(j.failOrder))
		shown := 0
		for _, err := range j.failOrder {
			tags := j.failures[err]
			fmt.Fprintf(&b, "\n  · %s（%d 项）：%s", oneLine(err), len(tags), strings.Join(tags, ", "))
			shown++
			if shown >= 5 {
				rest := len(j.failOrder) - shown
				if rest > 0 {
					fmt.Fprintf(&b, "\n  … 另有 %d 类未列出", rest)
				}
				break
			}
		}
		fmt.Fprintf(&b, "\n明细：batch__result {\"item_id\":\"%s\",\"status\":\"failed\"}", j.id)
	}
	out := b.String()
	const maxChars = 2048
	if utf8.RuneCountInString(out) > maxChars {
		out = string([]rune(out)[:maxChars]) + "…（已截断）"
	}
	return out
}

// oneLine 把多行错误压成一行（汇总里别被换行撑爆）。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	const max = 160
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

// truncateBytes 把响应摘要截到 n 字节（不切坏 UTF-8）。
func truncateBytes(raw []byte, n int) []byte {
	if len(raw) <= n {
		return raw
	}
	cut := raw[:n]
	for len(cut) > 0 && !utf8.Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// sortedTags 给测试与调试用（保序去重后的样例）。
func sortedTags(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
