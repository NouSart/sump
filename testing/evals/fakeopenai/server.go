// Package fakeopenai 是评测用的 OpenAI 兼容替身。
//
// 与 reasoner/llm.Fake 的分工：那个只会"按调用序号依次吐剧本"，表达不了
// "看到工具结果再决定下一步"；评测 harness 恰恰需要这种条件响应，
// 否则多跳链只能用调用序号硬编码，改一次 harness 剧本就得重写。
//
// 这里的剧本按**请求内容**匹配（工具结果条数 / 内容包含 / 第几次调用），
// 并支持注入故障（500 / 空 choices / 截断 / 非法 JSON / 延迟），
// 顺带回报 usage——这一条是成本指标唯一的数据来源。
package fakeopenai

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Usage 是 OpenAI 兼容的用量回报。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ToolCall 是剧本里的一次工具调用。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Reply 是剧本给出的 assistant 消息。
type Reply struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// When 是匹配条件；留空的字段不参与匹配，全部满足才算命中。
type When struct {
	// 工具结果（含异步补报）至少有几条。
	ToolResultsAtLeast int `json:"tool_results_at_least,omitempty"`
	// Contains：任意一条消息内容包含该子串。
	Contains string `json:"contains,omitempty"`
	// CallAtLeast：这是第几次及以后的调用（1 起算）。
	CallAtLeast int `json:"call_at_least,omitempty"`
	// CallAtMost：只在前几次调用生效。
	CallAtMost int `json:"call_at_most,omitempty"`
}

// Step 是一条剧本：命中 When 就回 Reply（或按 Fault 制造故障）。
type Step struct {
	// Name 用于报告里指认"哪一步被命中"，方便归因。
	Name  string `json:"name,omitempty"`
	When  When   `json:"when"`
	Reply Reply  `json:"reply"`
	// Fault 见 Fault* 常量；空 = 正常返回。
	Fault string `json:"fault,omitempty"`
	// DelayMS 是故意拖慢的毫秒数（测超时/慢响应）。
	DelayMS int `json:"delay_ms,omitempty"`
	// Usage 缺省时按内容长度估算，保证成本指标永远有值。
	Usage *Usage `json:"usage,omitempty"`
	// Repeat 为真时这一步可反复命中（默认只命中一次）。
	Repeat bool `json:"repeat,omitempty"`
}

// 故障类型。
const (
	FaultNone       = ""
	FaultHTTP500    = "http_500"
	FaultHTTP429    = "http_429"
	FaultEmpty      = "empty"        // 200 但 choices 为空
	FaultTruncate   = "truncate"     // 200 但 JSON 被截断
	FaultBadJSON    = "bad_json"     // 200 但返回非 JSON
	FaultNoToolCall = "no_tool_call" // 200 但 assistant 既无内容也无工具调用
)

// Script 是一整套剧本。
type Script struct {
	Steps []Step `json:"steps"`
	// Default 是没有任何步骤命中时的回复；为空则回一句占位文本。
	Default *Step `json:"default,omitempty"`
}

// Record 是一次请求的留痕（评测的基本事实来源）。
type Record struct {
	Index        int      `json:"index"`
	At           string   `json:"at"`
	Model        string   `json:"model"`
	ToolResults  int      `json:"tool_results"`
	ToolNames    []string `json:"tool_names"` // 本次请求暴露给模型的工具名
	MessageCount int      `json:"message_count"`
	StepName     string   `json:"step_name"`
	Fault        string   `json:"fault,omitempty"`
	LatencyMS    int64    `json:"latency_ms"`
	Usage        Usage    `json:"usage"`
	PromptChars  int      `json:"prompt_chars"`
	// HasReminder 表示本轮请求里带上了收尾门禁的提醒（harness 催过一次判定）。
	HasReminder bool `json:"has_reminder,omitempty"`
	// JudgeSeen 表示请求里已经出现"判定为…"的回执——即模型确实调过 task_status。
	// 这是 harness 自己产出的证据，比翻记忆记录可靠（内置工具不进轨迹）。
	JudgeSeen bool `json:"judge_seen,omitempty"`
	// Tail 是最后两条消息的开头（排查剧本为什么没命中用；不是判定依据）。
	Tail []string `json:"tail,omitempty"`
}

