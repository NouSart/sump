package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/NouSart/sump/protocol"
	"github.com/vmihailenco/msgpack/v5"
)

func TestStrictJSONRejectsAmbiguousPayloads(t *testing.T) {
	for _, raw := range []string{
		`[]`, `null`, `"text"`, `42`, `{}`, // {} is handled separately below.
		`{"id":"a","id":"b"}`,
		`{"id":"a","\u0069d":"b"}`,
		`{"value":{"id":1,"id":2}}`,
		`{"value":[{"id":1,"id":2}]}`,
		`{"value":1} {"value":2}`,
		`{"value":1} trailing`,
		`{"value":`,
		"{\"value\":\"\xff\"}",
	} {
		if raw == "{}" {
			continue
		}
		t.Run(raw, func(t *testing.T) {
			var dst map[string]any
			if err := StrictJSON([]byte(raw), &dst); err == nil {
				t.Fatal("ambiguous or invalid JSON was accepted")
			}
		})
	}
	var empty map[string]any
	if err := StrictJSON([]byte(`{}`), &empty); err != nil {
		t.Fatal(err)
	}
}

func TestStrictJSONDepthLimitAndExactInteger(t *testing.T) {
	for _, tc := range []struct {
		arrays int
		valid  bool
	}{{31, true}, {32, false}} {
		raw := `{"v":` + strings.Repeat("[", tc.arrays) + "1" + strings.Repeat("]", tc.arrays) + "}"
		var dst map[string]any
		if err := StrictJSON([]byte(raw), &dst); (err == nil) != tc.valid {
			t.Errorf("arrays=%d: %v", tc.arrays, err)
		}
	}
	var number struct {
		N int64 `json:"n"`
	}
	if err := StrictJSON([]byte(`{"n":9007199254740993}`), &number); err != nil {
		t.Fatal(err)
	}
	if number.N != 9007199254740993 {
		t.Fatalf("integer rounded: %d", number.N)
	}
	for _, raw := range []string{`{"n":9223372036854775808}`, `{"n":100.5}`, `{"n":1e3}`, `{"n":1,"other":2}`} {
		if err := StrictJSON([]byte(raw), &number); err == nil {
			t.Errorf("accepted invalid integer/field: %s", raw)
		}
	}
}

func TestBindingsDecodeInput(t *testing.T) {
	bindings := BuiltinBindings()
	file, err := bindings["files__read"].DecodeInput([]byte(`{"id":"file-中文"}`))
	if err != nil || file.(protocol.FileReadPayload).ID != "file-中文" {
		t.Fatalf("file: %#v, %v", file, err)
	}
	for _, tc := range []struct{ tool, raw string }{
		{"files__read", `{}`},
		{"files__read", `{"id":null}`},
		{"files__read", `{"id":" "}`},
		{"files__read", `{"ID":"x"}`},
		{"files__read", `{"id":"x","to":"settings-center"}`},
		{"files__read", `{"id":"x","boss":"reasoner"}`},
		{"files__read", `{"id":"x","from":"reasoner"}`},
	} {
		if _, err := bindings[tc.tool].DecodeInput([]byte(tc.raw)); err == nil {
			t.Errorf("%s accepted %s", tc.tool, tc.raw)
		}
	}
	for _, tc := range []struct {
		tool, key, value string
		other            map[string]any
	}{
		{"files__read", "id", strings.Repeat("中", 86), nil},
	} {
		fields := tc.other
		if fields == nil {
			fields = make(map[string]any)
		}
		fields[tc.key] = tc.value
		raw, _ := json.Marshal(fields)
		if _, err := bindings[tc.tool].DecodeInput(raw); err == nil {
			t.Errorf("%s accepted overlong %s", tc.tool, tc.key)
		}
	}
}

