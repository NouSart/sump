package spv2

import (
	"testing"
)

func TestAgentCannotClaimOwner(t *testing.T) {
	f, _ := NewCall("w1", AgentRef("a").WithAuthority(AuthorityOwner), ServiceRef("s"), "m", nil)
	if err := f.Validate(); err == nil {
		t.Fatal("agent with owner authority must be rejected")
	}
}

func TestAgentRuntimeAllowed(t *testing.T) {
	f, err := NewCall("w1", AgentRef("reasoner").WithAuthority(AuthorityRuntime), ServiceRef("frontend"), "deliver", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityNotSupported(t *testing.T) {
	f, _ := NewCall("w1", HumanRef("p"), CapabilityRef("cap1"), "m", nil)
	if err := f.Validate(); err == nil || !IsCode(err, CodeNotSupported) {
		t.Fatalf("want not_supported, got %v", err)
	}
}

func TestValidateTo(t *testing.T) {
	f, _ := NewCall("w1", HumanRef("p"), ServiceRef("other"), "m", nil)
	if err := ValidateTo(f, ServiceRef("self")); err == nil {
		t.Fatal("wrong target must be rejected")
	}
	if err := ValidateTo(f, ServiceRef("other")); err != nil {
		t.Fatal(err)
	}
}
