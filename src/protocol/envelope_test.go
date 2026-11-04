package protocol

import (
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

// TestEnvelopeRoundTrip 验证信封 + 复杂 payload 的编解码往返。
func TestEnvelopeRoundTrip(t *testing.T) {
	reg := RegisterPayload{
		Name:        "reasoner",
		Addr:        "ws://127.0.0.1:9101/ws",
		Description: "测试用名片",
		Provides:    []Provide{{Action: "echo", Input: "任意", Output: "原样返回"}},
		Settings:    []Setting{{Key: "demo.setting", Default: "5m"}},
		HeartbeatMS: 15000,
	}
	env, err := NewEnvelope(TypeRegister, "reasoner", "settings-center", "trace-1", reg)
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

	if got.V != ProtocolVersion {
		t.Fatalf("v = %d, want %d", got.V, ProtocolVersion)
	}
	if got.ID == "" || got.Trace != "trace-1" {
		t.Fatalf("id/trace 不符: id=%q trace=%q", got.ID, got.Trace)
	}
	if got.Type != TypeRegister || got.From != "reasoner" || got.To != "settings-center" {
		t.Fatalf("信封字段不符: %+v", got)
	}

	var reg2 RegisterPayload
	if err := got.DecodePayload(&reg2); err != nil {
		t.Fatal(err)
	}
	if reg2.Name != "reasoner" || reg2.Addr != "ws://127.0.0.1:9101/ws" {
		t.Fatalf("payload 不符: %+v", reg2)
	}
	if len(reg2.Provides) != 1 || reg2.Provides[0].Action != "echo" {
		t.Fatalf("provides 不符: %+v", reg2.Provides)
	}
	if len(reg2.Settings) != 1 || reg2.Settings[0].Key != "demo.setting" {
		t.Fatalf("settings 不符: %+v", reg2.Settings)
	}
	if reg2.HeartbeatMS != 15000 {
		t.Fatalf("heartbeat_ms 不符: %d", reg2.HeartbeatMS)
	}
	if reg2.Card().HeartbeatMS != 15000 {
		t.Fatalf("名片应携带心跳间隔: %+v", reg2.Card())
	}
}

// TestResponseDataPassthrough 验证 response.Data 可原样转发（RawMessage 不丢字节）。
func TestResponseDataPassthrough(t *testing.T) {
	data, err := EncodePayload(map[string]any{"pong": true, "who": "settings-center"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := NewEnvelope(TypeResponse, "settings-center", "reasoner", "trace-9", ResponsePayload{OK: true, Data: data})
	if err != nil {
		t.Fatal(err)
	}

	blob, err := Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(blob)
	if err != nil {
		t.Fatal(err)
	}

	var resp ResponsePayload
	if err := got.DecodePayload(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatal("ok 应为 true")
	}
	var out map[string]any
	if err := msgpack.Unmarshal(resp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out["pong"] != true || out["who"] != "settings-center" {
		t.Fatalf("data 不符: %+v", out)
	}
}

// TestUnknownFieldsAreIgnored 钉住向前兼容：新版本多带的字段不得让旧版本解码失败，
// 已认识的字段必须原样保留（信封顶层与 payload 内层都要成立）。
func TestUnknownFieldsAreIgnored(t *testing.T) {
	raw, err := msgpack.Marshal(map[string]any{
		"v": 1, "id": "01UNKNOWN0000000000000000", "trace": "TRACE-FUTURE",
		"type": TypeRegister, "from": "svc-a", "to": "settings-center",
		"future_top": map[string]any{"x": 1},
		"payload": map[string]any{
			"name": "svc-a", "addr": "ws://127.0.0.1:9999/ws",
			"future_payload": 42,
			"provides":       []any{map[string]any{"action": "echo", "future_flag": true}},
			"settings":       []any{map[string]any{"key": "demo.setting", "future_default": "x"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	env, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("未知字段不应导致解码失败: %v", err)
	}
	if env.V != ProtocolVersion || env.Type != TypeRegister || env.From != "svc-a" || env.To != "settings-center" {
		t.Fatalf("已知信封字段不符: %+v", env)
	}
	if env.Trace != "TRACE-FUTURE" {
		t.Fatalf("trace 不符: %q", env.Trace)
	}

	var reg RegisterPayload
	if err := env.DecodePayload(&reg); err != nil {
		t.Fatalf("未知 payload 字段不应导致解码失败: %v", err)
	}
	if reg.Name != "svc-a" || reg.Addr != "ws://127.0.0.1:9999/ws" {
		t.Fatalf("已知 payload 字段不符: %+v", reg)
	}
	if len(reg.Provides) != 1 || reg.Provides[0].Action != "echo" {
		t.Fatalf("provides 不符: %+v", reg.Provides)
	}
	if len(reg.Settings) != 1 || reg.Settings[0].Key != "demo.setting" {
		t.Fatalf("settings 不符: %+v", reg.Settings)
	}
}

// TestNewIDOrdered 验证 ULID 时间有序、非空。
func TestNewIDOrdered(t *testing.T) {
	a := NewID()
	b := NewID()
	if len(a) != 26 || len(b) != 26 {
		t.Fatalf("ULID 长度应为 26: %q %q", a, b)
	}
	if b <= a {
		t.Fatalf("同毫秒内应单调递增: %q -> %q", a, b)
	}
}
