package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/vmihailenco/msgpack/v5"
)

func reserveTestJob(t *testing.T, jobs *Jobs, trace string, release func()) string {
	t.Helper()
	id, err := jobs.Reserve("owner", "timer", "timer__sleep", trace, release)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func reportTestJob(t *testing.T, jobs *Jobs, trace, item, phase string, final bool) bool {
	t.Helper()
	ok, err := jobs.Report(protocol.Envelope{From: "timer", Trace: trace}, protocol.AsyncResultPayload{ItemID: item, Phase: phase, Text: phase, Final: final})
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func readTestJob(t *testing.T, jobs *Jobs, id string) JobSnapshot {
	t.Helper()
	snapshot, err := jobs.Read("owner", id)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestJobsOwnershipAndReportCorrelation(t *testing.T) {
	jobs := NewJobs(JobsConfig{})
	defer jobs.Close()
	id := reserveTestJob(t, jobs, "trace-a", nil)
	if _, err := jobs.Read("other", id); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("foreign read: %v", err)
	}
	if _, err := jobs.LookupCancel("other", id); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("foreign control: %v", err)
	}
	if err := jobs.ConfirmCanceled("other", id); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("foreign confirmation: %v", err)
	}
	if _, err := jobs.LookupCancel("owner", id); err == nil {
		t.Fatal("unbound cancellation accepted")
	}
	if err := jobs.BindReceipt(id, "item-a"); err != nil {
		t.Fatal(err)
	}
	for _, env := range []protocol.Envelope{{From: "foreign", Trace: "trace-a"}, {From: "timer", Trace: "other"}} {
		if ok, err := jobs.Report(env, protocol.AsyncResultPayload{ItemID: "item-a", Phase: "done", Final: true}); ok || err != nil {
			t.Fatalf("foreign report accepted: %v %v", ok, err)
		}
	}
	if reportTestJob(t, jobs, "trace-a", "other-item", "done", true) {
		t.Fatal("foreign item accepted")
	}
	if s := readTestJob(t, jobs, id); s.Final || s.State != "accepted" {
		t.Fatalf("foreign input changed state: %+v", s)
	}
	if !reportTestJob(t, jobs, "trace-a", "item-a", "done", true) {
		t.Fatal("matching report rejected")
	}
	if _, err := jobs.LookupCancel("owner", id); !errors.Is(err, ErrJobClosed) {
		t.Fatalf("final cancel lookup: %v", err)
	}
}

func TestJobsEarlyReceiptAndFirstFinalWin(t *testing.T) {
	jobs := NewJobs(JobsConfig{})
	defer jobs.Close()
	var released atomic.Int32
	id := reserveTestJob(t, jobs, "early", func() { released.Add(1) })
	reportTestJob(t, jobs, "early", "wrong", "done", true)
	reportTestJob(t, jobs, "early", "right", "working", false)
	if reportTestJob(t, jobs, "early", "right", "working", false) {
		t.Fatal("duplicate early report retained")
	}
	reportTestJob(t, jobs, "early", "right", "done", true)
	reportTestJob(t, jobs, "early", "right", "stale-progress", false)
	if s := readTestJob(t, jobs, id); s.Final || s.ItemID != "" || released.Load() != 0 {
		t.Fatal("report finalized before receipt correlation")
	}
	if err := jobs.BindReceipt(id, "right"); err != nil {
		t.Fatal(err)
	}
	if s := readTestJob(t, jobs, id); !s.Final || s.Phase != "done" || released.Load() != 1 {
		t.Fatalf("early final not matched: %+v", s)
	}
	if err := jobs.BindReceipt(id, "right"); err != nil {
		t.Fatal(err)
	}
	if err := jobs.BindReceipt(id, "wrong"); err == nil {
		t.Fatal("conflicting receipt accepted")
	}
	if reportTestJob(t, jobs, "early", "right", "canceled", true) {
		t.Fatal("late terminal replaced first terminal")
	}
	if err := jobs.ConfirmCanceled("owner", id); err != nil {
		t.Fatal(err)
	}
	jobs.MarkUnknown(id, "late failure")
	if s := readTestJob(t, jobs, id); s.Phase != "done" || s.Problem != "" {
		t.Fatalf("terminal regressed: %+v", s)
	}
	jobs.Close()
	if released.Load() != 1 {
		t.Fatal("admission released more than once")
	}
}

