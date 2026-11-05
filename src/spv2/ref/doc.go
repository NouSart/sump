// Package ref 是 SPv2.1 的参考链路：
//
//	entry -> reasoner -> memory.context -> tool.echo -> reasoner -> entry.deliver
//
// 四个服务都经真实 WS（spv2.Server / spv2.Dial）通信，reasoner 走固定流程，
// 不接 LLM。它用于证明协议链路可运行，不是生产实现。
//
// 已覆盖 memory.context / work_get / cancel / renew / lease_charge / message_*，
// reasoner.message，tool.echo，entry.deliver。
package ref
