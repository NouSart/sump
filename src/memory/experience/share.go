package experience

// 共享经验库的接缝：发布会话摘要 / 读会话目录 / 删共享物。
//
// 边界（写在代码里，也写在文档里）：
//   - 共享库只有提炼物（一句话在做什么、做到哪、用过的能力族），**没有原文**；
//   - 会话流水（messages / tasks）只在 session 服务，模型没有任何动作能读别的会话；
//   - 目录给模型看的是"入口 + 标题 + 摘要"，会话 id 只在归并时用；
//   - 本服务没有会话库：publish_session 的会话标题只认请求里给的内容，缺失就空。

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NouSart/sump/memory/experience/shared"
	"github.com/NouSart/sump/protocol"
)

// otherSessionLines 把"别的会话在干什么"渲染成几行（排除当前会话）。
// 读失败只省略本节：目录是背景信息，不能挡住这一轮推理。
func (s *Server) otherSessionLines(cid string) []string {
	if s.cfg.Shared == nil {
		return nil
	}
	limit, chars := s.shareLimits()
	if limit <= 0 {
		return nil
	}
	sessions, err := s.cfg.Shared.List(limit, cid, "")
	if err != nil {
		s.logger.Printf("会话目录读取失败（本节忽略）: %v", err)
		return nil
	}
	return sessionLines(sessions, chars)
}

// sessionLines 是纯渲染（测试直接调它，不必起服务）。
func sessionLines(sessions []shared.Session, digestChars int) []string {
	out := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		if len(sess.Tasks) == 0 {
			continue
		}
		latest := sess.Tasks[0]
		name := sess.Title
		if name == "" {
			name = latest.Title
		}
		if name == "" {
			name = "未命名会话"
		}
		body := joinDigest(latest.Goal, latest.Outcome)
		line := fmt.Sprintf("- %s「%s」：%s", entryLabel(sess.Entry), name, shared.Truncate(body, digestChars))

		var extras []string
		if len(sess.Tasks) > 1 {
			extras = append(extras, fmt.Sprintf("另有 %d 条任务摘要", len(sess.Tasks)-1))
		}
		if len(latest.Capabilities) > 0 {
			extras = append(extras, "用到 "+strings.Join(latest.Capabilities, "/"))
		}
		if ago := agoText(sess.UpdatedAt, time.Now()); ago != "" {
			extras = append(extras, ago)
		}
		if len(extras) > 0 {
			line += "（" + strings.Join(extras, "；") + "）"
		}
		out = append(out, line)
	}
	return out
}

// joinDigest 把"在做什么"与"做到哪了"接成一句（去重、去空）。
func joinDigest(goal, outcome string) string {
	g := strings.TrimSpace(goal)
	o := strings.TrimSpace(outcome)
	switch {
	case g == "":
		return o
	case o == "" || g == o:
		return g
	case strings.Contains(g, o):
		return g
	default:
		return g + "；" + o
	}
}

// entryLabel 把入口服务名翻成人话（未知入口原样显示）。
func entryLabel(entry string) string {
	switch strings.ToLower(strings.TrimSpace(entry)) {
	case "web", "frontend":
		return "门户"
	case "qq":
		return "QQ"
	case "shell":
		return "终端"
	case "":
		return "会话"
	default:
		return entry
	}
}

// agoText 是给人看的时间距离（一天内给相对时间，更早给日期）。
func agoText(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// toSharedViews 把共享库的行转成协议视图（工具输出 / 门户调试用）。
func toSharedViews(sessions []shared.Session, digestChars int) []protocol.SharedSessionView {
	out := make([]protocol.SharedSessionView, 0, len(sessions))
	for _, sess := range sessions {
		view := protocol.SharedSessionView{
			SessionID: sess.ID,
			Entry:     entryLabel(sess.Entry),
			Title:     sess.Title,
			UpdatedAt: sess.UpdatedAt.Format(time.RFC3339),
		}
		for _, t := range sess.Tasks {
			view.Tasks = append(view.Tasks, protocol.SharedTaskView{
				Title:        t.Title,
				Goal:         shared.Truncate(t.Goal, digestChars),
				Outcome:      shared.Truncate(t.Outcome, digestChars),
				Capabilities: t.Capabilities,
				CreatedAt:    t.CreatedAt.Format(time.RFC3339),
			})
		}
		out = append(out, view)
	}
	return out
}

// publishSession 写一条任务摘要（不暴露给模型）。
// 本服务没有会话库：会话标题只认请求里给的 title，缺失就空——不跨域读会话标题。
func (s *Server) publishSession(body []byte) (any, error) {
	var p protocol.PublishSessionPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	if s.cfg.Shared == nil {
		// 没有共享库不是错误：复盘是尽力而为的事，不该惊动调用方。
		return map[string]any{"published": false, "reason": "共享经验库未启用"}, nil
	}
	published, err := s.cfg.Shared.Publish(time.Now(), shared.PublishInput{
		SessionID:    p.ConversationID,
		SessionTitle: "",
		TaskID:       p.TaskID,
		TaskTitle:    p.Title,
		Goal:         p.Goal,
		Outcome:      p.Outcome,
		Capabilities: p.Capabilities,
		Private:      p.Private,
	})
	if err != nil {
		return nil, fmt.Errorf("publish_session: %w", err)
	}
	return map[string]any{"published": published, "private": p.Private}, nil
}

// sharedSessions 按需再取一次会话目录（自动注入只有几条，模型想多看时才用）。
func (s *Server) sharedSessions(body []byte) (any, error) {
	var p protocol.SharedSessionsPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	if s.cfg.Shared == nil {
		return nil, errors.New("共享经验库未启用")
	}
	limit := p.Limit
	if limit <= 0 {
		limit, _ = s.shareLimits()
	}
	_, chars := s.shareLimits()
	sessions, err := s.cfg.Shared.List(limit, p.Exclude, p.Query)
	if err != nil {
		return nil, fmt.Errorf("shared_sessions: %w", err)
	}
	return protocol.SharedSessionsResult{Sessions: toSharedViews(sessions, chars)}, nil
}

// sharedForget 删共享物（维护 / 门户删改用；不暴露给模型）。
func (s *Server) sharedForget(body []byte) (any, error) {
	var p protocol.SharedForgetPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	if s.cfg.Shared == nil {
		return nil, errors.New("共享经验库未启用")
	}
	res, err := s.cfg.Shared.Forget(p.SessionID, p.TaskID)
	if err != nil {
		return nil, fmt.Errorf("shared_forget: %w", err)
	}
	return protocol.SharedForgetResult{Sessions: res.Sessions, Tasks: res.Tasks}, nil
}
