package llm

// token 级流式：SSE chunk 解析（content 增量、usage、tool_calls 拼接）与 Fake 分块。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeepSeekChatStreamParsesSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewDeepSeekClient(srv.URL, "", "m")
	var deltas []string
	res, err := c.ChatStream(context.Background(), ChatRequest{Model: "m"}, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "你好" || res.Message.Content != "你好" {
		t.Fatalf("流式内容不符: deltas=%v content=%q", deltas, res.Message.Content)
	}
	if res.Usage.TotalTokens != 5 {
		t.Fatalf("usage 丢失: %+v", res.Usage)
	}
}

func TestFakeChatStreamChunksContent(t *testing.T) {
	f := &Fake{Replies: []Message{{Role: "assistant", Content: "流式答复"}}}
	var got string
	res, err := f.ChatStream(context.Background(), ChatRequest{}, func(d string) { got += d })
	if err != nil {
		t.Fatal(err)
	}
	if got != "流式答复" || res.Message.Content != "流式答复" {
		t.Fatalf("Fake 分块不符: got=%q content=%q", got, res.Message.Content)
	}
}
