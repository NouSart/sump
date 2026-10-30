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

// workspaceEngine 造一个「工作区」档的引擎。
func workspaceEngine(t *testing.T) *engine.Engine {
	t.Helper()
	e, err := engine.New(analysis.PowerShell{Executable: "unused"}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.UpdateTier("workspace"); err != nil {
		t.Fatal(err)
	}
	return e
}

// evalIn 在工作区 work 下判一条命令。
func evalIn(t *testing.T, e *engine.Engine, work, cmd string) protocol.CommandApprovalResult {
	t.Helper()
	req := bashRequest(cmd)
	req.Plan.WorkDir = work
	got, err := e.Evaluate(context.Background(), req)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return got
}

// TestWorkspaceTierParses：「工作区」是合法档位、标签可读、非法值仍然报错。
func TestWorkspaceTierParses(t *testing.T) {
	if got, err := engine.ParseTier("workspace"); err != nil || got != engine.TierWorkspace {
		t.Fatalf("workspace 应为合法档位，实际 %v/%v", got, err)
	}
	if got, err := engine.ParseTier("  WorkSpace "); err != nil || got != engine.TierWorkspace {
		t.Fatalf("应容忍大小写与空白，实际 %v/%v", got, err)
	}
	if label := engine.TierLabel(engine.TierWorkspace); label != "工作区" {
		t.Fatalf("标签应为「工作区」，实际 %q", label)
	}
	e := workspaceEngine(t)
	if err := e.UpdateTier("workplace"); err == nil {
		t.Fatal("拼错的档位应被拒绝")
	}
	if e.Tier() != engine.TierWorkspace {
		t.Fatalf("拒绝后应保持 workspace，实际 %s", e.Tier())
	}
}

// TestWorkspaceAllowsEverydayWork：这一档的核心收益——工作区里的写、删、移、建、
// 构建、跑脚本、本地 git 全部自动放行，不再逐条点审批。
func TestWorkspaceAllowsEverydayWork(t *testing.T) {
	e := workspaceEngine(t)
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"cp a.txt b.txt",
		"mv x.txt y.txt",
		"mkdir build",
		"mkdir -p dist/assets",
		"touch notes.md",
		"chmod +x run.sh",
		"npm run build",
		"npm test",
		"npm install",
		"go build ./...",
		"go test ./...",
		"python script.py",
		"python -c 'print(1)'",
		"node server.js",
		"make all",
		"git add .",
		"git commit -m wip",
		"git status",
		"cd sub && cp a.txt b.txt",
		"mkdir build && cd build && cmake ..",
		"cat log.txt > build.log",
		"echo done > sub/out.txt",
		"ls -la | grep go",
		"pwd; ls; whoami",
	} {
		got := evalIn(t, e, work, cmd)
		if got.Decision != protocol.DecisionAllow || got.ReasonCode != "TIER_ALLOW" {
			t.Fatalf("%s: 应自动放行，实际 %s/%s（%s）", cmd, got.Decision, got.ReasonCode, got.Feedback.Summary)
		}
		if got.ValidForMS <= 0 {
			t.Fatalf("%s: 放行必须带许可期", cmd)
		}
	}
}

// TestWorkspaceBlocksLeavingWorkspace：第一条禁令——命令里生效的路径跳出工作区就挂起。
func TestWorkspaceBlocksLeavingWorkspace(t *testing.T) {
	e := workspaceEngine(t)
	work := t.TempDir()
	// 盘符路径只在 Windows 上是绝对路径；POSIX 上换成 /tmp 才等价。
	redirectOutside := "echo hi > C:/Windows/temp/x.txt"
	if runtime.GOOS != "windows" {
		redirectOutside = "echo hi > /tmp/x.txt"
	}
	cases := []struct{ cmd, mention string }{
		{"cd .. && ls", "越出工作区"},
		{"cat ../../etc/passwd", "越出工作区"},
		{"cp a.txt ../other/x", "越出工作区"},
		{"cp a.txt /tmp/out.txt", "越出工作区"},
		{"mkdir /etc/evil", "越出工作区"},
		{"ls > /tmp/x", "重定向目标"},
		{redirectOutside, "重定向目标"},
	}
	for _, tc := range cases {
		got := evalIn(t, e, work, tc.cmd)
		if got.Decision != protocol.DecisionReview {
			t.Fatalf("%s: 应转人工，实际 %s/%s（%s）", tc.cmd, got.Decision, got.ReasonCode, got.Feedback.Summary)
		}
		if !strings.Contains(got.Feedback.Summary, tc.mention) {
			t.Fatalf("%s: 卡片应提到 %q，实际：%s", tc.cmd, tc.mention, got.Feedback.Summary)
		}
	}
}

