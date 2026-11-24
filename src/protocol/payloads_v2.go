package protocol

// 记忆中心 v2（S-001）的载荷类型：store 幂等 / 补报、context 组装、trigger 完成、
// 调用生命周期（call_open / call_control / call_status）与常驻目录（resident_list）。
// 命名一律按 S-001 §3.9 词汇表；线上键名是 snake_case，且**只增不改**。
// 词汇取值见 Spec：observe = waiting/not_waiting/closed；exec = created/running/paused/done/failed/cancelled/unknown；
// phase = done/failed/timeout/cancelled/orphaned/unknown；visibility = work/thread/task。

import "github.com/vmihailenco/msgpack/v5"

// ---------- store 幂等与补报（S-001 §3.0 / §3.0.1 / §4.3） ----------

// StoreReport 是 store 走补报路径时携带的入账元数据（S-001 §3.0）。
// 它是 report_receipts 的幂等键来源：同一 (source_service, report_id) 重放只入账一次；
// (call_id, report_seq) 的唯一约束防止“换 report_id 重放”。
// Phase / Final / Data 一并持久化；Data 原样透传 owner_service 自定的结构化补充（msgpack 字节不改写）。
type StoreReport struct {
	SourceService string             `msgpack:"source_service" json:"source_service"`
	ReportID      string             `msgpack:"report_id" json:"report_id"`
	ReportSeq     int64              `msgpack:"report_seq" json:"report_seq"`
	CallID        string             `msgpack:"call_id" json:"call_id"`
	Phase         string             `msgpack:"phase,omitempty" json:"phase,omitempty"`
	Final         bool               `msgpack:"final,omitempty" json:"final,omitempty"`
	Data          msgpack.RawMessage `msgpack:"data,omitempty" json:"data,omitempty"`
}

// StoreResult 是 store 的回执（S-001 §3.0）：item_id 是落库记录 id，seq 是会话内单调序号。
// Deduped = true 表示同 (work, client_item_id) 同 payload_hash 的重放命中，未重复写入。
type StoreResult struct {
	ItemID  string `msgpack:"item_id" json:"item_id"`
	Seq     int64  `msgpack:"seq" json:"seq"`
	Deduped bool   `msgpack:"deduped,omitempty" json:"deduped,omitempty"`
}

// ---------- context 组装（S-001 §3.1） ----------

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

// PendingRef 是一条“还在等”的引用的精简视图（S-001 §3.1 的可选扩展，归 C 切片）。
// 只给判定所需的最小事实：哪次 call、观察态与执行态各是什么。
type PendingRef struct {
	Call    string `msgpack:"call,omitempty" json:"call,omitempty"`
	Observe string `msgpack:"observe,omitempty" json:"observe,omitempty"`
	Exec    string `msgpack:"exec,omitempty" json:"exec,omitempty"`
}

// TaskBlocker 是一条任务门禁阻塞（S-001 §5.3 的可选扩展，归 C 切片）。
// Work 是被卡住的 work，Call 是具体卡住的调用（子 work 卡死时可为空），Reason 是给人看的原因。
type TaskBlocker struct {
	Work   string `msgpack:"work" json:"work"`
	Call   string `msgpack:"call,omitempty" json:"call,omitempty"`
	Reason string `msgpack:"reason" json:"reason"`
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
// A 切片核心承诺：messages / task / work / snapshot_id / expires_at / degraded / warnings；
// pending / task_blockers 是可选扩展（C 切片冻结）。Degraded 为 nil 表示无降级。
type ContextResponse struct {
	Schema       string        `msgpack:"schema" json:"schema"`
	SnapshotID   string        `msgpack:"snapshot_id" json:"snapshot_id"`
	ExpiresAt    string        `msgpack:"expires_at" json:"expires_at"`
	Messages     []LLMMessage  `msgpack:"messages" json:"messages"`
	Task         Task          `msgpack:"task" json:"task"`
	Work         WorkView      `msgpack:"work" json:"work"`
	Degraded     *Degraded     `msgpack:"degraded,omitempty" json:"degraded,omitempty"`
	Warnings     []string      `msgpack:"warnings,omitempty" json:"warnings,omitempty"`
	Pending      []PendingRef  `msgpack:"pending,omitempty" json:"pending,omitempty"`
	TaskBlockers []TaskBlocker `msgpack:"task_blockers,omitempty" json:"task_blockers,omitempty"`
	AssembledAt  string        `msgpack:"assembled_at" json:"assembled_at"`
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
// 只回能确认的字段：phase 允许为空（不得把“不认识”写成 orphaned）。
type CallStatusItem struct {
	ID           string `msgpack:"id" json:"id"`
	Observe      string `msgpack:"observe,omitempty" json:"observe,omitempty"`
	Exec         string `msgpack:"exec,omitempty" json:"exec,omitempty"`
	Phase        string `msgpack:"phase,omitempty" json:"phase,omitempty"`
	OwnerService string `msgpack:"owner_service,omitempty" json:"owner_service,omitempty"`
	UpdatedAt    string `msgpack:"updated_at,omitempty" json:"updated_at,omitempty"`
}

// CallStatusResult 是 call_status 的对账结果（S-001 §3.5）。
// OwnerRecoveryComplete 必须显式上线（false 也有意义）：为 false 时不得把“不认识该 call”判为 orphaned。
type CallStatusResult struct {
	OwnerRecoveryComplete bool             `msgpack:"owner_recovery_complete" json:"owner_recovery_complete"`
	Items                 []CallStatusItem `msgpack:"items" json:"items"`
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
