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
func candidates() []string {
	return []string{"bash", "pwsh", "powershell", "cmd"}
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
