package server

import (
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
)

func asyncHeldTask(t *testing.T) (*Server, *taskState, *llm.Fake) {
	t.Helper()
	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant"}}}
	s := New(Config{Name: "reasoner", SecretFile: "-", LLM: fake}, log.New(io.Discard, "", 0))
	st := asyncTestTask(s, "c", "trace")
	st.running = false
	st.seen = st.gen
	return s, st, fake
}

func waitAsyncWake(t *testing.T, fake *llm.Fake, output string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if req := fake.LastRequest(); req != nil {
			for _, msg := range req.Messages {
				if strings.Contains(msg.Content, output) {
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("held result was not consumed by the resumed model round: %q", output)
}

// A held task still receives and deduplicates its output. Explicit resume must
// wake that queued result even if the user-control message contains no notes.
func TestAsyncPrimaryHoldKeepsOutputUntilExplicitResume(t *testing.T) {
	s, st, fake := asyncHeldTask(t)
	j := reserveAsync(t, s, st, "call")
	if err := s.finishAsync(j, "item", protocol.BatchKindOnce); err != nil {
		t.Fatal(err)
	}
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c", Hold: protocol.UserHoldPrimary})
	got, err := acceptAsyncReport(s, "worker", "trace", "item", "held final", true)
	if err != nil || got["status"] != "accepted" || !s.heldFor("c") || fake.CallCount() != 0 {
		t.Fatalf("primary hold was bypassed: %v %v", got, err)
	}
	if len(st.messages) != 1 || len(s.asyncJobs) != 0 {
		t.Fatal("final result was lost or its async registration remained executable")
	}
	got, err = acceptAsyncReport(s, "worker", "trace", "item", "duplicate", true)
	if err != nil || got["status"] != "duplicate" || len(st.messages) != 1 {
		t.Fatalf("duplicate final was processed under hold: %v %v", got, err)
	}
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c", Hold: protocol.UserHoldNone})
	waitAsyncWake(t, fake, "held final")
}

func TestAsyncSecondaryHoldFinalWakesWithoutNotes(t *testing.T) {
	s, st, fake := asyncHeldTask(t)
	j := reserveAsync(t, s, st, "call")
	if err := s.finishAsync(j, "item", protocol.BatchKindOnce); err != nil {
		t.Fatal(err)
	}
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c", Hold: protocol.UserHoldSecondary})
	if _, err := acceptAsyncReport(s, "wrong-worker", "trace", "item", "wrong final", true); err != nil {
		t.Fatal(err)
	}
	if !s.heldFor("c") || fake.CallCount() != 0 {
		t.Fatal("a foreign result released secondary hold")
	}
	if _, err := acceptAsyncReport(s, "worker", "trace", "item", "last final", true); err != nil {
		t.Fatal(err)
	}
	if s.heldFor("c") {
		t.Fatal("final job was counted as executable during hold release")
	}
	waitAsyncWake(t, fake, "last final")
}

func TestAsyncPendingReceiptKeepsSecondaryHold(t *testing.T) {
	s, st, fake := asyncHeldTask(t)
	a, pending := reserveAsync(t, s, st, "a"), reserveAsync(t, s, st, "pending")
	if err := s.finishAsync(a, "a", protocol.BatchKindOnce); err != nil {
		t.Fatal(err)
	}
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c", Hold: protocol.UserHoldSecondary})
	if _, err := acceptAsyncReport(s, "worker", "trace", "a", "finished a", true); err != nil {
		t.Fatal(err)
	}
	if !s.heldFor("c") || fake.CallCount() != 0 {
		t.Fatal("unknown in-flight receipt was treated as a completed/resident task")
	}
	if err := s.finishAsync(pending, "resident", protocol.BatchKindResident); err != nil {
		t.Fatal(err)
	}
	if s.heldFor("c") || s.hasExecutableAsync("c") {
		t.Fatal("a confirmed resident task blocked secondary fallback")
	}
	waitAsyncWake(t, fake, "finished a")
}

func TestAsyncAbandonedReceiptReleasesSecondaryHold(t *testing.T) {
	s := asyncTestServer()
	st := asyncTestTask(s, "c", "trace")
	pending := reserveAsync(t, s, st, "pending")
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c", Hold: protocol.UserHoldSecondary})
	if !s.heldFor("c") {
		t.Fatal("pending receipt did not block fallback")
	}
	s.abandonAsync(pending)
	if s.heldFor("c") || len(s.asyncPending) != 0 {
		t.Fatal("a failed local receipt left the conversation held indefinitely")
	}
}

func TestAsyncEarlyFinalReleasesSecondaryHoldOnce(t *testing.T) {
	s, st, fake := asyncHeldTask(t)
	j := reserveAsync(t, s, st, "call")
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c", Hold: protocol.UserHoldSecondary})
	got, err := acceptAsyncReport(s, "worker", "trace", "item", "early final", true)
	if err != nil || got["status"] != "buffered" || !s.heldFor("c") {
		t.Fatalf("early report bypassed receipt ownership: %v %v", got, err)
	}
	if err := s.finishAsync(j, "item", protocol.BatchKindOnce); err != nil {
		t.Fatal(err)
	}
	waitAsyncWake(t, fake, "early final")
	if s.heldFor("c") || len(s.asyncJobs) != 0 || len(s.asyncEarly) != 0 {
		t.Fatal("early terminal left unresolved async or hold state")
	}
}

func TestSecondaryFallbackCannotReleasePrimaryHold(t *testing.T) {
	s := asyncTestServer()
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c", Hold: protocol.UserHoldPrimary, Notes: []string{"keep me"}})
	s.releaseHold("c", "late secondary callback")
	if level(s, "c") != protocol.UserHoldPrimary || len(pendingNotes(s, "c")) != 1 {
		t.Fatal("late secondary fallback undid a newer primary hold")
	}
}

func TestHoldSupplementRetainsAsyncTaskOwnership(t *testing.T) {
	s := asyncTestServer()
	origin := asyncTestTask(s, "c", "trace")
	j := reserveAsync(t, s, origin, "call")
	if err := s.finishAsync(j, "item", protocol.BatchKindOnce); err != nil {
		t.Fatal(err)
	}
	s.boardClose(origin)
	hold(t, s, protocol.UserHoldPayload{
		ConversationID: "c", Hold: protocol.UserHoldSecondary,
		PausedItems: []string{"item"}, Notes: []string{"用户暂停了任务"},
	})
	st := s.boardGet("c")
	if st == nil || st.followUpOf != origin || st.taskID != origin.taskID {
		t.Fatal("hold supplement lost the original task identity")
	}
	got, err := acceptAsyncReport(s, "worker", "trace", "item", "paused job stopped", true)
	if err != nil || got["delivery"] != "injected" || s.boardGet("c") != st {
		t.Fatalf("final result did not return to its own supplement: %v %v", got, err)
	}
}
