package protocol

import "testing"

func TestApprovalBindsEveryExecutionField(t *testing.T) {
	p := ExecutionPlan{Version: 1, Command: "Get-Date", WorkDir: "C:/work", ShellPath: "C:/pwsh.exe", Args: PowerShellArgs(), Dialect: "powershell", Platform: "windows", Profile: PowerShellProfile, TimeoutMS: 5000, MaxOutputBytes: 4096}
	mutations := []func(*ExecutionPlan){
		func(p *ExecutionPlan) { p.Version++ }, func(p *ExecutionPlan) { p.Command += ";whoami" }, func(p *ExecutionPlan) { p.WorkDir += "/other" },
		func(p *ExecutionPlan) { p.ShellPath += "x" }, func(p *ExecutionPlan) { p.Args = []string{"-Command"} }, func(p *ExecutionPlan) { p.Dialect = "bash" },
		func(p *ExecutionPlan) { p.Platform = "linux" }, func(p *ExecutionPlan) { p.Profile += "x" }, func(p *ExecutionPlan) { p.TimeoutMS++ }, func(p *ExecutionPlan) { p.MaxOutputBytes++ },
	}
	req := CommandApprovalRequest{Version: 1, ItemID: "a", Plan: p}
	good := CommandApprovalResult{Version: 1, ItemID: "a", DecisionID: "d", Decision: DecisionAllow, PlanDigest: p.Digest(), PolicyVersion: "p", ReasonCode: "RULE_ALLOW", ValidForMS: 30000, Feedback: ModelFeedback{Summary: "allowed"}}
	if err := good.ValidateFor(req); err != nil {
		t.Fatal(err)
	}
	for i, mutate := range mutations {
		other := p
		mutate(&other)
		r := req
		r.Plan = other
		if err := good.ValidateFor(r); err == nil {
			t.Fatalf("mutation %d accepted", i)
		}
	}
	for _, mutate := range []func(*CommandApprovalResult){func(r *CommandApprovalResult) { r.ItemID = "b" }, func(r *CommandApprovalResult) { r.Decision = "maybe" }, func(r *CommandApprovalResult) { r.ValidForMS = 0 }, func(r *CommandApprovalResult) { r.ValidForMS = 30001 }, func(r *CommandApprovalResult) { r.Feedback.Summary = "" }} {
		r := good
		mutate(&r)
		if err := r.ValidateFor(req); err == nil {
			t.Fatalf("invalid result accepted: %+v", r)
		}
	}
}
