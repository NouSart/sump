package engine

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/QAQ-awa-QAQ/sump/approval/analysis"
	"github.com/QAQ-awa-QAQ/sump/approval/policy"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// bashFinisher 与 PowerShell 路径共用同一套裁决出口（含 30 秒许可期）。
type bashFinisher func(decision, code, summary string) (protocol.CommandApprovalResult, error)

// bashRule 是一类只读命令的放行条件。
//
// 只读命令的参数只要全是字面量就安全——字面量不会被 shell 展开，也就改不了行为。
// Subcommands 用于子命令才决定语义的命令（git）：只看命令名会把 `git push` 一起放过去。
type bashRule struct {
	ID          string
	MaxArgs     int
	Subcommands []string
	Reason      string
}

// bashShapes 是允许走词法分析的 POSIX shell 可执行名。
//
// 这里只认名字：分析的是命令文本，与解释器路径无关；限制名字是为了不让
// "PowerShell 路径 + bash 方言"这种组合混进来（正确性上无害，语义上没有必要）。
var bashShapes = map[string]bool{"bash": true, "sh": true, "dash": true, "zsh": true, "ksh": true}

// bashReadOnly 是内置的只读白名单：查、看、读，不改文件也不改系统状态。
//
// 刻意不收录：
//   - env / printenv / set —— 会把环境变量（含密钥）打到输出里
//   - find —— -delete / -exec 会写或执行
//   - xargs —— 把输出变成别的命令的输入
//   - sed / awk —— 能加 -i 直接改文件
//   - tee / dd / touch / mkdir / rm / mv / cp —— 都会改文件
//
// 参数数量上限取"够用且不夸张"：ls -la 之类正常用法能过，超长参数串转人工。
var bashReadOnly = map[string]bashRule{
	"pwd":      {ID: "bash-pwd", MaxArgs: 0, Reason: "查看当前目录"},
	"ls":       {ID: "bash-ls", MaxArgs: 8, Reason: "列出目录内容（只读）"},
	"dir":      {ID: "bash-dir", MaxArgs: 8, Reason: "列出目录内容（只读）"},
	"cat":      {ID: "bash-cat", MaxArgs: 4, Reason: "读取文件内容（只读）"},
	"head":     {ID: "bash-head", MaxArgs: 4, Reason: "读取文件开头（只读）"},
	"tail":     {ID: "bash-tail", MaxArgs: 4, Reason: "读取文件结尾（只读）"},
	"wc":       {ID: "bash-wc", MaxArgs: 4, Reason: "统计行数/字数（只读）"},
	"grep":     {ID: "bash-grep", MaxArgs: 8, Reason: "按模式搜索文本（只读）"},
	"echo":     {ID: "bash-echo", MaxArgs: 32, Reason: "输出字面量文本"},
	"date":     {ID: "bash-date", MaxArgs: 2, Reason: "查看当前时间"},
	"whoami":   {ID: "bash-whoami", MaxArgs: 0, Reason: "查看当前用户"},
	"hostname": {ID: "bash-hostname", MaxArgs: 2, Reason: "查看主机名"},
	"uname":    {ID: "bash-uname", MaxArgs: 4, Reason: "查看系统信息"},
	"df":       {ID: "bash-df", MaxArgs: 2, Reason: "查看磁盘空间"},
	"du":       {ID: "bash-du", MaxArgs: 4, Reason: "查看目录占用（只读）"},
	"ps":       {ID: "bash-ps", MaxArgs: 4, Reason: "查看进程列表"},
	"file":     {ID: "bash-file", MaxArgs: 4, Reason: "判断文件类型"},
	"which":    {ID: "bash-which", MaxArgs: 2, Reason: "查看命令位置"},
	"git": {ID: "bash-git-readonly", MaxArgs: 8, Reason: "查看 git 状态/历史/差异（只读）",
		Subcommands: []string{"status", "log", "diff", "show", "blame"}},
}

