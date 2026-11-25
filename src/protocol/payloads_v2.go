package protocol

// 记忆中心 v2（S-001）的载荷类型——P1 子集：store 的幂等 / 补报（§3.0 / §3.0.1 / §4.3）；
// P2 子集：context 组装的四态与 trigger_complete（§3.1 / §3.1.1）、transcript（§3.2）；
// P3 子集：调用生命周期（call_open / call_control / call_status）与常驻目录（resident_list，
// §3.0.2 / §3.4 / §3.5 / §3.6）；R-029：执行服务侧的对账 / 控制面（owner_status / owner_control）。
// 命名一律按 S-001 §3.9 词汇表；线上键名是 snake_case，且**只增不改**。
// 没有写入方的字段不预留：pending / task_blockers / image 相关类型都没有搬。
//
// 词汇取值见 Spec：observe = waiting/not_waiting/closed；
// exec = created/running/paused/done/failed/cancelled/unknown；
// phase = done/failed/timeout/cancelled/orphaned/unknown。

import "github.com/vmihailenco/msgpack/v5"

// ---------- store 幂等与补报（S-001 §3.0 / §3.0.1 / §4.3） ----------

// StoreReport 是 store 走补报路径时携带的入账元数据（S-001 §3.0）。
// 它是 report_receipts 的幂等键来源：同一 (source_service, report_id) 重放只入账一次；
// (call_id, report_seq) 的唯一约束防止“换 report_id 重放”。
// Phase / Final / Data 一并持久化；Data 原样透传 owner_service 自定的结构化补充（msgpack 字节不改写）。
//
// RemoteItemID 是 owner 自己的受理句柄（R-030 加法字段）：owner 的补报经 agent 中转，
// agent 从受理回执的 item_id 拿到它，随补报带回 memory，用于回填 calls.remote_item_id。
// 省略 / 空串 = 本次补报不携带句柄（memory 保持原值，不回填空值）。
type StoreReport struct {
	SourceService string             `msgpack:"source_service" json:"source_service"`
	ReportID      string             `msgpack:"report_id" json:"report_id"`
	ReportSeq     int64              `msgpack:"report_seq" json:"report_seq"`
	CallID        string             `msgpack:"call_id" json:"call_id"`
	Phase         string             `msgpack:"phase,omitempty" json:"phase,omitempty"`
	Final         bool               `msgpack:"final,omitempty" json:"final,omitempty"`
	Data          msgpack.RawMessage `msgpack:"data,omitempty" json:"data,omitempty"`
	RemoteItemID  string             `msgpack:"remote_item_id,omitempty" json:"remote_item_id,omitempty"`
}

// StoreResult 是 store 的回执（S-001 §3.0）：item_id 是落库记录 id，seq 是会话内单调序号。
// Deduped = true 表示同 (work, client_item_id) 同 payload_hash 的重放命中，未重复写入。
type StoreResult struct {
	ItemID  string `msgpack:"item_id" json:"item_id"`
	Seq     int64  `msgpack:"seq" json:"seq"`
	Deduped bool   `msgpack:"deduped,omitempty" json:"deduped,omitempty"`
}

// ---------- work_open（R-026 P2c） ----------

// WorkOpenPayload 是 work_open 的入参：登记执行单元，并按会话解析 / 创建它所属的任务。
// Work 必填；ConversationID / TaskName 是首次建任务所需（省略时只登记 work，回既有 task id 或空）。
// Boss 是结果归属方（入口服务），落进 works.entry；省略时用帧头 from。
// 幂等：同一 work 重复调用不重建（已关联别的 task 时由会话库回 conflict）。
type WorkOpenPayload struct {
	Work           string `msgpack:"work" json:"work"`
	ConversationID string `msgpack:"conversation_id,omitempty" json:"conversation_id,omitempty"`
	TaskName       string `msgpack:"task_name,omitempty" json:"task_name,omitempty"`
	Boss           string `msgpack:"boss,omitempty" json:"boss,omitempty"`
}

