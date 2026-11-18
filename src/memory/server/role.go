package server

import "github.com/NouSart/sump/protocol"

// 角色化部署：同一实现按域拆成三个进程。
// memory = 门面（唯一注册到设置中心），session = 会话私有数据，experience = 共享经验。
// 角色只决定“本进程暴露哪些 method / 设置项”，不改变动作语义。

// Role 是进程角色。
type Role string

const (
	RoleAll        Role = ""           // 单进程全量（测试与旧部署）
	RoleFacade     Role = "facade"     // 门面：context + 透传
	RoleSession    Role = "session"    // 会话库权威
	RoleExperience Role = "experience" // 共享经验库权威
)

func normalizeRole(r Role) Role {
	switch r {
	case RoleFacade, RoleSession, RoleExperience:
		return r
	default:
		return RoleAll
	}
}

func (r Role) isFacade() bool     { return r == RoleFacade }
func (r Role) isSession() bool    { return r == RoleSession }
func (r Role) isExperience() bool { return r == RoleExperience }
func (r Role) isAll() bool        { return r == RoleAll }

// sessionMethods 是会话私有数据的权威 method（含内部 context_base）。
var sessionMethods = map[string]bool{
	"store": true, "history": true,
	"conversation_list": true, "conversation_rename": true,
	"conversation_open": true, "conversation_archive": true,
	"task_current": true, "task_open": true, "task_finish": true,
	"task_resume": true, "task_rename": true, "task_list": true,
	"work_get": true, "lease_charge": true, "lease_release": true,
	"work_update": true, "work_sweep": true, "work_cancel": true, "work_renew": true,
	"message_post": true, "message_claim": true, "message_ack": true, "message_nack": true,
	"context_base": true, "staged_pending": true, "staged_mark": true, "staged_retry": true,
}

// internalMethods 只在域内可见，不进门面名片、也不给外部调用方。
var internalMethods = map[string]bool{
	"context_base": true, "inject": true,
	"staged_pending": true, "staged_mark": true, "staged_retry": true,
}

// experienceMethods 是共享经验数据的权威 method（含内部 inject）。
var experienceMethods = map[string]bool{
	"remember": true, "forget": true, "consolidate": true,
	"note": true, "dig": true, "lineheads": true, "prune": true,
	"publish_session": true, "shared_sessions": true, "shared_forget": true,
	"inject": true,
}

// methodAllowed 判断角色是否暴露某 method。门面暴露全部对外 method，
// 但内部 method（context_base / inject）只在各自服务里可见。
func (r Role) methodAllowed(name string) bool {
	if name == "configure" {
		return true // 设置下发由骨架注册，三个角色都要能收
	}
	switch r {
	case RoleSession:
		return sessionMethods[name]
	case RoleExperience:
		return experienceMethods[name]
	case RoleFacade:
		return !internalMethods[name]
	default:
		return true
	}
}

// filterProvides 按角色裁剪名片：门面保留全部对外动作，内部服务只留本域。
func filterProvides(r Role, in []protocol.Provide) []protocol.Provide {
	if r == RoleAll || r == RoleFacade {
		return in
	}
	out := make([]protocol.Provide, 0, len(in))
	for _, p := range in {
		if r.methodAllowed(p.Action) {
			out = append(out, p)
		}
	}
	return out
}

// peerOf 返回对外 method 的数据归属；门面据此透传。
func peerOf(name string) string {
	if sessionMethods[name] {
		return "session"
	}
	if experienceMethods[name] {
		return "experience"
	}
	return ""
}

// settingAllowed 判断角色是否声明某设置项。
func (r Role) settingAllowed(key string) bool {
	switch r {
	case RoleSession:
		return key == "context.history_limit" || key == "db" || key == "name" || key == "addr"
	case RoleExperience:
		return key == "context.core_limit" || key == "context.core_max_chars" ||
			key == "context.rel_limit" || key == "context.rel_max_chars" ||
			key == "context.embedding" || key == "share.sessions_limit" ||
			key == "share.digest_chars" || key == "shared_db" || key == "name" || key == "addr"
	default:
		return true
	}
}