// Server 是假 LLM 的 HTTP 服务。
type Server struct {
	mu     sync.Mutex
	script Script
	used   []bool
	recs   []Record
	calls  int
	srv    *http.Server
	ln     net.Listener
	logf   func(string, ...any)
}

// New 启动假 LLM；logf 可为 nil。
func New(script Script, logf func(string, ...any)) (*Server, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{script: script, used: make([]bool, len(script.Steps)), ln: ln, logf: logf}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.srv = &http.Server{Handler: mux}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// URL 是 base URL（填给 reasoner 的 -llm-base）。
func (s *Server) URL() string { return "http://" + s.ln.Addr().String() }

// Close 停服务。
func (s *Server) Close() error { return s.srv.Close() }

// UnmatchedFaults 返回声明了故障但**一次都没命中**的步骤名。
// 这是"狐假虎威"的防线：故障没真打进去，就不能拿"扛住了故障"当结论。
func (s *Server) UnmatchedFaults() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for i, st := range s.script.Steps {
		if st.Fault == "" || st.Fault == FaultNone {
			continue
		}
		if !s.used[i] {
			out = append(out, st.Name)
		}
	}
	return out
}

// Reset 清空剧本进度与留痕——每次 trial 必须调用，否则第二次 trial 命中的是上一轮的残留。
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used = make([]bool, len(s.script.Steps))
	s.recs = nil
	s.calls = 0
}

// SetScript 换一套剧本（每条任务自带"被冻结的模型行为"），并清空进度。
func (s *Server) SetScript(script Script) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = script
	s.used = make([]bool, len(script.Steps))
	s.recs = nil
	s.calls = 0
}

// Records 返回请求留痕的副本。
func (s *Server) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.recs))
	copy(out, s.recs)
	return out
}