// evaluateBash 判定 POSIX shell 方言的命令：只读白名单内直过，其余转第二层。
//
// 与 PowerShell 路径共用 deny 规则：配置里显式拒绝的命令名仍然先被拒。
func (e *Engine) evaluateBash(ctx context.Context, p protocol.ExecutionPlan, snapshot *policy.Snapshot, finish bashFinisher) (protocol.CommandApprovalResult, error) {
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(p.ShellPath), ".exe"))
	if !bashShapes[base] || !filepath.IsAbs(p.WorkDir) || !filepath.IsAbs(p.ShellPath) || p.TimeoutMS <= 0 || p.MaxOutputBytes <= 0 {
		return finish(protocol.DecisionReview, "CONTEXT_UNKNOWN", "执行目录、解释器或固定启动环境不完整，本次未执行")
	}
	// 参数固定为 `-c`（命令文本由执行方拼接）：一旦这里换了写法，审批必须立刻报错，
	// 而不是拿旧假设继续放行。
	if len(p.Args) != 1 || p.Args[0] != "-c" {
		return finish(protocol.DecisionReview, "CONTEXT_UNKNOWN", "shell 启动参数不是约定的 -c，本次未执行")
	}
	if err := ctx.Err(); err != nil {
		return protocol.CommandApprovalResult{}, err
	}
	facts, err := (analysis.Bash{}).Parse(ctx, p.Command)
	if err != nil {
		return protocol.CommandApprovalResult{}, err
	}
	if len(facts.Unknowns) > 0 {
		result, err := finish(protocol.DecisionReview, "DYNAMIC_BEHAVIOR", "命令包含组合、动态或当前未覆盖的结构；本次未执行，需要进一步审批")
		return result, err
	}
	if facts.Nodes > 4096 || len(facts.Commands) != 1 {
		return finish(protocol.DecisionReview, "DYNAMIC_BEHAVIOR", "命令包含多条语句或结构过大；本次未执行，需要进一步审批")
	}
	cmd := facts.Commands[0]
	if snapshot.Denied(cmd.Name) {
		return finish(protocol.DecisionDeny, "POLICY_DENY", "命令包含明确禁止的操作："+cmd.Name+"；本次未执行")
	}
	rule, ok := bashRuleFor(cmd.Name, cmd.Arguments)
	if !ok {
		return finish(protocol.DecisionReview, "NO_MATCHING_RULE", "没有覆盖该命令及参数范围的允许规则，本次未执行")
	}
	if len(cmd.Arguments) > rule.MaxArgs {
		return finish(protocol.DecisionReview, "NO_MATCHING_RULE", "参数数量超出该命令的允许范围，本次未执行")
	}
	for _, arg := range cmd.Arguments {
		if arg.Kind != analysis.LiteralArgument {
			return finish(protocol.DecisionReview, "DYNAMIC_BEHAVIOR", "参数包含非静态内容，本次未执行")
		}
	}
	// 只读命令能读文件，所以"读哪里"必须有边界：白名单管"能做什么"，这里管"能碰哪里"。
	if bad, reason := bashPathViolation(cmd.Arguments, p.WorkDir); bad != "" {
		result, err := finish(protocol.DecisionReview, "PATH_OUTSIDE", reason)
		if err == nil {
			result.Feedback.Unknowns = []string{bad}
		}
		return result, err
	}
	return finishWithRule(finish, rule)
}

// bashPathViolation 找出第一个越出工作目录、或指向秘密文件的参数。
//
// 边界的两条：
//  1. 路径解析后必须仍在 WorkDir 之下（`..` 逃逸、绝对路径越界都算）；
//  2. 秘密文件（`*-secret.json`）即便在工作目录内也不许读——那是密钥，不是工作数据。
//
// 不解析符号链接：能在工作目录里造软链的前提是先有写权限，而写操作一律要人工审批。
func bashPathViolation(args []analysis.Argument, workDir string) (string, string) {
	if !filepath.IsAbs(workDir) {
		return "", ""
	}
	for _, arg := range args {
		// 裸文件名也要先查：`cat memory-secret.json` 不含分隔符，但它就是密钥。
		if isSecretFile(arg.Value) {
			return arg.Value, "参数指向本服务的秘密文件（密钥），本次未执行"
		}
		for _, candidate := range bashPathCandidates(arg.Value) {
			if isSecretFile(candidate) {
				return candidate, "参数指向本服务的秘密文件（密钥），本次未执行"
			}
			target, ok := resolveBashPath(candidate, workDir)
			if !ok {
				return candidate, "参数是工作目录之外的绝对路径，本次未执行"
			}
			if rel, err := filepath.Rel(workDir, target); err != nil ||
				rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return candidate, "参数越出工作目录，本次未执行"
			}
		}
	}
	return "", ""
}

