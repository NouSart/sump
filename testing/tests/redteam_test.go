package tests

// spec §17.2 红队六条红线的成组验收（一组一个 t.Run，失败信息直接点名红线）：
//   掩耳盗铃 / 声东击西 / 狐假虎威 / 概念漂移 / 多此一举 / 指鹿为马。
// 能用进程内真协议的就用进程内（快）；work 状态机与 authority 用真 memory 进程（真库）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// TestRedTeamGroup 是六条红线的成组验收入口。
func TestRedTeamGroup(t *testing.T) {
	t.Run("掩耳盗铃：丢帧必须有账号，调用方不能空等", testRedTeamDropped)
	t.Run("声东击西：to 与服务卡不符必须拒绝", testRedTeamMisdirection)
	t.Run("狐假虎威：没有 owner/runtime 不能控制 work", testRedTeamImpersonation)
	t.Run("概念漂移：帧字段/文档键名只允许一套", testRedTeamConceptDrift)
	t.Run("多此一举：works/work_messages 只允许 memory 一处", testRedTeamSingleton)
	t.Run("指鹿为马：终态/renew/非法状态转移必须 conflict", testRedTeamStateMachine)
}

// testRedTeamDropped：坏帧被丢要计数（不是静默吞掉）；handler 报错要有错误计数。
func testRedTeamDropped(t *testing.T) {
	srv := newStubServer(t, "peer", "红队对端", spv2.Method{Name: "boom", Shape: spv2.ShapeCall})
	srv.Handle("boom", func(_ context.Context, _ *spv2.Call) error {
		return spv2.ErrCode(spv2.CodeInternal, "下游拒收（测试注入）", true, nil)
	})
	h := serveStub(t, srv)
	c := dialSPV2(t, wsURL(h))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	call, err := spv2.NewCall("w-red", spv2.ServiceRef("e2e"), spv2.ServiceRef("peer"), "boom", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, call); err == nil {
		t.Fatal("handler 报错时调用方必须拿到错误（不能空等/假成功）")
	}
	if !metricHas(srv, "sump_errors_total", map[string]string{"code": spv2.CodeInternal}) {
		t.Fatalf("handler 错误没有进 sump_errors_total: %+v", srv.Metrics().Snapshot())
	}

	// 坏帧：裸连接发 hello 后发一段解不开的字节，必须计 dropped{reason=undecodable} 并断开。
	raw, _, err := websocket.DefaultDialer.Dial(wsURL(h), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	hello, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef("e2e"), spv2.ServiceRef("peer"), "hello", map[string]any{"protocol": spv2.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := spv2.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.WriteMessage(websocket.BinaryMessage, encoded); err != nil {
		t.Fatal(err)
	}
	_, _, _ = raw.ReadMessage() // hello 回执
	_ = raw.WriteMessage(websocket.BinaryMessage, []byte{0xff, 0xfe, 0xfd})
	_ = raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := raw.ReadMessage(); err == nil {
		t.Fatal("坏帧之后服务端应收掉连接（不能继续假装正常）")
	}
	waitMetric(t, srv, "sump_dropped_total", map[string]string{"reason": "undecodable"})
}

// testRedTeamMisdirection：to 指向别的服务时，接收方必须拒绝且 handler 不执行。
func testRedTeamMisdirection(t *testing.T) {
	var handled int32
	srv := newStubServer(t, "peer", "红队对端", spv2.Method{Name: "echo", Shape: spv2.ShapeCall})
	srv.Handle("echo", func(_ context.Context, c *spv2.Call) error {
		handled++
		return c.Reply("ok")
	})
	h := serveStub(t, srv)
	c := dialSPV2(t, wsURL(h))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if rp := callJumpFrom(t, c, "e2e", "other-service", "echo", nil, ctx); rp.OK {
		t.Fatal("to 不是本服务时不应被受理")
	} else if !strings.Contains(rp.Error, "invalid_argument") {
		t.Fatalf("目标不符应 invalid_argument: %s", rp.Error)
	}
	if handled != 0 {
		t.Fatal("目标不符的 call 进了 handler")
	}
}

// testRedTeamImpersonation：普通身份（service/无 authority）不能 cancel work；
// 只有 owner/runtime 可以（spec §6.5）。
func testRedTeamImpersonation(t *testing.T) {
	memURL := startRedTeamMemory(t)
	workID := "w-impersonate"
	contextWork(t, memURL, "rt-impersonate", workID)
	// 普通服务身份冒充控制者：permission_denied。
	if rp := callJumpFrom(t, dialSPV2(t, memURL), "e2e", "memory", "work_cancel", map[string]any{"work": workID}, mustCtx(t)); rp.OK {
		t.Fatal("普通身份 cancel 必须被拒")
	} else if !strings.Contains(rp.Error, "permission_denied") {
		t.Fatalf("cancel 越权错误码错误: %s", rp.Error)
	}
}

// testRedTeamConceptDrift：帧字段只允许 spec 那一套；文档与代码不许两套词。
func testRedTeamConceptDrift(t *testing.T) {
	want := map[string]bool{
		"v": true, "id": true, "type": true, "work": true, "from": true, "to": true,
		"method": true, "reply_to": true, "content_type": true, "schema": true,
		"idempotency_key": true, "stream": true, "status": true, "error": true, "body": true,
	}
	typ := reflect.TypeOf(spv2.Frame{})
	for i := 0; i < typ.NumField(); i += 1 {
		f := typ.Field(i)
		key := strings.Split(f.Tag.Get("msgpack"), ",")[0]
		if !want[key] {
			t.Fatalf("帧字段 %s 的线路键 %q 不在 spec 字段表里（概念漂移）", f.Name, key)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("帧缺少 spec 字段: %v", want)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "concepts", "spv2.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	for _, key := range []string{"reply_to", "content_type", "idempotency_key", "work", "from", "to"} {
		if !strings.Contains(text, key) {
			t.Fatalf("spec 文档里没有线路键 %q（代码与文档不一致）", key)
		}
	}
}

// testRedTeamSingleton：works / work_messages 的建表语句只允许在 memory 出现一次。
func testRedTeamSingleton(t *testing.T) {
	counts := map[string]int{}
	paths := map[string][]string{}
	legacyHits := map[string][]string{}
	for _, needle := range []string{"CREATE TABLE IF NOT EXISTS works (", "CREATE TABLE IF NOT EXISTS work_messages ("} {
		counts[needle] = 0
	}
	root := filepath.Join("..", "..", "src")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// src/spv2 是协议参考实现（库，不是服务）；红线约束的是"生产服务里只允许 memory 一处"。
		if strings.Contains(filepath.ToSlash(path), "/src/spv2/") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s := string(data)
		for _, legacy := range []string{"user_message", "UserMessagePayload"} {
			if strings.Contains(s, legacy) {
				legacyHits[legacy] = append(legacyHits[legacy], path)
			}
		}
		for needle := range counts {
			if strings.Contains(s, needle) {
				counts[needle] += 1
				paths[needle] = append(paths[needle], path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 概念/服务文档（除权威 spv2.md）不得再写已删除的入口/往返词汇，否则 agent 会照旧文档写新代码。
	for _, sub := range []string{"concepts", "services"} {
		docDir := filepath.Join("..", "..", "docs", sub)
		docEntries, err := os.ReadDir(docDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range docEntries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") || entry.Name() == "spv2.md" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(docDir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, legacy := range []string{"user_message", "recall"} {
				if strings.Contains(string(data), legacy) {
					t.Fatalf("%s/%s 仍写旧入口 %q（概念漂移）", sub, entry.Name(), legacy)
				}
			}
		}
	}
	for legacy, hits := range legacyHits {
		t.Fatalf("已删除的入口 %q 死灰复燃（概念漂移）: %v", legacy, hits)
	}
	for needle, n := range counts {
		if n != 1 {
			t.Fatalf("%q 出现 %d 次（只允许 memory 一处）: %v", needle, n, paths[needle])
		}
		if !strings.Contains(paths[needle][0], "memory") {
			t.Fatalf("%q 不在 memory 模块: %v", needle, paths[needle])
		}
	}
}

// testRedTeamStateMachine：done 后 cancel、stalled 直接 done 都必须 conflict。
func testRedTeamStateMachine(t *testing.T) {
	memURL := startRedTeamMemory(t)
	workID := "w-statetest"
	contextWork(t, memURL, "rt-state", workID)
	ctx := mustCtx(t)
	c := dialSPV2(t, memURL)
	runtimeRef := spv2.AgentRef("runtime").WithAuthority(spv2.AuthorityRuntime)

	update, err := spv2.NewCall(workID, runtimeRef, spv2.ServiceRef("memory"), "work_update", map[string]any{"work": workID, "status": spv2.WorkDone})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, update); err != nil {
		t.Fatalf("running→done 应允许: %v", err)
	}
	cancelCall, err := spv2.NewCall(workID, runtimeRef, spv2.ServiceRef("memory"), "work_cancel", map[string]any{"work": workID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, cancelCall); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("done 之后 cancel 应 conflict: %v", err)
	}

	// 另一条 work：stalled 直接 done 必须 conflict（stalled 只能 renew→running 或 cancel）。
	workID2 := "w-stalled"
	contextWork(t, memURL, "rt-stalled", workID2)
	stall, err := spv2.NewCall(workID2, runtimeRef, spv2.ServiceRef("memory"), "work_update", map[string]any{"work": workID2, "status": spv2.WorkStalled})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, stall); err != nil {
		t.Fatalf("running→stalled 应允许: %v", err)
	}
	done, err := spv2.NewCall(workID2, runtimeRef, spv2.ServiceRef("memory"), "work_update", map[string]any{"work": workID2, "status": spv2.WorkDone})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, done); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("stalled→done 应 conflict: %v", err)
	}
}

// ---------- 辅助 ----------

func mustCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func metricHas(srv *spv2.Server, name string, labels map[string]string) bool {
	for _, m := range srv.Metrics().Snapshot() {
		if m.Name != name {
			continue
		}
		ok := true
		for k, v := range labels {
			if m.Labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func waitMetric(t *testing.T, srv *spv2.Server, name string, labels map[string]string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if metricHas(srv, name, labels) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("指标 %s%v 没出现: %+v", name, labels, srv.Metrics().Snapshot())
}

// startRedTeamMemory 起 设置中心 + memory 真进程，返回 memory URL。
func startRedTeamMemory(t *testing.T) string {
	t.Helper()
	scBin := buildService(t, "github.com/NouSart/sump/settings-center")
	memBin := buildService(t, "github.com/NouSart/sump/memory")
	scPort := freePort(t)
	memPort := freePort(t)
	centerURL := spv2URL(scPort)
	memURL := spv2URL(memPort)
	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort))
	waitWSReady(t, centerURL, 60*time.Second)
	startProc(t, memBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
		"-center", centerURL,
		"-db", filepath.Join(t.TempDir(), "memory.db"),
	)
	waitWSReady(t, memURL, 60*time.Second)
	return memURL
}

// contextWork 用固定 work id 从 reasoner 身份要一次 context（memory 会 ensure work 行）。
func contextWork(t *testing.T, memURL, cid, workID string) {
	t.Helper()
	c := dialSPV2(t, memURL)
	ctx := mustCtx(t)
	call, err := spv2.NewCall(workID, spv2.ServiceRef("reasoner"), spv2.ServiceRef("memory"), "context", protocol.ContextPayload{
		Conversation: cid,
		Boss:         "e2e",
		Messages:     []protocol.Message{{Role: "user", Content: "红队"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, call); err != nil {
		t.Fatalf("context 建 work 失败: %v", err)
	}
}
