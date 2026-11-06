package tests

// 原生 SPv2 method：/spv2 的 call 直接解帧 body（ShellRunPayload），进程内只有这一条入口。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/shell/server"
	"github.com/NouSart/sump/spv2"
)

// dialSPv2 打开一条到 shell /spv2 的 SPv2.1 连接。
func dialSPv2(t *testing.T, url string) *spv2.Client {
	t.Helper()
	c, err := spv2.Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestShellNativeSPv2 直接打 shell 的 /spv2：run 走原生 handler（body 即 ShellRunPayload），
// 受理回执正常、补报按 frame.from.id 回到调用方；错误输入回 invalid_argument。
func TestShellNativeSPv2(t *testing.T) {
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Approver: allowAll{}})
	c := dialSPv2(t, s.WsURL())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 服务卡：run 是给模型的异步 method，且带 input/output schema（原生注册而非合成）。
	card, err := c.Hello(ctx, spv2.ServiceRef("tester"), spv2.ServiceRef("shell"))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := card.Method("run")
	if !ok {
		t.Fatalf("服务卡里没有 run: %+v", card.Methods)
	}
	if !m.Tool || !m.LongRunning || len(m.Input) == 0 || len(m.Output) == 0 {
		t.Fatalf("run method 声明不符: %+v", m)
	}

	call, err := spv2.NewCall("w-shell", spv2.ServiceRef("caller"), spv2.ServiceRef("shell"), "run",
		protocol.ShellRunPayload{Command: echoCommand("sump-native-ok")})
	if err != nil {
		t.Fatal(err)
	}
	var res protocol.ShellRunResult
	if err := c.CallDecode(ctx, call, &res); err != nil {
		t.Fatal(err)
	}
	if res.ItemID == "" || res.Phase != "queued" {
		t.Fatalf("受理回执不符: %+v", res)
	}

	done := waitReportItem(t, cl, "done", res.ItemID)
	if d := eventData(t, done); d.ExitCode != 0 || !strings.Contains(d.Stdout, "sump-native-ok") {
		t.Fatalf("原生 run 的执行结果不符: %+v", d)
	}
	if !done.Final {
		t.Fatal("done 应为终态")
	}

	// 错误路径：同一原生入口，空命令 / 越界工作目录都回 invalid_argument。
	for _, payload := range []protocol.ShellRunPayload{
		{Command: "   "},
		{Command: echoCommand("x"), WorkDir: ".."},
	} {
		bad, err := spv2.NewCall("w-shell-bad", spv2.ServiceRef("caller"), spv2.ServiceRef("shell"), "run", payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Call(ctx, bad); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
			t.Fatalf("非法输入应回 invalid_argument（payload=%+v），实际 %v", payload, err)
		}
	}
}
