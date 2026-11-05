package adapter

import (
	"bytes"
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// encodeV1 把 v1 payload 编码成信封载荷。
func encodeV1(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := protocol.EncodePayload(v)
	if err != nil {
		t.Fatalf("编码 v1 payload 失败: %v", err)
	}
	return raw
}

// jumpEnvelope 构造一条 v1 jump 信封。
func jumpEnvelope(t *testing.T, id, trace, from, to, action string, input []byte) protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEnvelope(protocol.TypeJump, from, to, trace, protocol.JumpPayload{
		Action: action,
		Input:  input,
	})
	if err != nil {
		t.Fatalf("构造 v1 jump 信封失败: %v", err)
	}
	if id != "" {
		env.ID = id
	}
	return env
}

func TestJumpRoundTrip(t *testing.T) {
	input := encodeV1(t, map[string]any{"text": "hi"})
	env := jumpEnvelope(t, "jump-1", "trace-1", "frontend", "reasoner", "message", input)

	f, err := FromV1(env)
	if err != nil {
		t.Fatalf("FromV1 失败: %v", err)
	}
	if f.V != spv2.ProtocolVersion {
		t.Errorf("v = %q, want %q", f.V, spv2.ProtocolVersion)
	}
	if f.Type != spv2.TypeCall {
		t.Errorf("type = %q, want %q", f.Type, spv2.TypeCall)
	}
	if f.ID != env.ID {
		t.Errorf("id = %q, want %q", f.ID, env.ID)
	}
	if f.Work != env.Trace {
		t.Errorf("work = %q, want %q", f.Work, env.Trace)
	}
	if f.From.Kind != "service" || f.From.ID != "frontend" {
		t.Errorf("from = %+v, want service/frontend", f.From)
	}
	if f.To.Kind != "service" || f.To.ID != "reasoner" {
		t.Errorf("to = %+v, want service/reasoner", f.To)
	}
	if f.Method != "message" {
		t.Errorf("method = %q, want message", f.Method)
	}
	if !bytes.Equal(f.Body, input) {
		t.Errorf("body = %q, want %q", f.Body, input)
	}
	if f.ContentType != spv2.ContentTypeMsgPack {
		t.Errorf("content_type = %q, want %q", f.ContentType, spv2.ContentTypeMsgPack)
	}

	back, err := ToV1(f)
	if err != nil {
		t.Fatalf("ToV1 失败: %v", err)
	}
	if back.V != protocol.ProtocolVersion {
		t.Errorf("v = %d, want %d", back.V, protocol.ProtocolVersion)
	}
	if back.Type != protocol.TypeJump {
		t.Errorf("type = %q, want %q", back.Type, protocol.TypeJump)
	}
	if back.ID != f.ID {
		t.Errorf("id = %q, want %q", back.ID, f.ID)
	}
	if back.Trace != f.Work {
		t.Errorf("trace = %q, want %q", back.Trace, f.Work)
	}
	if back.From != "frontend" || back.To != "reasoner" {
		t.Errorf("from/to = %q/%q, want frontend/reasoner", back.From, back.To)
	}
	var jp protocol.JumpPayload
	if err := back.DecodePayload(&jp); err != nil {
		t.Fatalf("解码回程 payload 失败: %v", err)
	}
	if jp.Action != "message" {
		t.Errorf("action = %q, want message", jp.Action)
	}
	if !bytes.Equal(jp.Input, input) {
		t.Errorf("input = %q, want %q", jp.Input, input)
	}
}

func TestResponseOK(t *testing.T) {
	req := jumpEnvelope(t, "req-1", "trace-1", "frontend", "reasoner", "message", nil)
	data := encodeV1(t, map[string]any{"reply": "ok"})
	resp, err := protocol.NewResponse(req, "reasoner", protocol.ResponsePayload{OK: true, Data: data})
	if err != nil {
		t.Fatalf("构造 v1 response 失败: %v", err)
	}

	f, err := FromV1(resp)
	if err != nil {
		t.Fatalf("FromV1 失败: %v", err)
	}
	if f.Type != spv2.TypeReply {
		t.Errorf("type = %q, want %q", f.Type, spv2.TypeReply)
	}
	if f.ID == "" {
		t.Error("reply id 不应为空")
	}
	if f.ReplyTo != resp.ID {
		t.Errorf("reply_to = %q, want %q", f.ReplyTo, resp.ID)
	}
	if f.Work != resp.Trace {
		t.Errorf("work = %q, want %q", f.Work, resp.Trace)
	}
	if f.Status != spv2.StatusOK {
		t.Errorf("status = %q, want %q", f.Status, spv2.StatusOK)
	}
	if f.Error != nil {
		t.Errorf("error = %+v, want nil", f.Error)
	}
	if !bytes.Equal(f.Body, data) {
		t.Errorf("body = %q, want %q", f.Body, data)
	}
	if f.From.ID != "reasoner" || f.To.ID != "frontend" {
		t.Errorf("from/to = %q/%q, want reasoner/frontend", f.From.ID, f.To.ID)
	}

	back, err := ToV1(f)
	if err != nil {
		t.Fatalf("ToV1 失败: %v", err)
	}
	if back.Type != protocol.TypeResponse {
		t.Errorf("type = %q, want %q", back.Type, protocol.TypeResponse)
	}
	if back.ID != f.ReplyTo {
		t.Errorf("id = %q, want %q", back.ID, f.ReplyTo)
	}
	if back.Trace != f.Work {
		t.Errorf("trace = %q, want %q", back.Trace, f.Work)
	}
	if back.From != "reasoner" || back.To != "frontend" {
		t.Errorf("from/to = %q/%q, want reasoner/frontend", back.From, back.To)
	}
	var rp protocol.ResponsePayload
	if err := back.DecodePayload(&rp); err != nil {
		t.Fatalf("解码回程 payload 失败: %v", err)
	}
	if !rp.OK {
		t.Error("ok = false, want true")
	}
	if rp.Error != "" {
		t.Errorf("error = %q, want 空", rp.Error)
	}
	if !bytes.Equal(rp.Data, data) {
		t.Errorf("data = %q, want %q", rp.Data, data)
	}
}

