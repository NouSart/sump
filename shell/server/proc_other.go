//go:build !windows

package server

import (
	"os"
	"syscall"
)

// candidates 是自动探测的候选解释器（按优先级）：bash 优先，回退 POSIX sh。
func candidates() []string {
	return []string{"bash", "sh"}
}

// sysProcAttr 让子进程自成一个进程组，便于整组终止（见 killTree）。
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killTree 终止整个进程组：负号 PID 即“该组”；失败再退回只杀直接进程（尽力而为，不静默）。
func killTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		return p.Kill()
	}
	return nil
}
