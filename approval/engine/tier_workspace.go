package engine

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/QAQ-awa-QAQ/sump/approval/analysis"
)

// 「工作区」档的口径：工作区内的读、写、删除、移动、复制、建目录、构建、跑脚本、
// 本地 git 操作一律放行；只拦**已经越过工作区**的五类动作。
//
// 与「中」档的差别只有两点：①中档拦工作区内的 rm/mv/cp/chmod，这一档放行；
// ②它的边界明确叫"工作区"，判据只有一条——解析后的路径是否还在工作区里。

// workspaceBlocked 是「工作区」档仍然拦下的命令名（转人工，不是硬拒：你仍可以点批准）。
//
// 只收"在工作区里做也仍然是越过工作区"的动作：提权、装系统软件、联网外发、
// 改系统状态、终止别人的进程。rm/mv/cp/mkdir/touch/chmod/python/node/npm 不在这里
// ——那些正是这一档要放行的日常动作。
var workspaceBlocked = map[string]string{
	// 提权
	"sudo": "提权执行", "su": "切换用户", "doas": "提权执行", "runas": "提权执行", "pkexec": "提权执行",

	// 装系统软件（在各档都拦；项目内装依赖走 workspaceGlobalInstall 另判）
	"apt": "装系统软件", "apt-get": "装系统软件", "yum": "装系统软件", "dnf": "装系统软件",
	"pacman": "装系统软件", "zypper": "装系统软件", "apk": "装系统软件", "brew": "装系统软件",
	"choco": "装系统软件", "winget": "装系统软件", "scoop": "装系统软件", "snap": "装系统软件",
	"msiexec": "装系统软件",

	// 联网外发
	"curl": "联网外发", "wget": "联网外发", "scp": "联网外发", "sftp": "联网外发",
	"rsync": "联网外发", "ssh": "远程登录", "telnet": "远程登录", "nc": "联网外发",
	"ncat": "联网外发", "nmap": "扫描网络", "ftp": "联网外发", "tftp": "联网外发",

	// 改系统状态：注册表、服务、分区、开关机、账户、定时任务、终止进程
	"systemctl": "改系统服务", "service": "改系统服务", "sc": "改系统服务", "reg": "改注册表",
	"shutdown": "开关机", "reboot": "开关机", "halt": "开关机", "poweroff": "开关机",
	"diskpart": "改分区", "mkfs": "格式化磁盘", "dd": "直接写块设备", "fdisk": "改分区", "parted": "改分区",
	"crontab": "改定时任务", "at": "改定时任务",
	"useradd": "改系统账户", "userdel": "改系统账户", "usermod": "改系统账户",
	"passwd": "改账户密码", "net": "改系统账户或服务",
	"kill": "终止进程", "killall": "终止进程", "pkill": "终止进程", "taskkill": "终止进程",
}

// blocksPipeIntoInterpreter 判断该档位要不要拦"管道进解释器"（curl … | sh 那一类）。
// 中档与工作区档都拦：它不是路径问题，是"把网上的内容直接执行"。
func blocksPipeIntoInterpreter(t Tier) bool {
	return t == TierMedium || t == TierWorkspace
}

// workspaceGlobalInstall 判"这次装依赖是不是装出了工作区"。
//
// npm run build 不是装依赖；npm install 装进 ./node_modules（在工作区里）→ 放行；
// npm install -g / --prefix 装到全局 → 转人工；pip / gem 默认装进共享解释器环境
// （不在工作区里），所以它们的安装动词一律转人工。
func workspaceGlobalInstall(name string, args []analysis.Argument) bool {
	switch name {
	case "pip", "pip3", "gem":
		return installs(args)
	}
	if !mediumPackageManagers[name] || !installs(args) {
		return false
	}
	for _, a := range args {
		v := strings.ToLower(a.Value)
		if v == "-g" || v == "--global" || v == "--location=global" || strings.HasPrefix(v, "--prefix") {
			return true
		}
	}
	return false
}

