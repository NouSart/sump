// Package protocol 是 SUMP 的跨服务业务协议：服务名片、业务载荷与
// MessagePack 编解码。传输层是 SPv2.1（Frame / call / reply / event / stream），
// 由 src/spv2 实现；本包只定义双方共享的业务数据结构。
package protocol

// 事件 method 名（SPv2.1 event 帧的 method 字段）。
const (
	TypeRoster   = "roster"   // 设置中心 → 各服务：全局清单推送（事件）
	TypeSettings = "settings" // 设置中心 → 各服务：设置变更与下发结果（事件）
)
