package tests

// 原生 SPv2.1 测试辅助：所有跨服务调用都直接构造 call 帧并等 reply，
// 不再经过 v1 信封与 jump 载荷。

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// callResult 是测试侧对一次原生 call 的转述：OK=false 时 Error 是原生错误文本，
// OK=true 时 Data 是 reply body。
type callResult struct {
	OK    bool
	Data  []byte
	Error string
}

func spv2URL(port int) string {
	return fmt.Sprintf("ws://127.0.0.1:%d/spv2", port)
}

// dialSPV2 建立原生连接，并在测试结束时关闭。
func dialSPV2(t *testing.T, url string, opts ...spv2.ClientOption) *spv2.Client {
	t.Helper()
	c, err := spv2.Dial(context.Background(), url, opts...)
	if err != nil {
		t.Fatalf("连接 %s 失败: %v", url, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// callJump 以观察者身份发起一次原生调用。
func callJump(t *testing.T, c *spv2.Client, to, action string, input any, ctx context.Context) callResult {
	t.Helper()
	return callJumpFrom(t, c, "e2e-observer", to, action, input, ctx)
}

// callJumpFrom 指定 from 身份发起一次原生调用：
// SPv2 没有 boss 字段，from 就是结果归属方。
func callJumpFrom(t *testing.T, c *spv2.Client, from, to, action string, input any, ctx context.Context) callResult {
	t.Helper()
	call, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef(from), spv2.ServiceRef(to), action, input)
	if err != nil {
		t.Fatalf("构造 %s→%s.%s 调用失败: %v", from, to, action, err)
	}
	resp, err := c.Call(ctx, call)
	if err != nil {
		return callResult{Error: err.Error()}
	}
	return callResult{OK: true, Data: resp.Body}
}

// newStubServer 构造一个测试替身服务：名片 Actor 与 Name 一致，
// 方法按声明注册（未声明的方法由 spv2 默认回 not_found）。
func newStubServer(t *testing.T, name, description string, methods ...spv2.Method) *spv2.Server {
	t.Helper()
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: name, Actor: spv2.ServiceRef(name),
		Version: name + "/test", Description: description,
		Methods: methods,
		Limits:  spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatalf("构造替身 %s 失败: %v", name, err)
	}
	return srv
}

// serveStub 把替身挂到 httptest 上，并在测试结束时关闭。
func serveStub(t *testing.T, srv *spv2.Server) *httptest.Server {
	t.Helper()
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(h.Close)
	return h
}

func wsURL(h *httptest.Server) string {
	return "ws" + strings.TrimPrefix(h.URL, "http") + "/spv2"
}

// registerService 以 name 身份把替身注册进真中心；连接保持到测试结束，
// 中心据此维持在线状态（连接一断即视为离线）。
func registerService(t *testing.T, centerURL, name, addr, description string) {
	t.Helper()
	c := dialSPV2(t, centerURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef(name), spv2.ServiceRef("settings-center"), "register", protocol.RegisterPayload{
		Name: name, Addr: addr, Description: description,
	})
	if err != nil {
		t.Fatal(err)
	}
	var roster protocol.RosterPayload
	if err := c.CallDecode(ctx, call, &roster); err != nil {
		t.Fatalf("%s 注册失败: %v", name, err)
	}
}
