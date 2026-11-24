package session

// 会话私有数据的读写动作：记录 / 历史 / 会话 / 任务。
// 这一层只做"账本"的事——不碰长期记忆，也不组装上下文。

import (
	"errors"
	"fmt"
	"strings"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// callMeta 是动作需要的调用元信息：SPv2 从帧取。
type callMeta struct {
	from  string // 调用方（work 的 agent 记它）
	trace string // 链路 id（= frame.work，work 账本的主键）
}

// store 记一条会话记录（对话消息幂等：与上一条完全相同时跳过）。
// Kind 为空按 Role 推断；新类型（reasoning/tool_call/tool_result）不去重也不进 LLM 上下文。
// 记录必须归属某个任务：调用方没给就归到当前进行中的任务（没有就现建一个）。
//
// 分派只看载荷里的 client_item_id：为空 = 旧调用方，走下面逐行不变的旧路径；
// 非空 = v2 契约，交给 storeV2 的幂等单事务（工作归属与补报回执都在那里）。
func (s *Server) store(body []byte) (any, error) {
	var p protocol.StorePayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	if p.ClientItemID != "" {
		return s.storeV2(&p)
	}
	if strings.TrimSpace(p.Content) == "" {
		return map[string]any{}, nil
	}
	cid := p.ConversationID
	if cid == "" {
		cid = "default"
	}
	taskID := p.TaskID
	if taskID == "" {
		if p.Kind == "" || p.Kind == "user" {
			// 用户消息可以开新任务（"没有任务必须创建"）。
			// 入口服务给了任务名就用它：正文可能带来源标注前缀（QQ 的群聊/私聊标签），
			// 从第一行推会推出一串标签，而任务名要显示在界面上。
			name := protocol.ClampTaskName(p.TaskName)
			if name == "" {
				name = taskNameFrom(p.Content)
			}
			task, err := s.cfg.Store.EnsureTask(cid, name)
			if err != nil {
				return nil, fmt.Errorf("任务解析失败: %w", err)
			}
			taskID = task.ID
		} else if t, ok, err := s.cfg.Store.LatestTask(cid); err == nil && ok {
			// 迟到的事件（答复 / 轨迹）没有任务归属：挂到最近那个任务上，绝不拿它当任务名新建。
			taskID = t.ID
		}
	}
	if err := s.cfg.Store.Record(cid, p.Role, p.Content, p.Kind, p.Meta, p.Seq, taskID); err != nil {
		return nil, fmt.Errorf("落库失败: %w", err)
	}
	// 用户说的话是长期记忆的候选；先在私有整理池留一份原文，等 experience 分类。
	// 助手答复 / 工具轨迹不进来：它们可重算，且容易把模型幻觉记成事实。
	if p.Role == "user" && (p.Kind == "" || p.Kind == "user") {
		if _, err := s.cfg.Store.Stage(cid, p.Role, p.Content); err != nil {
			s.logger.Printf("记忆候选入池失败（不影响本轮）: %v", err)
		}
	}
	// 会话的入口只在第一次写时填上（Record 已保证会话行存在，这里只补入口，幂等）。
	// 用 boss 而不是 from：记录是 reasoner 代写的，真正"从哪扇门进来"的是它替谁办事。
	if err := s.cfg.Store.EnsureConversation(cid, "", p.Boss); err != nil {
		s.logger.Printf("记录会话入口失败: %v", err)
	}
	if err := s.cfg.Store.TouchTask(taskID); err != nil {
		s.logger.Printf("刷新任务活跃时间失败: %v", err)
	}
	return map[string]any{}, nil
}

// taskNameFrom 从一条用户消息里取任务名：第一行、压掉空白、截到 24 个字。
// 模型之后可以用 task_rename 把它改准。
func taskNameFrom(text string) string {
	line := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	name := protocol.ClampTaskName(line)
	if name == "" {
		return "未命名任务"
	}
	return name
}

// history 把某会话最近的记录原样交还（界面回填用：不做记忆组装、不碰长期记忆、不改上下文）。
// TaskID 为空时给"当前任务"的记录；连进行中的任务都没有时退回最近一个任务。
func (s *Server) history(body []byte) (any, error) {
	var p protocol.HistoryPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	cid := p.ConversationID
	if cid == "" {
		cid = "default"
	}
	taskID := p.TaskID
	if p.AllTasks {
		// 整段会话（按任务分段的视图要的就是这个）：不做任务过滤。
		taskID = ""
	} else if taskID == "" {
		if t, ok, err := s.cfg.Store.CurrentTask(cid); err != nil {
			return nil, fmt.Errorf("查询任务失败: %w", err)
		} else if ok {
			taskID = t.ID
		} else if t, ok, err := s.cfg.Store.LatestTask(cid); err != nil {
			return nil, fmt.Errorf("查询任务失败: %w", err)
		} else if ok {
			taskID = t.ID
		}
	}
	msgs, err := s.cfg.Store.History(cid, p.Limit, p.AfterSeq, taskID)
	if err != nil {
		return nil, fmt.Errorf("查询历史失败: %w", err)
	}
	return protocol.HistoryResult{ConversationID: cid, Messages: msgs}, nil
}

// conversationList 列会话（给门户的历史记录页分行）。
// 不暴露给模型：这是界面导航，不是模型该自己调的决策。
func (s *Server) conversationList(body []byte) (any, error) {
	var p protocol.ConversationListPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	list, err := s.cfg.Store.ListConversations(p.Limit, p.IncludeArchived)
	if err != nil {
		return nil, fmt.Errorf("查询会话列表失败: %w", err)
	}
	return protocol.ConversationListResult{Conversations: list}, nil
}

// conversationRename 改会话名（空标题 = 清掉名字）。不暴露给模型：起名是人的事。
func (s *Server) conversationRename(body []byte) (any, error) {
	var p protocol.ConversationRenamePayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	c, err := s.cfg.Store.RenameConversation(p.ID, strings.TrimSpace(p.Title))
	if err != nil {
		return nil, err
	}
	return protocol.ConversationResult{Conversation: c}, nil
}

// conversationOpen 幂等建会话（门户按会话号打开旧对话时用；不暴露给模型）。
func (s *Server) conversationOpen(body []byte) (any, error) {
	var p protocol.ConversationOpenPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(p.ID)
	if id == "" {
		return nil, errors.New("conversation_open: id 必填")
	}
	entry := strings.TrimSpace(p.Entry)
	if entry == "" {
		entry = entryOf(id)
	}
	if err := s.cfg.Store.EnsureConversation(id, p.Title, entry); err != nil {
		return nil, fmt.Errorf("conversation_open: %w", err)
	}
	c, err := s.cfg.Store.ConversationByID(id)
	if err != nil {
		return nil, fmt.Errorf("conversation_open: %w", err)
	}
	return protocol.ConversationResult{Conversation: c}, nil
}

// conversationArchive 归档 / 取消归档（归档 = 移出默认列表，不拦写入）。
func (s *Server) conversationArchive(body []byte) (any, error) {
	var p protocol.ConversationArchivePayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	c, err := s.cfg.Store.SetConversationStatus(p.ID, p.Archived)
	if err != nil {
		return nil, fmt.Errorf("conversation_archive: %w", err)
	}
	return protocol.ConversationResult{Conversation: c}, nil
}

// entryOf 从会话号里取入口前缀（"portal:xxx" → "portal"）：会话号带门是入口约定。
func entryOf(id string) string {
	if i := strings.IndexByte(id, ':'); i > 0 {
		return id[:i]
	}
	return ""
}

// ---------- 任务 ----------

// taskCurrent 取当前任务（没有就返回 nil——调用方据此决定要不要建）。
func (s *Server) taskCurrent(body []byte) (any, error) {
	var p protocol.TaskCurrentPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	cid := p.ConversationID
	if cid == "" {
		cid = "default"
	}
	t, ok, err := s.cfg.Store.CurrentTask(cid)
	if err != nil {
		return nil, fmt.Errorf("查询当前任务失败: %w", err)
	}
	if !ok {
		return protocol.TaskResult{}, nil
	}
	return protocol.TaskResult{Task: &t}, nil
}

// taskOpen 新开一个进行中的任务。
func (s *Server) taskOpen(body []byte) (any, error) {
	var p protocol.TaskOpenPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	cid := p.ConversationID
	if cid == "" {
		cid = "default"
	}
	t, err := s.cfg.Store.OpenTask(cid, p.Name)
	if err != nil {
		return nil, fmt.Errorf("新建任务失败: %w", err)
	}
	s.logger.Printf("任务新建: %s「%s」", t.ID, t.Name)
	return protocol.TaskResult{Task: &t}, nil
}

// taskFinish 完结任务（判定权在 reasoner，这里只记账）。
func (s *Server) taskFinish(body []byte) (any, error) {
	var p protocol.TaskFinishPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	if p.TaskID == "" {
		return nil, errors.New("task_finish: task_id 必填")
	}
	t, err := s.cfg.Store.FinishTask(p.TaskID, p.Name)
	if err != nil {
		return nil, fmt.Errorf("完结任务失败: %w", err)
	}
	s.logger.Printf("任务完结: %s「%s」", t.ID, t.Name)
	return protocol.TaskResult{Task: &t}, nil
}

// taskResume 恢复一个未完结任务：顶成当前任务，并把它这一段的记录一并带走。
func (s *Server) taskResume(body []byte) (any, error) {
	var p protocol.TaskResumePayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	if p.TaskID == "" {
		return nil, errors.New("task_resume: task_id 必填")
	}
	t, err := s.cfg.Store.TaskByID(p.TaskID)
	if err != nil {
		return nil, fmt.Errorf("任务不存在: %w", err)
	}
	if t.Status != store.TaskActive {
		return nil, fmt.Errorf("任务「%s」已完结，只能看不能继续", t.Name)
	}
	t, err = s.cfg.Store.ResumeTask(t.ID)
	if err != nil {
		return nil, fmt.Errorf("恢复任务失败: %w", err)
	}
	msgs, err := s.cfg.Store.History(t.ConversationID, 200, 0, t.ID)
	if err != nil {
		return nil, fmt.Errorf("读取任务记录失败: %w", err)
	}
	s.logger.Printf("任务恢复: %s「%s」", t.ID, t.Name)
	return protocol.TaskResumeResult{Task: t, Messages: msgs}, nil
}

// taskRename 改任务名（判定时顺手把标题写准；不改状态）。
func (s *Server) taskRename(body []byte) (any, error) {
	var p protocol.TaskRenamePayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	if p.TaskID == "" {
		return nil, errors.New("task_rename: task_id 必填")
	}
	t, err := s.cfg.Store.RenameTask(p.TaskID, p.Name)
	if err != nil {
		return nil, fmt.Errorf("改任务名失败: %w", err)
	}
	s.logger.Printf("任务改名: %s「%s」", t.ID, t.Name)
	return protocol.TaskResult{Task: &t}, nil
}

// taskList 列出会话的任务（最近活跃在前）。
func (s *Server) taskList(body []byte) (any, error) {
	var p protocol.TaskListPayload
	if err := protocol.DecodeRaw(body, &p); err != nil {
		return nil, err
	}
	cid := p.ConversationID
	if cid == "" {
		cid = "default"
	}
	tasks, err := s.cfg.Store.ListTasks(cid, p.Limit)
	if err != nil {
		return nil, fmt.Errorf("查询任务列表失败: %w", err)
	}
	return protocol.TaskListResult{Tasks: tasks}, nil
}

// recallQuery 取任务上下文里最后一条用户消息文本——用来推任务名（不用于检索）。
func recallQuery(messages []protocol.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
			return messages[i].Content
		}
	}
	return ""
}

// metaOfCall 把 SPv2 帧翻成动作需要的调用元信息。
func metaOfCall(call *spv2.Call) callMeta {
	return callMeta{from: call.Frame.From.ID, trace: call.Frame.Work}
}