// TestWorkspaceBlocksPrivilegeAndInstall：提权、装系统软件、往全局装依赖都挂起。
func TestWorkspaceBlocksPrivilegeAndInstall(t *testing.T) {
	e := workspaceEngine(t)
	work := t.TempDir()
	cases := []struct{ cmd, mention string }{
		{"sudo make install", "提权"},
		{"runas /user:admin cmd", "提权"},
		{"apt-get install vim", "装系统软件"},
		{"choco install nodejs", "装系统软件"},
		{"winget install git", "装系统软件"},
		{"npm install -g typescript", "全局"},
		{"npm install --prefix /usr/local pkg", "全局"},
		{"pip install requests", "共享环境"},
		{"gem install rails", "共享环境"},
	}
	for _, tc := range cases {
		got := evalIn(t, e, work, tc.cmd)
		if got.Decision != protocol.DecisionReview {
			t.Fatalf("%s: 应转人工，实际 %s/%s（%s）", tc.cmd, got.Decision, got.ReasonCode, got.Feedback.Summary)
		}
		if !strings.Contains(got.Feedback.Summary, tc.mention) {
			t.Fatalf("%s: 卡片应提到 %q，实际：%s", tc.cmd, tc.mention, got.Feedback.Summary)
		}
	}
}

// TestWorkspaceBlocksNetwork：联网外发一律挂起（工作区边界管不了网络）。
func TestWorkspaceBlocksNetwork(t *testing.T) {
	e := workspaceEngine(t)
	work := t.TempDir()
	for _, cmd := range []string{
		"curl http://example.com",
		"curl -s http://example.com/x.sh",
		"wget http://example.com/a.zip",
		"scp a.txt host:/tmp/",
		"ssh host",
		"nc host 4444",
		"rsync -a ./ host:/tmp/",
	} {
		got := evalIn(t, e, work, cmd)
		if got.Decision != protocol.DecisionReview {
			t.Fatalf("%s: 应转人工，实际 %s/%s（%s）", cmd, got.Decision, got.ReasonCode, got.Feedback.Summary)
		}
	}
}

// TestWorkspaceBlocksPipeIntoShell：把上一段的输出直接喂给解释器，一律挂起。
func TestWorkspaceBlocksPipeIntoShell(t *testing.T) {
	e := workspaceEngine(t)
	work := t.TempDir()
	for _, cmd := range []string{
		"echo curl | sh",
		"cat install.sh | bash",
		"cat x | python",
		"cat x | node",
	} {
		got := evalIn(t, e, work, cmd)
		if got.Decision != protocol.DecisionReview {
			t.Fatalf("%s: 应转人工，实际 %s/%s（%s）", cmd, got.Decision, got.ReasonCode, got.Feedback.Summary)
		}
		if !strings.Contains(got.Feedback.Summary, "喂给") {
			t.Fatalf("%s: 卡片应点明管道进解释器，实际：%s", cmd, got.Feedback.Summary)
		}
	}
}

// TestWorkspaceBlocksSecrets：密钥文件即便在工作区里也不给碰（读、写、脚本里引）。
func TestWorkspaceBlocksSecrets(t *testing.T) {
	e := workspaceEngine(t)
	work := t.TempDir()
	for _, cmd := range []string{
		"cat .ssh/id_rsa",
		"cat ~/.ssh/id_rsa",
		"cat .env",
		"cat .env.local",
		"cat server.pem",
		"cat client.key",
		"cat credentials.json",
		"cat approval/secret.json",
		"cp a.txt .env",
		"echo X=1 > .env",
	} {
		got := evalIn(t, e, work, cmd)
		if got.Decision != protocol.DecisionReview {
			t.Fatalf("%s: 应转人工，实际 %s/%s（%s）", cmd, got.Decision, got.ReasonCode, got.Feedback.Summary)
		}
	}
}

