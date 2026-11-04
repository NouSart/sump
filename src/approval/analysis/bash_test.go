package analysis

// Bash 词法器的直接单测：引号、危险字符、去重与 Facts 形状。
// 上层 engine 的用例覆盖“裁决结果”，这里钉住“解析事实”，
// 免得引号/转义规则退化时只有一串 review 结果可看。

import (
	"context"
	"strings"
	"testing"
)

func TestTokenizeBashQuotesAndUnsafe(t *testing.T) {
	cases := []struct {
		name     string
		command  string
		tokens   []string
		unknowns []string
	}{
		{"single_quote", `echo 'hello world'`, []string{"echo", "hello world"}, nil},
		{"double_quote", `echo "hello world"`, []string{"echo", "hello world"}, nil},
		{"double_quote_expansion", `echo "$HOME"`, []string{"echo", "HOME"}, []string{"double_quoted_expansion"}},
		{"redirect", `cat a > b`, []string{"cat", "a", "b"}, []string{"shell_metacharacter"}},
		{"two_statements", `pwd; ls`, []string{"pwd", "ls"}, []string{"shell_metacharacter"}},
		{"unterminated", `echo 'oops`, []string{"echo", "oops"}, []string{"unterminated_quote"}},
		{"dedupe", `ls | grep x |`, []string{"ls", "grep", "x"}, []string{"shell_metacharacter"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokens, unknowns := tokenizeBash(tc.command)
			if strings.Join(tokens, "\x00") != strings.Join(tc.tokens, "\x00") {
				t.Fatalf("tokens = %q, want %q", tokens, tc.tokens)
			}
			if strings.Join(unknowns, ",") != strings.Join(tc.unknowns, ",") {
				t.Fatalf("unknowns = %q, want %q", unknowns, tc.unknowns)
			}
		})
	}
}

func TestBashFacts(t *testing.T) {
	ctx := context.Background()

	facts, err := (Bash{}).Parse(ctx, "cat main.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Commands) != 1 || facts.Commands[0].Name != "cat" {
		t.Fatalf("简单命令解析不符: %+v", facts)
	}
	if facts.Commands[0].Invocation != "Unknown" {
		t.Fatalf("词法器不判断调用语义: %+v", facts.Commands[0])
	}
	if len(facts.Commands[0].Arguments) != 1 || facts.Commands[0].Arguments[0] != (Argument{Kind: LiteralArgument, Value: "main.go"}) {
		t.Fatalf("参数应为字面量: %+v", facts.Commands[0].Arguments)
	}
	if facts.Nodes != 2 {
		t.Fatalf("Nodes = %d, want 2", facts.Nodes)
	}

	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		{"empty", "", "empty_command"},
		{"too_long", strings.Repeat("a", MaxCommandBytes+1), "command_limit"},
		{"dynamic", "cat $HOME/.bashrc", "variable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts, err := (Bash{}).Parse(ctx, tc.command)
			if err != nil {
				t.Fatal(err)
			}
			if len(facts.Commands) != 0 || !contains(facts.Unknowns, tc.want) {
				t.Fatalf("应拒绝并说明原因 %q: %+v", tc.want, facts)
			}
		})
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
