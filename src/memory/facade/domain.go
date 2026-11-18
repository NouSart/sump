package facade

import (
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// sessionMethods / experienceMethods 是门面必须能路由的对外 method。
// 它们来自协议归属表，不从内部实现推断；内部服务漏声明会在自检时报错。
var sessionMethods = map[string]bool{
	"store": true, "history": true,
	"conversation_list": true, "conversation_rename": true,
	"conversation_open": true, "conversation_archive": true,
	"task_current": true, "task_open": true, "task_finish": true,
	"task_resume": true, "task_rename": true, "task_list": true,
}

var experienceMethods = map[string]bool{
	"remember": true, "forget": true, "consolidate": true,
	"note": true, "dig": true, "lineheads": true, "prune": true,
	"publish_session": true, "shared_sessions": true, "shared_forget": true,
}

var requiredMethods = func() map[string]bool {
	out := map[string]bool{}
	for m := range sessionMethods {
		out[m] = true
	}
	for m := range experienceMethods {
		out[m] = true
	}
	return out
}()

// internalMethods 是域内 method：门面不得注册、不得出现在名片。
var internalMethods = map[string]bool{
	"context_base": true, "inject": true, "settings_schema": true,
	"staged_pending": true, "staged_mark": true, "staged_retry": true,
	"work_get": true, "lease_charge": true, "lease_release": true,
	"work_update": true, "work_sweep": true, "work_cancel": true, "work_renew": true,
	"message_post": true, "message_claim": true, "message_ack": true, "message_nack": true,
}

// internalSettingOwner 是 9 个内部设置键的唯一归属。
var internalSettingOwner = map[string]string{
	"context.history_limit":  "session",
	"context.core_limit":     "experience",
	"context.core_max_chars": "experience",
	"context.rel_limit":      "experience",
	"context.rel_max_chars":  "experience",
	"context.embedding":      "experience",
	"share.sessions_limit":   "experience",
	"share.digest_chars":     "experience",
	"db":                     "session",
	"shared_db":              "experience",
}

// settingOrder 是设置面板的稳定顺序（与拆分前一致）。
var settingOrder = []string{
	"context.history_limit",
	"context.core_limit",
	"context.core_max_chars",
	"context.rel_limit",
	"context.rel_max_chars",
	"context.embedding",
	"share.sessions_limit",
	"share.digest_chars",
	"db",
	"shared_db",
	"name",
	"addr",
	"center",
}

// provides 是 memory 对外能力名册；顺序与拆分前一致。
func provides() []protocol.Provide {
	return []protocol.Provide{
		{Action: "context", Input: "{conversation, messages, boss?, task_name?}（spec §7.2 同步上下文）", Output: "{messages, work, task}（组装后的上下文与 work 行）"},
		{Action: "store", Input: "{conversation_id, role, content}", Output: "落库回执"},
		{Action: "history", Input: "{conversation_id, limit}", Output: "{conversation_id, messages:[{role, content, at}]}（按时间正序；供界面回填，不做记忆组装）"},
		{Action: "conversation_list", Input: "{limit?, include_archived?}", Output: "{conversations:[{id, title, entry, status, created_at, updated_at}]}（按最近活跃排序；不暴露给模型 — 给界面列会话，不是给模型的工具）"},
		{Action: "conversation_rename", Input: "{id, title}（空标题 = 清掉名字）", Output: "{conversation}（不暴露给模型）"},
		{Action: "note", Input: "经验 {content, conversation_id?, task_id?, durability?}（core | durable | transient）", Output: "{id, node_ids, duplicate, skipped}（不暴露给模型：整理由复盘流程调；duplicate = 同一件事又说了一遍）"},
		{Action: "dig", Tool: true, Input: "{entry_id?, node_id?, limit?}（entry_id = 翻同簇兄弟，node_id = 展开整个大类）", Output: "{notes[], total}（顺着线头往下扒）"},
		{Action: "lineheads", Input: "{query, limit?}", Output: "{heads:[{node_id, text, count, score, task_id, members}]}（一行一条；recall 里自动带）"},
		{Action: "prune", Input: "{}", Output: "{entries, links}（软剪枝：只标状态、只断干枯的线，不删行）"},
		{Action: "remember", Tool: true, Input: "长期记忆条目 {kind, content, priority?, salience?}（kind 如 偏好/身份/知识/事件；priority>0 = 核心记忆，每次对话都会带上；salience 0..1 = 重要性，越大遗忘越慢）", Output: "{id, created}（同 kind+content 已有 active 条目时幂等跳过，salience/priority 只升不降）"},
		{Action: "consolidate", Input: "{}（整理池里有待处理候选时调用）", Output: "{processed, archived, skipped, failed, memnet_failed, conflicts, superseded, unresolved}（不暴露给模型：分类与冲突判断由 reasoner 完成）"},
		{Action: "publish_session", Input: "任务摘要 {conversation_id, task_id, title?, goal?, outcome?, capabilities?, private?}（复盘调用；已脱敏，不暴露给模型）", Output: "{published, private}"},
		{Action: "shared_sessions", Tool: true, Input: "{limit?, exclude?, query?}（别的会话在做什么的摘要；看不到它们的对话）", Output: "{sessions:[{entry, title, tasks:[{title, goal, outcome, capabilities, created_at}]}]}"},
		{Action: "shared_forget", Input: "{session_id?, task_id?}（删共享物；不暴露给模型）", Output: "{sessions, tasks}"},
		{Action: "conversation_open", Input: "{id, title?, entry?}（幂等建会话；不暴露给模型）", Output: "{conversation}"},
		{Action: "conversation_archive", Input: "{id, archived}（归档 = 不进默认列表，不拦写入；不暴露给模型）", Output: "{conversation}"},
		{Action: "task_current", Input: "{conversation_id}", Output: "{task?}（最近活跃的 active 任务；没有就是空）"},
		{Action: "task_open", Input: "{conversation_id, name}", Output: "{task}"},
		{Action: "task_finish", Input: "{task_id, name?}", Output: "{task}（status=done，之后只读）"},
		{Action: "task_rename", Input: "{task_id, name}", Output: "{task}"},
		{Action: "task_resume", Input: "{task_id}", Output: "{task, messages[]}（把未完任务顶成当前，记录一并带走）"},
		{Action: "task_list", Input: "{conversation_id, limit?}", Output: "{tasks[]}（最近活跃在前）"},
		{Action: "forget", Input: "{id}（长期记忆条目 id）", Output: "{id}（删掉一条不再对的记忆）"},
	}
}

func contextMethod() spv2.Method {
	return spv2.Method{
		Name:        "context",
		Description: "同步返回组装后的上下文与 work 行（spec §7.2；替代 recall+resume 两跳）",
		Input: objectSchema(map[string]any{
			"conversation": stringSchema(),
			"messages":     map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"boss":         stringSchema(),
			"task_name":    stringSchema(),
		}, "messages"),
		Output: objectSchema(map[string]any{
			"messages": map[string]any{"type": "array"},
			"work":     map[string]any{"type": "object"},
			"task":     map[string]any{"type": "object"},
		}),
	}
}

func objectSchema(props map[string]any, required ...any) map[string]any {
	out := map[string]any{"type": "object"}
	if len(props) > 0 {
		out["properties"] = props
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func stringSchema() map[string]any { return map[string]any{"type": "string"} }