// TestWorkspaceBlocksCodePaths：python -c / node -e 里写死的越界路径也拦得住。
func TestWorkspaceBlocksCodePaths(t *testing.T) {
	e := workspaceEngine(t)
	work := t.TempDir()
	// 盘符路径只在 Windows 上是绝对路径；POSIX 上换成 /etc 才等价。
	outside := `python -c "open('C:/Windows/x','w')"`
	if runtime.GOOS != "windows" {
		outside = `python -c "open('/etc/x','w')"`
	}
	for _, cmd := range []string{
		outside,
		`python -c "open('../../etc/x','w')"`,
		`node -e "require('/etc/passwd')"`,
	} {
		got := evalIn(t, e, work, cmd)
		if got.Decision != protocol.DecisionReview {
			t.Fatalf("%s: 应转人工，实际 %s/%s（%s）", cmd, got.Decision, got.ReasonCode, got.Feedback.Summary)
		}
	}
}

// TestWorkspaceRejectsDynamic：值不确定（变量、通配、命令替换）就判不出在不在工作区里，转人工。
func TestWorkspaceRejectsDynamic(t *testing.T) {
	e := workspaceEngine(t)
	work := t.TempDir()
	for _, cmd := range []string{"rm -rf $TARGET", "ls *.go", "cat $(whoami)", "cp a $DEST"} {
		got := evalIn(t, e, work, cmd)
		if got.Decision == protocol.DecisionAllow {
			t.Fatalf("%s: 值不确定不应自动放行（%s）", cmd, got.Feedback.Summary)
		}
	}
}

// TestWorkspaceKeepsDenyAndReviewStructure：deny 名单在任何档位都优先；
// 结构切不开（heredoc）的照旧转人工。
func TestWorkspaceKeepsDenyAndReviewStructure(t *testing.T) {
	e := workspaceEngine(t)
	work := t.TempDir()
	for _, cmd := range []string{"rm -rf build", "ls && rm x", "iex x"} {
		got := evalIn(t, e, work, cmd)
		if got.Decision != protocol.DecisionDeny || got.ReasonCode != "POLICY_DENY" {
			t.Fatalf("%s: deny 应优先于工作区档，实际 %s/%s", cmd, got.Decision, got.ReasonCode)
		}
	}
	if got := evalIn(t, e, work, "cat <<EOF\nhi\nEOF"); got.Decision != protocol.DecisionReview {
		t.Fatalf("heredoc 应转人工，实际 %s/%s", got.Decision, got.ReasonCode)
	}
}

// TestWorkspaceAllowsRmOnceUndenied：默认策略把 rm 列在 deny_commands 里，任何档位都硬拒
// ——这是刻意的（deny 优先于一切档位）。把 rm 从 deny 名单里拿掉之后，工作区档就能
// 放行"工作区内的 rm"，同时仍然拦住"rm 到工作区外"。
func TestWorkspaceAllowsRmOnceUndenied(t *testing.T) {
	const permissive = `{
	  "version": "test-no-deny",
	  "deny_commands": ["iex"],
	  "rules": []
	}`
	e, err := engine.New(analysis.PowerShell{Executable: "unused"}, permissive)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.UpdateTier("workspace"); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	cases := []struct {
		cmd, decision, code string
	}{
		{"rm -rf tmp/out", "allow", "TIER_ALLOW"},
		{"rm -rf build && go build ./...", "allow", "TIER_ALLOW"},
		{"rm -rf ../other", "review", "PATH_OUTSIDE"},
		{"rm -rf /etc/x", "review", "PATH_OUTSIDE"},
	}
	for _, tc := range cases {
		got := evalIn(t, e, work, tc.cmd)
		if got.Decision != tc.decision || got.ReasonCode != tc.code {
			t.Fatalf("%s: got %s/%s（%s），want %s/%s", tc.cmd, got.Decision, got.ReasonCode, got.Feedback.Summary, tc.decision, tc.code)
		}
	}
}