// WorkOpenResult 是 work_open 的回执：这次 work 归属的任务 id（没有任务时为空）。
type WorkOpenResult struct {
	TaskID string `msgpack:"task_id" json:"task_id"`
}

// ---------- context 组装（S-001 §3.1 / §3.1.1） ----------

// Trigger 是一次 context 组装的触发源（S-001 §3.1）。
// Kind 取 user / tool_result / timer / resume / spawn；Item 是已落库的触发条目 id。
type Trigger struct {
	Kind string `msgpack:"kind" json:"kind"`
	Item string `msgpack:"item" json:"item"`
}

// Degraded 是依赖降级说明（S-001 §3.1 / §8）：level 取 fatal / severe / warn。
// Since 是降级开始时间；RepairHint 是给人看的修复提示，均可选。
type Degraded struct {
	Level      string `msgpack:"level" json:"level"`
	Reason     string `msgpack:"reason" json:"reason"`
	Since      string `msgpack:"since,omitempty" json:"since,omitempty"`
	RepairHint string `msgpack:"repair_hint,omitempty" json:"repair_hint,omitempty"`
}

// LLMToolCall 是 assembled context 里 assistant 发起的一次工具调用（S-001 §3.1）。
// 与旧 Message 的 ToolCall 同形但更扁平：模型侧只需要 id / name / arguments。
type LLMToolCall struct {
	ID        string `msgpack:"id" json:"id"`
	Name      string `msgpack:"name" json:"name"`
	Arguments string `msgpack:"arguments" json:"arguments"`
}

// LLMMessage 是 context 响应里的模型消息（S-001 §3.1）：只承载模型输入所需字段。
// Role 取 system / user / assistant / tool；ToolCalls 仅 assistant 用；ToolCallID 仅 tool 用，指回对应调用。
type LLMMessage struct {
	Role       string        `msgpack:"role" json:"role"`
	Content    string        `msgpack:"content,omitempty" json:"content,omitempty"`
	ToolCalls  []LLMToolCall `msgpack:"tool_calls,omitempty" json:"tool_calls,omitempty"`
	ToolCallID string        `msgpack:"tool_call_id,omitempty" json:"tool_call_id,omitempty"`
	Name       string        `msgpack:"name,omitempty" json:"name,omitempty"`
}

// WorkView 是 context 响应里当前 work 的投影（S-001 §2.1 / §3.1 / §3.9）。
// Class 取 request / thread / spawn；Owner / Agent 是归属；Entry 是入口服务；Goal 是目标（可选）。
type WorkView struct {
	ID        string `msgpack:"id" json:"id"`
	Task      string `msgpack:"task" json:"task"`
	Class     string `msgpack:"class" json:"class"`
	Status    string `msgpack:"status" json:"status"`
	Owner     string `msgpack:"owner" json:"owner"`
	Agent     string `msgpack:"agent" json:"agent"`
	Entry     string `msgpack:"entry,omitempty" json:"entry,omitempty"`
	Goal      string `msgpack:"goal,omitempty" json:"goal,omitempty"`
	CreatedAt string `msgpack:"created_at" json:"created_at"`
	UpdatedAt string `msgpack:"updated_at" json:"updated_at"`
}

// ContextRequest 是 context 的入参（S-001 §3.1）：按 work + 触发源组装。
// Schema 是调用方期望的载荷版本（如 context/2）；省略时按服务端默认处理。
type ContextRequest struct {
	Work    string  `msgpack:"work" json:"work"`
	Trigger Trigger `msgpack:"trigger" json:"trigger"`
	Schema  string  `msgpack:"schema,omitempty" json:"schema,omitempty"`
}

