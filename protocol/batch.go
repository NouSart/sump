package protocol

import "github.com/vmihailenco/msgpack/v5"

// batch：把"一次做 N 项工作"压成一次调用，并让调用方自己决定什么时候被叫醒。
//
// 边界（与 shell 的"无队列"同一套哲学）：
//   - 受理即回执：`run` 校验通过、任务入库、开始派发后立即返回，不等任何一项完成；
//   - 任务级不排队：全局在飞项数超上限就同步拒绝（BATCH_BUSY），不替调用方保管意图；
//   - 任务内可以等：第 17 项等前 16 项，那是**调用方自己声明的 50 项**在等自己的槽位；
//   - 唤醒由调用方声明（all / key），每次任务至多三条推送（key / 首次失败 / 终态）；
//   - 不重试、不去重、不解释 input：batch 只搬运，重试策略与 item 语义都归调用方。

// BatchItem 是一项待执行的工作。
type BatchItem struct {
	// Input 原样透传给目标动作的 input；batch 不解析、不改写。
	Input msgpack.RawMessage `msgpack:"input"`
	// Key 标记关键项：全部关键项结束时触发一次 key 唤醒（mode=key 才有意义）。
	Key bool `msgpack:"key,omitempty"`
	// Tag 可选标识，汇总与明细里原样带回（给人看，也给模型认）。
	Tag string `msgpack:"tag,omitempty"`
}

// BatchWakePolicy 决定"什么时候叫醒调用方"。
//
// 只有两档：
//   - "" / "all"（默认）：全部项结束后唤醒（与终态补报重合，只发一条）；
//   - "key"       ：全部关键项结束后唤醒一次，全部项结束再发一条终态。
//
// key 的作用域**仅限本任务内部**：它不参与任何跨任务判断。
type BatchWakePolicy struct {
	Mode string `msgpack:"mode,omitempty"`
}

// 唤醒模式的取值。
const (
	BatchWakeAll = "all"
	BatchWakeKey = "key"
)

// 批量任务的类别：只影响唤醒聚合，不影响执行。
const (
	BatchKindOnce     = "once"     // 一次性（默认）
	BatchKindResident = "resident" // 常驻：一直跑、不操作不会结束的那类，跨任务判断不等它
)

// 失败处理与阶段词汇。
const (
	BatchOnErrorContinue = "continue" // 默认：单项失败只记该项
	BatchOnErrorStop     = "stop"     // 首项失败后：未开始的项转 skipped

	BatchPhaseAccepted  = "accepted"
	BatchPhaseRunning   = "running"
	BatchPhasePaused    = "paused"
	BatchPhaseProgress  = "progress" // 推送用：key 唤醒 / 首次失败（非终态）
	BatchPhaseDone      = "done"
	BatchPhaseFailed    = "failed"
	BatchPhaseCancelled = "cancelled"

	BatchItemPending = "pending"
	BatchItemRunning = "running"
	BatchItemOK      = "ok"
	BatchItemFailed  = "failed"
	BatchItemSkipped = "skipped"

	BatchOpPause  = "pause"
	BatchOpResume = "resume"
	BatchOpCancel = "cancel"

	BatchScopeOne = "one"
	BatchScopeAll = "all"
)

// BatchRunPayload 是 batch.run 的输入。
type BatchRunPayload struct {
	Service     string          `msgpack:"service"`
	Action      string          `msgpack:"action"`
	Items       []BatchItem     `msgpack:"items"`
	Concurrency int             `msgpack:"concurrency,omitempty"` // 0 = 服务默认
	OnError     string          `msgpack:"on_error,omitempty"`    // ""/continue | stop
	TimeoutMS   int64           `msgpack:"timeout_ms,omitempty"`  // 单项超时（0 = 默认）
	Kind        string          `msgpack:"kind,omitempty"`        // ""/once | resident
	Wake        BatchWakePolicy `msgpack:"wake,omitempty"`
}

// BatchRunResult 是受理回执。
//
// 字段名必须是 item_id：reasoner 的 trackAsync 只认这个键（reasoner/server/infer.go）。
// 少一个字母，后续所有唤醒都会被当成"没有对应在途受理"而丢弃。
type BatchRunResult struct {
	ItemID string `msgpack:"item_id"` // 任务句柄（补报、状态、明细、控制都用它）
	Total  int    `msgpack:"total"`
	Phase  string `msgpack:"phase"`          // accepted
	Kind   string `msgpack:"kind,omitempty"` // 回显 once / resident
}

// BatchStatusPayload 查单个任务的进度。
type BatchStatusPayload struct {
	ItemID string `msgpack:"item_id"`
}

// BatchStatusResult 是单个任务的进度快照。
type BatchStatusResult struct {
	ItemID    string `msgpack:"item_id"`
	Service   string `msgpack:"service"`
	Action    string `msgpack:"action"`
	Kind      string `msgpack:"kind,omitempty"`
	Phase     string `msgpack:"phase"`
	Total     int    `msgpack:"total"`
	Ok        int    `msgpack:"ok"`
	Failed    int    `msgpack:"failed"`
	Skipped   int    `msgpack:"skipped"`
	Pending   int    `msgpack:"pending"` // 尚未开始 + 正在跑
	KeyTotal  int    `msgpack:"key_total"`
	KeyDone   int    `msgpack:"key_done"`
	Woken     bool   `msgpack:"woken"` // key 唤醒是否已触发
	ElapsedMS int64  `msgpack:"elapsed_ms"`
}