// bashPathCandidates 摘出参数里"像路径"的部分：整个参数，以及 `--opt=value` 的 value。
func bashPathCandidates(v string) []string {
	var out []string
	if looksLikePath(v) {
		out = append(out, v)
	}
	if i := strings.IndexByte(v, '='); i >= 0 && i+1 < len(v) && looksLikePath(v[i+1:]) {
		out = append(out, v[i+1:])
	}
	return out
}

// looksLikePath 保守判断一个 token 是否可能是路径：宁可多查，不可漏查。
func looksLikePath(v string) bool {
	if v == "" || v == "-" || v == "--" {
		return false
	}
	if strings.HasPrefix(v, "-") { // 选项本身不是路径；`--opt=path` 由调用方拆开再看
		return false
	}
	if strings.HasPrefix(v, "/") || strings.HasPrefix(v, `\`) {
		return true
	}
	if strings.ContainsAny(v, `/\`) {
		return true
	}
	return v == "." || v == ".." || strings.HasPrefix(v, "./") || strings.HasPrefix(v, `.\`) ||
		strings.HasPrefix(v, "../") || strings.HasPrefix(v, `..\`)
}

// resolveBashPath 把参数解析成绝对路径；返回 false 表示"确定在工作目录之外"。
func resolveBashPath(v, workDir string) (string, bool) {
	if strings.HasPrefix(v, "/") {
		// POSIX 绝对路径：`/d/agent/x` 是 Git Bash 的盘符写法；其余（如 /etc/passwd）
		// 在 Windows 上没有对应位置，直接判越界。
		converted := posixDrivePath(v)
		if converted == "" {
			return "", false
		}
		v = converted
	}
	if !filepath.IsAbs(v) {
		v = filepath.Join(workDir, filepath.FromSlash(v))
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", false
	}
	return abs, true
}

// posixDrivePath 把 `/d/agent/x` 转成 `D:\agent\x`；不是这种形状就返回空串。
func posixDrivePath(v string) string {
	if len(v) < 3 || v[0] != '/' || !isASCIILetter(v[1]) || v[2] != '/' {
		return ""
	}
	return strings.ToUpper(string(v[1])) + ":" + filepath.FromSlash(v[2:])
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isSecretFile 认出服务自己落盘的秘密文件（命名见 service/secrets.go）。
func isSecretFile(v string) bool {
	base := strings.ToLower(filepath.Base(filepath.FromSlash(v)))
	return base == "secret.json" || strings.HasSuffix(base, "-secret.json")
}

// bashRuleFor 查内置只读表。配置只能收紧（deny_commands 先于这里生效），不能放宽：
// 白名单是"系统知识"，不该由一份可编辑的策略文件决定哪些命令可以不经人工。
func bashRuleFor(name string, args []analysis.Argument) (bashRule, bool) {
	rule, ok := bashReadOnly[strings.ToLower(name)]
	if !ok {
		return bashRule{}, false
	}
	if len(rule.Subcommands) > 0 {
		// 子命令决定语义的命令（git）：第一个参数必须是只读子命令。
		if len(args) == 0 || !containsFold(rule.Subcommands, args[0].Value) {
			return bashRule{}, false
		}
	}
	return rule, true
}

func containsFold(list []string, want string) bool {
	for _, item := range list {
		if strings.EqualFold(item, want) {
			return true
		}
	}
	return false
}

func finishWithRule(finish bashFinisher, rule bashRule) (protocol.CommandApprovalResult, error) {
	result, err := finish(protocol.DecisionAllow, "RULE_ALLOW", rule.Reason)
	if err != nil {
		return result, err
	}
	result.Feedback.RuleIDs = []string{rule.ID}
	return result, nil
}
