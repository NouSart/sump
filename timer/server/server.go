package server

// timer 服务的核心：受理「N 毫秒后唤醒我」，到点把一条补报跳回调用方。
//
// 边界（与 shell 同一套「受理即回执」协议，见 protocol.Provide.Async）：
//   - 受理回执立即返回（带 item 句柄），到点前**不占调用方的任何等待位**：调用方的收尾
//     不被一条计时扣住，期内的其它工具照常派发、照常回来（这就是「不阻塞」的落点）；
//   - 到点由本服务发 report_result 补报回调用方：调用方任务还在跑就注入并唤醒，已收尾
//     就以补报开一轮（结果不丢）；
//   - 计时不动消息循环：每条计时一个 time.Timer + 回调，绝不在处理函数里 Sleep 或等待
//     ——长计时与短计时互不影响，服务随时可服务其它请求。
//
// 计时只在内存里：进程重启即丢，调用方的在途记账由它自己的兜底（reasoner 侧 24h 过期）清理。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/service"
)

// Config 是 timer 的启动配置（引导只靠它：自己的名字/地址 + 设置中心地址 + 两条护栏 + 门户）。
type Config struct {
	Name              string        // 服务名（注册用）
	Listen            string        // 监听地址 host:port
	Center            string        // 设置中心 WS 地址
	HeartbeatInterval time.Duration // 心跳间隔（默认 15s）
	MaxDuration       time.Duration // 单次计时上限（默认 1h；天花板 23h，见 maxCeiling）
	MaxPending        int           // 在计时器上限（默认 64；满即拒，不排队）
	Portal            string        // 门户服务名（留空 = 不推门户，不展示倒计时）
}

// minDuration 是单次计时的下限：比它更短没有意义（模型一次往返都远超它），也顺手挡住忙等刷计时。
const minDuration = 100 * time.Millisecond

// maxCeiling 是单次计时的**天花板**（设置项也调不过它）：23 小时。
// 上限不是 timer 自己想要的，而是调用方的记账寿命决定的——reasoner 的受理登记只活
// asyncJobTTL = 24h，超过就清，补报到了也会被忽略（调用方永远醒不来）。
// 留 1 小时余量：贴着 24h 设，只会造出"看着能设、其实白设"的旋钮。
const maxCeiling = 23 * time.Hour

// maxNoteRunes 是 note（到点补报里原样带回的那句话）的长度上限，超出截断——
// 它会进调用方的模型上下文，长度得有个头。
const maxNoteRunes = 200

// Server 是 timer 服务实例（服务骨架 + 在计时器表）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger

	setMu       sync.Mutex
	maxDuration time.Duration
	maxPending  int
	portal      string // 门户服务名（timer.portal 热改；空 = 不推）

	mu      sync.Mutex
	pending map[string]*timing // item → 在计时器
	seq     int64              // 计时表的状态版本（每次受理/到点/撤销都 ++；快照与 list 都带它）
	pushed  map[string]bool    // 上一轮推过门户的 boss（本轮没有的要补一条空快照）
}

// portalName 返回当前门户服务名（空 = 不推快照）。
func (s *Server) portalName() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.portal
}

// setPortalName 更新门户服务名（设置下发 / 拉取调用）。
func (s *Server) setPortalName(v string) {
	s.setMu.Lock()
	s.portal = v
	s.setMu.Unlock()
}

// timing 是一条在计时器（受理后、到点前）。
type timing struct {
	item   string
	caller string // 到点补报回谁（信封 from）
	trace  string // 链路 id（原样沿用）
	boss   string // 任务原老板（补报轮回交付用；原样带回）
	ms     int64
	note   string
	fireAt time.Time
	timer  *time.Timer
}

