//go:build !windows

package analysis

import "os/exec"

func hideWindow(*exec.Cmd) {}
