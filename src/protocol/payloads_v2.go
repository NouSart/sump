package protocol

// 记忆中心 v2（S-001）的载荷类型——P1 子集：store 的幂等 / 补报（§3.0 / §3.0.1 / §4.3）。
// 命名一律按 S-001 §3.9 词汇表；线上键名是 snake_case，且**只增不改**。
// 本文件只放 store 直接引用的类型；context / trigger / call_* / resident 等载荷按相位后续追加。

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
