package shared

// 发布会话摘要前的脱敏：**纵深防御的第二道**，不是唯一防线。
// 第一道是形状（载荷里没有原文）与提示词（复盘明确禁止写路径 / 命令 / 账号）。
// 这里兜住模型偶尔漏掉的东西，并把单条摘要压到库里放得下的长度。

import (
	"regexp"
	"strings"
)

var (
	reURL     = regexp.MustCompile(`(?i)\b(?:https?|ftp|wss?)://[^\s"'<>）)，。]+`)
	reWinPath = regexp.MustCompile(`(?i)\b[A-Z]:\\[^\s"'<>，。；]*`)
	reUNCPath = regexp.MustCompile(`\\\\[^\s"'<>，。；]+`)
	// POSIX 绝对路径：至少两段，避免把 and/or 这类词当路径。
	rePosix   = regexp.MustCompile(`(^|[\s（(])(?:~|\.{1,2})?(?:/[A-Za-z0-9._-]+){2,}/?`)
	reEmail   = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reToken   = regexp.MustCompile(`(?i)\b(?:sk|pk|ghp|gho|ghu|ghs|glpat|xox[baprs])[-_][A-Za-z0-9_\-]{8,}`)
	reBearer  = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]+`)
	reCNPhone = regexp.MustCompile(`\b1[3-9]\d{9}\b`)
	reSpace   = regexp.MustCompile(`\s+`)
)

// Sanitize 把一段自由文本压成"可以进共享库"的一句话：
// 去链接 / 路径 / 邮箱 / 密钥形状 / 手机号，压平换行，按 max 个字符截断。
// max <= 0 时只做脱敏、不截断。
func Sanitize(text string, max int) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return ""
	}
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ").Replace(s)
	s = reURL.ReplaceAllString(s, "[链接]")
	s = reEmail.ReplaceAllString(s, "[邮箱]")
	s = reToken.ReplaceAllString(s, "[已隐去]")
	s = reBearer.ReplaceAllString(s, "[已隐去]")
	s = reWinPath.ReplaceAllString(s, "[路径]")
	s = reUNCPath.ReplaceAllString(s, "[路径]")
	s = rePosix.ReplaceAllString(s, "${1}[路径]")
	s = reCNPhone.ReplaceAllString(s, "[已隐去]")
	s = reSpace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	return Truncate(s, max)
}

// Truncate 按**字符**（不是字节）截断，超出时补一个省略号。
func Truncate(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// SanitizeCapabilities 规范能力族清单：小写、只留标识符字符、去重、封顶 6 个。
// 能力族来自 reasoner 的确定性统计（服务名），不来自模型措辞。
func SanitizeCapabilities(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		name := strings.ToLower(strings.TrimSpace(raw))
		name = strings.Trim(name, ".-_ ")
		if name == "" || len(name) > 24 || seen[name] {
			continue
		}
		clean := true
		for _, r := range name {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				clean = false
				break
			}
		}
		if !clean {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) == 6 {
			break
		}
	}
	return out
}
