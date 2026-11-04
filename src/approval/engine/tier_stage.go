package engine

import (
	"strings"

	"github.com/NouSart/sump/approval/analysis"
)

// checkMediumStage 判"中"档的一段一级：不碰明确危险动作就放行。
//
// 与低档的区别：不要求命令在只读清单里（python -c 这类判不出意图的也放行），
// 但仍然拦黑名单动作、管道进解释器（见 checkPipeIntoInterpreter），
// 以及往工作目录外写、碰密钥文件。
func checkMediumStage(stage analysis.Stage, cwd, root string) stageVerdict {
	if len(stage.Unknowns) > 0 {
		return stageVerdict{code: "DYNAMIC_BEHAVIOR", text: "含展开或转义（" + strings.Join(dedupe(stage.Unknowns), ", ") + "），值不确定"}
	}
	name := strings.ToLower(stage.Command.Name)
	if why, blocked := mediumBlocked[name]; blocked {
		return stageVerdict{code: "NO_MATCHING_RULE", text: why}
	}
	// 包管理器看子命令：npm run build 是构建，npm install 才是装包。
	if mediumPackageManagers[name] && installs(stage.Command.Arguments) {
		return stageVerdict{code: "NO_MATCHING_RULE", text: "装软件或改依赖"}
	}
	for _, r := range stage.Redirects {
		if r.Op == ">&" {
			return stageVerdict{code: "DYNAMIC_BEHAVIOR", text: "重定向把输出接到别的文件句柄"}
		}
		if r.Op == "<" {
			if bad, reason := bashPathViolationWithin([]analysis.Argument{{Kind: analysis.LiteralArgument, Value: r.Target}}, cwd, root); bad != "" {
				return stageVerdict{code: "PATH_OUTSIDE", text: reason}
			}
			continue
		}
		// 写重定向：允许写在工作目录里（构建产物、日志），写到外面一律转人工。
		if r.Target == "" {
			return stageVerdict{code: "DYNAMIC_BEHAVIOR", text: "重定向没有写目标"}
		}
		if isDeviceOrStream(r.Target) {
			continue
		}
		if bad, reason := bashPathViolationWithin([]analysis.Argument{{Kind: analysis.LiteralArgument, Value: r.Target}}, cwd, root); bad != "" {
			return stageVerdict{code: "PATH_OUTSIDE", text: reason}
		}
	}
	// 密钥文件任何档位都不许碰。
	for _, a := range stage.Command.Arguments {
		if isSecretFile(a.Value) {
			return stageVerdict{code: "PATH_OUTSIDE", text: "参数指向本服务的秘密文件（密钥），本次未执行"}
		}
	}
	return stageVerdict{ok: true, ruleID: "tier-medium"}
}

// isDeviceOrStream 认出 /dev/null、nul 这类"不是文件"的写目标。
func isDeviceOrStream(target string) bool {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "/dev/null", "/dev/stdout", "/dev/stderr", "nul", "con", "-":
		return true
	}
	return false
}

// checkPipeIntoInterpreter 在"中"档里额外拦一条结构性危险：管道右侧是解释器。
//
// 这一条要看到"上一级的输出接过来"，按整段判，所以单独走一遍。
func checkPipeIntoInterpreter(seg analysis.Segment) (bool, string) {
	for i := 1; i < len(seg.Stages); i++ {
		name := strings.ToLower(seg.Stages[i].Command.Name)
		if !mediumInterpreters[name] {
			continue
		}
		// 带脚本文件参数（python x.py、bash script.sh）是"执行文件"，不是"执行上一段的输出"。
		hasScript := false
		for _, a := range seg.Stages[i].Command.Arguments {
			if !strings.HasPrefix(a.Value, "-") {
				hasScript = true
				break
			}
		}
		if !hasScript {
			return true, "把上一段的输出直接喂给 " + name + " 执行"
		}
	}
	return false, ""
}

func totalStages(c analysis.Compound) int {
	n := 0
	for _, seg := range c.Segments {
		n += len(seg.Stages)
	}
	return n
}

func compoundNames(c analysis.Compound) []string {
	var out []string
	for _, seg := range c.Segments {
		for _, stage := range seg.Stages {
			if stage.Command.Name != "" {
				out = append(out, stage.Command.Name)
			}
		}
	}
	return out
}

// dedupe 保序去重：反馈里的名单给人看，重复项只会干扰。
func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// installs 判断包管理器的这次调用是不是在动依赖/全局环境。
//
// 保守口径：没有子命令（npm 裸跑 = 打印帮助，无害）或子命令不在安装动词里，
// 就当普通脚本执行（npm run build、npm test …）。
func installs(args []analysis.Argument) bool {
	for _, a := range args {
		if strings.HasPrefix(a.Value, "-") {
			continue // 选项不算子命令
		}
		return mediumInstallVerbs[strings.ToLower(a.Value)]
	}
	return false
}
