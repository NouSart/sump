// 执行：选定 shell、限时、限量地跑一条命令，并把它整理成一条可读的结果。
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// waitDelay 是一次命令被终止后，Wait 等待其 I/O 收尾的最长时间。
// 主要防一种卡死：kill 失败且孙进程还占着 stdout 管道时，Wait 会无限等下去——
// 那条命令会永久占住执行槽位，攒满并发上限后所有命令都被拒。有它就最多多占 10 秒。
const waitDelay = 10 * time.Second

// runCommand 真正执行一条命令。
// 返回的 error 只表示“没能跑起来 / 没能跑完”（shell 缺失、进程启动失败等）；
// 超时属于执行结果（TimedOut=true，退出码 -1）。
func (s *Server) runCommand(j *job) (protocol.ShellEventData, error) {
	res := protocol.ShellEventData{Command: j.command, WorkDir: j.workDir}
	exe, args, err := s.commandLine(j.command)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	res.Shell = exe

	ctx, cancel := context.WithTimeout(context.Background(), s.timeoutNow())
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = j.workDir
	cmd.SysProcAttr = sysProcAttr()
	stdout, stderr := newLimitWriter(s.maxOutputNow()), newLimitWriter(s.maxOutputNow())
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// 超时终止整棵进程树（只杀直接子进程会留下“还在跑”的孙进程）。
	// killErr 留档：终止失败要如实进补报——不许默认“已终止”。
	var killErr error
	cmd.Cancel = func() error {
		killErr = killTree(cmd.Process)
		return killErr
	}
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		res.Error = err.Error()
		return res, fmt.Errorf("启动失败: %w", err)
	}
	werr := cmd.Wait()

	res.Stdout = strings.ToValidUTF8(stdout.String(), "\uFFFD")
	res.Stderr = strings.ToValidUTF8(stderr.String(), "\uFFFD")
	res.StdoutBytes, res.StderrBytes = stdout.Total(), stderr.Total()
	res.Truncated = stdout.Truncated() || stderr.Truncated()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		// 只有确实杀掉了才叫“被终止”；杀不掉要把原样原因说清楚（进程可能仍在运行）。
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			res.Error = fmt.Sprintf("超时后终止失败（进程可能仍在运行）: %v", killErr)
		}
		return res, nil
	}
	var ee *exec.ExitError
	switch {
	case werr == nil:
		res.ExitCode = 0
	case errors.As(werr, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		res.Error = werr.Error()
		return res, fmt.Errorf("执行失败: %w", werr)
	}
	return res, nil
}

// commandLine 给出解释器与参数：设置里指定则用它，否则自动探测。
func (s *Server) commandLine(command string) (string, []string, error) {
	exe := s.shellNow()
	if exe == "" {
		detected, ok := detectShell(s.logger)
		if !ok {
			return "", nil, errors.New("未找到可用的 shell（可用设置 exec.shell 指定解释器）")
		}
		exe = detected
	}
	return exe, shellArgs(exe, command), nil
}

// shellArgs 按解释器给出“执行一条命令”的参数。
func shellArgs(exe, command string) []string {
	name := strings.ToLower(filepath.Base(strings.TrimSuffix(exe, ".exe")))
	switch name {
	case "cmd":
		return []string{"/c", command}
	case "powershell", "pwsh":
		return []string{"-NoProfile", "-NonInteractive", "-Command", command}
	default: // bash / sh / 其它 POSIX shell
		return []string{"-c", command}
	}
}

// detectShell 自动探测可用的 shell（候选顺序见 candidates，平台相关）。
func detectShell(logger interface{ Printf(string, ...any) }) (string, bool) {
	for _, c := range candidates() {
		if p, err := lookShell(c); err == nil {
			if logger != nil {
				logger.Printf("自动探测到 shell: %s", p)
			}
			return p, true
		}
	}
	return "", false
}

// lookShell 在 PATH 上找一个解释器（也接受绝对路径）。
func lookShell(name string) (string, error) {
	return exec.LookPath(name)
}

