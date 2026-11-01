package protocol

import (
	"encoding/json"
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

// TestUserMessageSenderCompat 验证来源标注的线上兼容性：
//   - 不带 sender（老入口服务 / 门户）解码后 Sender 为 nil，行为与旧版完全一致；
//   - 带 sender 时字段逐个保真（含"取不到就不填"的空群名 / 零人数）。
func TestUserMessageSenderCompat(t *testing.T) {
	old, err := EncodePayload(UserMessagePayload{Text: "你好"})
	if err != nil {
		t.Fatal(err)
	}
	var back UserMessagePayload
	if err := DecodeRaw(old, &back); err != nil {
		t.Fatal(err)
	}
	if back.Text != "你好" || back.Sender != nil {
		t.Fatalf("不带 sender 的旧载荷应解出 nil Sender: %+v", back)
	}

	// nil Sender 不能出现在线上（omitempty）——否则老读者会看到一片零值字段。
	if _, has := decodeMap(t, old)["sender"]; has {
		t.Fatal("Sender 为 nil 时不应出现在载荷里")
	}

	partial := UserMessagePayload{Text: "在吗", ConversationID: "qq:group:1", Sender: &Sender{
		Channel: "group", ID: "2", GroupID: "1", Mentioned: true,
	}}
	raw, err := EncodePayload(partial)
	if err != nil {
		t.Fatal(err)
	}
	var got UserMessagePayload
	if err := DecodeRaw(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Sender == nil || got.Sender.Channel != "group" || got.Sender.ID != "2" || got.Sender.GroupID != "1" {
		t.Fatalf("sender 关键字段丢失: %+v", got.Sender)
	}
	if got.Sender.Name != "" || got.Sender.GroupName != "" || got.Sender.GroupSize != 0 || got.Sender.Owner {
		t.Fatalf("没给的字段应保持零值（不造假）: %+v", got.Sender)
	}
	if !got.Sender.Mentioned {
		t.Fatal("mentioned 丢失")
	}
}

// decodeMap 把已编码的 payload 解成 map，用于断言某个键是否出现在线路上。
func decodeMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := DecodeRaw(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestSenderLabel 钉住来源标注的渲染格式：它是**事实**的固定写法，
// 记忆里存的就是这段文本（入口服务与推理侧共用同一份实现），改了它等于改历史语义。
func TestSenderLabel(t *testing.T) {
	cases := []struct {
		name string
		in   *Sender
		want string
	}{
		{"nil", nil, ""},
		{"empty", &Sender{}, ""},
		{"private_owner", &Sender{Channel: "private", ID: "2271917353", Name: "主人", Owner: true, Mentioned: true},
			"[私聊 · 主人(2271917353) · 主人]"},
		{"group_full", &Sender{Channel: "group", ID: "12345", Name: "张三", GroupID: "67890", GroupName: "编程交流", GroupSize: 12},
			"[群聊 · 群「编程交流」(67890) · 12 人可见 · 张三(12345) · 非主人 · 未@我]"},
		{"group_mentioned", &Sender{Channel: "group", ID: "12345", GroupID: "67890", GroupSize: 3, Owner: true, Mentioned: true},
			"[群聊 · 群 67890 · 3 人可见 · 12345 · 主人 · 已@我]"},
		// 人数取不到时必须明说未知：猜成 1 会让模型以为这是悄悄话。
		{"group_size_unknown", &Sender{Channel: "group", GroupID: "1", GroupName: "群A"},
			"[群聊 · 群「群A」(1) · 人数未知 · 非主人 · 未@我]"},
		{"group_no_ids", &Sender{Channel: "group"},
			"[群聊 · 群（未知） · 人数未知 · 非主人 · 未@我]"},
		{"unknown_channel", &Sender{Channel: "sms", ID: "1"}, "[sms · 1 · 非主人]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Label(); got != tc.want {
				t.Fatalf("Label() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMessageMeta 钉住落库 meta 的形状：入口服务（qq 只记录路径）与推理侧（reasoner）
// 必须写出同一个形状——正文标签给人看，meta 给程序读，两条路径不一致就等于"谁说过话"
// 只有半份数据。空内容不写键：`images: []` 这种占位只会让读的人多判断一次。
func TestMessageMeta(t *testing.T) {
	sender := &Sender{Channel: "group", ID: "12345", GroupID: "67890", GroupSize: 3}

	if got := MessageMeta(nil, nil, nil); got != nil {
		t.Fatalf("什么都没有时应返回 nil（不写空 meta），实际 %+v", got)
	}
	// 只有发送者：不带空数组。
	only := MessageMeta(sender, nil, nil)
	if len(only) != 1 {
		t.Fatalf("只该有 sender 一个键: %+v", only)
	}
	if only["sender"] != sender {
		t.Fatalf("sender 应是原指针: %+v", only["sender"])
	}
	// 附件 id 有时才写。
	full := MessageMeta(sender, []string{"img-1"}, []string{"file-1"})
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["sender"]; !ok {
		t.Fatalf("meta 应带 sender: %s", raw)
	}
	if got := wire["images"].([]any); len(got) != 1 || got[0] != "img-1" {
		t.Fatalf("images 不符: %s", raw)
	}
	if got := wire["files"].([]any); len(got) != 1 || got[0] != "file-1" {
		t.Fatalf("files 不符: %s", raw)
	}
	// 只有附件没有发送者（门户这类单用户入口）：不写 sender 这个空键。
	attachmentsOnly := MessageMeta(nil, []string{"img-2"}, nil)
	if _, ok := attachmentsOnly["sender"]; ok {
		t.Fatalf("没有 sender 时不该写这个键: %+v", attachmentsOnly)
	}
	if _, ok := attachmentsOnly["files"]; ok {
		t.Fatalf("没有 files 时不该写这个键: %+v", attachmentsOnly)
	}
}

// TestClampTaskName 钉住任务名的截断规则——它只有这一处实现，qq 直接落库与 reasoner 代写
// 记忆都靠它；分叉成两种长度，同一个任务在两处就会有两个名字。
func TestClampTaskName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"spaces_only", "   \n\t ", ""},
		{"trims_and_joins", "  看看\n 现在几点  ", "看看 现在几点"},
		{"short", "你好", "你好"},
		{"exactly_24", "一二三四五六七八九十一二三四五六七八九十一二三四", "一二三四五六七八九十一二三四五六七八九十一二三四"},
		{"over_24", "一二三四五六七八九十一二三四五六七八九十一二三四五", "一二三四五六七八九十一二三四五六七八九十一二三四…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampTaskName(tc.in); got != tc.want {
				t.Fatalf("ClampTaskName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSenderWireKeys 钉住 sender 的**线路键名**（snake_case）：它们是跨语言契约，
// 改了就与既有读者/夹具静默漂移（结构体往返测试两种实现一起改，抓不到这种错误）。
func TestSenderWireKeys(t *testing.T) {
	in := UserMessagePayload{Text: "hi", Sender: &Sender{
		Channel: "group", ID: "1", Name: "张三", GroupID: "2", GroupName: "群", GroupSize: 3,
		Owner: true, Mentioned: true,
	}}
	raw, err := EncodePayload(in)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, raw)
	sender, ok := m["sender"].(map[string]any)
	if !ok {
		t.Fatalf("sender 不在线路里: %+v", m)
	}
	for _, key := range []string{"channel", "id", "name", "group_id", "group_name", "group_size", "owner", "mentioned"} {
		if _, has := sender[key]; !has {
			t.Fatalf("sender 缺少线路键 %q（当前键：%v）", key, keysOf(sender))
		}
	}
	if len(sender) != 8 {
		t.Fatalf("sender 的线路键数量应为 8，实际 %d：%v", len(sender), keysOf(sender))
	}

	// 落库 meta 走 JSON：键名必须与线路一致（记忆记录是给人/程序读的另一条出口，
	// 两套键名会让读的人猜"这是 Go 字段名还是线路名"）。
	metaRaw, err := json.Marshal(MessageMeta(in.Sender, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	var metaJSON map[string]map[string]any
	if err := json.Unmarshal(metaRaw, &metaJSON); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"channel", "id", "name", "group_id", "group_name", "group_size", "owner", "mentioned"} {
		if _, has := metaJSON["sender"][key]; !has {
			t.Fatalf("JSON 里 sender 缺少键 %q（当前键：%v）", key, keysOf(metaJSON["sender"]))
		}
	}

	// store 的任务名同样有线路键名（记忆服务是另一个进程，改名对两端都生效、往返测试抓不到）。
	storeRaw, err := EncodePayload(StorePayload{ConversationID: "c", Role: "user", Content: "x", TaskName: "名字"})
	if err != nil {
		t.Fatal(err)
	}
	store := decodeMap(t, storeRaw)
	if _, has := store["task_name"]; !has {
		t.Fatalf("store 载荷缺少线路键 task_name（当前键：%v）", keysOf(store))
	}
	recallRaw, err := EncodePayload(RecallPayload{Context: TaskContext{TaskName: "名字"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := decodeMap(t, recallRaw)["context"].(map[string]any)
	if _, has := ctx["task_name"]; !has {
		t.Fatalf("recall 上下文缺少线路键 task_name（当前键：%v）", keysOf(ctx))
	}

	// 反方向：别的实现按文档写的键名，我们必须能解出来。
	foreign := map[string]any{"text": "hi", "sender": map[string]any{
		"channel": "group", "id": "1", "group_id": "2", "group_size": 3,
	}}
	rawForeign, err := EncodePayload(foreign)
	if err != nil {
		t.Fatal(err)
	}
	var got UserMessagePayload
	if err := DecodeRaw(rawForeign, &got); err != nil {
		t.Fatal(err)
	}
	if got.Sender == nil || got.Sender.Channel != "group" || got.Sender.GroupID != "2" || got.Sender.GroupSize != 3 {
		t.Fatalf("按文档键名写出的载荷解不出来: %+v", got.Sender)
	}
}

// keysOf 取 map 的键（失败信息里要看得见到底出现了哪些键）。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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
		{"userMessagePrivateSender", UserMessagePayload{Text: "hi", ConversationID: "qq:private:10001", Sender: &Sender{
			Channel: "private", ID: "10001", Name: "主人", Owner: true, Mentioned: true,
		}}, &UserMessagePayload{}},
		{"userMessageGroupSender", UserMessagePayload{Text: "hi", ConversationID: "qq:group:67890", Sender: &Sender{
			Channel: "group", ID: "12345", Name: "张三", GroupID: "67890", GroupName: "编程交流", GroupSize: 12,
		}}, &UserMessagePayload{}},
		{"fileSave", FileSavePayload{URL: "http://x/a.pdf", Filename: "a.pdf", Mime: "application/pdf"}, &FileSavePayload{}},
		{"fileSaveResult", FileSaveResult{ID: "01J", Filename: "a.pdf", Mime: "application/pdf", Size: 123}, &FileSaveResult{}},
		{"fileFetch", FileFetchPayload{ID: "01J"}, &FileFetchPayload{}},
		{"fileFetchResult", FileFetchResult{ID: "01J", Filename: "a.pdf", Mime: "application/pdf", Data: "AA==", Size: 1}, &FileFetchResult{}},
		{"fileRead", FileReadPayload{ID: "01J"}, &FileReadPayload{}},
		{"fileReadResult", FileReadResult{ID: "01J", Filename: "a.md", Text: "内容", Truncated: true}, &FileReadResult{}},
		{"fileWrite", FileWritePayload{ID: "01J", Sheet: "Sheet1", Edits: []FileWriteEdit{{Cell: "A1", Value: "姓名"}, {Cell: "B2", Value: "30"}}}, &FileWritePayload{}},
		{"fileWriteResult", FileWriteResult{ID: "01J", Sheet: "Sheet1", Updated: 2}, &FileWriteResult{}},
		{"fileWordEdit", FileWordEditPayload{ID: "01J", Operations: []WordEditOperation{{Op: "replace", Old: "旧", New: "新"}}}, &FileWordEditPayload{}},
		{"fileWordEditResult", FileWordEditResult{ID: "01J", Replaced: 1}, &FileWordEditResult{}},
		{"filePptEdit", FilePptEditPayload{ID: "01J", Operations: []PptEditOperation{{Op: "replace", Old: "旧标题", New: "新标题"}}}, &FilePptEditPayload{}},
		{"filePptEditResult", FilePptEditResult{ID: "01J", Replaced: 1}, &FilePptEditResult{}},
		{"filePdfEdit", FilePdfEditPayload{ID: "01J", Operations: []PdfEditOperation{{Op: "rotate", Pages: "1-3", Degrees: 90}, {Op: "set_info", Properties: map[string]string{"title": "标题"}}}}, &FilePdfEditPayload{}},
		{"filePdfEditResult", FilePdfEditResult{ID: "01J", Applied: 2}, &FilePdfEditResult{}},
		{"save", ImageSavePayload{URL: "http://x/i.png", Mime: "image/png"}, &ImageSavePayload{}},
		{"saveResult", ImageSaveResult{ID: "01J", Mime: "image/png", Size: 123}, &ImageSaveResult{}},
		{"fetch", ImageFetchPayload{ID: "01J"}, &ImageFetchPayload{}},
		{"fetchResult", ImageFetchResult{ID: "01J", Mime: "image/jpeg", Data: "AA==", Size: 1}, &ImageFetchResult{}},
		{"settingsList", SettingsListPayload{Service: "svc-a"}, &SettingsListPayload{}},
		{"setSetting", SetSettingPayload{Service: "svc-a", Key: "k", Value: "v"}, &SetSettingPayload{}},
		{"resetSetting", ResetSettingPayload{Service: "svc-a", Key: "k"}, &ResetSettingPayload{}},
		{"configure", ConfigurePayload{Key: "tool.notify_mode", Value: "batch", Default: "each", Overridden: true}, &ConfigurePayload{}},
		{"settingsEvent", SettingsEventPayload{Cause: "set_setting", Service: "svc-a", Results: []SettingResult{
			{Key: "k", Value: "v", OK: true},
			{Key: "j", OK: false, Error: "未送达: 连接失败"},
		}}, &SettingsEventPayload{}},
		{"settingsEventNoResults", SettingsEventPayload{Cause: "register", Service: "svc-a"}, &SettingsEventPayload{}},
		{"report", ReportPayload{Kind: "fire", Message: "跳转失败"}, &ReportPayload{}},
		{"reportNoKind", ReportPayload{Message: "boom"}, &ReportPayload{}},
		{"remember", RememberPayload{Kind: "偏好", Content: "爱吃苹果", Priority: 1, ConversationID: "c1"}, &RememberPayload{}},
		{"rememberResult", RememberResult{ID: 7, Created: true}, &RememberResult{}},
		{"embed", EmbedPayload{Texts: []string{"苹果", "爱吃苹果"}, Query: true}, &EmbedPayload{}},
		{"embedResult", EmbedResult{Vectors: [][]float64{{0.5, 0.5}, {1, 0}}, Dim: 2, Model: "bge-small-zh-v1.5"}, &EmbedResult{}},
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
