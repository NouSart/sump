package analysis

import (
	"context"
	"strings"
)

// Bash 是 POSIX shell 的**保守词法器**：它不解析语法，只判断"这条命令是不是一个命令名
// 加一串字面量参数"。任何可能改变语义的结构（管道、重定向、命令替换、变量、通配、
// 多语句、未闭合引号）都记进 Unknowns，由调用方转人工。
//
// 为什么不起外部进程：bash 没有可用的 AST 输出接口；而"看不懂就当不安全"这条边界
// 本来就不需要真解析器。纯 Go 还有一个好处——机器上没装 bash 也不会让审批失效。
type Bash struct{}

// RuntimePath 对 Bash 无意义（不启动外部解释器），返回空串。
func (Bash) RuntimePath() string { return "" }

// bashUnsafe 是只要在引号外出现就放弃自动判定的字符。
//
// 覆盖：管道、后台、顺序、重定向、子 shell、分组、glob、家目录展开、变量与命令替换、
// 转义、历史展开、注释、换行（多语句）。
const bashUnsafe = "|&;<>(){}[]*?~$`\\!#\n\r"

// bashUnsafeInDouble 是双引号内仍然生效的展开语义：变量与命令替换。
const bashUnsafeInDouble = "$`\\"

// Parse 把命令切成 token；任何不确定都留在 Unknowns 里。
func (Bash) Parse(_ context.Context, command string) (Facts, error) {
	if len(command) > MaxCommandBytes {
		return Facts{Unknowns: []string{"command_limit"}}, nil
	}
	tokens, unknowns := tokenizeBash(command)
	if len(unknowns) > 0 {
		return Facts{Unknowns: unknowns, Nodes: len(tokens)}, nil
	}
	if len(tokens) == 0 {
		return Facts{Unknowns: []string{"empty_command"}}, nil
	}
	args := make([]Argument, 0, len(tokens)-1)
	for _, t := range tokens[1:] {
		args = append(args, Argument{Kind: LiteralArgument, Value: t})
	}
	return Facts{
		Commands: []Command{{Name: tokens[0], Invocation: "Unknown", Arguments: args}},
		Nodes:    len(tokens),
	}, nil
}

// tokenizeBash 按空白分词，识别单/双引号；引号外的危险字符一律记 unknown。
func tokenizeBash(command string) ([]string, []string) {
	var (
		tokens   []string
		unknowns []string
		cur      strings.Builder
		inToken  bool
		quote    byte // 0 = 引号外；'\'' 或 '"'
	)
	flush := func() {
		if inToken {
			tokens = append(tokens, cur.String())
			cur.Reset()
			inToken = false
		}
	}
	note := func(kind string) {
		unknowns = append(unknowns, kind)
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch quote {
		case '\'':
			if c == '\'' {
				quote = 0
				continue
			}
			cur.WriteByte(c)
			inToken = true
			continue
		case '"':
			if c == '"' {
				quote = 0
				continue
			}
			if strings.IndexByte(bashUnsafeInDouble, c) >= 0 {
				note("double_quoted_expansion")
				continue
			}
			cur.WriteByte(c)
			inToken = true
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			inToken = true
			continue
		}
		if c == ' ' || c == '\t' {
			flush()
			continue
		}
		if strings.IndexByte(bashUnsafe, c) >= 0 {
			switch c {
			case '\n', '\r':
				note("multiple_statements")
			default:
				// 与 ParseCompound 共用一套原因词表，避免同一条命令在不同档位
				// 给出 “variable” / “shell_metacharacter” 两种解释。
				note(unsafeKind(c))
			}
			continue
		}
		cur.WriteByte(c)
		inToken = true
	}
	if quote != 0 {
		note("unterminated_quote")
	}
	flush()
	return tokens, dedupeStrings(unknowns)
}

// dedupeStrings 保序去重：Facts.Unknowns 是给人和模型看的，重复项只会干扰判断。
func dedupeStrings(in []string) []string {
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