// Calls 返回已处理的请求数。
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type openAIRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	started := time.Now()
	body, _ := io.ReadAll(r.Body)

	var req openAIRequest
	_ = json.Unmarshal(body, &req)

	contents := make([]string, 0, len(req.Messages))
	toolResults := 0
	promptChars := 0
	hasReminder := false
	judgeSeen := false
	for _, m := range req.Messages {
		text := rawText(m.Content)
		contents = append(contents, text)
		promptChars += len([]rune(text))
		if m.Role == "user" && (strings.Contains(text, "[工具结果]") || strings.Contains(text, "[后续补报]")) {
			toolResults++
		}
		if strings.Contains(text, "你还没有调用 task_status") {
			hasReminder = true
		}
		if strings.Contains(text, "判定为") {
			judgeSeen = true
		}
	}
	toolNames := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		toolNames = append(toolNames, t.Function.Name)
	}

	s.mu.Lock()
	s.calls++
	idx := s.calls
	step, stepName := s.pick(idx, toolResults, contents)
	s.mu.Unlock()

	if step != nil && step.DelayMS > 0 {
		time.Sleep(time.Duration(step.DelayMS) * time.Millisecond)
	}

	usage := estimateUsage(promptChars, step)
	rec := Record{
		Index: idx, At: started.UTC().Format(time.RFC3339Nano),
		Model: req.Model, ToolResults: toolResults, ToolNames: toolNames,
		MessageCount: len(req.Messages), StepName: stepName,
		Usage: usage, PromptChars: promptChars, HasReminder: hasReminder,
		JudgeSeen: judgeSeen,
		Tail:      tail(contents, 2),
	}
	if step != nil {
		rec.Fault = step.Fault
	}

	switch faultOf(step) {
	case FaultHTTP500:
		s.finish(rec, started)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"fake: internal error"}}`))
		return
	case FaultHTTP429:
		s.finish(rec, started)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"fake: rate limited"}}`))
		return
	case FaultBadJSON:
		s.finish(rec, started)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("<html>not json</html>"))
		return
	case FaultTruncate:
		s.finish(rec, started)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assist`))
		return
	}

	resp := map[string]any{
		"id":      fmt.Sprintf("fake-%d", idx),
		"object":  "chat.completion",
		"created": started.Unix(),
		"model":   req.Model,
		"usage":   usage,
	}
	switch faultOf(step) {
	case FaultEmpty:
		resp["choices"] = []any{}
	case FaultNoToolCall:
		resp["choices"] = []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant"},
		}}
	default:
		reply := Reply{Content: "(fake: 无匹配剧本)"}
		if step != nil {
			reply = step.Reply
		}
		msg := map[string]any{"role": "assistant"}
		if reply.Content != "" {
			msg["content"] = reply.Content
		}
		if len(reply.ToolCalls) > 0 {
			calls := make([]any, 0, len(reply.ToolCalls))
			for i, tc := range reply.ToolCalls {
				id := tc.ID
				if id == "" {
					id = fmt.Sprintf("call_%d_%d", idx, i+1)
				}
				calls = append(calls, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
				})
			}
			msg["tool_calls"] = calls
		}
		resp["choices"] = []any{map[string]any{
			"index": 0, "finish_reason": "stop", "message": msg,
		}}
	}

	s.finish(rec, started)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) finish(rec Record, started time.Time) {
	rec.LatencyMS = time.Since(started).Milliseconds()
	s.mu.Lock()
	s.recs = append(s.recs, rec)
	s.mu.Unlock()
	s.logf("fake-llm #%d step=%q fault=%q tool_results=%d", rec.Index, rec.StepName, rec.Fault, rec.ToolResults)
}

// pick 选出命中步骤；调用方持有锁。
func (s *Server) pick(call, toolResults int, contents []string) (*Step, string) {
	for i := range s.script.Steps {
		st := &s.script.Steps[i]
		if s.used[i] && !st.Repeat {
			continue
		}
		if !st.When.match(call, toolResults, contents) {
			continue
		}
		s.used[i] = true
		return st, st.Name
	}
	if s.script.Default != nil {
		return s.script.Default, s.script.Default.Name
	}
	return nil, ""
}

func (w When) match(call, toolResults int, contents []string) bool {
	if w.CallAtLeast > 0 && call < w.CallAtLeast {
		return false
	}
	if w.CallAtMost > 0 && call > w.CallAtMost {
		return false
	}
	if w.ToolResultsAtLeast > 0 && toolResults < w.ToolResultsAtLeast {
		return false
	}
	if w.Contains != "" {
		found := false
		for _, c := range contents {
			if strings.Contains(c, w.Contains) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func faultOf(st *Step) string {
	if st == nil {
		return FaultNone
	}
	return st.Fault
}

// tail 取最后 n 条消息的开头，便于在轨迹里看清"模型当时看到了什么"。
func tail(contents []string, n int) []string {
	if len(contents) == 0 {
		return nil
	}
	start := len(contents) - n
	if start < 0 {
		start = 0
	}
	out := make([]string, 0, n)
	for _, c := range contents[start:] {
		r := []rune(c)
		if len(r) > 120 {
			c = string(r[:120]) + "…"
		}
		out = append(out, c)
	}
	return out
}

func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	// 多模态内容（数组）折算成文本片段，够匹配用。
	return string(raw)
}

// estimateUsage 在剧本没给 usage 时按字符数估：CJK 1 字 1 token，其余 4 字符 1 token
// （与 reasoner 的估算口径一致，便于横向比对）。
func estimateUsage(promptChars int, st *Step) Usage {
	if st != nil && st.Usage != nil {
		return *st.Usage
	}
	in := promptChars / 2
	if in < 1 {
		in = 1
	}
	out := 1
	if st != nil {
		out = len([]rune(st.Reply.Content))/2 + len(st.Reply.ToolCalls)*8
		if out < 1 {
			out = 1
		}
	}
	return Usage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out}
}