// renderDone 把执行结果整理成一条可直接阅读的文本（补报的 Text 字段）。
// 超时但终止失败时不得说“被终止”——原因原文照写（数据字段里也有）。
func renderDone(j *job, res protocol.ShellEventData, limit int64) string {
	var b strings.Builder
	switch {
	case res.TimedOut && res.Error != "":
		fmt.Fprintf(&b, "命令超时，且未能终止（进程可能仍在运行；退出码 -1，耗时 %dms）：%s\n", res.DurationMS, res.Error)
	case res.TimedOut:
		fmt.Fprintf(&b, "命令超时并被终止（退出码 -1，耗时 %dms）\n", res.DurationMS)
	default:
		fmt.Fprintf(&b, "命令执行完成（退出码 %d，耗时 %dms）\n", res.ExitCode, res.DurationMS)
	}
	fmt.Fprintf(&b, "$ %s\n", j.command)
	if res.Shell != "" {
		fmt.Fprintf(&b, "（shell: %s，目录: %s）\n", res.Shell, j.workDir)
	}
	writeStream(&b, "stdout", res.Stdout, res.StdoutBytes, res.Truncated, limit)
	writeStream(&b, "stderr", res.Stderr, res.StderrBytes, res.Truncated, limit)
	return strings.TrimRight(b.String(), "\n")
}

// writeStream 追加一路输出；超限时标注“保留了多少、实际多少”。
func writeStream(b *strings.Builder, name, text string, total int64, truncated bool, limit int64) {
	if text == "" && total == 0 {
		fmt.Fprintf(b, "[%s]（空）\n", name)
		return
	}
	fmt.Fprintf(b, "[%s]\n", name)
	if truncated && total > limit {
		fmt.Fprintf(b, "……（已截断：共 %d 字节，保留前 %d 字节）\n", total, limit)
	}
	b.WriteString(text)
	if !strings.HasSuffix(text, "\n") {
		b.WriteString("\n")
	}
}

// ---------- 工作目录 ----------

// resolveWorkDir 解析并校验工作目录：必须在根目录之内（越界、不存在、不可访问都拒绝）。
// 软链先解析再判边界——不许用链接绕过根目录。
func (s *Server) resolveWorkDir(dir string) (string, error) {
	root := s.rootNow()
	abs := root
	if strings.TrimSpace(dir) != "" {
		p := dir
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		var err error
		if abs, err = filepath.Abs(p); err != nil {
			return "", fmt.Errorf("run: 工作目录无效 %q: %v", dir, err)
		}
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("run: 工作目录不存在或不可访问 %q: %v", dir, err)
	}
	if st, err := os.Stat(resolved); err != nil || !st.IsDir() {
		return "", fmt.Errorf("run: 工作目录不是目录 %q", dir)
	}
	if !within(root, resolved) {
		return "", fmt.Errorf("run: 工作目录越界（根目录 %s）: %s", root, resolved)
	}
	return resolved, nil
}

// within 判断 p 是否为 root 本身或位于 root 之内（两者都是绝对路径）。
func within(root, p string) bool {
	rel, err := filepath.Rel(normPath(root), normPath(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// normPath 平台差异：Windows 路径大小写不敏感，比较前归一。
func normPath(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// ---------- 输出收集 ----------

// limitWriter 收集输出但只保留前 limit 字节：超限只丢弃、绝不返回错误——
// 管道照常排水（命令照常跑完），同时留下“一共多少字节”的事实。
type limitWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int64
	total int64
}

func newLimitWriter(limit int64) *limitWriter { return &limitWriter{limit: limit} }

func (w *limitWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += int64(len(p))
	if remain := w.limit - int64(w.buf.Len()); remain > 0 {
		if int64(len(p)) <= remain {
			w.buf.Write(p)
		} else {
			w.buf.Write(p[:remain])
		}
	}
	return len(p), nil
}

func (w *limitWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// Total 返回截断前收到的总字节数。
func (w *limitWriter) Total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

// Truncated 返回是否发生过截断。
func (w *limitWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total > w.limit
}
