package protocol

import (
	"path/filepath"
	"strings"
)

// PowerShellArgs 是 powershell-clean-v1 的固定启动参数；末尾另接原始命令。
func PowerShellArgs() []string {
	return []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command"}
}

// PowerShellEnvironment 是该 profile 的纯数据转换，供解析端和执行端保持一致。
// 不读取操作系统状态，不授予权限，不构成文件系统沙箱。
func PowerShellEnvironment(environ []string, executable string) []string {
	var env []string
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "PSMODULE") || strings.HasPrefix(upper, "POWERSHELL_") || upper == "PSEXECUTIONPOLICYPREFERENCE" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "PSModulePath="+filepath.Join(filepath.Dir(executable), "Modules"))
}