func TestJobsUnknownLateFinalAndTimeout(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	jobs := NewJobs(JobsConfig{Timeout: time.Minute, TTL: 5 * time.Minute, Now: func() time.Time { return now }})
	defer jobs.Close()
	var released atomic.Int32
	id := reserveTestJob(t, jobs, "unknown", func() { released.Add(1) })
	now = now.Add(time.Minute)
	jobs.Sweep()
	if s := readTestJob(t, jobs, id); s.State != "unknown" || s.Final || s.ItemID != "" || released.Load() != 0 {
		t.Fatalf("timeout incorrectly finalized: %+v", s)
	}
	if !reportTestJob(t, jobs, "unknown", "late-item", "working", false) {
		t.Fatal("unknown did not accept correlated late report")
	}
	if s := readTestJob(t, jobs, id); s.ItemID != "late-item" || s.State != "unknown" {
		t.Fatalf("late observation lost unknown state: %+v", s)
	}
	if reportTestJob(t, jobs, "unknown", "other-item", "done", true) {
		t.Fatal("late report changed bound identity")
	}
	if err := jobs.BindReceipt(id, "late-item"); err != nil {
		t.Fatal(err)
	}
	if !reportTestJob(t, jobs, "unknown", "late-item", "done", true) {
		t.Fatal("late final rejected")
	}
	if s := readTestJob(t, jobs, id); s.State != "final" || s.Problem != "" || released.Load() != 1 {
		t.Fatalf("late final not retained: %+v", s)
	}
}

func TestJobsLostReceiptReplaysBufferedEvidence(t *testing.T) {
	jobs := NewJobs(JobsConfig{})
	defer jobs.Close()
	id := reserveTestJob(t, jobs, "lost", nil)
	reportTestJob(t, jobs, "lost", "native-item", "done", true)
	jobs.MarkUnknown(id, "receipt unavailable")
	if s := readTestJob(t, jobs, id); !s.Final || s.Phase != "done" || s.ItemID != "native-item" {
		t.Fatalf("confirmed evidence lost: %+v", s)
	}
}

