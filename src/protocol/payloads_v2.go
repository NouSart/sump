package protocol

// 记忆中心 v2（S-001）的载荷类型——P1 子集：store 的幂等 / 补报（§3.0 / §3.0.1 / §4.3）；
// P2 子集：context 组装的四态与 trigger_complete（§3.1 / §3.1.1）。
// 命名一律按 S-001 §3.9 词汇表；线上键名是 snake_case，且**只增不改**。
// call_* / resident 等载荷按相位后续追加——没有写入方的字段不预留。

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
