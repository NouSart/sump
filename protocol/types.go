// Package protocol 是 SUMP v2 的服务间协议：信封、消息类型与
// MessagePack 编解码。各服务共享此包作为“协议规范”的 Go 实现。
package protocol

// 消息类型（信封 type 字段的取值）。
const (
	TypeRegister  = "register"  // 服务 → 设置中心：注册（请求）
	TypeResponse  = "response"  // 通用响应（id 与请求配对）
	TypeRoster    = "roster"    // 设置中心 → 各服务：全局清单推送（事件）
	TypeJump      = "jump"      // 任意服务 → 任意服务：跳转访问（请求）
	TypeHeartbeat = "heartbeat" // 服务 → 设置中心：报活（事件）
	TypeSettings  = "settings"  // 设置中心 → 各服务：设置变更与下发结果（事件）
)

// ProtocolVersion 是当前协议版本（信封 v 字段）。
const ProtocolVersion = 1
