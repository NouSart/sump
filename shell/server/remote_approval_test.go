package server

import (
	"github.com/QAQ-awa-QAQ/sump/protocol"
	"testing"
	"time"
)

func TestExecutionRequiresUnchangedUnexpiredApproval(t *testing.T) {
	newJob := func() *job {
		p := &protocol.ExecutionPlan{Version: 1, Command: "Get-Date", WorkDir: "work", TimeoutMS: 5000, MaxOutputBytes: 4096}
		return &job{command: p.Command, workDir: p.WorkDir, plan: p, approvedUntil: time.Now().Add(time.Second), approval: &protocol.CommandApprovalResult{Decision: protocol.DecisionAllow, PlanDigest: p.Digest()}}
	}
	if err := newJob().validateBeforeExecution(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*job){func(j *job) { j.command += "; whoami" }, func(j *job) { j.plan.TimeoutMS++ }, func(j *job) { j.workDir = "other" }, func(j *job) { j.approvedUntil = time.Now().Add(-time.Second) }, func(j *job) { j.approval.Decision = "review" }, func(j *job) { j.approval = nil }} {
		j := newJob()
		mutate(j)
		if err := j.validateBeforeExecution(); err == nil {
			t.Fatal("invalid approval accepted")
		}
	}
}