func packResult(t *testing.T, v any) msgpack.RawMessage {
	t.Helper()
	raw, err := msgpack.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBindingFileOutputTruncatesSerializedUTF8(t *testing.T) {
	binding := BuiltinBindings()["files__read"]
	source := protocol.FileReadResult{ID: "id", Filename: "资料.txt", Text: strings.Repeat("中🙂\n<>\"", 100)}
	raw := packResult(t, source)
	for _, budget := range []int{80, 100, 101, 512} {
		out, err := binding.DecodeOutput(raw, budget, protocol.FileReadPayload{ID: "id"})
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		encoded, err := json.Marshal(out)
		if err != nil || len(encoded) > budget {
			t.Fatalf("serialized bytes %d exceed %d: %v", len(encoded), budget, err)
		}
		text := out["text"].(string)
		if !utf8.ValidString(text) || !strings.HasPrefix(source.Text, text) || out["truncated"] != true {
			t.Fatalf("incorrect UTF-8 truncation: %#v", out)
		}
		// The next complete rune must not fit: a byte budget must not clip
		// unnecessarily or forget JSON's escaping/metadata overhead.
		_, width := utf8.DecodeRuneInString(source.Text[len(text):])
		out["text"] = source.Text[:len(text)+width]
		encoded, _ = json.Marshal(out)
		if len(encoded) <= budget {
			t.Fatalf("truncated more text than necessary at budget %d", budget)
		}
	}
	if _, err := binding.DecodeOutput(raw, 10, protocol.FileReadPayload{ID: "id"}); err == nil {
		t.Fatal("metadata over budget was accepted")
	}
	plain := protocol.FileReadResult{ID: "id", Text: "content", Truncated: true}
	out, err := binding.DecodeOutput(packResult(t, plain), 1024, protocol.FileReadPayload{ID: "id"})
	if err != nil || out["text"] != "content" || out["truncated"] != true {
		t.Fatalf("upstream truncation lost: %#v, %v", out, err)
	}
	plain.Text = "\xff"
	if _, err := binding.DecodeOutput(packResult(t, plain), 1024, protocol.FileReadPayload{ID: "id"}); err == nil {
		t.Fatal("invalid UTF-8 output accepted")
	}
}

func TestBindingSchemasAreIndependentAndExplicit(t *testing.T) {
	one, two := BuiltinBindings(), BuiltinBindings()
	for name, binding := range one {
		schema := binding.Tool.InputSchema.(map[string]any)
		if schema["additionalProperties"] != false || schema["type"] != "object" {
			t.Fatalf("%s does not have a closed input contract", name)
		}
		binding.Tool.Description = "modified"
		if two[name].Tool.Description == "modified" {
			t.Fatalf("%s shares mutable tool metadata", name)
		}
	}
}

func TestFileReadBindingInputContinuation(t *testing.T) {
	binding := BuiltinBindings()["files__read"]
	got, err := binding.DecodeInput([]byte(`{"id":"file","offset":12,"limit":34}`))
	if err != nil {
		t.Fatal(err)
	}
	payload := got.(protocol.FileReadPayload)
	if payload.ID != "file" || payload.Offset != 12 || payload.Limit != 34 {
		t.Fatalf("续读参数未映射到协议 payload: %#v", payload)
	}
	zero, err := binding.DecodeInput([]byte(`{"id":"file","offset":0}`))
	if err != nil || zero.(protocol.FileReadPayload).Offset != 0 {
		t.Fatalf("offset=0 应等价于从头读: %#v, %v", zero, err)
	}
	for _, raw := range []string{
		`{"id":"file","offset":-1}`,
		`{"id":"file","offset":1.5}`,
		`{"id":"file","offset":"1"}`,
		`{"id":"file","offset":null}`,
		`{"id":"file","limit":0}`,
		`{"id":"file","limit":-1}`,
		`{"id":"file","limit":true}`,
		`{"id":"file","limit":null}`,
	} {
		if _, err := binding.DecodeInput([]byte(raw)); err == nil {
			t.Errorf("accepted invalid continuation input %s", raw)
		}
	}
}

func TestFileReadBindingExposesContinuationAndCompleteness(t *testing.T) {
	binding := BuiltinBindings()["files__read"]
	source := protocol.FileReadResult{
		ID: "id", Filename: "f.txt", Text: "text", Truncated: true,
		Total: 4, Next: 2, Warnings: []string{"sheet 2 failed"},
	}
	out, err := binding.DecodeOutput(packResult(t, source), 1<<20, protocol.FileReadPayload{ID: "id"})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"id": "id", "filename": "f.txt", "text": "text", "truncated": true,
		"total": 4, "next": 2,
	} {
		if out[key] != want {
			t.Errorf("%s = %#v, 期望 %#v（完整结果 %#v）", key, out[key], want, out)
		}
	}
	warnings, ok := out["warnings"].([]string)
	if !ok || len(warnings) != 1 || warnings[0] != "sheet 2 failed" {
		t.Fatalf("warnings 未透出: %#v", out["warnings"])
	}
}

