package runner

// 这一条是**真进程**测试：它验证的是评测脚手架自己（起栈、下发设置、发消息、拉轨迹、boss 收活），
// 不验证被测 harness 的行为（那由具体任务集的断言负责）。
//
// 需要编译五个服务（有源码指纹缓存，通常几秒）。用 `-short` 可跳过：
//
//	go test -short ./...

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/evals/fakeopenai"
)

func TestStackRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过需要真进程的用例")
	}
	root := repoRootForTest(t)
	cache := filepath.Join(root, "data", "evals", "bin")
	bins, err := BuildBinariesCached(root, cache, []string{"settings-center", "memory", "reasoner"}, false)
	if err != nil {
		t.Fatalf("编译服务失败: %v", err)
	}

	fake, err := fakeopenai.New(fakeopenai.Script{
		Steps: []fakeopenai.Step{
			{Name: "judge", When: fakeopenai.When{CallAtMost: 1}, Reply: fakeopenai.Reply{
				ToolCalls: []fakeopenai.ToolCall{{ID: "cj", Name: "task_status", Arguments: `{"status":"done","title":"自检"}`}},
			}},
			{Name: "answer", When: fakeopenai.When{Contains: "判定为"}, Reply: fakeopenai.Reply{Content: "自检完成"}},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()

	dir := t.TempDir()
	stack, err := StartStack(StackOptions{
		Dir:      dir,
		Bin:      bins,
		LLMBase:  fake.URL(),
		Settings: map[string]string{"reasoner.tool.notify_mode": "batch"},
	})
	if err != nil {
		t.Fatalf("起栈失败: %v", err)
	}
	defer stack.Close()

	// 设置必须真的生效：下发失败时 StartStack 就该报错，这里再确认键值能被中心接受。
	if err := stack.applySettings(map[string]string{"reasoner.tool.notify_mode": "each"}); err != nil {
		t.Fatalf("重复下发设置应成功: %v", err)
	}

	if err := stack.Send("selfcheck-1", "自检一下"); err != nil {
		t.Fatalf("发消息失败: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && len(stack.Boss.Delivered()) == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	got := stack.Boss.Delivered()
	if len(got) == 0 {
		t.Fatal("boss 没收到任何交付")
	}
	if !strings.Contains(strings.Join(got, "|"), "自检完成") {
		t.Fatalf("交付内容不符: %v", got)
	}

	hist, err := stack.History("selfcheck-1")
	if err != nil {
		t.Fatalf("拉轨迹失败: %v", err)
	}
	if len(hist) == 0 {
		t.Fatal("轨迹应为空以外的东西")
	}
	foundUser := false
	for _, h := range hist {
		if h.Kind == "user" || h.Role == "user" {
			foundUser = true
		}
	}
	if !foundUser {
		t.Fatalf("轨迹里应含用户消息: %+v", hist)
	}

	// 工作区目录必须真的建好（文件类断言相对它解析）。
	if _, err := os.Stat(stack.WorkDir()); err != nil {
		t.Fatalf("工作区不存在: %v", err)
	}

	// 进程清理：Close 之后这些子进程必须退出，否则下一次 run 会撞端口。
	stack.Close()
	for _, p := range stack.procs {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s 未在 Close 后退出", p.name)
		}
	}
}

// repoRootForTest 从当前文件位置回到仓库根（evals/runner → 上两级）。
func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("定位本文件失败")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}