// New 创建实例并注册动作。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.MaxDuration <= 0 {
		cfg.MaxDuration = time.Hour
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 64
	}
	s := &Server{
		cfg:    cfg,
		logger: logger,

		maxDuration: cfg.MaxDuration,
		maxPending:  cfg.MaxPending,
		portal:      cfg.Portal,

		pending: map[string]*timing{},
		pushed:  map[string]bool{},
	}
	if s.maxDuration > maxCeiling {
		s.maxDuration = maxCeiling
	}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Title:       "定时",
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Description:       "计时器：受理「N 毫秒后唤醒我」，到点把补报跳回调用方（受理即回执，全程不阻塞）",
		Provides: []protocol.Provide{
			{
				Action: "sleep",
				Tool:   true,
				Async:  true,
				Input:  "{ms, note?}——ms 是时长（毫秒）；note 是到点补报里原样带回来的一句话（给你自己看的备注）",
				Output: "受理回执 {item_id, phase, fire_at}；到点后由 report_result 补报送达（可能在你收尾之后才到）",
			},
			{
				Action: "cancel",
				Tool:   true,
				Input:  "{item_id}——sleep 受理回执里给出的句柄",
				Output: "{item_id, status}——canceled（已撤销，补报已发出）/ already_fired（早就到点了）/ unknown（没有这个句柄）",
			},
			{
				// 给门户（或任何想知道"现在有哪些计时在跑"的服务）用；不作为 LLM 工具暴露。
				Action: "list",
				Input:  "{}",
				Output: "{seq, timers:[{item_id, caller, ms, fire_at, note}]}——seq 单调递增，用来丢弃乱序的旧快照",
			},
		},
		Logger: logger,
	})
	// 可设置项：预设 = 本服务的启动引导值（来源见 main.go 的 flag）。
	s.HandleSetting("max_ms", strconv.FormatInt(cfg.MaxDuration.Milliseconds(), 10), func(v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < minDuration.Milliseconds() {
			return fmt.Errorf("需要 ≥%d 的整数（毫秒），收到 %q", minDuration.Milliseconds(), v)
		}
		if n > maxCeiling.Milliseconds() {
			return fmt.Errorf("不接受 %dms：天花板 %s（调用方的受理登记只活 24h，再长补报会被忽略）", n, maxCeiling)
		}
		s.setMaxDuration(time.Duration(n) * time.Millisecond)
		return nil
	}, service.Label("单次定时上限"),
		service.Hint("最多能定多久，单位毫秒；超过这个值会被直接拒绝"),
		service.As("number"))
	s.HandleSetting("max_pending", strconv.Itoa(cfg.MaxPending), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("需要 ≥1 的整数，收到 %q", v)
		}
		s.setMaxPending(n)
		return nil
	}, service.Label("同时挂起的定时上限"),
		service.Hint("最多同时有多少个等唤醒的定时，满了新请求会被拒绝"),
		service.As("number"))
	s.Handle("sleep", s.actionSleep)
	s.Handle("cancel", s.actionCancel)
	s.Handle("list", s.actionList)
	// 门户服务名：热改（空 = 不推快照；改名后下一次状态变化就推给新门户）。
	s.HandleSetting("portal", cfg.Portal, func(v string) error {
		s.setPortalName(strings.TrimSpace(v))
		return nil
	}, service.Label("把状态推给哪个门户"),
		service.Hint("定时到点/新增/撤销时的快照推给这个服务；留空表示谁都不推"))
	// 只读项：启动参数带起来的（换它要重启）。
	s.HandleReadOnly("name", cfg.Name)
	s.HandleReadOnly("addr", cfg.Listen)
	s.HandleReadOnly("center", cfg.Center)
	return s
}

// Start 启动服务骨架；设置同步由骨架负责（注册后全量下发 + 拉取兜底）。
func (s *Server) Start(ctx context.Context) error {
	return s.Service.Start(ctx)
}

// maxDurationValue 返回当前的单次计时上限。
func (s *Server) maxDurationValue() time.Duration {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	if s.maxDuration <= 0 {
		return time.Hour
	}
	return s.maxDuration
}

func (s *Server) setMaxDuration(d time.Duration) {
	s.setMu.Lock()
	s.maxDuration = d
	s.setMu.Unlock()
}

// maxPendingValue 返回当前的在计时器上限。
func (s *Server) maxPendingValue() int {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	if s.maxPending < 1 {
		return 64
	}
	return s.maxPending
}

