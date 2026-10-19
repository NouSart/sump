//go:build windows

package server

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// candidates 是自动探测的候选解释器（按优先级）：优先 bash（Git Bash），
// 其后再 pwsh / Windows PowerShell / cmd——与“bash 优先，必要时回退”的姿态一致。
//
// Git Bash 的绝对路径排在裸名 `bash` 之前：PATH 里的 `bash` 通常是
// `C:\Windows\System32\bash.exe`，那是 WSL 启动器——Linux 文件系统、另一套工具链，
// 拿它执行 Windows 工程目录里的命令会处处碰壁（路径、编码、可用程序全不同）。
func candidates() []string {
	gitBash := []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
	}
	out := make([]string, 0, len(gitBash)+4)
	for _, p := range gitBash {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return append(out, "bash", "pwsh", "powershell", "cmd")
}

// sysProcAttr 返回平台相关的进程属性：Windows 下由 taskkill /T 负责整棵树，这里不额外设标志。
func sysProcAttr() *syscall.SysProcAttr { return nil }

// killTree 终止整棵进程树：taskkill /T /F；失败再退回只杀直接进程（尽力而为，不静默）。
func killTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid))
	if err := kill.Run(); err != nil {
		return p.Kill()
	}
	return nil
}