// ContextResponse 是 context 的响应（S-001 §3.1）。
// A 切片核心承诺：messages / task / work / snapshot_id / expires_at / degraded / warnings。
// pending / task_blockers 是可选扩展（C 切片）：P2 没有写入方，因此这里不出现，
// 要加时按「只增不改」补字段。
//
// TriggerState / Completed 是 v4 评审补上的加法字段：本轮触发走到哪一步要随响应一起回去。
// Completed=true 表示这轮已经 trigger_complete 过，响应**只补 ACK**、不再带消息
// （§3.1.1：完成之后重复取上下文不该重开推理，也不该把历史再灌一遍）。
type ContextResponse struct {
	Schema       string       `msgpack:"schema" json:"schema"`
	SnapshotID   string       `msgpack:"snapshot_id" json:"snapshot_id"`
	ExpiresAt    string       `msgpack:"expires_at" json:"expires_at"`
	Messages     []LLMMessage `msgpack:"messages" json:"messages"`
	Task         Task         `msgpack:"task" json:"task"`
	Work         WorkView     `msgpack:"work" json:"work"`
	Degraded     *Degraded    `msgpack:"degraded,omitempty" json:"degraded,omitempty"`
	Warnings     []string     `msgpack:"warnings,omitempty" json:"warnings,omitempty"`
	AssembledAt  string       `msgpack:"assembled_at" json:"assembled_at"`
	TriggerState string       `msgpack:"trigger_state,omitempty" json:"trigger_state,omitempty"`
	Completed    bool         `msgpack:"completed,omitempty" json:"completed,omitempty"`
}

// TriggerCompletePayload / TriggerCompleteResult：agent 在模型输出与后续动作安全落库后
// 显式标记本轮 trigger 已完成（S-001 §3.1.1）。幂等；对 pending 触发调用必须回 conflict。
type TriggerCompletePayload struct {
	Work    string  `msgpack:"work" json:"work"`
	Trigger Trigger `msgpack:"trigger" json:"trigger"`
}

// TriggerCompleteResult 是 trigger_complete 的回执。
type TriggerCompleteResult struct {
	Completed bool `msgpack:"completed" json:"completed"`
}

// ---------- transcript（S-001 §3.2，A 切片） ----------

// TranscriptPayload 是 transcript 的入参（S-001 §3.2）：把一份已组装好的上下文快照
// 渲染成文本。SnapshotID 必填——文本必须对应一份确定的历史投影，服务端不猜「最新那份」；
// Budget 是可选的裁剪预算，省略 / MaxChars=0 表示不额外限制（仍有 1 MiB 硬顶）。
// S-001 §3.2 没有定义 budget 形状，这里的默认口径写在 docs 与 session/transcript.go 文件头。
type TranscriptPayload struct {
	Work       string           `msgpack:"work" json:"work"`
	SnapshotID string           `msgpack:"snapshot_id" json:"snapshot_id"`
	Budget     TranscriptBudget `msgpack:"budget" json:"budget"`
}

// TranscriptBudget 是 transcript 的裁剪预算：MaxChars 按字符（rune）计，负数非法。
// 不叫 limit / max_length：单位是「字」不是字节，字段名要把这件事说清楚。
type TranscriptBudget struct {
	MaxChars int `msgpack:"max_chars,omitempty" json:"max_chars,omitempty"`
}

// TranscriptResult 是 transcript 的回执（S-001 §3.2）：Text 是按消息顺序渲染的文本；
// Truncated=true 表示末尾被预算截断；Omitted 是因预算整条丢弃的消息条数。
// 后两个字段没有 omitempty：false / 0 是「完整给出」的显式回答，不是缺省。
type TranscriptResult struct {
	Text      string `msgpack:"text" json:"text"`
	Truncated bool   `msgpack:"truncated" json:"truncated"`
	Omitted   int    `msgpack:"omitted" json:"omitted"`
}

// ---------- 调用生命周期：call_open / call_control / call_status（S-001 §3.0.2、§3.4、§3.5） ----------