func TestJobsSnapshotDataAndNativeHandlePrivacy(t *testing.T) {
	jobs := NewJobs(JobsConfig{})
	defer jobs.Close()
	id := reserveTestJob(t, jobs, "data", nil)
	if err := jobs.BindReceipt(id, "private-native-handle"); err != nil {
		t.Fatal(err)
	}
	raw, err := protocol.EncodePayload(map[string]any{"value": "original"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := jobs.Report(protocol.Envelope{From: "timer", Trace: "data"}, protocol.AsyncResultPayload{ItemID: "private-native-handle", Phase: "done", Data: raw, Final: true}); !ok || err != nil {
		t.Fatalf("data report: %v %v", ok, err)
	}
	for i := range raw {
		raw[i] = 0
	}
	snapshot := readTestJob(t, jobs, id)
	encoded, _ := json.Marshal(snapshot)
	if bytes.Contains(encoded, []byte("private-native-handle")) {
		t.Fatal("public observation exposed native handle")
	}
	if !bytes.Contains(snapshot.Data, []byte("original")) {
		t.Fatal("input mutation changed retained data")
	}
	for i := range snapshot.Data {
		snapshot.Data[i] = 0
	}
	if !bytes.Contains(readTestJob(t, jobs, id).Data, []byte("original")) {
		t.Fatal("snapshot mutation changed retained data")
	}
}

func TestJobsLimitsAndExpiryReleaseOutsideLock(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	jobs := NewJobs(JobsConfig{MaxJobs: 1, TTL: 2 * time.Minute, Timeout: time.Minute, Now: func() time.Time { return now }})
	var releases atomic.Int32
	id := reserveTestJob(t, jobs, "capacity", func() {
		if !jobs.mu.TryLock() {
			t.Error("release callback ran with store lock held")
		} else {
			jobs.mu.Unlock()
		}
		releases.Add(1)
	})
	var failedCallback atomic.Int32
	if _, err := jobs.Reserve("owner", "timer", "timer__sleep", "blocked", func() { failedCallback.Add(1) }); !errors.Is(err, ErrJobsFull) {
		t.Fatalf("capacity accepted: %v", err)
	}
	if failedCallback.Load() != 0 {
		t.Fatal("failed Reserve consumed caller callback")
	}
	if err := jobs.BindReceipt(id, "item"); err != nil {
		t.Fatal(err)
	}
	reportTestJob(t, jobs, "capacity", "item", "done", true)
	if releases.Load() != 1 {
		t.Fatal("confirmed final did not release execution capacity")
	}
	if _, err := jobs.Reserve("owner", "timer", "timer__sleep", "blocked", nil); !errors.Is(err, ErrJobsFull) {
		t.Fatal("retained final evaded observation cap")
	}
	now = now.Add(2 * time.Minute)
	jobs.Sweep()
	if _, err := jobs.Read("owner", id); !errors.Is(err, ErrJobNotFound) {
		t.Fatal("expired job still readable")
	}
	reserveTestJob(t, jobs, "next", func() { releases.Add(1) })
	jobs.Close()
	jobs.Close()
	if releases.Load() != 2 || failedCallback.Load() != 0 || jobs.bytes != 0 {
		t.Fatal("expiry/close leaked or double released capacity")
	}
}

func TestJobsEarlyAndByteBudgetsAreAtomic(t *testing.T) {
	jobs := NewJobs(JobsConfig{MaxEarlyReports: 1})
	defer jobs.Close()
	id := reserveTestJob(t, jobs, "early-budget", nil)
	reportTestJob(t, jobs, "early-budget", "item", "working", false)
	before := jobs.bytes
	if ok, err := jobs.Report(protocol.Envelope{From: "timer", Trace: "early-budget"}, protocol.AsyncResultPayload{ItemID: "item", Phase: "done", Final: true}); ok || !errors.Is(err, ErrJobResultLimit) {
		t.Fatalf("early count limit bypassed: %v %v", ok, err)
	}
	if jobs.bytes != before {
		t.Fatal("rejected report changed accounting")
	}
	if err := jobs.BindReceipt(id, "item"); err != nil {
		t.Fatal(err)
	}
	if s := readTestJob(t, jobs, id); s.Phase != "working" || s.Final {
		t.Fatalf("rejected report changed snapshot: %+v", s)
	}
	jobs.mu.Lock()
	jobs.cfg.MaxTotalBytes = jobs.bytes
	jobs.mu.Unlock()
	if ok, err := jobs.Report(protocol.Envelope{From: "timer", Trace: "early-budget"}, protocol.AsyncResultPayload{ItemID: "item", Phase: "done", Text: strings.Repeat("x", 2000), Final: true}); ok || !errors.Is(err, ErrJobResultLimit) {
		t.Fatalf("aggregate byte limit bypassed: %v %v", ok, err)
	}
	if s := readTestJob(t, jobs, id); s.Phase != "working" || s.Final {
		t.Fatal("failed atomic replacement damaged snapshot")
	}
	tiny := NewJobs(JobsConfig{MaxBytes: 10})
	defer tiny.Close()
	if _, err := tiny.Reserve("owner", "timer", "timer__sleep", "tiny", nil); !errors.Is(err, ErrJobsFull) {
		t.Fatal("tiny per-record limit accepted")
	}
}

func TestJobsCloseAndRestartForgetHandles(t *testing.T) {
	jobs := NewJobs(JobsConfig{})
	id := reserveTestJob(t, jobs, "closed", nil)
	jobs.Close()
	if _, err := jobs.Reserve("owner", "timer", "timer__sleep", "new", nil); !errors.Is(err, ErrJobClosed) {
		t.Fatal("closed store accepted reservation")
	}
	if reportTestJob(t, jobs, "closed", "item", "done", true) {
		t.Fatal("closed store accepted report")
	}
	if err := jobs.BindReceipt(id, "item"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("closed store accepted receipt: %v", err)
	}
	restarted := NewJobs(JobsConfig{})
	defer restarted.Close()
	if _, err := restarted.Read("owner", id); !errors.Is(err, ErrJobNotFound) {
		t.Fatal("restart invented old state")
	}
	if next := reserveTestJob(t, restarted, "closed", nil); next == id {
		t.Fatal("restart reused old handle")
	}
}

func TestJobsConcurrentFinalAndCancellation(t *testing.T) {
	jobs := NewJobs(JobsConfig{})
	defer jobs.Close()
	var released atomic.Int32
	id := reserveTestJob(t, jobs, "race", func() { released.Add(1) })
	if err := jobs.BindReceipt(id, "item"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 32; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				_, err := jobs.Report(protocol.Envelope{From: "timer", Trace: "race"}, protocol.AsyncResultPayload{ItemID: "item", Phase: "done", Final: true})
				if err != nil {
					t.Error(err)
				}
			} else if err := jobs.ConfirmCanceled("owner", id); err != nil {
				t.Error(err)
			}
		}(n)
	}
	wg.Wait()
	snapshot := readTestJob(t, jobs, id)
	if !snapshot.Final || (snapshot.Phase != "done" && snapshot.Phase != "canceled") || released.Load() != 1 {
		t.Fatalf("terminal race inconsistent: %+v released=%d", snapshot, released.Load())
	}
}

