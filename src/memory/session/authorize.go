package session

// work 归属授权原语（S-001 §9；R-011 §2.2 红队修复；R-023 P4）。
//
// 两个判定覆盖 v2 入口：
//   - authorizeTargetWork：载荷里指定了目标 work 的入口（store v2 / context_base /
//     trigger_complete / call_open）。普通调用方必须「帧头 work == 载荷 work」且身份属于
//     目标 work（agent / owner / entry）；owner / runtime 保留跨 work 通道，但 owner 必须
//     精确匹配 works.owner.id，不能只凭 authority 字段跨 work。
//   - authorizeCallOp：按目标 call 的 work 归属判定（call_control / call_status）。目标 call
//     的 work 行缺失时没有可核对的归属，fail-closed。
//
// 匿名（from.id 为空）一律 permission_denied，且先于 authority 分支：runtime 也不能借
// 空身份跨 work。拒绝文案只描述「不匹配 / 无权」，不回显目标 work 的 owner / agent /
// entry，避免把错误信息变成归属探测接口。

import (
	"context"
	"errors"
	"strings"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/spv2"
)

// caller 是一次原生调用的帧头归属：帧头 work 与 from 身份。
// handler 从 *spv2.Call 取（callerOf），测试可直接构造；授权只看它与载荷 work。
type caller struct {
	work string
	from spv2.Ref
}

// callerOf 把帧头元信息收成授权输入（不做任何校验或补全）。
func callerOf(call *spv2.Call) caller {
	return caller{work: call.WorkID(), from: call.Frame.From}
}

// anonymous 表示帧头身份为空；所有 v2 入口都必须拒绝，不能按 authority 放行。
func (c caller) anonymous() bool {
	return strings.TrimSpace(c.from.ID) == ""
}

// authDenied 构造统一的 permission_denied；reason 不得包含目标 work 的归属字段。
func authDenied(op, reason string) error {
	return spv2.ErrCode(spv2.CodePermissionDenied, op+": "+reason, false, nil)
}

// authorizeTargetWork 校验调用方能否操作载荷里指定的目标 work。
//
// 分支顺序是契约的一部分：
//  1. 载荷 work 为空 → invalid_argument（没有归属的读写不能靠猜）；
//  2. 匿名 from.id 为空 → permission_denied（先于查库与 authority：匿名不能探测 work 是否存在）；
//  3. 目标 work 不存在 → not_found（不含归属信息）；
//  4. runtime → 放行（运行期管控需要跨 work）；
//  5. owner → from.id 必须与 works.owner.id 精确相等；
//  6. 普通服务 → 帧头 work 必须等于目标 work，且身份属于该 work
//     （agent / owner / entry，口径与 authorizeWork 相同）。
func (s *Server) authorizeTargetWork(ctx context.Context, targetWork, callerWork, op string, from spv2.Ref) (spv2.Work, error) {
	targetWork = strings.TrimSpace(targetWork)
	if targetWork == "" {
		return spv2.Work{}, spv2.ErrCode(spv2.CodeInvalidArgument, op+": work 必填", false, nil)
	}
	fromID := strings.TrimSpace(from.ID)
	if fromID == "" {
		return spv2.Work{}, authDenied(op, "调用方身份为空，拒绝操作")
	}
	w, err := s.cfg.Store.GetWork(ctx, targetWork)
	if err != nil {
		// 目标 work 查不到翻成 not_found；其它存储错误按协议错误码透出。
		return spv2.Work{}, mapCallError(err)
	}

	switch from.Authority {
	case spv2.AuthorityRuntime:
		return w, nil
	case spv2.AuthorityOwner:
		if fromID == strings.TrimSpace(w.Owner.ID) {
			return w, nil
		}
		return spv2.Work{}, authDenied(op, "owner 与目标 work 不匹配")
	}

	if strings.TrimSpace(callerWork) != strings.TrimSpace(w.ID) {
		return spv2.Work{}, authDenied(op, "调用方 work 与目标 work 不一致")
	}
	if err := authorizeWork(w, from); err != nil {
		return spv2.Work{}, authDenied(op, "无权操作目标 work")
	}
	return w, nil
}

// authorizeCallOp 判定调用方可否控制 / 查询目标 call（S-001 §9；R-011 §2.2）。
//
// 红队问题：同一个 agent 服务同时服务多个 work 时，仅按 works.agent.id 比对会允许
// w-alice 的 agent 拿 w-bob 的 call_id 取消别人的调用。因此：
//   - 普通调用方（agent / 入口 / 服务）：调用方 work 必须等于目标 call 的 work，
//     且身份属于该 work（agent / owner / entry，与 authorizeWork 同口径）；
//   - 人类 owner 入口（from.Authority=owner 且 works.owner 与 from.id 匹配）：可跨 work
//     处置自己的调用（用户从自己的入口取消时，调用方 work 可能不是目标 work）；
//   - 运行期管控（runtime authority）：保留运维处置通道，可跨 work；
//   - 匿名 / work 行缺失 / 其它一律 permission_denied（错误文案不泄漏目标参数与归属）。
func (s *Server) authorizeCallOp(ctx context.Context, row store.CallRow, callerWork string, from spv2.Ref, op string) error {
	fromID := strings.TrimSpace(from.ID)
	if fromID == "" {
		return authDenied(op, "调用方身份为空，拒绝操作")
	}
	w, err := s.cfg.Store.GetWork(ctx, row.Work)
	if err != nil {
		if spv2.IsCode(err, spv2.CodeNotFound) || errors.Is(err, store.ErrWorkNotFound) {
			// work 行缺失：没有可核对的归属，fail-closed。
			return authDenied(op, "目标调用的 work 归属不可核对，拒绝操作")
		}
		return mapCallError(err)
	}
	switch from.Authority {
	case spv2.AuthorityRuntime:
		return nil
	case spv2.AuthorityOwner:
		// owner 权限必须精确对应 works.owner，不能凭 authority 字段跨 work。
		if fromID == strings.TrimSpace(w.Owner.ID) {
			return nil
		}
		return authDenied(op, "无权操作该调用（owner 与调用方不匹配）")
	}
	if strings.TrimSpace(callerWork) != strings.TrimSpace(w.ID) {
		return authDenied(op, "调用方 work 与目标调用不一致，拒绝跨 work 操作")
	}
	if err := authorizeWork(w, from); err != nil {
		return authDenied(op, "无权操作该 work 的调用（调用方不是该 work 的 agent / 入口）")
	}
	return nil
}