// workspaceSecret 认出仓库里常见的密钥文件：.ssh 目录、.env、*.pem / *.key、
// credentials*、id_rsa / id_ed25519，以及服务自己的 secret.json。
//
// 这些文件即便就在工作区里也不给碰：读它就是读密钥，写它就是替换密钥。
func workspaceSecret(v string) bool {
	if isSecretFile(v) {
		return true
	}
	slash := strings.ToLower(filepath.ToSlash(v))
	if strings.HasPrefix(slash, ".ssh/") || strings.Contains(slash, "/.ssh/") ||
		strings.HasPrefix(slash, "~/.ssh") || strings.Contains(slash, "id_rsa") || strings.Contains(slash, "id_ed25519") {
		return true
	}
	base := strings.ToLower(filepath.Base(filepath.FromSlash(v)))
	switch {
	case base == ".env" || strings.HasPrefix(base, ".env."):
		return true
	case strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".pfx") || strings.HasSuffix(base, ".p12"):
		return true
	case strings.HasPrefix(base, "credentials"):
		return true
	}
	return false
}

// workspacePathViolation 复用低/中档的路径边界判定，只把措辞统一成「工作区」：
// 这一档说"越出工作区"，不再说"工作目录"——判据只认工作区。
func workspacePathViolation(args []analysis.Argument, base, boundary string) (string, string) {
	bad, reason := bashPathViolationWithin(args, base, boundary)
	reason = strings.ReplaceAll(reason, "工作目录", "工作区")
	return bad, reason
}

