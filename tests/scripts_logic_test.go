package tests

// 仓库脚本（scripts/*.ps1）里"拿主意"的三件事，单独拿出来验：
//
//   1. 停服务的顺序（reload）：先停会派生别的东西的，最后停基础设施；
//   2. 起服务前的端口闸（run-local）：端口被占要抛错；进程已退出时"端口能连"也不许当成起来了；
//   3. -DryRun 只报告不动手——这是用户敢用它预览的唯一理由。
//
// 不测的东西也有理由：Has-Root / Test-PortOpen 这类单行判据没有独立价值，测它们只是给测试
// 凑数；真正会出错的是上面三处"判断错了就会杀错进程 / 起错服务"的地方。
//
// 办法：脚本支持 `-SourceOnly`（只定义函数、不执行主流程），dot-source 之后直接调函数。
// 不启服务、不杀进程。Windows-only：脚本按 Windows 平台行为写。

import (
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// psSnippet 在 dot-source 了某个脚本之后跑一段 PowerShell，返回标准输出。
func psSnippet(t *testing.T, script, body string) string {
	t.Helper()
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("没有 powershell（非 Windows 环境跳过脚本测试）")
	}
	root := repoRoot(t)
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
		". '"+filepath.Join(root, "scripts", script)+"' -SourceOnly; "+body)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell 执行失败: %v\n%s", err, out)
	}
	// 只解析 ASCII 输出：控制台编码因机器而异，断言不该依赖它。
	return string(out)
}

// freeAddr 返回一个当前没人监听的地址（先占再放，拿到的端口通常还没被别人抢）。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestReloadStopOrder 停止顺序：先停派生者，最后停基础设施。
// 反了的话，settings-center 一停其余服务自然掉线，后面的 taskkill 全是空转；
// shell.exe 先停则把它派生的子进程树留在原地。
func TestReloadStopOrder(t *testing.T) {
	chain := []string{
		"cmd.exe", "powershell.exe", "shell.exe", "reasoner.exe", "node.exe", "python.exe",
		"timer.exe", "sump-approval.exe", "qq.exe", "sump-files.exe", "sump-images.exe",
		"memory.exe", "settings-center.exe",
	}
	body := "$names = @('" + strings.Join(chain, "','") + "','whatever.exe'); " +
		"$names | ForEach-Object { \"$_=\" + (Get-StopRank $_) }; " +
		"\"UPPER=\" + (Get-StopRank 'MEMORY.EXE')"
	out := psSnippet(t, "reload-sump.ps1", body)

	rank := map[string]int{}
	upper := -1
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			continue
		}
		if k == "UPPER" {
			upper = n
			continue
		}
		rank[k] = n
	}
	if len(rank) != len(chain)+1 {
		t.Fatalf("没解析到全部停止优先级：%v\n原始输出：%s", rank, out)
	}

	for i := 1; i < len(chain); i++ {
		if rank[chain[i]] <= rank[chain[i-1]] {
			t.Fatalf("停止顺序不对：%s(%d) 应晚于 %s(%d) 停", chain[i], rank[chain[i]], chain[i-1], rank[chain[i-1]])
		}
	}
	// 认不出来的进程排最后：宁可晚停，也别抢在基础设施前面瞎杀。
	if rank["whatever.exe"] <= rank["settings-center.exe"] {
		t.Fatalf("未知进程应排最后：whatever=%d settings-center=%d", rank["whatever.exe"], rank["settings-center.exe"])
	}
	// Windows 进程名大小写混杂，判定不能因此变。
	if upper != rank["memory.exe"] {
		t.Fatalf("Get-StopRank 应忽略大小写：MEMORY.EXE=%d memory.exe=%d", upper, rank["memory.exe"])
	}
}

// TestRunLocalPortGuards 起服务前的两道端口闸：
//   - Assert-PortFree：被占用要**抛错**（不然起不来还查不出原因）；
//   - Wait-Port：进程已退出时即使端口能连也必须返回 False——那是上一轮残留占着端口，
//     当成就绪会把所有服务一起拖死（脚本里的注释记着这次事故）。
func TestRunLocalPortGuards(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	busyAddr := ln.Addr().String()
	defer ln.Close()
	idleAddr := freeAddr(t)

	body := "try { Assert-PortFree '" + idleAddr + "'; \"IDLE=OK\" } catch { \"IDLE=THREW\" }; " +
		"try { Assert-PortFree '" + busyAddr + "'; \"BUSY=NOWHERE\" } catch { \"BUSY=THREW\" }; " +
		"\"OPENWAIT=\" + (Wait-Port '" + busyAddr + "' 2); " +
		"$p = Start-Process -FilePath 'cmd.exe' -ArgumentList '/c','exit' -PassThru; $p.WaitForExit(); " +
		"\"DEADWAIT=\" + (Wait-Port '" + busyAddr + "' 3 $p)"
	out := psSnippet(t, "run-local.ps1", body)
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			got[k] = strings.TrimSpace(v)
		}
	}
	if got["IDLE"] != "OK" || got["BUSY"] != "THREW" {
		t.Fatalf("端口自检不符：%v（原始输出：%s）", got, out)
	}
	if got["OPENWAIT"] != "True" {
		t.Fatalf("Wait-Port 对能连的端口应返回 True：%v", got)
	}
	if got["DEADWAIT"] != "False" {
		t.Fatalf("进程已退出时 Wait-Port 必须返回 False（端口能连不等于自己起来了）：%v", got)
	}
}

// TestReloadDryRunSmoke -DryRun 是唯一一条"完整跑主流程但不动手"的路径：
// 它要能成功退出并明确写着 DryRun（否则用户不敢用这个开关预览）。
func TestReloadDryRunSmoke(t *testing.T) {
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("没有 powershell（非 Windows 环境跳过脚本测试）")
	}
	root := repoRoot(t)
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass",
		"-File", filepath.Join(root, "scripts", "reload-sump.ps1"), "-DryRun")
	cmd.Dir = root
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("reload-sump.ps1 -DryRun 超时（它不该真的去等进程/端口）")
	}
	if err != nil {
		t.Fatalf("DryRun 应该成功退出: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "DryRun") {
		t.Fatalf("DryRun 输出里应明确写着 DryRun：%s", out)
	}
}
