package llm

// llm 包的单个文件测试：HTTP 客户端（httptest 冒充 DeepSeek）与 Fake 的行为。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatParsesToolCalls(t *testing.T) {
	var gotAuth, gotPath string
	var gotReq ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "echo__echo",
							"arguments": `{"input":{"msg":"hi"}}`,
						},
					}},
				},
			}},
		})
	}))
	defer srv.Close()

	c := NewDeepSeekClient(srv.URL, "test-key", "deepseek-chat")
	msg, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Tools: []Tool{{
			Type:     "function",
			Function: ToolFunction{Name: "echo__echo", Description: "echo 测试动作", Parameters: map[string]any{"type": "object"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("Authorization 头不符: %q", gotAuth)
	}
	if !strings.HasSuffix(gotPath, "/chat/completions") {
		t.Fatalf("请求路径不符: %q", gotPath)
	}
	if gotReq.Model != "deepseek-chat" {
		t.Fatalf("model 不符: %q", gotReq.Model)
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Function.Name != "echo__echo" {
		t.Fatalf("tools 不符: %+v", gotReq.Tools)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "echo__echo" {
		t.Fatalf("解析 tool_calls 失败: %+v", msg)
	}
	if msg.ToolCalls[0].Function.Arguments != `{"input":{"msg":"hi"}}` {
		t.Fatalf("arguments 不符: %q", msg.ToolCalls[0].Function.Arguments)
	}
}

func TestChatErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "invalid api key"}})
	}))
	defer srv.Close()

	c := NewDeepSeekClient(srv.URL, "bad", "deepseek-chat")
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("期望错误包含 invalid api key，实际: %v", err)
	}
}

func TestFakeSequence(t *testing.T) {
	f := &Fake{Replies: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Type: "function", Function: FunctionCall{Name: "a", Arguments: "{}"}}}},
		{Role: "assistant", Content: "done"},
	}}
	ctx := context.Background()

	m1, _ := f.Chat(ctx, ChatRequest{})
	if len(m1.ToolCalls) != 1 || m1.ToolCalls[0].Function.Name != "a" {
		t.Fatalf("第 1 次应返回 tool_call: %+v", m1)
	}
	m2, _ := f.Chat(ctx, ChatRequest{})
	if m2.Content != "done" {
		t.Fatalf("第 2 次应返回文本: %+v", m2)
	}
	m3, _ := f.Chat(ctx, ChatRequest{})
	if m3.Content != "done" {
		t.Fatalf("第 3 次应重复最后一条: %+v", m3)
	}
	if f.CallCount() != 3 {
		t.Fatalf("CallCount = %d", f.CallCount())
	}
}

// TestRequestBodyMultimodal 验证图片引用 → 多模态内容的序列化与占位降级。
func TestRequestBodyMultimodal(t *testing.T) {
	req := ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "看看这个", ImageIDs: []string{"img-1", "img-old"}},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: "x__y", Arguments: "{}"}}}},
			{Role: "tool", Content: "结果", ToolCallID: "c1"},
			{Role: "user", Content: "以前的图", ImageIDs: []string{"img-gone"}},
		},
		Images: map[string]string{"img-1": "data:image/png;base64,AAAA"},
	}
	body, err := buildRequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 5 {
		t.Fatalf("消息数不符: %d", len(got.Messages))
	}

	// 有数据的引用 → content 数组 = [text, image_url]（无数据的 img-old 被忽略）。
	parts, ok := got.Messages[1]["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("多模态 content 不符: %#v", got.Messages[1]["content"])
	}
	if p0 := parts[0].(map[string]any); p0["type"] != "text" || p0["text"] != "看看这个" {
		t.Fatalf("text 块不符: %#v", parts[0])
	}
	p1 := parts[1].(map[string]any)
	if p1["type"] != "image_url" || p1["image_url"].(map[string]any)["url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("image_url 块不符: %#v", parts[1])
	}

	// tool_calls / tool_call_id 保留。
	if _, ok := got.Messages[2]["tool_calls"]; !ok {
		t.Fatalf("tool_calls 丢失: %#v", got.Messages[2])
	}
	if tm := got.Messages[3]; tm["tool_call_id"] != "c1" || tm["content"] != "结果" {
		t.Fatalf("tool 消息不符: %#v", tm)
	}

	// 无内联数据的引用 → 文本占位。
	if c, ok := got.Messages[4]["content"].(string); !ok || !strings.Contains(c, "[图片]") {
		t.Fatalf("更早的图片应转文本占位: %#v", got.Messages[4]["content"])
	}
	// 纯图无文本：占位兜底。
	pure := marshalMessage(Message{Role: "user", ImageIDs: []string{"x"}}, nil)
	if pure["content"] != "[图片]" {
		t.Fatalf("纯图占位不符: %#v", pure["content"])
	}
}
