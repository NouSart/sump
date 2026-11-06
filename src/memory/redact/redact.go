// Package redact 是共享经验库的写入边界规则：正文脱敏 + 标识列收口。
//
// 规则只在这里定义一份。共享库的三条写入路（长期记忆 Add、经验网 Attach、
// 会话目录 Publish）与一次性迁移都调它，不再各写一套——两套实现迟早漂移，
// 漂移的方向是漏。
//
// 两类列用两种待遇，理由不同：
//   - 正文（content / title / goal / outcome）：掩码。经验本身要留下，形状要洗掉。
//   - 标识（conversation_id / task_id / source）：收口。它们不是文本，掩码只会
//     得到一堆不可用的星号；会话 id 用「入口 + 单向摘要」保住分组能力，
//     其余只允许不透明 id 形状，不合规就拒绝写入（fail-closed）。
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

var (
	reURL = regexp.MustCompile(`(?i)\b(?:https?|ftp|wss?)://[^\s"'<>）)，。]+`)
	// reWinPath 两种分隔符都认。只认反斜杠是 2026-11-06 实测出来的漏：
	// 共享库里 `mem_entries` rowid 80 那句 `open('C:/Windows/x','w')` 原样躺着。
	// 正斜杠形态不是罕见拼写——Git Bash 与 filepath.ToSlash 在 Windows 上产出的就是它。
	// 顺序上不用担心与链接抢：Text 先跑 reURL，`http://…` / `ws://…` 整段已被换成
	// `[链接]`，到这里只剩 `C:/` 这种真盘符开头；宁可过掩也不漏（见下方判别器的取舍）。
	// 全库校准：加正斜杠前这一条命中 0 行（反斜杠形态早被洗掉了），加后命中 1 行。
	reWinPath = regexp.MustCompile(`(?i)\b[A-Z]:[\\/](?:[^\s"'<>，。；]*)`)
	reUNCPath = regexp.MustCompile(`\\\\[^\s"'<>，。；]+`)
	// POSIX 绝对路径：至少两段，避免把 and/or 这类词当路径。
	rePosix   = regexp.MustCompile(`(^|[\s（(])(?:~|\.{1,2})?(?:/[A-Za-z0-9._-]+){2,}/?`)
	reEmail   = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reToken   = regexp.MustCompile(`(?i)\b(?:sk|pk|ghp|gho|ghu|ghs|glpat|xox[baprs])[-_][A-Za-z0-9_\-]{8,}`)
	reBearer  = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]+`)
	reCNPhone = regexp.MustCompile(`\b1[3-9]\d{9}\b`)
	reSpace   = regexp.MustCompile(`\s+`)

	// reOpaqueSession 是 SessionID 的输出形状：入口 + 12 位十六进制摘要。
	// 认它，是为了让 SessionID 可重复作用——目录里读回的 id 会原样传回来删除，
	// 再摘要一次就变成另一个值，那条摘要就再也删不掉了。
	reOpaqueSession = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}:[0-9a-f]{12}$`)
	reOpaqueBare    = regexp.MustCompile(`^[0-9a-f]{12}$`)
	reEntry         = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	// reRefID 是标识列的允许形状：不透明 id，不含冒号 / 斜杠 / 反斜杠 / at / 空格。
	reRefID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// Text 掩码一段自由文本：去链接 / 路径 / 邮箱 / 密钥形状 / 手机号，压平换行。
// 不截断（截断是调用方的语义，脱敏不是）。
func Text(s string) string {
	s = strings.TrimSpace(s)
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
	return strings.TrimSpace(s)
}