func (s *Server) setMaxPending(n int) {
	s.setMu.Lock()
	s.maxPending = n
	s.setMu.Unlock()
}

// PendingCount 返回在计时器条数（退出日志用）。
func (s *Server) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// StopAll 停掉所有在计时器（退出前调用：不再触发到点补报）。
func (s *Server) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for item, t := range s.pending {
		t.timer.Stop()
		delete(s.pending, item)
	}
}

// ---------- 动作 ----------

// actionSleep 受理一次计时：立即回执，到点由 report_result 补报（处理函数里不等待）。
func (s *Server) actionSleep(_ context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var req protocol.TimerSleepPayload
	if err := protocol.DecodeRaw(in.Input, &req); err != nil {
		return nil, err
	}
	if req.MS < minDuration.Milliseconds() {
		return nil, fmt.Errorf("不接受时长 %dms：至少 %dms", req.MS, minDuration.Milliseconds())
	}
	if maxMS := s.maxDurationValue().Milliseconds(); req.MS > maxMS {
		return nil, fmt.Errorf("不接受时长 %dms：上限 %dms（设置项 max_ms 可调）", req.MS, maxMS)
	}
	note, clipped := clipRunes(req.Note, maxNoteRunes)

	t := &timing{
		item:   protocol.NewID(),
		caller: env.From,
		trace:  env.Trace,
		boss:   env.Boss,
		ms:     req.MS,
		note:   note,
		fireAt: time.Now().Add(time.Duration(req.MS) * time.Millisecond),
	}
	// 计时先起、再登记：撤销路径只要在表里看得见它，就一定能 Stop 到它。
	t.timer = time.AfterFunc(time.Duration(req.MS)*time.Millisecond, func() { s.fire(t.item) })

	s.mu.Lock()
	full := len(s.pending) >= s.maxPendingValue()
	if !full {
		s.pending[t.item] = t
		s.seq++
	}
	s.mu.Unlock()
	if full {
		t.timer.Stop()
		return nil, fmt.Errorf("在计时器已满（%d 条），不排队：稍后再试或先 cancel 掉不用的", s.maxPendingValue())
	}

	s.logger.Printf("受理计时 %s（来自 %s，boss=%s）：%dms 后到点%s", t.item, t.caller, t.boss, req.MS, tripped(clipped))
	s.pushPortalState(env.Trace)
	return protocol.TimerSleepResult{
		ItemID: t.item,
		Phase:  "queued",
		FireAt: t.fireAt.Format(time.RFC3339),
	}, nil
}

// actionCancel 撤销一次已受理、尚未到点的计时；撤销即发一条 final 补报，
// 免得调用方那边的在途记账一直挂着（它靠 final 释放）。
func (s *Server) actionCancel(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var req protocol.TimerCancelPayload
	if err := protocol.DecodeRaw(in.Input, &req); err != nil {
		return nil, err
	}
	if req.ItemID == "" {
		return nil, errors.New("cancel: item_id 必填")
	}

	s.mu.Lock()
	t, ok := s.pending[req.ItemID]
	if ok {
		delete(s.pending, req.ItemID)
		s.seq++
	}
	s.mu.Unlock()

	if !ok {
		// 已到点（补报已发）/ 已撤销 / 从没存在过：如实回，不假装成功。
		// 到点与撤销都在同一把锁下摘登记，所以这里不会和一个正要发补报的到点撞上。
		return protocol.TimerCancelResult{ItemID: req.ItemID, Status: "unknown"}, nil
	}
	t.timer.Stop()
	s.report(t, "canceled", fmt.Sprintf("计时已撤销：原本还会在 %s 后到点，调用方无须再等。", durationText(t.ms)), true)
	s.pushPortalState(t.trace)
	s.logger.Printf("撤销计时 %s（来自 %s）", t.item, t.caller)
	return protocol.TimerCancelResult{ItemID: req.ItemID, Status: "canceled"}, nil
}

