package tests

// 本地栈的静态对端接线（R-029 ⑨）：
// session 是调用账本的 owner——pause / resume / cancel 与 call_status 对账都要**按服务名**
// 找到执行服务。它不注册设置中心（独立模式），所以这些名字只能来自 run-local 的 `-peers`；
// 脚本漏配的症状是控制与对账全部 fail-visible（不假成功，但也永远不成），
// 而且没有测试盯着：一次脚本合并就能把它悄悄改回去，或"顺手"改成注册中心。
//
// 这里只钉接线本身，不判断服务是否真的在线（那是 e2e 的事）：
//   - 四个 owner 服务名必须都在 session 的静态对端里；
//   - session 不许改成连设置中心——内部服务一注册，设置中心就会同时收到它们的设置声明，
//     `context.history_limit` / `context.*` 会出现第二组真相（设置归属仍是门面），见 TODO「owner 恢复」⑤。
//
// 为什么盯脚本文本：接线就写在这里，没有别的可执行载体；断言的是"这一行带了什么"，
// 不改脚本行为，也不启动任何进程。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunLocalSessionOwnerPeers(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "run-local.ps1"))
	if err != nil {
		t.Fatalf("读 run-local.ps1 失败: %v", err)
	}
	var line string
	for _, l := range strings.Split(string(raw), "\n") {
		if !strings.Contains(l, "'-role', 'session'") {
			continue
		}
		if line != "" {
			t.Fatalf("run-local 里 session 有不止一处启动，先决定该盯哪一处")
		}
		line = l
	}
	if line == "" {
		t.Fatal("run-local.ps1 里找不到 '-role', 'session' 的启动行")
	}
	for _, name := range []string{"shell", "timer", "batch", "mcp"} {
		if !strings.Contains(line, name+"=ws://") {
			t.Fatalf("session 的静态对端缺 %s（按服务名解析不到，控制与对账会一直失败）：%s",
				name, strings.TrimSpace(line))
		}
	}
	if strings.Contains(line, "'-center'") {
		t.Fatalf("session 不应连设置中心（注册后设置键会出现第二组真相）：%s", strings.TrimSpace(line))
	}
}