// CallOpenPayload 是 call_open 的入参（S-001 §3.0.2）：memory 在派发前分配全局唯一的 call_id。
// Mode 取 once / resident；Pausable 是 owner 对可暂停能力的声明；
// TTLMS 约束等待时长，ExecDeadlineMS 只约束 owner 已受理之后的执行（不能用来判断派发成功）。
type CallOpenPayload struct {
	Work           string `msgpack:"work" json:"work"`
	Service        string `msgpack:"service" json:"service"`
	Action         string `msgpack:"action" json:"action"`
	Mode           string `msgpack:"mode" json:"mode"`
	Pausable       bool   `msgpack:"pausable,omitempty" json:"pausable,omitempty"`
	TTLMS          int64  `msgpack:"ttl_ms,omitempty" json:"ttl_ms,omitempty"`
	ExecDeadlineMS int64  `msgpack:"exec_deadline_ms,omitempty" json:"exec_deadline_ms,omitempty"`
}

// CallOpenResult 是 call_open 的回执：memory 分配并持有的 call_id。
type CallOpenResult struct {
	CallID string `msgpack:"call_id" json:"call_id"`
}

// CallControlPayload 是 call_control 的入参（S-001 §3.4）：Op 取 pause / resume / cancel；
// ControlID 幂等（call_controls 表），TTLMS 约束本次控制等待 owner 确认的时长。
type CallControlPayload struct {
	ID        string `msgpack:"id" json:"id"`
	ControlID string `msgpack:"control_id" json:"control_id"`
	Op        string `msgpack:"op" json:"op"`
	TTLMS     int64  `msgpack:"ttl_ms,omitempty" json:"ttl_ms,omitempty"`
}

// CallControlResult 是 call_control 的回执（S-001 §3.4）：两维状态 + 原因。
// ACK 丢失时 exec=unknown、observe=not_waiting，且不写 phase。
type CallControlResult struct {
	ID        string `msgpack:"id" json:"id"`
	ControlID string `msgpack:"control_id" json:"control_id"`
	Observe   string `msgpack:"observe" json:"observe"`
	Exec      string `msgpack:"exec" json:"exec"`
	Reason    string `msgpack:"reason,omitempty" json:"reason,omitempty"`
}

// CallStatusPayload 是 call_status 对账的入参（S-001 §3.5）：一批待核对的 call id。
type CallStatusPayload struct {
	IDs []string `msgpack:"ids" json:"ids"`
}

// CallStatusItem 是单个 call 的对账结果（S-001 §3.5）。owner 恢复未完成 / 不可判定时
// 只回能确认的字段：phase 允许为空（不得把"不认识"写成 orphaned）。
type CallStatusItem struct {
	ID           string `msgpack:"id" json:"id"`
	Observe      string `msgpack:"observe,omitempty" json:"observe,omitempty"`
	Exec         string `msgpack:"exec,omitempty" json:"exec,omitempty"`
	Phase        string `msgpack:"phase,omitempty" json:"phase,omitempty"`
	OwnerService string `msgpack:"owner_service,omitempty" json:"owner_service,omitempty"`
	UpdatedAt    string `msgpack:"updated_at,omitempty" json:"updated_at,omitempty"`
}

// CallStatusResult 是 call_status 的对账结果（S-001 §3.5）。
// OwnerRecoveryComplete 必须显式上线（false 也有意义）：为 false 时不得把"不认识该 call"判为 orphaned。
type CallStatusResult struct {
	OwnerRecoveryComplete bool             `msgpack:"owner_recovery_complete" json:"owner_recovery_complete"`
	Items                 []CallStatusItem `msgpack:"items" json:"items"`
}

// ---------- owner 侧对账 / 控制面（R-029；call_status / call_control 的执行服务对端） ----------

// owner_control 的 op 取值：与 S-001 §3.4 的 pause / resume / cancel 是同一套动作。
const (
	OwnerOpPause  = "pause"
	OwnerOpResume = "resume"
	OwnerOpCancel = "cancel"
)

