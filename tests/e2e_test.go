package tests

// 总测试：真实进程 + 真实 WS 的端到端验证（见 docs/concepts/architecture.md）。

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
)

// TestHelloWeb 是 M1 的验收：两个真实进程（settings-center + reasoner）
// 跑通最小闭环——注册 / 名册 / 跨服务跳转 / 自我跳转。
func TestHelloWeb(t *testing.T) {
	scBin := buildService(t, "github.com/NouSart/sump/settings-center")
	alBin := buildService(t, "github.com/NouSart/sump/reasoner")

	scPort := freePort(t)
	alPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	reasonerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", alPort)

	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort))
	waitWSReady(t, centerURL, 60*time.Second)

	startProc(t, alBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", alPort),
		"-center", centerURL,
		"-reflect=false", // 复盘会多花一次模型调用；这条用例只验启动链
	)

	// 观察者客户端：作为第三方接入设置中心（不依赖任何服务内部状态）。
	obs, err := protocol.Dial(centerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })

	// 断言 a：reasoner 注册成功，名册可见（含设置中心自身）。
	waitRosterHas(t, obs, "reasoner", 60*time.Second)
	waitRosterHas(t, obs, "settings-center", 10*time.Second)

	// 观察者连 reasoner，做跳转断言。
	ac, err := protocol.Dial(reasonerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })

	// 断言 b：reasoner → settings-center 跳转（ping → pong）。
	ctxB, cancelB := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelB()
	rp := callJump(t, ac, "reasoner", "debug_jump", map[string]any{
		"to":     "settings-center",
		"action": "ping",
	}, ctxB)
	if !rp.OK {
		t.Fatalf("跨服务跳转失败: %s", rp.Error)
	}
	var pong struct {
		Pong bool   `msgpack:"pong"`
		Who  string `msgpack:"who"`
	}
	if err := protocol.DecodeRaw(rp.Data, &pong); err != nil {
		t.Fatal(err)
	}
	if !pong.Pong || pong.Who != "settings-center" {
		t.Fatalf("ping 结果不符: %+v", pong)
	}

	// 断言 c：自我跳转——循环地基。
	ctxC, cancelC := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelC()
	rp2 := callJump(t, ac, "reasoner", "debug_jump", map[string]any{
		"to":     "self",
		"action": "echo",
		"input":  map[string]any{"msg": "hello-web"},
	}, ctxC)
	if !rp2.OK {
		t.Fatalf("自我跳转失败: %s", rp2.Error)
	}
	var echo struct {
		Who  string `msgpack:"who"`
		Echo struct {
			Msg string `msgpack:"msg"`
		} `msgpack:"echo"`
	}
	if err := protocol.DecodeRaw(rp2.Data, &echo); err != nil {
		t.Fatal(err)
	}
	if echo.Who != "reasoner" || echo.Echo.Msg != "hello-web" {
		t.Fatalf("自我跳转结果不符: %+v", echo)
	}
}

// ---------- 辅助 ----------

// buildService 构建服务二进制到临时目录（在仓库根执行，走 go.work）。
func buildService(t *testing.T, pkg string) string {
	t.Helper()
	return buildServiceNamed(t, pkg, filepath.Base(pkg))
}

// buildServiceNamed 同上，但可指定输出名。
// 用于规避杀软对个别文件名的误报（本机 McAfee 拦截 "images.exe" 这个名字）。
func buildServiceNamed(t *testing.T, pkg, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", bin, pkg)
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建 %s 失败: %v\n%s", pkg, err, out)
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// freePort 探测一个空闲端口（先占再放，供子进程使用）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startProc 启动子进程并在测试结束时清理（返回句柄，便于中途重启演练）。
func startProc(t *testing.T, bin string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// waitWSReady 重试拨号直到目标可连接。
func waitWSReady(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := protocol.Dial(url, nil)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待 %s 就绪超时", url)
}

// waitRosterHas 轮询拉取名册，直到包含指定服务。
func waitRosterHas(t *testing.T, obs *protocol.Client, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		rp := callJump(t, obs, "settings-center", "roster", nil, ctx)
		cancel()
		if rp.OK {
			var r protocol.RosterPayload
			if err := protocol.DecodeRaw(rp.Data, &r); err == nil {
				for _, s := range r.Services {
					if s.Name == name {
						return
					}
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("等待名册出现 %s 超时", name)
}

// callJump 以观察者身份发起一次跳转，返回响应 payload。
func callJump(t *testing.T, c *protocol.Client, to, action string, input any, ctx context.Context) protocol.ResponsePayload {
	t.Helper()
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "e2e-observer", to, "", protocol.JumpPayload{Action: action, Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	return rp
}
