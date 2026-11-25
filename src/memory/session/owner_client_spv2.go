package session

// owner 生产客户端（R-029）：通过会话服务的出站一跳（service.Service.Call）调用
// owner_status / owner_control，按服务名解析目标，不按服务名写适配器。
//
// 边界：
//   - 只做通用 SPv2 调用 + 错误/回执映射；控制是否算「确认」由调用点判定（见 owner_client.go）。
//   - id 是 owner 的受理句柄（calls.remote_item_id），原样发上线；本层不碰 memory 的 call_id。
//   - 超时由调用方的 ctx 决定：call_control 走 ownerControlContext（ttl_ms / 默认 60s），
//     call_status 与周期对账走 ownerStatusContext（10s）。本层不再自设定时器——多一层只会
//     制造第二个上限，排障时分不清是谁先放弃。
//   - owner 的错误码（not_found / not_supported / deadline_exceeded …）保留在返回错误里，
//     并补上「哪个服务、哪个对象、哪个动作」；ctx 的取消 / 超时用 %w 返回，不吞成 nil。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// OwnerCaller 是生产客户端需要的出站一跳。*service.Service 满足；单测可替换。
type OwnerCaller interface {
	Call(ctx context.Context, to, action string, payload any, trace string) (msgpack.RawMessage, error)
}

// ownerStatusMethod / ownerControlMethod 是 owner 侧对端的 method 名（R-029）。
// 字面量集中在这里：客户端不认服务名，只认协议。
const (
	ownerStatusMethod  = "owner_status"
	ownerControlMethod = "owner_control"
)

// spv2OwnerClient 是 OwnerClient 的 SPv2 生产实现。
type spv2OwnerClient struct {
	caller OwnerCaller
}

// NewSPv2OwnerClient 构造生产 owner 客户端。caller 通常是 session 服务自己的
// service.Service（帧头 from = session，按名册/静态对端解析 owner 服务名）。
func NewSPv2OwnerClient(caller OwnerCaller) OwnerClient {
	return &spv2OwnerClient{caller: caller}
}

// Status 批量问 owner_status（S-001 §3.5）。
//
// 回执只信本批发出过的句柄：owner 多回或回空 exec 的键都不带回来（不猜、不补值）。
// owner_status 的不认识表现为 states 里没有该句柄，而不是错误；整批错误（含 unknown
// method 的 not_found、not_supported、离线、超时）原样返回，调用方据此保持
// recovery_complete=false，不把「问不到」判成 orphaned。
func (c *spv2OwnerClient) Status(ctx context.Context, service string, ids []string) (map[string]string, bool, error) {
	if len(ids) == 0 {
		return map[string]string{}, true, nil
	}
	cctx, cancel := ownerStatusContext(ctx)
	defer cancel()
	raw, err := c.caller.Call(cctx, service, ownerStatusMethod,
		protocol.OwnerStatusPayload{IDs: ids}, "")
	if err != nil {
		return nil, false, mapOwnerWireError(service, ownerStatusMethod, batchScope(ids), err)
	}
	var res protocol.OwnerStatusResult
	if err := protocol.DecodeRaw(raw, &res); err != nil {
		return nil, false, fmt.Errorf("owner %s %s %s: 回执解析失败: %w",
			service, ownerStatusMethod, batchScope(ids), err)
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	states := make(map[string]string, len(res.States))
	for id, exec := range res.States {
		exec = strings.TrimSpace(exec)
		if !wanted[id] || exec == "" {
			continue
		}
		states[id] = exec
	}
	return states, res.RecoveryComplete, nil
}

// Pause 请求 owner 暂停一次已受理的调用。
func (c *spv2OwnerClient) Pause(ctx context.Context, service, remoteItemID string) (string, error) {
	return c.control(ctx, service, remoteItemID, protocol.OwnerOpPause)
}

// Resume 请求 owner 恢复一次已受理的调用。
func (c *spv2OwnerClient) Resume(ctx context.Context, service, remoteItemID string) (string, error) {
	return c.control(ctx, service, remoteItemID, protocol.OwnerOpResume)
}

// Cancel 请求 owner 取消一次已受理的调用。
func (c *spv2OwnerClient) Cancel(ctx context.Context, service, remoteItemID string) (string, error) {
	return c.control(ctx, service, remoteItemID, protocol.OwnerOpCancel)
}

// control 发一次 owner_control，回 owner 确认后的执行维。
//
// 本层不判定「算不算确认」：空串 / 矛盾值原样带回，由调用点按 ownerConfirmed 判定。
// 但回执对象必须与请求一致——owner 答的是别的句柄时不能把它当成这次调用的答案。
func (c *spv2OwnerClient) control(ctx context.Context, service, remoteItemID, op string) (string, error) {
	remoteItemID = strings.TrimSpace(remoteItemID)
	if remoteItemID == "" {
		return "", errors.New("owner " + service + " " + op + ": 受理句柄为空，不能向 owner 发起控制")
	}
	raw, err := c.caller.Call(ctx, service, ownerControlMethod, protocol.OwnerControlPayload{
		ID: remoteItemID, Op: op,
	}, "")
	if err != nil {
		return "", mapOwnerWireError(service, op, remoteItemID, err)
	}
	var res protocol.OwnerControlResult
	if err := protocol.DecodeRaw(raw, &res); err != nil {
		return "", fmt.Errorf("owner %s %s %q: 回执解析失败: %w", service, op, remoteItemID, err)
	}
	if strings.TrimSpace(res.ID) != remoteItemID {
		return "", spv2.ErrCode(spv2.CodeInternal, fmt.Sprintf(
			"owner %s %s %q: 回执对象不符（回执 id=%q）", service, op, remoteItemID, res.ID), false, nil)
	}
	return res.Exec, nil
}

// mapOwnerWireError 给 owner 的失败补上上下文，并保留错误码：
// 调用方按码归类（not_found / not_supported / deadline_exceeded …），加了前缀不能退化成 internal。
// ctx 的取消 / 超时用 %w 包装，errors.Is 仍能判断。
func mapOwnerWireError(service, op, object string, err error) error {
	var e *spv2.Error
	if errors.As(err, &e) {
		return spv2.ErrCode(e.Code, fmt.Sprintf("owner %s %s %s: %s", service, op, object, e.Message),
			e.Retryable, e.Details)
	}
	// 非 SPv2 错误（含 ctx 取消 / 超时）用 %w：errors.Is(err, context.DeadlineExceeded)
	// 仍然成立，ctx 错误不会被吞成 nil。
	return fmt.Errorf("owner %s %s %s: %w", service, op, object, err)
}

// batchScope 给一批句柄的日志/错误一个短标识（不把整批 id 塞进错误）。
func batchScope(ids []string) string {
	return fmt.Sprintf("(%d 个句柄)", len(ids))
}
