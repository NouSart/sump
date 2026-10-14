package protocol

import (
	"reflect"
	"testing"
)

// TestTaskContextRoundTrip 验证记忆往返上下文（含工具调用链）的编解码保真。
func TestTaskContextRoundTrip(t *testing.T) {
	in := RecallPayload{Context: TaskContext{
		ConversationID: "conv-1",
		Boss:           "qq",
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "你好", ImageIDs: []string{"img-1", "img-2"}, FileIDs: []string{"file-1"}},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "c1", Type: "function",
				Function: FunctionCall{Name: "svc__act", Arguments: `{"a":1}`},
			}}},
			{Role: "tool", Content: `{"ok":true}`, ToolCallID: "c1"},
		},
	}}
	env, err := NewEnvelope(TypeJump, "reasoner", "memory", "trace-1", in)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var out RecallPayload
	if err := got.DecodePayload(&out); err != nil {
		t.Fatal(err)
	}
	if out.Context.ConversationID != in.Context.ConversationID || out.Context.Boss != in.Context.Boss {
		t.Fatalf("上下文头不符: %+v", out.Context)
	}
	if len(out.Context.Messages) != len(in.Context.Messages) {
		t.Fatalf("消息条数不符: %d != %d", len(out.Context.Messages), len(in.Context.Messages))
	}
	m := out.Context.Messages[2]
	if m.Role != "assistant" || len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "svc__act" || m.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("工具调用消息不符: %+v", m)
	}
	if out.Context.Messages[3].ToolCallID != "c1" {
		t.Fatalf("tool_call_id 丢失: %+v", out.Context.Messages[3])
	}
	if len(out.Context.Messages[1].ImageIDs) != 2 || out.Context.Messages[1].ImageIDs[0] != "img-1" {
		t.Fatalf("ImageIDs 丢失: %+v", out.Context.Messages[1])
	}
	if len(out.Context.Messages[1].FileIDs) != 1 || out.Context.Messages[1].FileIDs[0] != "file-1" {
		t.Fatalf("FileIDs 丢失: %+v", out.Context.Messages[1])
	}

	// Resume 与 Store 同构走一遍。
	rp := ResumePayload{Context: out.Context}
	if _, err := NewEnvelope(TypeJump, "memory", "reasoner", "trace-1", rp); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEnvelope(TypeJump, "reasoner", "memory", "trace-1", StorePayload{ConversationID: "conv-1", Role: "user", Content: "你好"}); err != nil {
		t.Fatal(err)
	}
}

// TestAuxPayloadsRoundTrip 验证交付与图片相关 payload 的编解码保真。
func TestAuxPayloadsRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   any
		out  any
	}{
		{"deliver", DeliverPayload{Text: "hi", ConversationID: "qq:private:10001"}, &DeliverPayload{}},
		{"userMessage", UserMessagePayload{Text: "hi", ConversationID: "qq:private:10001", Images: []string{"img-1"}}, &UserMessagePayload{}},
		{"userMessageFiles", UserMessagePayload{Text: "hi", Files: []string{"file-1", "file-2"}}, &UserMessagePayload{}},
		{"fileSave", FileSavePayload{URL: "http://x/a.pdf", Filename: "a.pdf", Mime: "application/pdf"}, &FileSavePayload{}},
		{"fileSaveResult", FileSaveResult{ID: "01J", Filename: "a.pdf", Mime: "application/pdf", Size: 123}, &FileSaveResult{}},
		{"fileFetch", FileFetchPayload{ID: "01J"}, &FileFetchPayload{}},
		{"fileFetchResult", FileFetchResult{ID: "01J", Filename: "a.pdf", Mime: "application/pdf", Data: "AA==", Size: 1}, &FileFetchResult{}},
		{"fileRead", FileReadPayload{ID: "01J"}, &FileReadPayload{}},
		{"fileReadResult", FileReadResult{ID: "01J", Filename: "a.md", Text: "内容", Truncated: true}, &FileReadResult{}},
		{"save", ImageSavePayload{URL: "http://x/i.png", Mime: "image/png"}, &ImageSavePayload{}},
		{"saveResult", ImageSaveResult{ID: "01J", Mime: "image/png", Size: 123}, &ImageSaveResult{}},
		{"fetch", ImageFetchPayload{ID: "01J"}, &ImageFetchPayload{}},
		{"fetchResult", ImageFetchResult{ID: "01J", Mime: "image/jpeg", Data: "AA==", Size: 1}, &ImageFetchResult{}},
		{"settingsList", SettingsListPayload{Service: "svc-a"}, &SettingsListPayload{}},
		{"setSetting", SetSettingPayload{Service: "svc-a", Key: "k", Value: "v"}, &SetSettingPayload{}},
		{"resetSetting", ResetSettingPayload{Service: "svc-a", Key: "k"}, &ResetSettingPayload{}},
		{"configure", ConfigurePayload{Key: "tool.notify_mode", Value: "batch", Default: "each", Overridden: true}, &ConfigurePayload{}},
		{"settingsResult", SettingsResult{Services: []ServiceSettings{{
			Service: "svc-a",
			Settings: []SettingView{{
				Key: "k", Value: "v", Default: "d", Overridden: true,
			}},
		}}}, &SettingsResult{}},
	}
	for _, tc := range cases {
		env, err := NewEnvelope(TypeJump, "a", "b", "trace-1", tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		data, err := Marshal(env)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got, err := Unmarshal(data)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := got.DecodePayload(tc.out); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !reflect.DeepEqual(tc.in, reflect.ValueOf(tc.out).Elem().Interface()) {
			t.Fatalf("%s 不符: %+v != %+v", tc.name, tc.in, tc.out)
		}
	}
}
