package spv2

import (
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	f, err := NewCall("w1", HumanRef("portal"), ServiceRef("reasoner"), "message", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != f.ID || got.Work != "w1" || got.Method != "message" || got.From.Authority != AuthorityOwner {
		t.Fatalf("got = %+v", got)
	}
	var body map[string]any
	if err := got.DecodeBody(&body); err != nil || body["text"] != "hi" {
		t.Fatalf("body = %+v, %v", body, err)
	}
}

func TestValidateReplyError(t *testing.T) {
	req, _ := NewCall("w1", HumanRef("p"), ServiceRef("s"), "m", nil)
	f := NewErrorReply(req, ServiceRef("s"), CodeNotFound, "no", false, nil)
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	f.Error = nil
	if err := f.Validate(); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateStreamEnd(t *testing.T) {
	req, _ := NewCall("w1", HumanRef("p"), ServiceRef("s"), "m", nil)
	if _, err := NewStreamFrame(req, ServiceRef("s"), "st", 1, true, nil, StatusError); err == nil {
		t.Fatal("expected error stream end without error")
	}
}

func TestJSONAndTextBody(t *testing.T) {
	f, _ := NewCall("w1", HumanRef("p"), ServiceRef("s"), "m", nil)
	f.ContentType = ContentTypeJSON
	f.Body = []byte("{\"text\":\"hi\"}")
	var out map[string]string
	if err := f.DecodeBody(&out); err != nil || out["text"] != "hi" {
		t.Fatalf("json body = %+v, %v", out, err)
	}
	f.ContentType = ContentTypeText
	f.Body = []byte("hello")
	var s string
	if err := f.DecodeBody(&s); err != nil || s != "hello" {
		t.Fatalf("text body = %q, %v", s, err)
	}
	f.ContentType = "application/xml"
	if err := f.Validate(); err == nil {
		t.Fatal("unsupported content type must fail")
	}
}

func TestErrorStreamFrame(t *testing.T) {
	req, _ := NewCall("w1", HumanRef("p"), ServiceRef("s"), "m", nil)
	e := ErrCode(CodeDeadlineExceeded, "too slow", true, nil)
	f, err := NewErrorStreamFrame(req, ServiceRef("s"), "st", 3, e)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if f.Stream == nil || !f.Stream.End || f.Status != StatusError || f.Error == nil {
		t.Fatalf("frame = %+v", f)
	}
}

func TestValidateConditionalFields(t *testing.T) {
	f, _ := NewCall("w1", HumanRef("p"), ServiceRef("s"), "m", nil)
	f.Status = StatusOK
	if err := f.Validate(); err == nil {
		t.Fatal("call must not carry status")
	}
}
