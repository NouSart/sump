package server

// message（spec §7.1）：统一的人类/agent 消息入口。
// 权限矩阵（§6.5）：human 必须 authority=owner；agent 必须 authority=none，且只能回复
// 已有进行中的任务（同 work/participant）；runtime/owner 不能冒充 agent 发消息。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

func (s *Server) message(ctx context.Context, from spv2.Ref, trace string, body []byte) (any, error) {
	var p protocol.MessagePayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Text) == "" {
		return nil, errors.New("message: text 必填")
	}
	cid := p.Conversation
	if cid == "" {
		cid = "default"
	}
	switch from.Kind {
	case "human":
		if from.Authority != spv2.AuthorityOwner {
			return nil, &permissionError{msg: fmtMessageDenied(from, "human 必须带 authority=owner")}
		}
		// 人类消息走唯一主链（没有第二套入口）。
		return s.humanMessage(ctx, from.ID, trace, p)
	case "service":
		// 入口服务是人类的代理：service + authority=none + sender 必填，才能进人类主链；
		// 结果归属方 = 入口服务（from），deliver 沿 from 回去。
		if from.Authority != spv2.AuthorityNone {
			return nil, &permissionError{msg: fmtMessageDenied(from, "service authority 必须是 none")}
		}
		if p.Sender == nil {
			return nil, &permissionError{msg: fmtMessageDenied(from, "service 必须带 sender（入口不能冒充人类）")}
		}
		return s.humanMessage(ctx, from.ID, trace, p)
	case "agent":
		if from.Authority != spv2.AuthorityNone {
			return nil, &permissionError{msg: fmtMessageDenied(from, "agent 的 message 永远 authority=none")}
		}
		// A2A：显式带目标 work 的消息经 memory 队列入队，等该 work 下次开任务时注入。
		if p.Work != "" && p.Work != trace {
			if err := s.postA2AMessage(ctx, p.Work, p.Text, trace); err != nil {
				return nil, err
			}
			return map[string]any{"status": "accepted", "queued": true}, nil
		}
		return s.injectAgentMessage(from, p, cid)
	default:
		return nil, &permissionError{msg: fmtMessageDenied(from, "message 只接受 human/service/agent")}
	}
}

func fmtMessageDenied(from spv2.Ref, why string) string {
	return fmt.Sprintf("message: 拒绝来源 %s:%s（%s）", from.Kind, from.ID, why)
}

// injectAgentMessage 把 agent 消息注入同一会话进行中的任务，并唤醒一轮。
// 没有进行中的任务 = 没有可归属的 work：拒绝，而不是凭空开任务。
func (s *Server) injectAgentMessage(from spv2.Ref, p protocol.MessagePayload, cid string) (any, error) {
	st := s.boardGet(cid)
	if st == nil {
		return nil, &permissionError{msg: "message: 会话 " + cid + " 没有进行中的任务，agent 只能回复同 work"}
	}
	label := "agent:" + from.ID
	if p.Sender != nil {
		if l := p.Sender.Label(); l != "" {
			label = l
		}
	}
	text := "（来自 " + label + "）" + p.Text
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil, &permissionError{msg: "message: 会话 " + cid + " 的任务已收尾，拒绝注入"}
	}
	st.messages = append(st.messages, llm.Message{Role: "user", Content: text})
	st.gen++
	st.mu.Unlock()
	s.boardKick(st)
	return map[string]any{"status": "accepted"}, nil
}
