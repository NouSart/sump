package analysis

// ParseCompound 的直接单测：段 / 管道级 / 重定向 / 动态参数 / 整条命令级未知。
// 上层 tier 用例验的是裁决；这里验结构，失败时能一眼看出是哪层切错了。

import (
	"context"
	"testing"
)

func TestParseCompoundStructure(t *testing.T) {
	ctx := context.Background()

	c, err := (Bash{}).ParseCompound(ctx, "cat a | grep b")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Segments) != 1 || len(c.Segments[0].Stages) != 2 {
		t.Fatalf("一条管道应切成一段两级: %+v", c)
	}
	first := c.Segments[0].Stages[0]
	if first.Command.Name != "cat" || len(first.Command.Arguments) != 1 || first.Command.Arguments[0].Kind != LiteralArgument {
		t.Fatalf("第一级不符: %+v", first)
	}
	if second := c.Segments[0].Stages[1]; second.Command.Name != "grep" || second.Command.Arguments[0].Value != "b" {
		t.Fatalf("第二级不符: %+v", second)
	}

	c, err = (Bash{}).ParseCompound(ctx, "pwd; ls")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Segments) != 2 || len(c.Segments[0].Stages) != 1 || len(c.Segments[1].Stages) != 1 {
		t.Fatalf("分号应切成两段: %+v", c)
	}
}

func TestParseCompoundRedirectsAndDynamic(t *testing.T) {
	ctx := context.Background()

	c, err := (Bash{}).ParseCompound(ctx, "cat a > out")
	if err != nil {
		t.Fatal(err)
	}
	stage := c.Segments[0].Stages[0]
	if len(stage.Redirects) != 1 || stage.Redirects[0] != (Redirect{Op: ">", Target: "out"}) {
		t.Fatalf("重定向不符: %+v", stage.Redirects)
	}

	c, err = (Bash{}).ParseCompound(ctx, "echo $HOME")
	if err != nil {
		t.Fatal(err)
	}
	stage = c.Segments[0].Stages[0]
	if len(stage.Command.Arguments) != 1 || stage.Command.Arguments[0].Kind != DynamicArgument || stage.Command.Arguments[0].Value != "HOME" {
		t.Fatalf("含变量的参数应标成 Dynamic: %+v", stage.Command.Arguments)
	}
	if !contains(stage.Unknowns, "variable") {
		t.Fatalf("应说明动态原因: %+v", stage.Unknowns)
	}

	c, err = (Bash{}).ParseCompound(ctx, `echo "a $HOME"`)
	if err != nil {
		t.Fatal(err)
	}
	stage = c.Segments[0].Stages[0]
	if !contains(stage.Unknowns, "double_quoted_expansion") {
		t.Fatalf("双引号内展开应记入未知: %+v", stage.Unknowns)
	}
}

func TestParseCompoundWholeCommandProblems(t *testing.T) {
	ctx := context.Background()

	c, err := (Bash{}).ParseCompound(ctx, "cat <<EOF")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Unknowns, "heredoc") {
		t.Fatalf("heredoc 应记在整条命令级: %+v", c)
	}

	c, err = (Bash{}).ParseCompound(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Unknowns, "empty_command") {
		t.Fatalf("空命令应明确报空: %+v", c)
	}

	c, err = (Bash{}).ParseCompound(ctx, "echo 'oops")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Segments[0].Stages[0].Unknowns, "unterminated_quote") {
		t.Fatalf("未闭合引号应记在该级: %+v", c)
	}

	long := make([]byte, MaxCommandBytes+1)
	for i := range long {
		long[i] = 'a'
	}
	c, err = (Bash{}).ParseCompound(ctx, string(long))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(c.Unknowns, "command_limit") {
		t.Fatalf("超长命令应直接拒绝: %+v", c)
	}
}
