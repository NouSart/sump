package spv2

import "testing"

func authorityWork() Work {
	return Work{
		ID: "w1", Root: "w1", Class: ClassThread,
		Owner: HumanRef("owner"), Agent: AgentRef("reasoner"),
		Participants: []Participant{{Actor: AgentRef("worker"), Role: "member", Status: "active"}},
	}
}

func TestAuthorizeDeliver(t *testing.T) {
	w := authorityWork()
	if err := AuthorizeDeliver(w, HumanRef("owner")); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeDeliver(w, AgentRef("reasoner").WithAuthority(AuthorityRuntime)); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeDeliver(w, AgentRef("worker")); err == nil {
		t.Fatal("participant must not deliver")
	}
}

func TestAuthorizeMessage(t *testing.T) {
	w := authorityWork()
	if err := AuthorizeMessage(w, HumanRef("owner")); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeMessage(w, AgentRef("worker")); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeMessage(w, AgentRef("worker").WithAuthority(AuthorityOwner)); err == nil {
		t.Fatal("agent owner authority must be rejected")
	}
	if err := AuthorizeMessage(w, AgentRef("reasoner").WithAuthority(AuthorityRuntime)); err == nil {
		t.Fatal("agent runtime authority must not be used for message")
	}
	if err := AuthorizeMessage(w, AgentRef("stranger")); err == nil {
		t.Fatal("stranger must be rejected")
	}
}