func TestResponseError(t *testing.T) {
	req := jumpEnvelope(t, "req-1", "trace-1", "frontend", "reasoner", "message", nil)
	resp, err := protocol.NewResponse(req, "reasoner", protocol.ResponsePayload{OK: false, Error: "boom"})
	if err != nil {
		t.Fatalf("构造 v1 response 失败: %v", err)
	}

	f, err := FromV1(resp)
	if err != nil {
		t.Fatalf("FromV1 失败: %v", err)
	}
	if f.Type != spv2.TypeReply {
		t.Errorf("type = %q, want %q", f.Type, spv2.TypeReply)
	}
	if f.Status != spv2.StatusError {
		t.Errorf("status = %q, want %q", f.Status, spv2.StatusError)
	}
	if f.Error == nil {
		t.Fatal("error = nil, want internal error")
	}
	if f.Error.Code != spv2.CodeInternal {
		t.Errorf("error.code = %q, want %q", f.Error.Code, spv2.CodeInternal)
	}
	if f.Error.Message != "boom" {
		t.Errorf("error.message = %q, want boom", f.Error.Message)
	}
	if f.Error.Retryable {
		t.Error("error.retryable = true, want false")
	}
	if f.ReplyTo != resp.ID {
		t.Errorf("reply_to = %q, want %q", f.ReplyTo, resp.ID)
	}

	back, err := ToV1(f)
	if err != nil {
		t.Fatalf("ToV1 失败: %v", err)
	}
	var rp protocol.ResponsePayload
	if err := back.DecodePayload(&rp); err != nil {
		t.Fatalf("解码回程 payload 失败: %v", err)
	}
	if rp.OK {
		t.Error("ok = true, want false")
	}
	if rp.Error != "boom" {
		t.Errorf("error = %q, want boom", rp.Error)
	}
}

func TestUnsupportedTypes(t *testing.T) {
	for _, typ := range []string{
		protocol.TypeRegister,
		protocol.TypeRoster,
		protocol.TypeHeartbeat,
		protocol.TypeSettings,
		"bogus",
	} {
		env := protocol.Envelope{V: protocol.ProtocolVersion, ID: "m1", Type: typ, From: "a", To: "b"}
		if _, err := FromV1(env); err == nil {
			t.Errorf("FromV1(%q) 应返回错误", typ)
		}
	}

	for _, typ := range []string{spv2.TypeEvent, spv2.TypeStream, "bogus"} {
		f := spv2.Frame{V: spv2.ProtocolVersion, ID: "f1", Type: typ, Work: "w1"}
		if _, err := ToV1(f); err == nil {
			t.Errorf("ToV1(%q) 应返回错误", typ)
		}
	}
}

func TestEmptyWorkFallsBackToID(t *testing.T) {
	// jump：trace 为空时用信封 id 兜底。
	jump := protocol.Envelope{
		V:       protocol.ProtocolVersion,
		ID:      "jump-1",
		Type:    protocol.TypeJump,
		From:    "frontend",
		To:      "reasoner",
		Payload: encodeV1(t, protocol.JumpPayload{Action: "health"}),
	}
	f, err := FromV1(jump)
	if err != nil {
		t.Fatalf("FromV1(jump) 失败: %v", err)
	}
	if f.Work != "jump-1" {
		t.Errorf("jump work = %q, want jump-1", f.Work)
	}

	// response：trace 为空时同样用信封 id 兜底（v1 响应 id 即请求 id）。
	resp := protocol.Envelope{
		V:       protocol.ProtocolVersion,
		ID:      "req-2",
		Type:    protocol.TypeResponse,
		From:    "reasoner",
		To:      "frontend",
		Payload: encodeV1(t, protocol.ResponsePayload{OK: true}),
	}
	f, err = FromV1(resp)
	if err != nil {
		t.Fatalf("FromV1(response) 失败: %v", err)
	}
	if f.Work != "req-2" {
		t.Errorf("response work = %q, want req-2", f.Work)
	}

	// trace 与 id 都为空：不能凭空造 work，应被 Validate 拦下。
	empty := protocol.Envelope{
		V:       protocol.ProtocolVersion,
		Type:    protocol.TypeJump,
		From:    "frontend",
		To:      "reasoner",
		Payload: encodeV1(t, protocol.JumpPayload{Action: "health"}),
	}
	if _, err := FromV1(empty); err == nil {
		t.Error("FromV1(空 trace 且空 id) 应返回错误")
	}
}