// owner_status 回的执行维 / owner_control 确认后的执行维：S-001 §3.9 的 exec 词汇
// （owner 与 memory 共用同一套值，串一个都不行）。owner_control 的成功值只有
// paused / running / cancelled；其余值出现在 owner_status 里表示调用已终结或尚未开跑。
const (
	OwnerExecCreated   = "created"
	OwnerExecRunning   = "running"
	OwnerExecPaused    = "paused"
	OwnerExecDone      = "done"
	OwnerExecFailed    = "failed"
	OwnerExecCancelled = "cancelled"
	OwnerExecUnknown   = "unknown"
)

// OwnerStatusPayload 是 owner_status 的入参（R-029）：一批待对账的 call id。
// memory 的 call_status 按 owner_service 分组后问到这里；id 是 owner 自己发过的受理句柄。
type OwnerStatusPayload struct {
	IDs []string `msgpack:"ids" json:"ids"`
}

// OwnerStatusResult 是 owner_status 的结果（R-029）。
//
// States 只包含本服务认识的 call（call_id → 执行维）；不认识的 id 不出现，也不猜一个值。
// RecoveryComplete 必须显式上线（false 也有意义）：为 false（启动早期，持久受理记录尚未
// 恢复完）时，「不认识」不能判成 orphaned（S-001 §3.5 规则 1）。
type OwnerStatusResult struct {
	RecoveryComplete bool              `msgpack:"recovery_complete" json:"recovery_complete"`
	States           map[string]string `msgpack:"states" json:"states"`
}

// OwnerControlPayload 是 owner_control 的入参（R-029）：对一次已受理的执行做控制。
// Op 取 OwnerOp*；TTLMS 是本轮控制等待 owner 确认的上限（省略 = 不额外限制，
// 仍受服务 action timeout 约束）。
type OwnerControlPayload struct {
	ID    string `msgpack:"id" json:"id"`
	Op    string `msgpack:"op" json:"op"`
	TTLMS int64  `msgpack:"ttl_ms,omitempty" json:"ttl_ms,omitempty"`
}

// OwnerControlResult 是 owner_control 的回执（R-029）。
//
// Exec 是 owner 确认后的执行维：成功时分别为 paused / running / cancelled；
// 空串 = 已确认但未带状态维（调用方按约定值处理）；其它值 = 未确认（Reason 说明为什么，
// 例如「取消已受理，但在飞项还要跑完」）。失败必须走错误码（not_found / not_supported /
// deadline_exceeded …），不用结果字段假装成功。
type OwnerControlResult struct {
	ID     string `msgpack:"id" json:"id"`
	Exec   string `msgpack:"exec" json:"exec"`
	Reason string `msgpack:"reason,omitempty" json:"reason,omitempty"`
}

// ---------- 常驻目录（S-001 §3.6，同 v4） ----------

// ResidentItem 是常驻调用目录的一行（S-001 §3.6）：origin_session 是不透明会话 ID。
type ResidentItem struct {
	ID            string `msgpack:"id" json:"id"`
	Kind          string `msgpack:"kind" json:"kind"`
	Title         string `msgpack:"title,omitempty" json:"title,omitempty"`
	Observe       string `msgpack:"observe" json:"observe"`
	Exec          string `msgpack:"exec" json:"exec"`
	Phase         string `msgpack:"phase,omitempty" json:"phase,omitempty"`
	OwnerService  string `msgpack:"owner_service" json:"owner_service"`
	OriginSession string `msgpack:"origin_session,omitempty" json:"origin_session,omitempty"`
	CreatedAt     string `msgpack:"created_at" json:"created_at"`
	UpdatedAt     string `msgpack:"updated_at" json:"updated_at"`
}

// ResidentListPayload 是 resident_list 的入参：limit + 不透明游标。
type ResidentListPayload struct {
	Limit  int    `msgpack:"limit,omitempty" json:"limit,omitempty"`
	Cursor string `msgpack:"cursor,omitempty" json:"cursor,omitempty"`
}

// ResidentListResult 是 resident_list 的结果：Next 为空表示没有下一页。
type ResidentListResult struct {
	Items []ResidentItem `msgpack:"items" json:"items"`
	Next  string         `msgpack:"next,omitempty" json:"next,omitempty"`
}
