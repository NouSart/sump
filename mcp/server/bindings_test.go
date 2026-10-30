package server

import (
	"encoding/json"
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
		out, err := binding.DecodeOutput(raw, budget)
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
	if _, err := binding.DecodeOutput(raw, 10); err == nil {
		t.Fatal("metadata over budget was accepted")
	}
	plain := protocol.FileReadResult{ID: "id", Text: "content", Truncated: true}
	out, err := binding.DecodeOutput(packResult(t, plain), 1024)
	if err != nil || out["text"] != "content" || out["truncated"] != true {
		t.Fatalf("upstream truncation lost: %#v, %v", out, err)
	}
	plain.Text = "\xff"
	if _, err := binding.DecodeOutput(packResult(t, plain), 1024); err == nil {
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
