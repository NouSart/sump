package fakeopenai

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// post 发一次 chat/completions 请求（模拟 reasoner 的调用），返回状态码与原始响应体。
func post(t *testing.T, s *Server, msgs []map[string]any, tools []string) (int, string) {
	t.Helper()
	req := map[string]any{"model": "frozen-model", "messages": msgs}
	if len(tools) > 0 {
		var ts []any
		for _, n := range tools {
			ts = append(ts, map[string]any{"type": "function", "function": map[string]any{"name": n}})
		}
		req["tools"] = ts
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(s.URL()+"/chat/completions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func user(text string) map[string]any { return map[string]any{"role": "user", "content": text} }
func assistant(text string) map[string]any {
	return map[string]any{"role": "assistant", "content": text}
}

func newServer(t *testing.T, sc Script) *Server {
	t.Helper()
	s, err := New(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestMatchesByCallNumber：按第几次调用命中，且默认只命中一次（耗尽后走 Default）。
func TestMatchesByCallNumber(t *testing.T) {
	s := newServer(t, Script{
		Steps: []Step{
			{Name: "first", When: When{CallAtMost: 1}, Reply: Reply{Content: "A"}},
		},
		Default: &Step{Name: "rest", Reply: Reply{Content: "B"}},
	})
	for i, want := range []string{"A", "B", "B"} {
		code, body := post(t, s, []map[string]any{user("hi")}, nil)
		if code != 200 {
			t.Fatalf("#%d 状态码 %d", i+1, code)
		}
		if !strings.Contains(body, want) {
			t.Fatalf("#%d 应含 %q，实际 %s", i+1, want, body)
		}
	}
	if got := s.Calls(); got != 3 {
		t.Fatalf("调用数 %d, want 3", got)
	}
}

// TestMatchesByToolResults：按"[工具结果]"条数命中——这是"看到结果再决定下一步"的关键能力，
// 按调用序号写剧本做不到（工具完成时机不确定）。
func TestMatchesByToolResults(t *testing.T) {
	s := newServer(t, Script{Steps: []Step{
		{Name: "after-result", When: When{ToolResultsAtLeast: 1}, Reply: Reply{Content: "看到结果了"}},
	}})
	_, body := post(t, s, []map[string]any{user("任务"), assistant("已受理"), user("[工具结果] x（job=c1）：\nok")}, nil)
	if !strings.Contains(body, "看到结果了") {
		t.Fatalf("应命中按结果数的步骤，实际 %s", body)
	}
	if got := s.Records()[0].StepName; got != "after-result" {
		t.Fatalf("StepName=%q", got)
	}
	if n := s.Records()[0].ToolResults; n != 1 {
		t.Fatalf("ToolResults=%d, want 1", n)
	}
}

// TestMatchesByContains：内容匹配（判定回执、错误原文都能当条件）。
func TestMatchesByContains(t *testing.T) {
	s := newServer(t, Script{Steps: []Step{
		{Name: "hit", When: When{Contains: "判定为"}, Reply: Reply{Content: "答"}},
	}})
	_, body := post(t, s, []map[string]any{user("已记录：任务《x》判定为 已完成。")}, nil)
	if !strings.Contains(body, `"答"`) {
		t.Fatalf("应命中 contains 条件，实际 %s", body)
	}
}

// TestFaults 逐个验证故障注入真的生效——注入不生效时"扛住了故障"就是假的。
//
// 注意：不能用"响应里含某段文字"当判据。曾经这么写过，结果「截断」异变存活：
// 正常响应里也含 `"role":"assist…`，断言恒真。所以坏 JSON 类的判据必须是
// **解析失败**，而不是文本包含。
func TestFaults(t *testing.T) {
	cases := []struct {
		fault        string
		wantCode     int
		wantInBody   string
		mustNotParse bool
	}{
		{FaultHTTP500, 500, "internal error", false},
		{FaultHTTP429, 429, "rate limited", false},
		{FaultBadJSON, 200, "<html>", true},
		{FaultTruncate, 200, "", true},
		{FaultEmpty, 200, `"choices":[]`, false},
		{FaultNoToolCall, 200, `"message":{"role":"assistant"}`, false},
	}
	for _, c := range cases {
		t.Run(c.fault, func(t *testing.T) {
			s := newServer(t, Script{Steps: []Step{{Name: "f", When: When{CallAtMost: 1}, Fault: c.fault}}})
			code, body := post(t, s, []map[string]any{user("hi")}, nil)
			if code != c.wantCode {
				t.Fatalf("状态码 %d, want %d（body=%s）", code, c.wantCode, body)
			}
			if c.wantInBody != "" && !strings.Contains(body, c.wantInBody) {
				t.Fatalf("响应应含 %q，实际 %s", c.wantInBody, body)
			}
			if c.mustNotParse {
				var v map[string]any
				if err := json.Unmarshal([]byte(body), &v); err == nil {
					t.Fatalf("响应不应是合法 JSON，实际 %s", body)
				}
			}
			recs := s.Records()
			if len(recs) != 1 || recs[0].Fault != c.fault {
				t.Fatalf("留痕应记下故障 %q: %+v", c.fault, recs)
			}
		})
	}
}

// TestUnmatchedFaults：声明了故障却没命中必须能被发现（否则"注入无效"会被当成通过）。
func TestUnmatchedFaults(t *testing.T) {
	s := newServer(t, Script{Steps: []Step{
		{Name: "never", When: When{Contains: "永远不会出现"}, Fault: FaultHTTP500},
	}})
	if got := s.UnmatchedFaults(); len(got) != 1 || got[0] != "never" {
		t.Fatalf("未命中故障应报出来: %v", got)
	}
	post(t, s, []map[string]any{user("[工具结果] 永远不会出现")}, nil)
	if got := s.UnmatchedFaults(); len(got) != 0 {
		t.Fatalf("命中后不应再报: %v", got)
	}
}

// TestReminderAndJudgeDetection：harness 的两类信号要能被认出来
// （收尾门禁的催办、以及模型确实调过 task_status 的回执）。
func TestReminderAndJudgeDetection(t *testing.T) {
	s := newServer(t, Script{Steps: []Step{{Name: "x", Reply: Reply{Content: "ok"}}}})
	post(t, s, []map[string]any{
		user("【系统】你还没有调用 task_status 判定这个任务是否完成。"),
		user("已记录：任务《t》判定为 已完成。"),
	}, nil)
	r := s.Records()[0]
	if !r.HasReminder {
		t.Fatal("应识别出收尾门禁提醒")
	}
	if !r.JudgeSeen {
		t.Fatal("应识别出 task_status 判定回执")
	}
}

// TestResetClearsProgress：每次 trial 前必须清干净，否则第二次 trial 命中的是上一轮残留。
func TestResetClearsProgress(t *testing.T) {
	s := newServer(t, Script{
		Steps:   []Step{{Name: "once", When: When{CallAtMost: 1}, Reply: Reply{Content: "A"}}},
		Default: &Step{Name: "rest", Reply: Reply{Content: "B"}},
	})
	post(t, s, []map[string]any{user("hi")}, nil)
	s.Reset()
	if s.Calls() != 0 || len(s.Records()) != 0 {
		t.Fatal("Reset 后调用数与留痕都该归零")
	}
	_, body := post(t, s, []map[string]any{user("hi")}, nil)
	if !strings.Contains(body, `"A"`) {
		t.Fatalf("Reset 后应重新从第一步开始，实际 %s", body)
	}
}

// TestSetScript：换剧本同时清进度（任务集里每条任务自带剧本）。
func TestSetScript(t *testing.T) {
	s := newServer(t, Script{Default: &Step{Name: "old", Reply: Reply{Content: "旧"}}})
	post(t, s, []map[string]any{user("hi")}, nil)
	s.SetScript(Script{Default: &Step{Name: "new", Reply: Reply{Content: "新"}}})
	_, body := post(t, s, []map[string]any{user("hi")}, nil)
	if !strings.Contains(body, "新") {
		t.Fatalf("应使用新剧本，实际 %s", body)
	}
	if got := s.Records()[0].StepName; got != "new" {
		t.Fatalf("StepName=%q", got)
	}
}

// TestUsageAlwaysPresent：成本指标依赖 usage；剧本没给时必须估算，不能是 0。
func TestUsageAlwaysPresent(t *testing.T) {
	s := newServer(t, Script{Default: &Step{Name: "d", Reply: Reply{Content: strings.Repeat("字", 20)}}})
	_, body := post(t, s, []map[string]any{user("提示")}, nil)
	if !strings.Contains(body, `"usage"`) {
		t.Fatalf("响应必须带 usage，实际 %s", body)
	}
	u := s.Records()[0].Usage
	if u.PromptTokens <= 0 || u.CompletionTokens <= 0 || u.TotalTokens != u.PromptTokens+u.CompletionTokens {
		t.Fatalf("usage 不合理: %+v", u)
	}
}

// TestUsageFromScript：剧本显式给了 usage 就用它（真模型回放时按真实数字记账）。
func TestUsageFromScript(t *testing.T) {
	s := newServer(t, Script{Default: &Step{Name: "d", Usage: &Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}}})
	post(t, s, []map[string]any{user("x")}, nil)
	if got := s.Records()[0].Usage; got.PromptTokens != 7 || got.CompletionTokens != 3 {
		t.Fatalf("应使用剧本给的 usage，实际 %+v", got)
	}
}

// TestTailRecordsWhatModelSaw：留痕要带上"模型当时看到了什么"（归因用）。
func TestTailRecordsWhatModelSaw(t *testing.T) {
	s := newServer(t, Script{Default: &Step{Name: "d", Reply: Reply{Content: "ok"}}})
	long := strings.Repeat("很长的内容", 100)
	post(t, s, []map[string]any{user("第一句"), user(long)}, nil)
	tail := s.Records()[0].Tail
	if len(tail) != 2 {
		t.Fatalf("tail 应有 2 条，实际 %d", len(tail))
	}
	if len([]rune(tail[1])) > 130 {
		t.Fatalf("tail 应截断，实际长度 %d", len([]rune(tail[1])))
	}
}

// TestToolNamesRecorded：工具暴露面是 harness 的一等公民，必须留痕。
func TestToolNamesRecorded(t *testing.T) {
	s := newServer(t, Script{Default: &Step{Name: "d", Reply: Reply{Content: "ok"}}})
	post(t, s, []map[string]any{user("x")}, []string{"memory__remember", "wait"})
	got := s.Records()[0].ToolNames
	if len(got) != 2 || got[0] != "memory__remember" || got[1] != "wait" {
		t.Fatalf("工具名单不对: %v", got)
	}
}

// TestNonCompletionPath404：非 chat/completions 路径返回 404，别把误配当成功。
func TestNonCompletionPath404(t *testing.T) {
	s := newServer(t, Script{})
	resp, err := http.Post(s.URL()+"/v1/other", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("状态码 %d, want 404", resp.StatusCode)
	}
}