// checkWorkspaceStage 判「工作区」档的一段一级。
//
// 顺序就是这一档的五条禁令：密钥文件 → 越过工作区的路径（含重定向目标、
// 解释器 -c 代码里的路径字面量）→ 提权/装系统软件 → 联网外发 → 管道进解释器
// （最后一条按整段判，见 evaluateBashTier）。其余一律放行。
func checkWorkspaceStage(stage analysis.Stage, cwd, root string) stageVerdict {
	if len(stage.Unknowns) > 0 {
		// 值不确定就判不出"在不在工作区里"，退回底线：转人工。
		return stageVerdict{code: "DYNAMIC_BEHAVIOR", text: "含展开或转义（" + strings.Join(dedupe(stage.Unknowns), ", ") + "），判不出是否还在工作区内"}
	}
	// 没有可用的边界就判不出"在不在工作区里"：宁可全部转人工，也不能当成"没越界"。
	// 注意 bashPathViolationWithin 在边界不是绝对路径时返回"无违规"，那是给低/中档
	// 留的宽松口子；这一档的全部收益都建立在边界之上，不能沿用这个宽松口径。
	if !filepath.IsAbs(root) || !filepath.IsAbs(cwd) {
		return stageVerdict{code: "CONTEXT_UNKNOWN", text: "没有可用的工作区边界（调用方未提供工作目录），判不出是否越界"}
	}
	name := strings.ToLower(stage.Command.Name)
	if why, blocked := workspaceBlocked[name]; blocked {
		return stageVerdict{code: "NO_MATCHING_RULE", text: why + "（越过工作区，工作区档也不放行）"}
	}
	if workspaceGlobalInstall(name, stage.Command.Arguments) {
		return stageVerdict{code: "NO_MATCHING_RULE", text: "往共享环境或全局装软件（装到了工作区之外）"}
	}
	// 重定向：工作区内随便写；写到工作区外、或写到密钥文件上，转人工。
	for _, r := range stage.Redirects {
		if r.Op == ">&" { // 文件句柄复制，不碰磁盘
			continue
		}
		if r.Target == "" {
			return stageVerdict{code: "DYNAMIC_BEHAVIOR", text: "重定向没有写目标"}
		}
		if isDeviceOrStream(r.Target) {
			continue
		}
		if workspaceSecret(r.Target) {
			return stageVerdict{code: "PATH_OUTSIDE", text: "重定向目标指向密钥文件（" + r.Target + "），本次未执行"}
		}
		if bad, reason := workspacePathViolation([]analysis.Argument{{Kind: analysis.LiteralArgument, Value: r.Target}}, cwd, root); bad != "" {
			return stageVerdict{code: "PATH_OUTSIDE", text: "重定向目标" + reason}
		}
	}
	// 参数里的密钥文件与越界路径。
	for _, a := range stage.Command.Arguments {
		if workspaceSecret(a.Value) {
			return stageVerdict{code: "PATH_OUTSIDE", text: "参数指向密钥文件（" + a.Value + "），本次未执行"}
		}
	}
	if bad, reason := workspacePathViolation(stage.Command.Arguments, cwd, root); bad != "" {
		return stageVerdict{code: "PATH_OUTSIDE", text: reason}
	}
	// cd 只挪动本条命令自己的进程目录：跳不出工作区就放行，后续段按新目录校验。
	if name == "cd" {
		if len(stage.Command.Arguments) != 1 {
			return stageVerdict{code: "NO_MATCHING_RULE", text: "cd 不带目录会切到家目录，超出工作区"}
		}
		next, ok := resolveBashPath(stage.Command.Arguments[0].Value, cwd)
		if !ok {
			return stageVerdict{code: "PATH_OUTSIDE", text: "目标目录越出工作区，本次未执行"}
		}
		if rel, err := filepath.Rel(root, next); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return stageVerdict{code: "PATH_OUTSIDE", text: "目标目录越出工作区"}
		}
		return stageVerdict{ok: true, ruleID: "workspace-cd", cwdAfter: next}
	}
	// 解释器把代码写在参数里时（python -c / node -e），尽力把代码里的路径字面量也查一遍。
	if code, ok := interpreterCode(stage); ok {
		for _, literal := range pathLiterals(code) {
			if workspaceSecret(literal) {
				return stageVerdict{code: "PATH_OUTSIDE", text: "脚本里的路径指向密钥文件（" + literal + "），本次未执行"}
			}
			if bad, reason := workspacePathViolation([]analysis.Argument{{Kind: analysis.LiteralArgument, Value: literal}}, cwd, root); bad != "" {
				return stageVerdict{code: "PATH_OUTSIDE", text: "脚本里的路径" + reason}
			}
		}
	}
	return stageVerdict{ok: true, ruleID: "tier-workspace"}
}

// interpreterCode 认出"代码直接写在参数里"的调用（python -c …、node -e …、
// powershell -Command …），返回那段代码文本。
func interpreterCode(stage analysis.Stage) (string, bool) {
	if !mediumInterpreters[strings.ToLower(stage.Command.Name)] {
		return "", false
	}
	args := stage.Command.Arguments
	for i := 0; i+1 < len(args); i++ {
		switch strings.ToLower(args[i].Value) {
		case "-c", "-e", "--eval", "-command", "-encodedcommand":
			return args[i+1].Value, true
		}
	}
	return "", false
}

// pathLiteralPattern 摘代码文本里引号包起来的一小段；够用即可，宁可多查。
var pathLiteralPattern = regexp.MustCompile(`["']([^"'\s]{1,260})["']`)

// pathLiterals 从代码文本里尽力摘出"像路径"的字面量。
//
// 只是尽力而为：字符串拼接出来的路径（open(os.environ["T"] + "/x")）摘不到，
// 所以「工作区」档不承诺"工作区内绝对安全"，只承诺"看得见的越界都拦"。
func pathLiterals(code string) []string {
	var out []string
	for _, m := range pathLiteralPattern.FindAllStringSubmatch(code, 8) {
		v := m[1]
		if looksLikePath(v) || strings.ContainsAny(v, `/\`) || strings.Contains(v, "..") || workspaceSecret(v) {
			out = append(out, v)
		}
	}
	return out
}
