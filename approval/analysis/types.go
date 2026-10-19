// Package analysis 只解析命令，不执行命令。
// PowerShell 走原生 AST；POSIX shell 走保守词法器（见 bash.go）。
package analysis

import "context"

const MaxCommandBytes = 16 << 10

// LiteralArgument 是 Bash 词法器给出的参数种类：已确认是字面量，不含展开语义。
const LiteralArgument = "Literal"

// StringConstantArgument / ConstantArgument 是 PowerShell AST 给出的静态常量种类。
const (
	StringConstantArgument = "StringConstantExpressionAst"
	ConstantArgument       = "ConstantExpressionAst"
)

type Argument struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type Command struct {
	Name       string     `json:"name"`
	Invocation string     `json:"invocation"`
	Arguments  []Argument `json:"arguments"`
}

// Facts 中 Unknowns 必须保留；不能把不支持的节点当成无副作用。
type Facts struct {
	Commands     []Command `json:"commands"`
	Unknowns     []string  `json:"unknowns"`
	SyntaxErrors []string  `json:"syntax_errors"`
	Nodes        int       `json:"nodes"`
}

type Parser interface {
	RuntimePath() string
	Parse(context.Context, string) (Facts, error)
}
