package analysis

import (
	"context"
	"strings"
)

// DynamicArgument 是词法器认出"含展开或转义、值不确定"的参数。
//
// 与 LiteralArgument 的区别在于：字面量不会被 shell 再解释，动态参数会被
// （变量、命令替换、通配、家目录……）。上层据此决定该档位能不能放行。
const DynamicArgument = "Dynamic"

// bashUnsafeExtra 是"只要在引号外出现就说明这条简单命令里有动态内容"的字符。
// 段分隔符（; && || 换行）和管道（|）不在这里——它们由 ParseCompound 切开。
//
// 反斜杠用 string(rune(92)) 拼出来，源码里就不出现转义序列（本文件曾被
// 传输层的反斜杠折叠写坏过一次，索性绕开）。
var bashUnsafeExtra = "(){}[]*?~$`" + string(rune(92)) + "!#"

// bashBackslash 是反斜杠的字节值，用于 switch 里替代 rune 字面量。
const bashBackslash = 92

// Redirect 是一条重定向。
type Redirect struct {
	Op     string `json:"op"`     // ">", ">>", ">&", "<"
	Target string `json:"target"` // 目标 token；空 = 没写目标
}

// Stage 是管道里的一级：一条简单命令加它自己的重定向。
type Stage struct {
	Command   Command    `json:"command"`
	Redirects []Redirect `json:"redirects"`
	Unknowns  []string   `json:"unknowns"`
}

// Segment 是一段：由 `;`、`&&`、`||`、换行 分隔出来的独立执行单元，内部可能是管道。
type Segment struct {
	Stages []Stage `json:"stages"`
}

// Compound 是整条命令的切分结果。
//
// Unknowns 记"整体切不开"的问题（heredoc 等）；每段每级自己的问题留在 Stage.Unknowns。
// 切分只做结构识别，不重排命令；动态 token 的可见片段可能已去掉展开字符
// （如 `$HOME` 只剩 `HOME`）——安全判断只看 Unknowns/Kind，不依赖该值。
// 上层拿到的 token 顺序与命令原文一致。
type Compound struct {
	Segments []Segment `json:"segments"`
	Unknowns []string  `json:"unknowns"`
}

// ParseCompound 把命令切成"段 → 管道级 → 简单命令"，供按档位逐段判定使用。
//
// 与 Parse 的分工：Parse 只认一条简单命令，任何语法结构都进 Unknowns（"关"档沿用）；
// ParseCompound 保留结构，让"只读段拼出来的组合命令"也能被逐段检验。
func (Bash) ParseCompound(_ context.Context, command string) (Compound, error) {
	if len(command) > MaxCommandBytes {
		return Compound{Unknowns: []string{"command_limit"}}, nil
	}
	s := &compoundScanner{cmd: command}
	s.scan()
	s.endSegment()
	if len(s.compound.Segments) == 0 && len(s.compound.Unknowns) == 0 {
		s.compound.Unknowns = []string{"empty_command"}
	}
	s.compound.Unknowns = dedupeStrings(s.compound.Unknowns)
	return s.compound, nil
}

// compoundScanner 是 ParseCompound 的一次性扫描状态机（引号感知、按层归位）。
type compoundScanner struct {
	cmd      string
	compound Compound
	segment  Segment
	stage    Stage
	tokens   []string
	dynamic  []bool // 与 tokens 对齐：该 token 含动态内容
	quote    byte   // 0 = 引号外；单引号或双引号字节
	cur      strings.Builder
	curDyn   bool
	inToken  bool
	redirect string // 非空 = 正在等这个重定向的目标 token
}

