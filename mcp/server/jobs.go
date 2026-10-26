package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/vmihailenco/msgpack/v5"
	"github.com/vmihailenco/msgpack/v5/msgpcode"
)

var (
	ErrJobNotFound    = errors.New("job not found or no longer observable")
	ErrJobsFull       = errors.New("job observation capacity exhausted")
	ErrJobClosed      = errors.New("job observation is closed")
	ErrJobResultLimit = errors.New("job result exceeds its observation limits")
)

// JobsConfig bounds retained observations, not the remote execution lifecycle.
// MaxBytes covers each retained snapshot plus its early reports and identities;
// MaxTotalBytes bounds their aggregate encoded charge, not the Go process heap.
type JobsConfig struct {
	MaxJobs, MaxBytes, MaxTotalBytes, MaxEarlyReports int
	TTL, Timeout                                      time.Duration
	Now                                               func() time.Time
}

// JobSnapshot separates local observation state from the service's own phase.
// An unknown observation is still allowed to receive a later confirmed result.
type JobSnapshot struct {
	JobID     string          `json:"job_id"`
	State     string          `json:"state"`
	Service   string          `json:"service"`
	Tool      string          `json:"tool"`
	ItemID    string          `json:"-"`
	Phase     string          `json:"phase,omitempty"`
	Text      string          `json:"text,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Final     bool            `json:"final"`
	Truncated bool            `json:"truncated,omitempty"`
	Problem   string          `json:"problem,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// JobTarget is internal routing data, never an externally authorized handle.
type JobTarget struct{ Service, Tool, Trace, ItemID string }

type jobEvent struct {
	ItemID    string          `json:"item_id"`
	Phase     string          `json:"phase"`
	Text      string          `json:"text,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Final     bool            `json:"final"`
	Truncated bool            `json:"truncated,omitempty"`
}

type jobEntry struct {
	snapshot     JobSnapshot
	owner, trace string
	early        []jobEvent
	charge       int
	onRelease    func()
}

// Jobs owns bounded, volatile observations. Callbacks run after unlocking and
// exactly once on a confirmed final result, expiry or Close. Timeout alone does
// not release admission: unconfirmed work must not escape the in-flight limit.
type Jobs struct {
	mu      sync.Mutex
	cfg     JobsConfig
	byID    map[string]*jobEntry
	byTrace map[string]*jobEntry
	bytes   int
	closed  bool
}

func NewJobs(cfg JobsConfig) *Jobs {
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = 128
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 32 << 10
	}
	if cfg.MaxTotalBytes <= 0 {
		cfg.MaxTotalBytes = 8 << 20
	}
	if cfg.MaxEarlyReports <= 0 {
		cfg.MaxEarlyReports = 8
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 15 * time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Jobs{cfg: cfg, byID: make(map[string]*jobEntry), byTrace: make(map[string]*jobEntry)}
}

func validJobIdentity(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value)
}

// Reserve must precede dispatch. A failed reservation does not take ownership
// of onRelease; the caller still owns and must release its admission slot.
func (j *Jobs) Reserve(owner, service, tool, trace string, onRelease func()) (string, error) {
	for _, value := range []string{owner, service, tool, trace} {
		if !validJobIdentity(value) {
			return "", errors.New("invalid job identity")
		}
	}
	j.mu.Lock()
	var releases []func()
	defer j.unlockAndRelease(&releases)
	now := j.cfg.Now()
	j.sweepLocked(now, &releases)
	if j.closed {
		return "", ErrJobClosed
	}
	if len(j.byID) >= j.cfg.MaxJobs {
		return "", ErrJobsFull
	}
	if j.byTrace[trace] != nil {
		return "", errors.New("job trace already reserved")
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", fmt.Errorf("create job ID: %w", err)
	}
	id := hex.EncodeToString(idBytes[:])
	if j.byID[id] != nil {
		return "", errors.New("job ID collision")
	}
	e := &jobEntry{owner: strings.Clone(owner), trace: strings.Clone(trace), onRelease: onRelease,
		snapshot: JobSnapshot{JobID: id, State: "reserved", Service: strings.Clone(service), Tool: strings.Clone(tool), CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(j.cfg.TTL)}}
	charge := jobCharge(e)
	if charge > j.cfg.MaxBytes || charge > j.cfg.MaxTotalBytes-j.bytes {
		return "", ErrJobsFull
	}
	e.charge = charge
	j.bytes += charge
	j.byID[id], j.byTrace[trace] = e, e
	return id, nil
}

func (j *Jobs) BindReceipt(id, itemID string) error {
	if !validJobIdentity(itemID) {
		return errors.New("invalid receipt item_id")
	}
	j.mu.Lock()
	var releases []func()
	defer j.unlockAndRelease(&releases)
	now := j.cfg.Now()
	j.sweepLocked(now, &releases)
	e := j.byID[id]
	if e == nil {
		return ErrJobNotFound
	}
	if e.snapshot.ItemID != "" && e.snapshot.ItemID != itemID {
		return errors.New("receipt item_id does not match the observed operation")
	}
	if e.snapshot.Final {
		return nil
	}
	next := *e
	next.snapshot.ItemID = strings.Clone(itemID)
	if next.snapshot.State != "unknown" {
		next.snapshot.State = "accepted"
	}
	next.snapshot.UpdatedAt = now
	next.early = nil
	for _, event := range e.early {
		if event.ItemID == itemID {
			applyJobEvent(&next, event, now)
			if event.Final {
				break
			}
		}
	}
	if !j.replaceLocked(e, &next) {
		return ErrJobResultLimit
	}
	if e.snapshot.Final {
		releaseJob(e, &releases)
	}
	return nil
}

// Report retains only reports for an already reserved source/trace pair.
// Before a receipt, bounded early reports wait for its exact item ID. If the
// receipt was lost, unknown observations may bind the first valid report using
// that unique trace. This relies on SUMP's trusted internal routing boundary.
func (j *Jobs) Report(env protocol.Envelope, p protocol.AsyncResultPayload) (bool, error) {
	j.mu.Lock()
	var releases []func()
	defer j.unlockAndRelease(&releases)
	now := j.cfg.Now()
	j.sweepLocked(now, &releases)
	e := j.byTrace[env.Trace]
	if e == nil || e.snapshot.Service != env.From || e.snapshot.Final {
		return false, nil
	}
	if e.snapshot.ItemID != "" && e.snapshot.ItemID != p.ItemID {
		return false, nil
	}
	event, err := encodeJobEvent(p, j.cfg.MaxBytes/2)
	if err != nil {
		return false, err
	}
	next := *e
	if e.snapshot.ItemID == "" && e.snapshot.State == "reserved" {
		for _, old := range e.early {
			if equalJobEvent(old, event) {
				return false, nil
			}
		}
		if len(e.early) >= j.cfg.MaxEarlyReports {
			return false, ErrJobResultLimit
		}
		next.early = append(append([]jobEvent(nil), e.early...), event)
		next.snapshot.UpdatedAt = now
	} else {
		if e.snapshot.ItemID == "" {
			next.snapshot.ItemID = event.ItemID
		}
		if eventMatchesSnapshot(event, e.snapshot) {
			return false, nil
		}
		applyJobEvent(&next, event, now)
	}
	if !j.replaceLocked(e, &next) {
		return false, ErrJobResultLimit
	}
	if e.snapshot.Final {
		releaseJob(e, &releases)
	}
	return true, nil
}

func (j *Jobs) MarkUnknown(id, reason string) {
	j.mu.Lock()
	var releases []func()
	defer j.unlockAndRelease(&releases)
	now := j.cfg.Now()
	j.sweepLocked(now, &releases)
	if e := j.byID[id]; e != nil {
		j.unknownLocked(e, reason, now, &releases)
	}
}

func (j *Jobs) Read(owner, id string) (JobSnapshot, error) {
	j.mu.Lock()
	var releases []func()
	defer j.unlockAndRelease(&releases)
	j.sweepLocked(j.cfg.Now(), &releases)
	e := j.byID[id]
	if e == nil || e.owner != owner {
		return JobSnapshot{}, ErrJobNotFound
	}
	snapshot := e.snapshot
	snapshot.Data = append(json.RawMessage(nil), snapshot.Data...)
	return snapshot, nil
}

func (j *Jobs) LookupCancel(owner, id string) (JobTarget, error) {
	j.mu.Lock()
	var releases []func()
	defer j.unlockAndRelease(&releases)
	j.sweepLocked(j.cfg.Now(), &releases)
	e := j.byID[id]
	if e == nil || e.owner != owner {
		return JobTarget{}, ErrJobNotFound
	}
	if e.snapshot.Final {
		return JobTarget{}, ErrJobClosed
	}
	if e.snapshot.ItemID == "" {
		return JobTarget{}, errors.New("backend item_id is not yet known")
	}
	return JobTarget{Service: e.snapshot.Service, Tool: e.snapshot.Tool, Trace: e.trace, ItemID: e.snapshot.ItemID}, nil
}

// ConfirmCanceled may be called only after the execution service confirms its
// own cancellation. A racing confirmed completion remains the first final fact.
func (j *Jobs) ConfirmCanceled(owner, id string) error {
	j.mu.Lock()
	var releases []func()
	defer j.unlockAndRelease(&releases)
	now := j.cfg.Now()
	j.sweepLocked(now, &releases)
	e := j.byID[id]
	if e == nil || e.owner != owner {
		return ErrJobNotFound
	}
	if e.snapshot.Final {
		return nil
	}
	if e.snapshot.ItemID == "" {
		return errors.New("cannot confirm cancellation without a backend item_id")
	}
	next := *e
	applyJobEvent(&next, jobEvent{ItemID: e.snapshot.ItemID, Phase: "canceled", Final: true}, now)
	if !j.replaceLocked(e, &next) {
		return ErrJobResultLimit
	}
	releaseJob(e, &releases)
	return nil
}

func (j *Jobs) Sweep() {
	j.mu.Lock()
	var releases []func()
	defer j.unlockAndRelease(&releases)
	j.sweepLocked(j.cfg.Now(), &releases)
}

// Close forgets observations and releases local capacity; it does not cancel
// the remote operations. Old handles cannot refer to new entries after restart.
func (j *Jobs) Close() {
	j.mu.Lock()
	var releases []func()
	defer j.unlockAndRelease(&releases)
	j.closed = true
	for _, e := range j.byID {
		releaseJob(e, &releases)
	}
	j.byID, j.byTrace, j.bytes = make(map[string]*jobEntry), make(map[string]*jobEntry), 0
}

func (j *Jobs) unlockAndRelease(releases *[]func()) {
	j.mu.Unlock()
	for _, f := range *releases {
		f()
	}
}

func releaseJob(e *jobEntry, releases *[]func()) {
	if e.onRelease != nil {
		*releases = append(*releases, e.onRelease)
		e.onRelease = nil
	}
}

func (j *Jobs) sweepLocked(now time.Time, releases *[]func()) {
	for id, e := range j.byID {
		if !now.Before(e.snapshot.ExpiresAt) {
			delete(j.byID, id)
			delete(j.byTrace, e.trace)
			j.bytes -= e.charge
			releaseJob(e, releases)
		} else if !e.snapshot.Final && e.snapshot.State != "unknown" && !now.Before(e.snapshot.CreatedAt.Add(j.cfg.Timeout)) {
			j.unknownLocked(e, "completion not confirmed within the local wait window; operation may still run", now, releases)
		}
	}
}

func (j *Jobs) unknownLocked(e *jobEntry, reason string, now time.Time, releases *[]func()) {
	if e.snapshot.Final {
		return
	}
	next := *e
	next.snapshot.State, next.snapshot.UpdatedAt = "unknown", now
	next.snapshot.Problem = jobTextPrefix(reason, 256)
	next.early = nil
	for _, event := range e.early {
		if next.snapshot.ItemID == "" {
			next.snapshot.ItemID = event.ItemID
		}
		if event.ItemID == next.snapshot.ItemID {
			applyJobEvent(&next, event, now)
			if event.Final {
				break
			}
		}
	}
	if !j.replaceLocked(e, &next) {
		// Keep existing bounded evidence rather than discarding it. A minimal
		// unknown marker never invents an execution failure.
		next = *e
		next.snapshot.State = "unknown"
		next.snapshot.Problem = ""
		next.snapshot.UpdatedAt = now
		next.early = nil
		j.replaceLocked(e, &next)
	}
	if e.snapshot.Final {
		releaseJob(e, releases)
	}
}

func applyJobEvent(e *jobEntry, event jobEvent, now time.Time) {
	e.snapshot.ItemID, e.snapshot.Phase = event.ItemID, event.Phase
	e.snapshot.Text, e.snapshot.Data = event.Text, event.Data
	e.snapshot.Final, e.snapshot.Truncated = event.Final, event.Truncated
	e.snapshot.UpdatedAt = now
	if event.Final {
		e.snapshot.State, e.snapshot.Problem = "final", ""
	}
}

func (j *Jobs) replaceLocked(e, next *jobEntry) bool {
	charge := jobCharge(next)
	if charge > j.cfg.MaxBytes || charge-e.charge > j.cfg.MaxTotalBytes-j.bytes {
		return false
	}
	j.bytes += charge - e.charge
	next.charge = charge
	*e = *next
	return true
}

func jobCharge(e *jobEntry) int {
	raw, _ := json.Marshal(e.snapshot)
	charge := len(raw) + len(e.owner) + len(e.trace) + len(e.snapshot.ItemID) + 128
	for _, event := range e.early {
		raw, _ := json.Marshal(event)
		charge += len(raw) + 64
	}
	return charge
}

func equalJobEvent(a, b jobEvent) bool {
	return a.ItemID == b.ItemID && a.Phase == b.Phase && a.Text == b.Text && a.Final == b.Final && a.Truncated == b.Truncated && bytes.Equal(a.Data, b.Data)
}
func eventMatchesSnapshot(e jobEvent, s JobSnapshot) bool {
	return e.ItemID == s.ItemID && e.Phase == s.Phase && e.Text == s.Text && e.Final == s.Final && e.Truncated == s.Truncated && bytes.Equal(e.Data, s.Data)
}

func encodeJobEvent(p protocol.AsyncResultPayload, limit int) (jobEvent, error) {
	if !validJobIdentity(p.ItemID) || !validJobIdentity(p.Phase) {
		return jobEvent{}, errors.New("invalid report identity or phase")
	}
	e := jobEvent{ItemID: strings.Clone(p.ItemID), Phase: strings.Clone(p.Phase), Final: p.Final}
	if len(p.Data) > limit {
		return jobEvent{}, ErrJobResultLimit
	}
	if len(p.Data) > 0 {
		data, err := decodeJobData(p.Data)
		if err != nil {
			return jobEvent{}, err
		}
		e.Data, err = json.Marshal(data)
		if err != nil {
			return jobEvent{}, err
		}
	}
	base, _ := json.Marshal(e)
	if len(base) > limit {
		return jobEvent{}, ErrJobResultLimit
	}
	text := jobTextPrefix(p.Text, limit)
	e.Truncated = text != p.Text
	e.Text = text
	raw, _ := json.Marshal(e)
	if len(raw) <= limit {
		return e, nil
	}
	e.Truncated = true
	e.Text = ""
	base, _ = json.Marshal(e)
	if len(base) > limit {
		return jobEvent{}, ErrJobResultLimit
	}
	lo, hi := 0, len(text)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		e.Text = jobTextPrefix(text, mid)
		raw, _ = json.Marshal(e)
		if len(raw) <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	e.Text = strings.Clone(jobTextPrefix(text, lo))
	return e, nil
}

func jobTextPrefix(text string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(text) > n {
		text = text[:n]
	}
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 1 {
			return strings.Clone(text[:i])
		}
		i += size
	}
	return strings.Clone(text)
}

// Decode MessagePack containers with explicit size/depth limits before allocating.
// Structured reports must be JSON-shaped: extensions, binary values, nonstring
// keys and unsafe integers are rejected instead of silently changing meaning.
func decodeJobData(raw []byte) (any, error) {
	r := bytes.NewReader(raw)
	d := msgpack.NewDecoder(r)
	budget := 4096
	v, err := decodeJobValue(d, r, 0, &budget)
	if err != nil {
		return nil, err
	}
	if r.Len() != 0 {
		return nil, errors.New("report data contains trailing MessagePack values")
	}
	return v, nil
}

func decodeJobValue(d *msgpack.Decoder, r *bytes.Reader, depth int, budget *int) (any, error) {
	if depth > 32 || *budget <= 0 {
		return nil, ErrJobResultLimit
	}
	*budget--
	c, err := d.PeekCode()
	if err != nil {
		return nil, errors.New("invalid report MessagePack data")
	}
	if msgpcode.IsFixedArray(c) || c == msgpcode.Array16 || c == msgpcode.Array32 {
		n, err := d.DecodeArrayLen()
		if err != nil || n < 0 || n > *budget {
			return nil, ErrJobResultLimit
		}
		out := make([]any, n)
		for i := range out {
			out[i], err = decodeJobValue(d, r, depth+1, budget)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	if msgpcode.IsFixedMap(c) || c == msgpcode.Map16 || c == msgpcode.Map32 {
		n, err := d.DecodeMapLen()
		if err != nil || n < 0 || n > *budget/2 {
			return nil, ErrJobResultLimit
		}
		out := make(map[string]any, n)
		for i := 0; i < n; i++ {
			code, err := d.PeekCode()
			if err != nil || !msgpcode.IsString(code) {
				return nil, errors.New("report data key must be a string")
			}
			key, err := decodeJobString(d, r)
			if err != nil {
				return nil, err
			}
			if _, ok := out[key]; ok {
				return nil, errors.New("report data contains duplicate keys")
			}
			*budget--
			out[key], err = decodeJobValue(d, r, depth+1, budget)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	if msgpcode.IsString(c) {
		return decodeJobString(d, r)
	}
	if msgpcode.IsExt(c) || c == msgpcode.Bin8 || c == msgpcode.Bin16 || c == msgpcode.Bin32 {
		return nil, errors.New("report data has no declared binary or extension mapping")
	}
	v, err := d.DecodeInterface()
	if err != nil {
		return nil, errors.New("invalid report MessagePack scalar")
	}
	const maxExact = 1<<53 - 1
	switch n := v.(type) {
	case int64:
		if n > maxExact || n < -maxExact {
			return nil, errors.New("report integer exceeds exact JSON range")
		}
	case uint64:
		if n > maxExact {
			return nil, errors.New("report integer exceeds exact JSON range")
		}
	case float32:
		f := float64(n)
		if math.IsInf(f, 0) || math.IsNaN(f) || (math.Trunc(f) == f && math.Abs(f) > maxExact) {
			return nil, errors.New("report number is not safely representable")
		}
	case float64:
		if math.IsInf(n, 0) || math.IsNaN(n) || (math.Trunc(n) == n && math.Abs(n) > maxExact) {
			return nil, errors.New("report number is not safely representable")
		}
	}
	return v, nil
}

func decodeJobString(d *msgpack.Decoder, r *bytes.Reader) (string, error) {
	n, err := d.DecodeBytesLen()
	if err != nil || n < 0 || n > r.Len() {
		return "", errors.New("invalid report string length")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil || !utf8.Valid(buf) {
		return "", errors.New("report data string is not valid UTF-8")
	}
	return string(buf), nil
}
