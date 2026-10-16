package server

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/approval/analysis"
	"github.com/QAQ-awa-QAQ/sump/approval/engine"
	"github.com/QAQ-awa-QAQ/sump/approval/policy"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

type blockingParser struct {
	path    string
	entered chan struct{}
}

func (p blockingParser) RuntimePath() string { return p.path }
func (p blockingParser) Parse(ctx context.Context, _ string) (analysis.Facts, error) {
	p.entered <- struct{}{}
	<-ctx.Done()
	return analysis.Facts{}, ctx.Err()
}

func TestBoundedConcurrencyAndCancellation(t *testing.T) {
	parser := blockingParser{path: filepath.Join(t.TempDir(), "pwsh.exe"), entered: make(chan struct{}, 1)}
	e, err := engine.New(parser, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Engine: e, Concurrency: 1, PolicyJSON: policy.DefaultJSON, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	req := protocol.CommandApprovalRequest{Version: 1, ItemID: "bounded", Plan: protocol.ExecutionPlan{Version: 1, Command: "Get-Date", WorkDir: t.TempDir(), ShellPath: parser.path, Args: protocol.PowerShellArgs(), Dialect: "powershell", Platform: runtime.GOOS, Profile: protocol.PowerShellProfile, TimeoutMS: 5000, MaxOutputBytes: 4096}}
	raw, err := protocol.EncodePayload(req)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.approve(ctx, protocol.Envelope{}, protocol.JumpPayload{Input: raw}); done <- err }()
	select {
	case <-parser.entered:
	case <-time.After(time.Second):
		t.Fatal("parser not started")
	}
	if _, err := s.approve(context.Background(), protocol.Envelope{}, protocol.JumpPayload{Input: raw}); err == nil || !strings.Contains(err.Error(), "approval_busy") {
		t.Fatalf("busy admission: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel approved")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release job")
	}
	if len(s.slots) != 0 {
		t.Fatal("slot leaked")
	}
	if _, err := s.approve(context.Background(), protocol.Envelope{}, protocol.JumpPayload{Input: make([]byte, (64<<10)+1)}); err == nil {
		t.Fatal("oversized payload accepted")
	}
}
