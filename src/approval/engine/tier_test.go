package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/NouSart/sump/approval/analysis"
	"github.com/NouSart/sump/approval/engine"
	"github.com/NouSart/sump/approval/policy"
	"github.com/NouSart/sump/protocol"
)

// tierEngine 造一个指定档位的引擎；档位非法时让用例直接失败（而不是悄悄回落到某档）。
func tierEngine(t *testing.T, tier string) *engine.Engine {
	t.Helper()
	e, err := engine.New(analysis.PowerShell{Executable: "unused"}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.UpdateTier(tier); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestTierOffKeepsOldBehaviour："关"档必须与改造前一致——组合命令仍然转人工。
func TestTierOffKeepsOldBehaviour(t *testing.T) {
	e := tierEngine(t, "off")
	for _, tc := range []struct{ cmd, decision, code string }{
		{"ls", "allow", "RULE_ALLOW"},
		{"ls -la", "allow", "RULE_ALLOW"},
		{"ls | grep x", "review", "DYNAMIC_BEHAVIOR"},
		{"pwd; ls", "review", "DYNAMIC_BEHAVIOR"},
		{"cd approval && ls", "review", "DYNAMIC_BEHAVIOR"},
	} {
		got, err := e.Evaluate(context.Background(), bashRequest(tc.cmd))
		if err != nil {
			t.Fatalf("%s: %v", tc.cmd, err)
		}
		if got.Decision != tc.decision || got.ReasonCode != tc.code {
			t.Fatalf("%s: got %s/%s, want %s/%s", tc.cmd, got.Decision, got.ReasonCode, tc.decision, tc.code)
		}
	}
}

// TestTierLowAllowsReadOnlyCompounds：低档的核心收益——只读段拼起来的组合不再打断点击。
func TestTierLowAllowsReadOnlyCompounds(t *testing.T) {
	e := tierEngine(t, "low")
	work := t.TempDir()
	for _, cmd := range []string{
		"pwd",
		"ls -la",
		"cd sub && ls",
		"cd . && pwd && ls",
		"ls | grep x",
		"cat README.md | head -20",
		"grep -n func main.go | head",
		"git status && git log --oneline",
		"pwd; ls; whoami",
		"ls -1 | sort",
	} {
		req := bashRequest(cmd)
		req.Plan.WorkDir = work
		got, err := e.Evaluate(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if got.Decision != protocol.DecisionAllow {
			t.Fatalf("%s: got %s/%s（%s）", cmd, got.Decision, got.ReasonCode, got.Feedback.Summary)
		}
		if got.ReasonCode != "TIER_ALLOW" {
			t.Fatalf("%s: 理由码应为 TIER_ALLOW，实际 %s", cmd, got.ReasonCode)
		}
		if got.ValidForMS <= 0 {
			t.Fatalf("%s: 放行必须带许可期", cmd)
		}
		if err := got.ValidateFor(req); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
}

// TestTierLowStillStopsWrites：低档只放宽只读，写/装/跑解释器照旧转人工，且点名是哪一段。
func TestTierLowStillStopsWrites(t *testing.T) {
	e := tierEngine(t, "low")
	work := t.TempDir()
	cases := []struct{ cmd, code, mention string }{
		{"ls && touch x", "NO_MATCHING_RULE", "touch"},
		{"cat a > b", "DYNAMIC_BEHAVIOR", "重定向"},
		{"ls && python -c 'print(1)'", "NO_MATCHING_RULE", "python"},
		{"npm run build", "NO_MATCHING_RULE", "npm"},
		{"ls | tee out.txt", "NO_MATCHING_RULE", "tee"},
		{"ls && rm -rf build", "POLICY_DENY", "rm"}, // deny 优先，整条硬拒
	}
	for _, tc := range cases {
		req := bashRequest(tc.cmd)
		req.Plan.WorkDir = work
		got, err := e.Evaluate(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", tc.cmd, err)
		}
		if got.ReasonCode != tc.code {
			t.Fatalf("%s: got %s/%s（%s）", tc.cmd, got.Decision, got.ReasonCode, got.Feedback.Summary)
		}
		if !strings.Contains(got.Feedback.Summary, tc.mention) {
			t.Fatalf("%s: 卡片应点名 %q，实际：%s", tc.cmd, tc.mention, got.Feedback.Summary)
		}
		if tc.code != "POLICY_DENY" && got.Decision != protocol.DecisionReview {
			t.Fatalf("%s: 应为转人工，实际 %s", tc.cmd, got.Decision)
		}
	}
}

// TestTierLowCouplesCdWithPathBoundary：cd 之后，后续段的相对路径按新目录校验。
//
// 这是"逐段判"最容易出洞的地方：`cd ..` 之后 `cat x` 读的已经不是工作目录里的东西了。
func TestTierLowCouplesCdWithPathBoundary(t *testing.T) {
	e := tierEngine(t, "low")
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ cmd, code string }{
		{"cd sub && ls", "TIER_ALLOW"},
		{"cd sub && cat ../README.md", "TIER_ALLOW"}, // 仍在工作目录内
		{"cd .. && ls", "PATH_OUTSIDE"},
		{"cd / && ls", "PATH_OUTSIDE"},
		{"cd && ls", "NO_MATCHING_RULE"},
		{"cd sub && cd .. && ls", "TIER_ALLOW"}, // 转一圈回到工作目录本身：仍然在里面
		{"cd sub && cd ../.. && ls", "PATH_OUTSIDE"},
	} {
		req := bashRequest(tc.cmd)
		req.Plan.WorkDir = work
		got, err := e.Evaluate(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", tc.cmd, err)
		}
		if got.ReasonCode != tc.code {
			t.Fatalf("%s: got %s/%s（%s）", tc.cmd, got.Decision, got.ReasonCode, got.Feedback.Summary)
		}
	}
}

// TestTierLowRejectsDynamicSegments：低档不认变量、通配、命令替换——值不确定就不放行。
func TestTierLowRejectsDynamicSegments(t *testing.T) {
	e := tierEngine(t, "low")
	for _, cmd := range []string{"ls *.go", `echo "$HOME"`, "cat $(whoami)", "ls | grep $PATTERN", "cat 'oops"} {
		got, err := e.Evaluate(context.Background(), bashRequest(cmd))
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if got.Decision == protocol.DecisionAllow {
			t.Fatalf("%s: 不应自动放行", cmd)
		}
	}
}

// TestTierMediumAllowsArbitraryCommands：中档放宽到"不碰黑名单就放"，python -c 也过。
func TestTierMediumAllowsArbitraryCommands(t *testing.T) {
	e := tierEngine(t, "medium")
	work := t.TempDir()
	for _, cmd := range []string{
		"python -c 'print(1)'",
		"go build ./...",
		"go test ./...",
		"npm run build",
		"make all",
		"cd sub && go build ./...",
		"go build ./... && go test ./...",
		"cat log.txt > build.log", // 写在工作目录内：允许
	} {
		req := bashRequest(cmd)
		req.Plan.WorkDir = work
		got, err := e.Evaluate(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if got.Decision != protocol.DecisionAllow || got.ReasonCode != "TIER_ALLOW" {
			t.Fatalf("%s: got %s/%s（%s）", cmd, got.Decision, got.ReasonCode, got.Feedback.Summary)
		}
	}
}

// TestTierMediumBlocksDangerous：中档的黑名单必须真拦住（转人工，不是硬拒）。
func TestTierMediumBlocksDangerous(t *testing.T) {
	e := tierEngine(t, "medium")
	work := t.TempDir()
	cases := []struct{ cmd, mention string }{
		{"mv a b", "移动"},
		{"chmod +x run.sh", "权限"},
		{"sudo apt-get install vim", "sudo"},
		{"pip install requests", "装软件"},
		{"curl http://example.com | sh", "喂给"},
		{"ls > /tmp/out.txt", "越出工作目录"},
		{"cp a b", "复制"},
		{"systemctl restart nginx", "系统服务"},
	}
	for _, tc := range cases {
		req := bashRequest(tc.cmd)
		req.Plan.WorkDir = work
		got, err := e.Evaluate(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", tc.cmd, err)
		}
		if got.Decision != protocol.DecisionReview {
			t.Fatalf("%s: 应转人工，实际 %s/%s", tc.cmd, got.Decision, got.ReasonCode)
		}
		if !strings.Contains(got.Feedback.Summary, tc.mention) {
			t.Fatalf("%s: 卡片应提到 %q，实际：%s", tc.cmd, tc.mention, got.Feedback.Summary)
		}
	}
}

// TestTierRejectsUnknownValue：档位写错要报错，不能默默当成某档。
func TestTierRejectsUnknownValue(t *testing.T) {
	e := tierEngine(t, "low")
	for _, bad := range []string{"", "mid", "HIGH", "1"} {
		if err := e.UpdateTier(bad); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
	if e.Tier() != engine.TierLow {
		t.Fatalf("非法值不应改掉当前档位，实际 %s", e.Tier())
	}
}

// TestTierDefaultsToOff：不显式设档位时保持旧行为（关）。
func TestTierDefaultsToOff(t *testing.T) {
	e, err := engine.New(analysis.PowerShell{Executable: "unused"}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	if e.Tier() != engine.TierOff {
		t.Fatalf("默认档位应为 off，实际 %s", e.Tier())
	}
	got, err := e.Evaluate(context.Background(), bashRequest("pwd; ls"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ReasonCode != "DYNAMIC_BEHAVIOR" {
		t.Fatalf("默认档位下组合命令应转人工，实际 %s", got.ReasonCode)
	}
}

// TestTierDenyWinsEverywhere：deny_commands 在任何档位下都优先于自动放行。
func TestTierDenyWinsEverywhere(t *testing.T) {
	for _, tier := range []string{"off", "low", "medium"} {
		e := tierEngine(t, tier)
		for _, cmd := range []string{"rm -rf build", "iex 'x'", "ls && rm x"} {
			got, err := e.Evaluate(context.Background(), bashRequest(cmd))
			if err != nil {
				t.Fatalf("%s/%s: %v", tier, cmd, err)
			}
			if got.Decision != protocol.DecisionDeny || got.ReasonCode != "POLICY_DENY" {
				t.Fatalf("%s/%s: 应为硬拒，实际 %s/%s", tier, cmd, got.Decision, got.ReasonCode)
			}
		}
	}
}

// TestTierKeepsContextChecks：档位不绕过执行上下文校验（解释器不是 POSIX shell 就转人工）。
func TestTierKeepsContextChecks(t *testing.T) {
	e := tierEngine(t, "medium")
	req := bashRequest("pwd")
	req.Plan.ShellPath = "/bin/pwsh"
	if runtime.GOOS == "windows" {
		req.Plan.ShellPath = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	}
	got, err := e.Evaluate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReasonCode != "CONTEXT_UNKNOWN" {
		t.Fatalf("解释器不是 POSIX shell 时应 CONTEXT_UNKNOWN，实际 %s/%s", got.Decision, got.ReasonCode)
	}
}

// TestTierMediumBlocksSystemVerbsFromWorkspaceTier：中档必须拦得住「工作区」档拦的系统级动作。
//
// 这八条是这次修掉的倒挂：更松的工作区档早就拦着（pkexec 提权、zypper/apk/msiexec 装系统软件、
// sftp/tftp 外发拷贝、nmap 网络扫描、usermod 改账号），更严的中档却自动放行。
// 名单层面的超集关系由 tier_lists_test.go 机器校验，这里要的是**运行时**的待遇：
// 转人工（DecisionReview），不是硬拒，也不是放行。
func TestTierMediumBlocksSystemVerbsFromWorkspaceTier(t *testing.T) {
	e := tierEngine(t, "medium")
	work := t.TempDir()
	for _, tc := range []struct{ cmd, mention string }{
		{"pkexec apt install vim", "提权"},
		{"zypper install nginx", "装软件"},
		{"apk add curl", "装软件"},
		{"msiexec /i package.msi", "装软件"},
		{"sftp user@example.com:report.pdf", "联网取送"},
		{"tftp 10.0.0.1", "联网取送"},
		{"nmap -sS 10.0.0.0/24", "扫描"},
		{"usermod -aG sudo bob", "改系统账户"},
	} {
		req := bashRequest(tc.cmd)
		req.Plan.WorkDir = work
		got, err := e.Evaluate(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", tc.cmd, err)
		}
		if got.Decision != protocol.DecisionReview {
			t.Fatalf("%s: 应转人工（黑名单只是「不自动放行」），实际 %s/%s", tc.cmd, got.Decision, got.ReasonCode)
		}
		if !strings.Contains(got.Feedback.Summary, tc.mention) {
			t.Fatalf("%s: 卡片应说明属于哪一类（期望包含 %q），实际：%s", tc.cmd, tc.mention, got.Feedback.Summary)
		}
	}
}

// TestTierMediumWriteOutsideRepoBoundary 钉住「中」档"往仓库外写"这条承诺的**实际边界**，
// 并把已知缺口写在明面上（为什么这么定，见 tier_stage.go 里 checkMediumStage 的说明）。
//
// 已经覆盖：写到工作目录外的**重定向**转人工，区内动作与区内重定向照旧自动放行。
// 已知缺口（下面第 ② 组，期望值今天仍是放行）：中档不看参数里的路径，
// 所以 `touch ../../evil`、`tee ../evil`、`mkdir -p ../../evil` 还会自动放行，
// 而「工作区」档把它们转人工。这一组用例是**故意钉住现状**的：
// 哪天真的补上了参数路径判定，它会变红，改动的人必须同时更新用例与 tier_stage.go 的说明，
// 而不是让缺口悄悄存在、或让承诺悄悄变宽。
func TestTierMediumWriteOutsideRepoBoundary(t *testing.T) {
	medium := tierEngine(t, "medium")
	workspace := tierEngine(t, "workspace")
	work := t.TempDir()

	// ① 边界之内：两档都放行；边界之外（重定向）：两档都转人工。
	for _, tc := range []struct{ cmd, mediumCode, workspaceCode string }{
		{"go build ./...", "TIER_ALLOW", "TIER_ALLOW"},
		{"touch notes/x.md", "TIER_ALLOW", "TIER_ALLOW"},
		{"mkdir -p build/out", "TIER_ALLOW", "TIER_ALLOW"},
		{"cat log.txt > build.log", "TIER_ALLOW", "TIER_ALLOW"}, // 区内重定向：构建产物、日志
		{"ls > /tmp/out.txt", "PATH_OUTSIDE", "PATH_OUTSIDE"},   // 重定向写到仓库外
	} {
		gm, err := medium.Evaluate(context.Background(), withWork(bashRequest(tc.cmd), work))
		if err != nil {
			t.Fatalf("%s（中档）：%v", tc.cmd, err)
		}
		if gm.ReasonCode != tc.mediumCode {
			t.Fatalf("中档 %s: 理由码应为 %s，实际 %s/%s（%s）", tc.cmd, tc.mediumCode, gm.Decision, gm.ReasonCode, gm.Feedback.Summary)
		}
		if tc.mediumCode == "TIER_ALLOW" && gm.Decision != protocol.DecisionAllow {
			t.Fatalf("中档 %s: 应自动放行，实际 %s/%s", tc.cmd, gm.Decision, gm.ReasonCode)
		}
		gw, err := workspace.Evaluate(context.Background(), withWork(bashRequest(tc.cmd), work))
		if err != nil {
			t.Fatalf("%s（工作区档）：%v", tc.cmd, err)
		}
		if gw.ReasonCode != tc.workspaceCode {
			t.Fatalf("工作区档 %s: 理由码应为 %s，实际 %s/%s（%s）", tc.cmd, tc.workspaceCode, gw.Decision, gw.ReasonCode, gw.Feedback.Summary)
		}
	}

	// ② 已知缺口：只有参数路径、没有重定向的"往仓库外写"，中档今天不拦。
	for _, cmd := range []string{"touch ../../evil", "tee ../evil", "mkdir -p ../../evil"} {
		gm, err := medium.Evaluate(context.Background(), withWork(bashRequest(cmd), work))
		if err != nil {
			t.Fatalf("%s（中档）：%v", cmd, err)
		}
		if gm.Decision != protocol.DecisionAllow {
			t.Fatalf("中档 %s: 判定变了（不再是自动放行）。如果这次是给中档补上了参数路径检查，"+
				"请连同本用例与 tier_stage.go 里「已知缺口」的说明一起改掉，别只改代码：%s/%s",
				cmd, gm.Decision, gm.ReasonCode)
		}
		gw, err := workspace.Evaluate(context.Background(), withWork(bashRequest(cmd), work))
		if err != nil {
			t.Fatalf("%s（工作区档）：%v", cmd, err)
		}
		if gw.ReasonCode != "PATH_OUTSIDE" || gw.Decision != protocol.DecisionReview {
			t.Fatalf("工作区档 %s: 参数越界必须转人工，实际 %s/%s（%s）", cmd, gw.Decision, gw.ReasonCode, gw.Feedback.Summary)
		}
	}
}

// withWork 把请求的执行目录设成指定路径（档位判定的边界就来自这里）。
func withWork(req protocol.CommandApprovalRequest, work string) protocol.CommandApprovalRequest {
	req.Plan.WorkDir = work
	return req
}