func (s *compoundScanner) scan() {
	for i := 0; i < len(s.cmd); i++ {
		c := s.cmd[i]
		if s.quote != 0 {
			if c == s.quote {
				s.quote = 0
				continue
			}
			if s.quote == '"' && strings.IndexByte(bashUnsafeInDouble, c) >= 0 {
				s.stageNote("double_quoted_expansion")
				s.curDyn = true
				continue
			}
			s.cur.WriteByte(c)
			s.inToken = true
			continue
		}
		switch {
		case c == '\'' || c == '"':
			s.quote = c
			s.inToken = true
		case c == ' ' || c == '\t':
			s.flushToken()
		case c == '\n' || c == '\r':
			s.endSegment()
		case c == ';':
			s.endSegment()
		case c == '&':
			if i+1 < len(s.cmd) && s.cmd[i+1] == '&' {
				i++
				s.endSegment()
				continue
			}
			s.flushToken()
			s.stageNote("background")
		case c == '|':
			if i+1 < len(s.cmd) && s.cmd[i+1] == '|' {
				i++
				s.endSegment()
				continue
			}
			s.flushStage()
		case c == '>':
			s.flushToken()
			op := ">"
			if i+1 < len(s.cmd) && s.cmd[i+1] == '>' {
				op = ">>"
				i++
			} else if i+1 < len(s.cmd) && s.cmd[i+1] == '&' {
				op = ">&"
				i++
			}
			s.startRedirect(op)
		case c == '<':
			if i+1 < len(s.cmd) && s.cmd[i+1] == '<' {
				i++
				s.compound.Unknowns = append(s.compound.Unknowns, "heredoc")
				continue
			}
			s.flushToken()
			s.startRedirect("<")
		default:
			if strings.IndexByte(bashUnsafeExtra, c) >= 0 {
				s.stageNote(unsafeKind(c))
				s.curDyn = true
				s.inToken = true
				continue
			}
			s.cur.WriteByte(c)
			s.inToken = true
		}
	}
	s.flushToken()
	if s.quote != 0 {
		// 必须先记在**当前**级上再 flushStage：flushStage 会重置 stage，
		// 放在它后面记的未知项会被下一级/段直接丢掉（未闭合引号曾因此静默通过）。
		s.stageNote("unterminated_quote")
	}
	s.flushStage()
}

// unsafeKind 给每种"并非简单命令"的字符一个可读的原因，卡片上要写给人看。
func unsafeKind(c byte) string {
	switch c {
	case '$':
		return "variable"
	case '`':
		return "command_substitution"
	case '*', '?', '[', ']':
		return "glob"
	case '~':
		return "tilde_expansion"
	case bashBackslash:
		return "escape"
	case '#':
		return "comment"
	case '(', ')':
		return "subshell"
	case '{', '}':
		return "brace_expansion"
	case '!':
		return "history_expansion"
	}
	return "shell_metacharacter"
}

func (s *compoundScanner) stageNote(kind string) { s.stage.Unknowns = append(s.stage.Unknowns, kind) }

func (s *compoundScanner) startRedirect(op string) {
	if s.redirect != "" {
		s.stageNote("redirect_target")
	}
	s.redirect = op
}

// flushToken 收下一个 token：要么是某条重定向的目标，要么是本级的参数。
func (s *compoundScanner) flushToken() {
	if !s.inToken {
		return
	}
	token := s.cur.String()
	dyn := s.curDyn
	s.cur.Reset()
	s.curDyn = false
	s.inToken = false
	if s.redirect != "" {
		s.stage.Redirects = append(s.stage.Redirects, Redirect{Op: s.redirect, Target: token})
		s.redirect = ""
		return
	}
	s.tokens = append(s.tokens, token)
	s.dynamic = append(s.dynamic, dyn)
}

// flushStage 收下管道里的一级。
func (s *compoundScanner) flushStage() {
	s.flushToken()
	if s.redirect != "" {
		s.stageNote("redirect_target")
		s.redirect = ""
	}
	if len(s.tokens) == 0 && len(s.stage.Redirects) == 0 {
		s.stage = Stage{} // 多余的 ; 或空管道级：忽略，不当作一条命令
		return
	}
	stage := s.stage
	if len(s.tokens) == 0 {
		stage.Unknowns = append(stage.Unknowns, "empty_command")
	} else {
		args := make([]Argument, 0, len(s.tokens)-1)
		for i, token := range s.tokens[1:] {
			kind := LiteralArgument
			if s.dynamic[i+1] {
				kind = DynamicArgument
			}
			args = append(args, Argument{Kind: kind, Value: token})
		}
		stage.Command = Command{Name: s.tokens[0], Invocation: "Unknown", Arguments: args}
		if s.dynamic[0] {
			stage.Unknowns = append(stage.Unknowns, "dynamic_command_name")
		}
	}
	stage.Unknowns = dedupeStrings(stage.Unknowns)
	s.segment.Stages = append(s.segment.Stages, stage)
	s.stage = Stage{}
	s.tokens = nil
	s.dynamic = nil
}

func (s *compoundScanner) endSegment() {
	s.flushStage()
	if len(s.segment.Stages) == 0 {
		return
	}
	s.compound.Segments = append(s.compound.Segments, s.segment)
	s.segment = Segment{}
}
