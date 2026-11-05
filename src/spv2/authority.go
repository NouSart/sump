package spv2

// AuthorizeDeliver 校验 deliver：只有 owner 或 work.agent(runtime) 可以。
func AuthorizeDeliver(w Work, from Ref) error {
	if from.Kind == "human" && from.ID == w.Owner.ID && from.Authority == AuthorityOwner {
		return nil
	}
	if from.ID == w.Agent.ID && from.Authority == AuthorityRuntime {
		return nil
	}
	return ErrCode(CodePermissionDenied, "只有 work owner 或 runtime 可以 deliver", false, map[string]any{
		"work": w.ID, "actor": from.ID, "authority": from.Authority,
	})
}

// AuthorizeControl 校验 cancel/renew：owner 或 runtime。
func AuthorizeControl(w Work, from Ref) error {
	if from.Kind == "human" && from.ID == w.Owner.ID && from.Authority == AuthorityOwner {
		return nil
	}
	if from.ID == w.Agent.ID && from.Authority == AuthorityRuntime {
		return nil
	}
	return ErrCode(CodePermissionDenied, "只有 work owner 或 runtime 可以控制 work", false, nil)
}

// AuthorizeMessage 校验 message：owner 或 participant；agent 不得带 owner 权限。
func AuthorizeMessage(w Work, from Ref) error {
	if from.Kind == "human" {
		if from.ID == w.Owner.ID && from.Authority == AuthorityOwner {
			return nil
		}
		return ErrCode(CodePermissionDenied, "human 不是 work owner", false, nil)
	}
	if from.Kind != "agent" {
		return ErrCode(CodePermissionDenied, "只有 human/agent 可以 message", false, nil)
	}
	if from.Authority != AuthorityNone {
		return ErrCode(CodePermissionDenied, "agent message 必须 authority=none", false, nil)
	}
	if from.ID == w.Agent.ID || IsParticipant(w, from) {
		return nil
	}
	return ErrCode(CodePermissionDenied, "agent 不是 work participant", false, nil)
}

// IsParticipant 判断 actor 是否在 participants 中。
func IsParticipant(w Work, actor Ref) bool {
	for _, p := range w.Participants {
		if p.Actor.Kind == actor.Kind && p.Actor.ID == actor.ID && p.Status != "left" {
			return true
		}
	}
	return false
}
