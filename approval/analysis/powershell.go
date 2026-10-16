package analysis

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unicode/utf16"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

//go:embed parse.ps1
var parserScript string

type PowerShell struct{ Executable string }

func (p PowerShell) RuntimePath() string { return p.Executable }

func FindPowerShell() (string, error) {
	for _, name := range []string{"pwsh", "powershell"} {
		if path, err := exec.LookPath(name); err == nil {
			absolute, err := filepath.Abs(path)
			if err != nil {
				return "", err
			}
			return filepath.EvalSymlinks(absolute)
		}
	}
	return "", errors.New("找不到 PowerShell；请安装 PowerShell 7 或使用 Windows PowerShell")
}

// Parse 只启动固定的 AST 解析脚本。命令通过 stdin 作为数据传入。
// 每次调用有独立的可终止进程；首版选择清楚的隔离边界，后续再测量是否需要进程池。
func (p PowerShell) Parse(ctx context.Context, command string) (Facts, error) {
	if len(command) > MaxCommandBytes {
		return Facts{Unknowns: []string{"command_limit"}}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	input, err := json.Marshal(struct {
		Command string `json:"command"`
	}{command})
	if err != nil {
		return Facts{}, err
	}
	cmd := exec.CommandContext(ctx, p.Executable, "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedScript(parserScript))
	hideWindow(cmd)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = protocol.PowerShellEnvironment(os.Environ(), p.Executable)
	cmd.WaitDelay = time.Second
	var stdout, stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Facts{}, ctx.Err()
		}
		return Facts{}, fmt.Errorf("PowerShell 解析进程失败: %w", err)
	}
	if stdout.overflow {
		return Facts{}, errors.New("解析器输出超过限制")
	}
	var facts Facts
	if err := json.Unmarshal(bytes.TrimPrefix(stdout.Bytes(), []byte{0xef, 0xbb, 0xbf}), &facts); err != nil {
		return Facts{}, fmt.Errorf("解析器响应不是有效 JSON: %w", err)
	}
	return facts, nil
}

func encodedScript(script string) string {
	units := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[2*i:], u)
	}
	return base64.StdEncoding.EncodeToString(b)
}

type limitedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (256 << 10) - b.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
