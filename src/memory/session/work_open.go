package session

// work_open（R-026 P2c）：reasoner 建链前的唯一 work / task 登记入口。
//
// 旧 context 顺带做 EnsureTask + ensureContextWork；P2 切换后 context 只按 work 组装，
// 登记挪到这里，语义保持：
//   - work 归属：owner = 部署默认人，agent = 帧头 from，entry = boss（入口服务；空则 from），
//     goal = 任务名；状态 running（建链即开跑）；
//   - 给了 conversation_id / task_name 就解析或新建当前任务，并把 work 挂到它（works.task）；
//   - 幂等：同一 work 重复调用不重建；已挂到别的任务时由 LinkWorkTask 回 conflict。
//
// 授权：work 还不存在，不能走 authorizeTargetWork；这里要求非 owner/runtime 的调用方
// 帧头 work 必须等于载荷 work（不能替别人的 trace 登记），匿名一律拒绝。work 建好之后
// 的读取 / 写入由 authorizeTargetWork 按归属判定。

import (
	"context"
	"strings"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// workOpenMethod 是 work_open 的服务卡声明。
func workOpenMethod() spv2.Method {
	return spv2.Method{
		Name:        "work_open",
		Description: "登记 work 并解析/创建它所属的任务，返回 task_id（幂等；建链前调用）",
		Input: objectSchema(map[string]any{
			"work":            stringSchema(),
			"conversation_id": stringSchema(),
			"task_name":       stringSchema(),
			"boss":            stringSchema(),
		}, "work"),
		Output: objectSchema(map[string]any{
			"task_id": stringSchema(),
		}, "task_id"),
		Tool: false,
	}
}

// nativeWorkOpen 是 work_open 的原生入口。
func (s *Server) nativeWorkOpen(ctx context.Context, call *spv2.Call) error {
	out, err := s.workOpen(ctx, call.Frame.Body, callerOf(call))
	if err != nil {
		return replyWorkError(call, err)
	}
	return call.Reply(out)
}

// workOpen 是 work_open 的共享实现（handler 只做转发）。
func (s *Server) workOpen(ctx context.Context, body []byte, c caller) (any, error) {
	var req protocol.WorkOpenPayload
	if err := protocol.DecodeRaw(body, &req); err != nil {
		return nil, v2Error(spv2.CodeInvalidArgument, "work_open body 无效: %v", err)
	}
	work := strings.TrimSpace(req.Work)
	if work == "" {
		return nil, v2Error(spv2.CodeInvalidArgument, "work_open: work 必填")
	}
	if c.anonymous() {
		return nil, authDenied("work_open", "调用方身份为空，拒绝登记 work")
	}
	from := c.from
	if from.Kind != "agent" && from.Kind != "service" &&
		from.Authority != spv2.AuthorityOwner && from.Authority != spv2.AuthorityRuntime {
		return nil, v2Error(spv2.CodePermissionDenied,
			"work_open: from.kind 必须是 agent/service（或 owner/runtime）")
	}
	if from.Authority != spv2.AuthorityOwner && from.Authority != spv2.AuthorityRuntime &&
		strings.TrimSpace(c.work) != work {
		return nil, authDenied("work_open", "帧头 work 与载荷 work 不一致")
	}

	conversation := strings.TrimSpace(req.ConversationID)
	taskName := protocol.ClampTaskName(req.TaskName)
	taskID := ""
	if conversation != "" || taskName != "" {
		if conversation == "" {
			conversation = "default"
		}
		if taskName == "" {
			taskName = "未命名任务"
		}
		task, err := s.cfg.Store.EnsureTask(conversation, taskName)
		if err != nil {
			return nil, v2Error(spv2.CodeInternal, "work_open: 任务解析失败: %v", err)
		}
		taskID = task.ID
	}

	entry := strings.TrimSpace(req.Boss)
	if entry == "" {
		entry = from.ID
	}
	if _, err := s.cfg.Store.EnsureWork(ctx, spv2.Work{
		ID:     work,
		Root:   work,
		Class:  spv2.ClassRequest,
		Owner:  spv2.HumanRef("owner"),
		Agent:  spv2.AgentRef(from.ID),
		Entry:  spv2.ServiceRef(entry),
		Goal:   taskName,
		Status: spv2.WorkRunning,
	}); err != nil {
		return nil, err
	}
	if taskID != "" {
		if err := s.cfg.Store.LinkWorkTask(ctx, work, taskID); err != nil {
			return nil, mapCallError(err)
		}
	} else {
		// 没带任务信息：回既有归属（可能是空串）。work 刚 Ensure 过，这里只可能是读库错误。
		existing, err := s.cfg.Store.TaskIDOfWork(ctx, work)
		if err != nil {
			return nil, mapCallError(err)
		}
		taskID = existing
	}
	return protocol.WorkOpenResult{TaskID: taskID}, nil
}
