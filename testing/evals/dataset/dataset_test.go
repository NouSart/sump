package dataset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write 写一个临时任务集文件（不带 BOM，模拟正常编辑结果）。
func write(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `{"id":"t1","prompt":"p","expect":[{"delivered":true}]}`

// TestLoadMinimal：最小任务可加载，预算取默认值。
func TestLoadMinimal(t *testing.T) {
	tasks, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("任务数 %d, want 1", len(tasks))
	}
	if tasks[0].Budget.MaxHops != 20 || tasks[0].Budget.MaxWallMS != 120000 {
		t.Fatalf("预算默认值不对: %+v", tasks[0].Budget)
	}
}

// TestLoadSkipsBlankAndComment：空行与 # 注释跳过（否则手写任务集很容易被空行绊倒）。
func TestLoadSkipsBlankAndComment(t *testing.T) {
	tasks, err := Load(write(t, "# 注释", "", minimal, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("任务数 %d, want 1", len(tasks))
	}
}

// TestLoadRejectsBOM：带 BOM 的文件必须报错而不是"悄悄少一条任务"。
// 真实踩过：PowerShell 的 Set-Content -Encoding UTF8 会写 BOM，解析报 invalid character 'ï'。
func TestLoadRejectsBOM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, append([]byte{0xEF, 0xBB, 0xBF}, []byte(minimal+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("带 BOM 的任务集应当报错")
	}
	if !strings.Contains(err.Error(), "解析失败") {
		t.Fatalf("错误信息应指明解析失败: %v", err)
	}
}

// TestValidateErrors 覆盖每一条拒绝规则：缺 id / 缺 prompt / 缺 expect / 断言字段数不对 / sandbox。
func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		task Task
		want string
	}{
		{"缺 id", Task{Prompt: "p", Expect: []Expectation{{NoToolFailure: true}}}, "缺少 id"},
		{"缺 prompt", Task{ID: "x", Expect: []Expectation{{NoToolFailure: true}}}, "缺少 prompt"},
		{"缺 expect", Task{ID: "x", Prompt: "p"}, "缺少 expect"},
		{"断言字段为 0", Task{ID: "x", Prompt: "p", Expect: []Expectation{{}}}, "必须且只能写一个"},
		{"断言字段为 2", Task{ID: "x", Prompt: "p", Expect: []Expectation{{NoToolFailure: true, HopsAtMost: 3}}}, "必须且只能写一个"},
		{"sandbox 未支持", Task{ID: "x", Prompt: "p", Expect: []Expectation{{NoToolFailure: true}}, Sandbox: true}, "sandbox=true"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.task.Validate()
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应含 %q，实际 %v", c.want, err)
			}
		})
	}
}

// TestValidateAcceptsEveryAssertion：每个断言字段单独出现都必须合法——
// 这条用例防的是"新加断言忘了登记到计数里"，那会让所有用它的任务莫名报错。
func TestValidateAcceptsEveryAssertion(t *testing.T) {
	no := false
	all := []Expectation{
		{FileExists: "a"},
		{FileContains: &FileContains{Path: "a", Text: "b"}},
		{MemoryHas: &MemoryHas{Kind: "tool_result", Contains: "c"}},
		{DeliverContains: "d"},
		{Delivered: &no},
		{FailureNotified: &no},
		{NoToolFailure: true},
		{RemindersAtLeast: 1},
		{GateRejectsAtLeast: 1},
		{HopsAtMost: 3},
		{FileAbsent: "z"},
	}
	for i, e := range all {
		task := Task{ID: "x", Prompt: "p", Expect: []Expectation{e}}
		if err := task.Validate(); err != nil {
			t.Fatalf("expect[%d] 应当合法: %v", i, err)
		}
	}
}

// TestLoadRejectsEmptyFile：空任务集必须报错——"0 条任务、0 条失败"看起来是满分，其实什么都没测。
func TestLoadRejectsEmptyFile(t *testing.T) {
	_, err := Load(write(t, "# 只有注释"))
	if err == nil || !strings.Contains(err.Error(), "没有任务") {
		t.Fatalf("空任务集应报错，实际 %v", err)
	}
}

// TestLoadMissingFile：文件不存在时报错而不是静默返回空集合。
func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Fatal("文件不存在应报错")
	}
}
