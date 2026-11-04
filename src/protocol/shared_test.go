package protocol

// 共享经验库 / 多会话的载荷形状：MessagePack 往返之后字段不丢，
// 且**发布载荷里没有任何承载原文的字段**（这条边界靠结构保证，不靠调用方自觉）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishSessionPayloadRoundTrip(t *testing.T) {
	in := PublishSessionPayload{
		ConversationID: "web:private:owner",
		TaskID:         "task-1",
		Title:          "修序号",
		Goal:           "把序号倒流修掉",
		Outcome:        "已修好",
		Capabilities:   []string{"shell", "files"},
		Private:        true,
	}
	raw, err := EncodePayload(in)
	if err != nil {
		t.Fatal(err)
	}
	var out PublishSessionPayload
	if err := DecodeRaw(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ConversationID != in.ConversationID || out.TaskID != in.TaskID || out.Title != in.Title ||
		out.Goal != in.Goal || out.Outcome != in.Outcome || !out.Private {
		t.Fatalf("往返后字段不符: %+v", out)
	}
	if len(out.Capabilities) != 2 || out.Capabilities[0] != "shell" {
		t.Fatalf("能力族丢了: %+v", out.Capabilities)
	}
}

// TestPublishSessionHasNoRawContent 是本文件的重点：
// 发布会话摘要的载荷里不能出现 messages / content 这类原文通道。
// 名字写死在这里——将来有人想"顺手加个原文"时，这条会先红。
func TestPublishSessionHasNoRawContent(t *testing.T) {
	raw, err := EncodePayload(PublishSessionPayload{ConversationID: "c", TaskID: "t", Goal: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := DecodeRaw(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"messages", "content", "stdout", "stderr", "command", "history"} {
		if _, ok := m[banned]; ok {
			t.Fatalf("发布载荷里出现了原文通道 %q: %v", banned, m)
		}
	}
}

func TestSharedSessionsPayloadDefaults(t *testing.T) {
	// 全部字段可选：模型经常只给一个 query。
	raw, err := EncodePayload(SharedSessionsPayload{Query: "embedding"})
	if err != nil {
		t.Fatal(err)
	}
	var out SharedSessionsPayload
	if err := DecodeRaw(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Query != "embedding" || out.Limit != 0 || out.Exclude != "" {
		t.Fatalf("可选字段往返不符: %+v", out)
	}
}

func TestSharedSessionsResultRoundTrip(t *testing.T) {
	in := SharedSessionsResult{Sessions: []SharedSessionView{{
		SessionID: "web:x", Entry: "门户", Title: "配色调整",
		UpdatedAt: "2026-11-01T10:00:00Z",
		Tasks: []SharedTaskView{{
			Title: "改主题", Goal: "跟随本地时间", Outcome: "没定稿",
			Capabilities: []string{"shell"}, CreatedAt: "2026-11-01T09:00:00Z",
		}},
	}}}
	raw, err := EncodePayload(in)
	if err != nil {
		t.Fatal(err)
	}
	var out SharedSessionsResult
	if err := DecodeRaw(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].Title != "配色调整" {
		t.Fatalf("会话往返不符: %+v", out)
	}
	if len(out.Sessions[0].Tasks) != 1 || out.Sessions[0].Tasks[0].Goal != "跟随本地时间" {
		t.Fatalf("任务往返不符: %+v", out.Sessions[0].Tasks)
	}
}

func TestConversationOpenAndArchivePayload(t *testing.T) {
	raw, err := EncodePayload(ConversationOpenPayload{ID: "web:private:owner-x", Title: "新会话", Entry: "web"})
	if err != nil {
		t.Fatal(err)
	}
	var opened ConversationOpenPayload
	if err := DecodeRaw(raw, &opened); err != nil {
		t.Fatal(err)
	}
	if opened.ID != "web:private:owner-x" || opened.Title != "新会话" || opened.Entry != "web" {
		t.Fatalf("conversation_open 往返不符: %+v", opened)
	}

	raw, err = EncodePayload(ConversationArchivePayload{ID: "web:private:owner-x", Archived: true})
	if err != nil {
		t.Fatal(err)
	}
	var archived ConversationArchivePayload
	if err := DecodeRaw(raw, &archived); err != nil {
		t.Fatal(err)
	}
	if !archived.Archived {
		t.Fatalf("archived 丢了: %+v", archived)
	}
}

func TestSharedForgetPayloadRoundTrip(t *testing.T) {
	raw, err := EncodePayload(SharedForgetPayload{TaskID: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	var out SharedForgetPayload
	if err := DecodeRaw(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.TaskID != "task-1" || out.SessionID != "" {
		t.Fatalf("往返不符: %+v", out)
	}
	// 空载荷必须能被安全解出来（删除动作的两个 id 都是可选的）。
	if err := DecodeRaw(nil, &out); err != nil {
		t.Fatalf("空载荷不该报错: %v", err)
	}
}

// TestSharedPayloadNamesRegistered 用命名基线表守着"名字只增不改"。
func TestSharedPayloadNamesRegistered(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "docs", "concepts", "protocol.md"))
	if err != nil {
		t.Skipf("拿不到协议文档（不在仓库里跑）：%v", err)
	}
	text := string(doc)
	for _, name := range []string{"publish_session", "shared_sessions", "shared_forget", "conversation_open", "conversation_archive"} {
		if !strings.Contains(text, name) {
			t.Fatalf("动作 %s 没登记进 protocol.md 的命名基线表", name)
		}
	}
	for _, field := range []string{"goal", "outcome", "capabilities", "private", "session_id"} {
		if !strings.Contains(text, field) {
			t.Fatalf("载荷字段 %s 没登记进 protocol.md 的命名基线表", field)
		}
	}
}