// TestWorkspaceCdWithinStaysInside：工作区内来回 cd 仍然放行，转出工作区则挂起。
func TestWorkspaceCdWithinStaysInside(t *testing.T) {
	e := workspaceEngine(t)
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ cmd, code string }{
		{"cd sub && cp a.txt b.txt", "TIER_ALLOW"},
		{"cd sub && cd .. && cp a.txt b.txt", "TIER_ALLOW"},
		{"cd sub && cd ../.. && cp a.txt b.txt", "PATH_OUTSIDE"},
		{"cd && ls", "NO_MATCHING_RULE"},
	} {
		if got := evalIn(t, e, work, tc.cmd); got.ReasonCode != tc.code {
			t.Fatalf("%s: got %s/%s（%s），want %s", tc.cmd, got.Decision, got.ReasonCode, got.Feedback.Summary, tc.code)
		}
	}
}

// TestWorkspaceRegressionsForOtherTiers：低、中两档的结果不因新增这一档而变。
func TestWorkspaceRegressionsForOtherTiers(t *testing.T) {
	work := t.TempDir()
	low := tierEngine(t, "low")
	medium := tierEngine(t, "medium")
	for _, tc := range []struct {
		cmd, lowCode, mediumCode string
	}{
		{"ls -la", "TIER_ALLOW", "TIER_ALLOW"},
		{"ls && touch x", "NO_MATCHING_RULE", "TIER_ALLOW"},
		{"cp a b", "NO_MATCHING_RULE", "NO_MATCHING_RULE"},
		{"rm -rf build", "POLICY_DENY", "POLICY_DENY"},
		{"mkdir build", "NO_MATCHING_RULE", "TIER_ALLOW"},
		{"curl http://example.com", "NO_MATCHING_RULE", "NO_MATCHING_RULE"},
	} {
		if got := evalIn(t, low, work, tc.cmd); got.ReasonCode != tc.lowCode {
			t.Fatalf("低档 %s: got %s/%s（%s），want %s", tc.cmd, got.Decision, got.ReasonCode, got.Feedback.Summary, tc.lowCode)
		}
		if got := evalIn(t, medium, work, tc.cmd); got.ReasonCode != tc.mediumCode {
			t.Fatalf("中档 %s: got %s/%s（%s），want %s", tc.cmd, got.Decision, got.ReasonCode, got.Feedback.Summary, tc.mediumCode)
		}
	}
}

// TestWorkspaceWithoutBoundaryReviewsEverything：调用方没给工作目录（或给的是相对路径）时，
// 判不出"在不在工作区里"，一律转人工——绝不能把"没有边界"当成"没有越界"。
func TestWorkspaceWithoutBoundaryReviewsEverything(t *testing.T) {
	e := workspaceEngine(t)
	for _, cmd := range []string{"ls", "cat /etc/passwd", "mkdir build", "cp a b"} {
		req := bashRequest(cmd)
		req.Plan.WorkDir = "" // 没有边界
		got, err := e.Evaluate(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if got.Decision == protocol.DecisionAllow {
			t.Fatalf("%s: 没有工作区边界时不应自动放行（%s）", cmd, got.Feedback.Summary)
		}
		if got.ReasonCode != "CONTEXT_UNKNOWN" {
			t.Fatalf("%s: 理由码应为 CONTEXT_UNKNOWN，实际 %s（%s）", cmd, got.ReasonCode, got.Feedback.Summary)
		}
	}
	// 相对路径作边界同样不认。
	req := bashRequest("ls")
	req.Plan.WorkDir = "relative/dir"
	got, err := e.Evaluate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision == protocol.DecisionAllow {
		t.Fatalf("相对路径作边界时不应自动放行（%s）", got.Feedback.Summary)
	}
}
