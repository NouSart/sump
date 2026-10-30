package server

// 任务交付后的复盘：把这一段做过的事，变成以后能用的经验。
//
// 时机：交付 boss **之后**，在后台跑。老板已经拿到答复了，复盘成不成都不影响他——
// 这是"不能容忍一丁点阻塞"的具体落法。
//
// 谁写：llm 服务自己（reasoner 就是那个服务）。不带工具、不开任务、不写历史——
// 就是一次补全（与 `think` 同一条路，只是这里有具体的提示词）。
//
// 写几条：模型自己判断，允许 0 条。'我完成了任务' 这种没有信息量的话不允许写；
// 硬上限 3 条（一条经验一句话，两条压成一句会让向量被压扁）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
)

const (
	// reflectMaxNotes 是一次复盘最多产出几条经验。
	reflectMaxNotes = 3
	// reflectMaxTokens 是复盘答复的输出上限（一小段 JSON 足够）。
	reflectMaxTokens = 1024
	// reflectTimeout 是复盘这次补全的墙钟上限。
	reflectTimeout = 120 * time.Second
)

// reflectSystemPrompt 是复盘器的系统提示。
const reflectSystemPrompt = `你是 SUMP 的复盘器。你会看到一段刚结束的任务记录（用户要求、做过的工具调用、
它们的输出、最终答复）。你的工作是把这次**值得以后复用**的东西挑出来。

只写下面几类，别的都不要写：
- 做法：某个问题的正确解法（"并发写要带 seq，否则顺序会乱"）
- 坑：踩过并且会再踩的（"进程内计数器做 seq，重启之后序号会倒流"）
- 事实：这套环境/代码里确定成立的事（"SQLite 连接池只有 1，游标开着时另开查询会死锁"）
- 偏好：用户明确表达的喜好（"配色要跟随本地时间"）
- 取舍：两个方案之间为什么选了这个

硬性要求：
- 一条经验一句话，独立可读——不要写"上面那个方法"，读者看不到上下文。
- 不许写"我完成了任务""已成功交付"这类没有信息量的流水账。
- 不许把用户的原话抄一遍当经验；要提炼。
- 没有值得记的就返回空数组。宁可 0 条，也不要凑数。
- 最多 3 条。

只输出 JSON，不要解释、不要代码块围栏：
{"notes":[{"content":"一句话经验","kind":"做法|坑|事实|偏好|取舍"}]}`

// reflectReply 是复盘模型的输出。
type reflectReply struct {
	Notes []struct {
		Content string `json:"content"`
		Kind    string `json:"kind"`
	} `json:"notes"`
}

// reflectOnTask 在后台复盘一次任务：交付已完成，这里失败只记日志。
//
// 为什么**不设"太短就不复盘"的门槛**：最短的是最值钱的——"以后都用中文回复"只有十个字，
// 但它恰恰是必须记住的偏好。长度门槛会把这类经验精准地筛掉。所以宁可让模型看一眼
// 再自己说"没有值得记的"（提示词里明确允许返回空数组），也不按字数猜。
func (s *Server) reflectOnTask(st *taskState, finalAnswer string) {
	if !s.reflectOn() {
		return
	}
	client := s.llmClient()
	if client == nil {
		return
	}

	st.mu.Lock()
	msgs := make([]llm.Message, len(st.messages))
	copy(msgs, st.messages)
	cid, taskID, trace, judgedDone := st.cid, st.taskID, st.trace, st.judgedDone
	st.mu.Unlock()

	transcript, _ := buildTranscript(msgs, finalAnswer)
	if strings.TrimSpace(transcript) == "" {
		return
	}

	go func() {
		verdict := "任务已达成"
		if !judgedDone {
			verdict = "任务未达成（半途交付）"
		}
		prompt := fmt.Sprintf("这次任务的结局：%s\n\n%s", verdict, transcript)
		ctx, cancel := context.WithTimeout(context.Background(), reflectTimeout)
		defer cancel()
		resp, err := client.Chat(ctx, llm.ChatRequest{
			Messages: []llm.Message{
				{Role: "system", Content: reflectSystemPrompt},
				{Role: "user", Content: prompt},
			},
			MaxTokens: reflectMaxTokens,
		})
		if err != nil {
			s.logger.Printf("会话 %s：复盘失败（不影响交付）: %v", cid, err)
			return
		}
		notes := parseNotes(resp.Content)
		if len(notes) == 0 {
			s.logger.Printf("会话 %s：复盘完成，没有值得记的经验", cid)
			return
		}
		for _, n := range notes {
			s.Fire(s.memoryName(), "note", protocol.NotePayload{
				Content:        n.Content,
				ConversationID: cid,
				TaskID:         taskID,
				Durability:     n.Durability,
			}, trace, s.cfg.Name)
		}
		s.logger.Printf("会话 %s：复盘写回 %d 条经验", cid, len(notes))
	}()
}

// noteToWrite 是一条待写的经验（解析后的形状）。
type noteToWrite struct {
	Content    string
	Durability string
}

// parseNotes 解析复盘输出。模型偶尔会包一层 ```json 围栏或加几句解释，
// 所以这里取第一个 '{' 到最后一个 '}' 之间的内容——解析不了就当没写（宁缺勿滥）。
func parseNotes(raw string) []noteToWrite {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return nil
	}
	var reply reflectReply
	if err := json.Unmarshal([]byte(raw[start:end+1]), &reply); err != nil {
		return nil
	}
	out := make([]noteToWrite, 0, len(reply.Notes))
	for _, n := range reply.Notes {
		content := strings.TrimSpace(n.Content)
		if content == "" {
			continue
		}
		// 用户偏好是"不该忘"的那一类；其余走遗忘曲线。
		// 不让模型自己挑耐用度——它会把每一条都标成 core，网就再也不遗忘了。
		dur := "durable"
		if strings.Contains(n.Kind, "偏好") {
			dur = "core"
		}
		out = append(out, noteToWrite{Content: content, Durability: dur})
		if len(out) >= reflectMaxNotes {
			break
		}
	}
	return out
}