// Sanitize 掩码 + 按字符截断；max <= 0 时只掩码不截断。
func Sanitize(s string, max int) string {
	return Truncate(Text(s), max)
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

// Capabilities 规范能力族清单：小写、只留标识符字符、去重、封顶 6 个。
// 能力族来自 reasoner 的确定性统计（服务名），不来自模型措辞。
func Capabilities(in []string) []string {
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

// SessionID 把会话 id 收成「入口 + 单向摘要」：`qq:private:22…` → `qq:6f2a1c…`。
// 强度边界见 digest 上的注释：它保证库里不再出现账号原文，不保证摘要不可还原。
//
// 为什么摘要而不是原样存：会话 id 的尾巴是真实账号（QQ 号 / 用户 id），共享库
// 按定义不得出现账号。为什么摘要而不是直接拒绝：目录要按会话归并、要排除自己，
// 去掉账号就得留住「同一会话每次算出同一个值」这件事，否则目录散架。
// 入口（`qq` / `web`）留着，因为它是服务名不是账号；展示也只用它。
//
// 空入参返回空（许多列允许为空）。形状已收口的入参原样返回（可重复作用）。
func SessionID(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if reOpaqueSession.MatchString(s) || reOpaqueBare.MatchString(s) {
		return s, nil
	}
	if Text(s) != s {
		return "", fmt.Errorf("会话标识 %q 含路径 / 链接 / 邮箱形状，不是会话 id", truncateForError(s))
	}
	entry, rest := "", s
	if i := strings.IndexByte(s, ':'); i > 0 {
		entry, rest = strings.ToLower(s[:i]), s[i+1:]
	}
	if entry != "" && !reEntry.MatchString(entry) {
		return "", fmt.Errorf("会话标识 %q 的入口段 %q 不是服务名形状", truncateForError(s), entry)
	}
	if strings.TrimSpace(rest) == "" {
		return "", fmt.Errorf("会话标识 %q 缺少账号段，无法归并", truncateForError(s))
	}
	sum := digest(rest)
	if entry == "" {
		return sum, nil
	}
	return entry + ":" + sum, nil
}

// MustSessionID 是 SessionID 的宽松版：不合规时退化成空串，供一次性迁移用。
// 迁移面对的是既成事实的历史数据——丢一个分组值比整条丢掉经验划算，
// 也比把账号搬进共享库划算。新写入方应该用 SessionID 并让错误浮出来。
func MustSessionID(raw string) string {
	v, err := SessionID(raw)
	if err != nil {
		return ""
	}
	return v
}

// RefID 收口 task_id / source 这类标识列：只允许不透明 id 形状。
// what 用于错误信息里指明是哪一列。空入参返回空。
func RefID(raw, what string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if !reRefID.MatchString(s) {
		return "", fmt.Errorf("%s %q 不是不透明 id（不得含冒号 / 斜杠 / at / 空格）", what, truncateForError(s))
	}
	return s, nil
}

// Detect 返回文本里第一个命中的泄露类别，没有则空串。
//
// 它是"什么算泄露"的唯一定义源：写入前的过滤、存量清理后的复检、测试里的断言，
// 都问它，免得三处各写一套谓词、清理脚本自称干净。
//
// 局限要写明白：它认的是**形状**（链接 / 路径 / 邮箱 / 密钥形状 / 手机号），
// 认不出一个裸的 QQ 号——那没有形状可言。所以标识列不看 Detect，看结构：
// conversation_id 必须已经是收口后的形状，见 IsOpaqueSessionID。
func Detect(s string) string {
	switch {
	case reURL.MatchString(s):
		return "链接"
	case reWinPath.MatchString(s), reUNCPath.MatchString(s), rePosix.MatchString(s):
		return "路径"
	case reEmail.MatchString(s):
		return "邮箱"
	case reToken.MatchString(s), reBearer.MatchString(s):
		return "密钥形状"
	case reCNPhone.MatchString(s):
		return "手机号"
	default:
		return ""
	}
}

// IsOpaqueSessionID 判断一个值是否已经是收口后的会话键形状。
// 存量清理用它决定「这一格还需要不需要动」。
func IsOpaqueSessionID(s string) bool {
	return reOpaqueSession.MatchString(s) || reOpaqueBare.MatchString(s) || s == ""
}

// IsRefID 判断一个值是否已经是不透明 id 形状（空值算通过：那些列允许为空）。
func IsRefID(s string) bool {
	return strings.TrimSpace(s) == "" || reRefID.MatchString(strings.TrimSpace(s))
}

// digest 取 sha256 的前 12 位十六进制。
// 12 位（48 bit）在「同一台机器上的会话数」这个量级上碰撞概率可忽略。
//
// 强度要说准：这一层去的是**账号原文出现在共享库里**这件事，不是密码学承诺。
// QQ 号只有 10 位十进制（~2^33 空间），拿着摘要离线枚举 sha256 就能把原号试出来。
// 所以摘要防的是"共享库被当作文档翻、提炼物随上下文发给模型"这类**搬运**，
// 不防拿着库文件的定向还原——那需要加盐，而盐只能存进同一个库，防不住同一对手。
// 真到需要抗还原的那天，盐要存在库外（配置 / 密钥文件），并带上换盐重摘要的迁移。
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func truncateForError(s string) string {
	r := []rune(s)
	if len(r) <= 48 {
		return s
	}
	return string(r[:48]) + "…"
}