// BatchResultPayload 取明细（分页）。
type BatchResultPayload struct {
	ItemID string `msgpack:"item_id"`
	Offset int    `msgpack:"offset,omitempty"`
	Limit  int    `msgpack:"limit,omitempty"`  // 默认 20，上限 100
	Status string `msgpack:"status,omitempty"` // 只看一类：ok | failed | skipped | pending
}

// BatchItemResult 是一项的结果摘要（完整响应不落库）。
type BatchItemResult struct {
	Seq     int    `msgpack:"seq"`
	Tag     string `msgpack:"tag,omitempty"`
	Key     bool   `msgpack:"key,omitempty"`
	Status  string `msgpack:"status"`
	Error   string `msgpack:"error,omitempty"`
	Preview string `msgpack:"preview,omitempty"` // 响应摘要（截断到 preview_bytes）
	Size    int    `msgpack:"size,omitempty"`    // 原始响应字节数（截断前）
	MS      int64  `msgpack:"ms,omitempty"`
}

// BatchResultResult 是一页明细。
type BatchResultResult struct {
	ItemID  string            `msgpack:"item_id"`
	Offset  int               `msgpack:"offset"`
	Limit   int               `msgpack:"limit"`
	Matched int               `msgpack:"matched"` // 满足 Status 过滤的总条数
	Items   []BatchItemResult `msgpack:"items"`
}

// BatchListPayload 查任务队列（LLM 与前端共用）。
type BatchListPayload struct {
	Phase  string `msgpack:"phase,omitempty"`  // "" = 全部
	Limit  int    `msgpack:"limit,omitempty"`  // 默认 20，上限 100
	Caller string `msgpack:"caller,omitempty"` // 只看某个发起方
}

// BatchJobView 是队列里的一行。
type BatchJobView struct {
	ItemID    string `msgpack:"item_id"`
	Service   string `msgpack:"service"`
	Action    string `msgpack:"action"`
	Caller    string `msgpack:"caller"`
	Kind      string `msgpack:"kind,omitempty"`
	Phase     string `msgpack:"phase"`
	Total     int    `msgpack:"total"`
	Ok        int    `msgpack:"ok"`
	Failed    int    `msgpack:"failed"`
	Skipped   int    `msgpack:"skipped"`
	Running   int    `msgpack:"running"`
	Pending   int    `msgpack:"pending"`
	KeyTotal  int    `msgpack:"key_total"`
	KeyDone   int    `msgpack:"key_done"`
	Summary   string `msgpack:"summary,omitempty"` // 一句话摘要（失败原因归组；无失败时为空）
	ElapsedMS int64  `msgpack:"elapsed_ms"`
}

// BatchCounts 是队列的汇总计数。
type BatchCounts struct {
	Running int `msgpack:"running"`
	Paused  int `msgpack:"paused"`
	Done    int `msgpack:"done"`
	Failed  int `msgpack:"failed"`
}

// BatchListResult 是队列快照。
type BatchListResult struct {
	Jobs   []BatchJobView `msgpack:"jobs"`
	Counts BatchCounts    `msgpack:"counts"`
}

// BatchControlPayload 是暂停 / 恢复 / 停止。
//
// 三种操作合成一个动作：工具表少两条，模型也少犯"该用哪个"的错。
type BatchControlPayload struct {
	Op     string `msgpack:"op"`                // pause | resume | cancel
	Scope  string `msgpack:"scope,omitempty"`   // ""/one（默认） | all
	ItemID string `msgpack:"item_id,omitempty"` // scope=one 时必填
}

// BatchControlResult 是控制操作的结回。
type BatchControlResult struct {
	Op       string `msgpack:"op"`
	Scope    string `msgpack:"scope"`
	Affected int    `msgpack:"affected"` // 实际改变了状态的任务数
	Paused   int    `msgpack:"paused"`   // 操作后仍处于 paused 的任务数
	Running  int    `msgpack:"running"`
}

// UserHoldPayload 是入口服务把"用户停手"这件事立刻告诉推理侧。
//
// 停手分三级，由入口服务按"这次操作碰到了哪些任务"决定（见 batch/DESIGN.md §3.10）：
//   - primary  ：全部操作且碰到非常驻任务 → 停手，只等用户下一条消息；
//   - secondary：单个/部分操作且碰到非常驻任务 → 停手，用户不说话时等其他非常驻任务全结束再唤醒；
//   - none     ：只碰到常驻任务 → 不停手，附注随下一次汇报带出去。
type UserHoldPayload struct {
	ConversationID string   `msgpack:"conversation_id"`
	Hold           string   `msgpack:"hold,omitempty"`         // ""/none | secondary | primary
	PausedItems    []string `msgpack:"paused_items,omitempty"` // 被暂停/停止的异步句柄（不再算"可执行"）
	Notes          []string `msgpack:"notes,omitempty"`        // 随之投递的附注（与 UserMessagePayload.Notes 同名同义）
}

// 停手级别的取值。
const (
	UserHoldNone      = "none"
	UserHoldSecondary = "secondary"
	UserHoldPrimary   = "primary"
)