func TestFileReadBindingLocalCutMovesContinuation(t *testing.T) {
	binding := BuiltinBindings()["files__read"]
	source := protocol.FileReadResult{
		ID: "id", Filename: "资料.txt", Text: strings.Repeat("中", 200),
		Truncated: true, Total: 500, Next: 300,
	}
	request := protocol.FileReadPayload{ID: "id", Offset: 100, Limit: 200}
	raw := packResult(t, source)

	full, err := binding.DecodeOutput(raw, 1<<20, request)
	if err != nil {
		t.Fatal(err)
	}
	if full["next"] != 300 || full["total"] != 500 {
		t.Fatalf("未裁剪的块应保留上游游标: %#v", full)
	}

	cut, err := binding.DecodeOutput(raw, 240, request)
	if err != nil {
		t.Fatal(err)
	}
	text := cut["text"].(string)
	if text == "" || !strings.HasPrefix(source.Text, text) || cut["truncated"] != true {
		t.Fatalf("本地裁剪结果不符: %#v", cut)
	}
	next, ok := cut["next"].(int)
	if !ok || next != request.Offset+utf8.RuneCountInString(text) {
		t.Fatalf("本地裁剪后的 next 应指向被裁掉的位置: %#v", cut)
	}
	if next == source.Next {
		t.Fatal("本地裁剪后沿用上游 next 会跳过被裁掉的正文")
	}
	encoded, err := json.Marshal(cut)
	if err != nil || len(encoded) > 240 {
		t.Fatalf("裁剪结果超出字节预算: %d 字节, %v", len(encoded), err)
	}
}

// 协议是唯一事实源，这份绑定是手写的显式投影。协议新增字段而不更新绑定
// 不会破坏编译，却会让 MCP 客户端悄悄看不到新能力；这里把“漏掉”变成红灯。
// 某个字段若确实不该透出，也必须在这里显式登记并写明原因，不能默认忽略。
func TestFileReadBindingTracksProtocolFields(t *testing.T) {
	binding := BuiltinBindings()["files__read"]
	inputFields := msgpackFieldNames(reflect.TypeOf(protocol.FileReadPayload{}))
	knownInput := make(map[string]bool, len(inputFields))
	for _, name := range inputFields {
		knownInput[name] = true
	}
	properties := binding.Tool.InputSchema.(map[string]any)["properties"].(map[string]any)
	notExposedInput := map[string]string{} // 形如 "字段": "为什么不透出"；当前无
	for _, name := range inputFields {
		if _, skip := notExposedInput[name]; skip {
			continue
		}
		if _, ok := properties[name]; !ok {
			t.Errorf("协议输入字段 %q 未在 files__read 的输入契约中登记", name)
		}
	}
	for name := range properties {
		if !knownInput[name] {
			t.Errorf("files__read 输入契约暴露了协议里不存在的字段 %q", name)
		}
	}

	notExposed := map[string]string{} // 形如 "字段": "为什么不透出"；当前无
	source := protocol.FileReadResult{
		ID: "id", Filename: "f.txt", Text: "text", Truncated: true,
		Total: 4, Next: 2, Warnings: []string{"warning"},
	}
	out, err := binding.DecodeOutput(packResult(t, source), 1<<20, protocol.FileReadPayload{ID: "id"})
	if err != nil {
		t.Fatal(err)
	}
	resultFields := msgpackFieldNames(reflect.TypeOf(protocol.FileReadResult{}))
	knownOutput := make(map[string]bool, len(resultFields))
	for _, name := range resultFields {
		knownOutput[name] = true
		if _, skip := notExposed[name]; skip {
			continue
		}
		if _, ok := out[name]; !ok {
			t.Errorf("协议结果字段 %q 未在 files__read 的结果中透出", name)
		}
	}
	for name := range out {
		if !knownOutput[name] {
			t.Errorf("files__read 结果含协议里不存在的字段 %q", name)
		}
	}
}

func msgpackFieldNames(t reflect.Type) []string {
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("msgpack"), ",")[0]
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}