func TestJobsTextTruncationIsUTF8AndBounded(t *testing.T) {
	for _, input := range []string{strings.Repeat("测", 1000), strings.Repeat("\"\n\\", 1000), "valid\xffinvalid"} {
		event, err := encodeJobEvent(protocol.AsyncResultPayload{ItemID: "id", Phase: "done", Text: input, Final: true}, 256)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(event)
		if !event.Truncated || !utf8.ValidString(event.Text) || len(raw) > 256 {
			t.Fatalf("invalid bounded projection: %d %+v", len(raw), event)
		}
	}
	if _, err := encodeJobEvent(protocol.AsyncResultPayload{ItemID: "id", Phase: "done", Data: make([]byte, 257)}, 256); !errors.Is(err, ErrJobResultLimit) {
		t.Fatal("oversized structured input accepted")
	}
}

func TestJobsMessagePackValidation(t *testing.T) {
	encode := func(value any) []byte {
		t.Helper()
		raw, err := msgpack.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	invalid := map[string][]byte{
		"array_length":    {0xdd, 0x7f, 0xff, 0xff, 0xff},
		"map_length":      {0xdf, 0x7f, 0xff, 0xff, 0xff},
		"string_length":   {0xdb, 0x7f, 0xff, 0xff, 0xff},
		"invalid_utf8":    {0xa1, 0xff},
		"duplicate_keys":  {0x82, 0xa1, 'a', 1, 0xa1, 'a', 2},
		"non_string_key":  {0x81, 1, 2},
		"trailing_value":  {1, 2},
		"binary":          {0xc4, 1, 1},
		"extension":       {0xd4, 1, 1},
		"unknown_code":    {0xc1},
		"unsafe_signed":   encode(int64(1 << 53)),
		"unsafe_unsigned": encode(uint64(1 << 53)),
		"unsafe_float64":  encode(float64(1 << 53)),
		"unsafe_float32":  encode(float32(1 << 60)),
		"nan":             encode(math.NaN()),
		"infinity":        encode(math.Inf(1)),
		"depth":           append(bytes.Repeat([]byte{0x91}, 34), 0xc0),
		"node_budget":     append([]byte{0xdc, 0x10, 0x00}, bytes.Repeat([]byte{0xc0}, 4096)...),
	}
	for name, raw := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeJobData(raw); err == nil {
				t.Fatal("invalid structured data accepted")
			}
		})
	}
	for name, value := range map[string]any{"nil": nil, "bool": true, "string": "中文", "array": []any{1, "value", false}, "map": map[string]any{"nested": []any{nil, "ok"}}, "safe_int": int64(1<<53 - 1), "negative": int64(-(1<<53 - 1)), "decimal": 1.25} {
		t.Run(name, func(t *testing.T) {
			decoded, err := decodeJobData(encode(value))
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(value)
			if !bytes.Equal(got, want) {
				t.Fatalf("JSON semantics changed: %s != %s", got, want)
			}
		})
	}
}
