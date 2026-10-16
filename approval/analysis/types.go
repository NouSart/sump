// Package analysis 只解析命令，不执行命令。首版支持 PowerShell 原生 AST。
package analysis

import "context"

const MaxCommandBytes = 16 << 10

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