// fire 到点：摘登记（摘不到 = 已被撤销）后发补报。在计时回调里跑，不碰消息循环。
func (s *Server) fire(item string) {
	s.mu.Lock()
	t, ok := s.pending[item]
	if ok {
		delete(s.pending, item)
		s.seq++
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	s.report(t, "done", wakeText(t), true)
	s.pushPortalState(t.trace)
	s.logger.Printf("到点 %s（来自 %s，%dms）", t.item, t.caller, t.ms)
}

// actionList 回报当前在计时器（门户冷启动时拉一次；之后的变动由 pushPortalState 主动推）。
func (s *Server) actionList(_ context.Context, _ protocol.Envelope, _ protocol.JumpPayload) (any, error) {
	s.mu.Lock()
	items := make([]protocol.TimerItem, 0, len(s.pending))
	for _, t := range s.pending {
		items = append(items, timerItem(t))
	}
	seq := s.seq
	s.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].FireAt < items[j].FireAt })
	return protocol.TimerListResult{Seq: seq, Timers: items}, nil
}

// pushPortalState 把当前在计时器按 boss 分组推给门户（一次变动一条快照）。
// 未接入门户（-portal 留空）时什么也不做。
//
// 两条不显眼但必要的事：
//   - 上一轮推过、这一轮已经没有的 boss 要补一条**空快照**——否则门户那边的倒计时会一直挂着
//     （门户只认快照，不去猜"它是不是到点了"）；
//   - 每条快照带单调递增的 seq——骨架的 Fire 各自成 goroutine，**到达顺序无保证**
//     （shell 的测试里就实测到过倒序到达），门户只认序号更大的。
func (s *Server) pushPortalState(trace string) {
	portal := s.portalName()
	if portal == "" {
		return
	}
	s.mu.Lock()
	groups := map[string][]protocol.TimerItem{}
	cur := map[string]bool{}
	for _, t := range s.pending {
		groups[t.boss] = append(groups[t.boss], timerItem(t))
		cur[t.boss] = true
	}
	prev := s.pushed
	s.pushed = cur
	seq := s.seq
	s.mu.Unlock()

	for boss, items := range groups {
		s.Fire(portal, "timer_state", protocol.TimerStatePayload{Boss: boss, Seq: seq, Timers: items}, trace, boss)
	}
	for boss := range prev {
		if !cur[boss] {
			s.Fire(portal, "timer_state", protocol.TimerStatePayload{Boss: boss, Seq: seq}, trace, boss)
		}
	}
}

// timerItem 把一条在计时器描述成对外形状。
func timerItem(t *timing) protocol.TimerItem {
	return protocol.TimerItem{
		ItemID: t.item,
		Caller: t.caller,
		MS:     t.ms,
		FireAt: t.fireAt.Format(time.RFC3339),
		Note:   t.note,
	}
}

// report 向调用方发一条补报（受理回执已带走句柄，这里只送后续消息）。
func (s *Server) report(t *timing, phase, text string, final bool) {
	if t.caller == "" {
		s.logger.Printf("计时 %s 无调用方，补报丢弃（phase=%s）", t.item, phase)
		return
	}
	s.Fire(t.caller, "report_result", protocol.AsyncResultPayload{
		ItemID: t.item,
		Phase:  phase,
		Text:   text,
		Final:  final,
	}, t.trace, t.boss)
}

// wakeText 是到点补报注入调用方的那句话——调用方的模型看到的就是它。
func wakeText(t *timing) string {
	text := fmt.Sprintf("到点了：%s 的计时结束。", durationText(t.ms))
	if t.note != "" {
		text += "\n你当时留的备注：" + t.note
	}
	text += "\n（还要接着等就再设一次计时。）"
	return text
}

// durationText 把毫秒写成人读的时长（Go 自己的格式：500ms / 30s / 1h30m0s）。
func durationText(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).String()
}

// clipRunes 按字符（不是字节）截断，避免把一个汉字切成半个。
func clipRunes(s string, max int) (string, bool) {
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]) + "…", true
}

// tripped 在日志里标注 note 被截断了。
func tripped(clipped bool) string {
	if clipped {
		return "（note 超长已截断）"
	}
	return ""
}
