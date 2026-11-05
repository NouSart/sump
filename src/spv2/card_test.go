package spv2

import "testing"

func testCard() Card {
	return Card{
		Protocol: ProtocolVersion, Name: "svc", Actor: ServiceRef("svc"), Version: "0.1.0",
		Methods:       []Method{{Name: "echo", Shape: ShapeCall, Input: map[string]any{"type": "object"}}},
		Limits:        Limits{MaxBody: 1024, MaxStream: 4},
		Observability: Observability{Health: true, Metrics: true},
	}
}

func TestCardValidate(t *testing.T) {
	c := testCard()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Method("echo"); !ok {
		t.Fatal("method missing")
	}
	dup := testCard()
	dup.Methods = append(dup.Methods, dup.Methods[0])
	if err := dup.Validate(); err == nil {
		t.Fatal("duplicate method must fail")
	}
	bad := testCard()
	bad.Methods[0].Tool = true
	bad.Methods[0].Input = nil
	if err := bad.Validate(); err == nil {
		t.Fatal("tool without input must fail")
	}
}
