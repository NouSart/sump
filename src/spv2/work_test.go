package spv2

import (
	"testing"
	"time"
)

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to string
		ok       bool
	}{
		{WorkOpen, WorkRunning, true},
		{WorkOpen, WorkDone, false},
		{WorkOpen, WorkStalled, true},
		{WorkRunning, WorkStalled, true},
		{WorkStalled, WorkRunning, true},
		{WorkDone, WorkRunning, false},
		{WorkCancelled, WorkDone, false},
		{WorkFailed, WorkFailed, true},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.ok {
			t.Errorf("CanTransition(%s,%s)=%v want %v", c.from, c.to, got, c.ok)
		}
	}
}

func TestLeaseDefaults(t *testing.T) {
	l := DefaultLease(time.Now())
	if l.MaxToolCalls != 50 || l.MaxTokens != 100000 || l.MaxApprovals != 5 {
		t.Fatalf("lease = %+v", l)
	}
	if _, ok := DeadlineFromLease(l); !ok {
		t.Fatal("deadline should parse")
	}
}
