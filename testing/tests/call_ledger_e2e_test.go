package tests

// R-030 端到端：真 settings-center + memory 三进程 + timer + reasoner + 假 boss/假 LLM。
// 验证一条完整链：模型调 timer.sleep → reasoner 派发前 call_open → owner 受理回执经
// reasoner relay 成 accepted 补报（回填 calls.remote_item_id）→ timer 到点补报 relay 成
// done（推进账本）。断言直接读会话库：calls 行有 owner 句柄且已关闭，report_receipts
// 有 accepted + done 两条。
//
// 为什么直接读库：memory 没有「按 work 列出 calls」的读接口（call_status 只按 call_id 读，
// 而 call_id 是 memory 在 call_open 时分配的，测试进程看不到）。这里只读终态账本，
// 不参与写入。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
	_ "modernc.org/sqlite"
)

// ledgerLLM 是 OpenAI 兼容的假模型：定时器剧本（sleep → wait → task_status → 文本）。
type ledgerLLM struct {
	mu    sync.Mutex
	calls int
	srv   *httptest.Server
}

func startLedgerLLM(t *testing.T) *ledgerLLM {
	t.Helper()
	l := &ledgerLLM{}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		l.mu.Lock()
		l.calls++
		n := l.calls
		l.mu.Unlock()

		var msg map[string]any
		switch n {
		case 1:
			msg = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call_timer", "type": "function",
				"function": map[string]any{"name": "timer__sleep", "arguments": `{"ms":300,"note":"e2e 账本"}`},
			}}}
		case 2:
			msg = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call_wait", "type": "function",
				"function": map[string]any{"name": "wait", "arguments": `{}`},
			}}}
		case 3:
			msg = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call_judge", "type": "function",
				"function": map[string]any{"name": "task_status", "arguments": `{"status":"done"}`},
			}}}
		default:
			msg = map[string]any{"role": "assistant", "content": "E2E 计时器已送达"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *ledgerLLM) URL() string { return l.srv.URL }

// openLedgerDB 只读打开会话库（会话进程仍持有写句柄；只读连接不参与写入）。
func openLedgerDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("只读打开会话库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// waitLedgerRow 轮询 calls 行，直到该 work 的调用已关闭并拿到 owner 句柄。
func waitLedgerRow(t *testing.T, db *sql.DB, work string) (callID, remote string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var (
			id, service, action, mode, observe, exec, phase string
			pausable                                        int
			remoteID, reason                                string
		)
		err := db.QueryRow(
			`SELECT id, service, action, COALESCE(mode, ''), pausable,
			        COALESCE(remote_item_id, ''), observe_state, exec_state,
			        COALESCE(phase, ''), COALESCE(reason, '')
			   FROM calls WHERE work = ? ORDER BY created_at DESC LIMIT 1`, work,
		).Scan(&id, &service, &action, &mode, &pausable, &remoteID, &observe, &exec, &phase, &reason)
		if err == nil && remoteID != "" && exec == "done" {
			if service != "timer" || action != "sleep" || mode != "once" || pausable != 1 {
				t.Fatalf("call 行字段不符: service=%q action=%q mode=%q pausable=%d", service, action, mode, pausable)
			}
			if observe != "closed" || phase != "done" {
				t.Fatalf("call 终态不符: observe=%q exec=%q phase=%q", observe, exec, phase)
			}
			if reason != "" {
				t.Fatalf("正常终态不应带 reason（%q）", reason)
			}
			return id, remoteID
		}
		if err != nil && err != sql.ErrNoRows {
			t.Fatalf("读 calls 行失败: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 work=%s 的 calls 行关闭且回填 remote_item_id 超时", work)
	return "", ""
}

// TestCallLedgerE2E：timer 一条完整链，账本从 created 走到 done，句柄来自 owner 回执。
func TestCallLedgerE2E(t *testing.T) {
	scBin := buildService(t, "github.com/NouSart/sump/settings-center")
	timerBin := buildServiceNamed(t, "github.com/NouSart/sump/timer", "sump-timer")
	rsBin := buildService(t, "github.com/NouSart/sump/reasoner")

	scPort, timerPort, rsPort := freePort(t), freePort(t), freePort(t)
	centerURL := spv2URL(scPort)
	reasonerURL := spv2URL(rsPort)

	// -settings ""：本用例不改全局设置，测试中心也不要落盘到仓库 data/。
	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort), "-settings", "")
	waitWSReady(t, centerURL, 60*time.Second)

	dir := t.TempDir()
	dbFile := filepath.Join(dir, "memory.db")
	memURL := startMemoryStackAt(t, centerURL, nil, dbFile, filepath.Join(dir, "experience.db"))

	boss := startE2EBoss(t)
	bossClient := boss.register(t, centerURL)
	t.Cleanup(func() { _ = bossClient.Close() })

	startProc(t, timerBin, "-addr", fmt.Sprintf("127.0.0.1:%d", timerPort), "-center", centerURL)

	llm := startLedgerLLM(t)
	startProc(t, rsBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", rsPort),
		"-center", centerURL,
		"-llm-base", llm.URL(),
		"-llm-key", "test-key",
		"-llm-model", "fake-model",
		"-reflect=false",
	)
	waitWSReady(t, reasonerURL, 60*time.Second)

	obs := dialSPV2(t, centerURL)
	waitRosterHas(t, obs, "reasoner", 60*time.Second)
	waitRosterHas(t, obs, "memory", 10*time.Second)
	waitRosterHas(t, obs, "timer", 10*time.Second)

	const work = "e2e-call-ledger"
	ac := dialSPV2(t, reasonerURL)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	call, err := spv2.NewCall(work, spv2.ServiceRef("e2e-boss"), spv2.ServiceRef("reasoner"), "message",
		protocol.MessagePayload{
			Text: "定个计时器，验证调用账本", Conversation: "e2e-call-ledger-conv",
			Sender: &protocol.Sender{Channel: "private", ID: "e2e-boss", Owner: true},
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Call(ctx, call); err != nil {
		t.Fatalf("message 未被受理: %v", err)
	}

	// 交付不是本用例的重点，但它证明整条链没有卡死。
	select {
	case <-boss.delivered:
	case <-time.After(30 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	db := openLedgerDB(t, dbFile)
	callID, remote := waitLedgerRow(t, db, work)
	if remote == "" {
		t.Fatal("calls.remote_item_id 未回填（owner 受理句柄）")
	}

	// 补报账本：accepted（受理回执）+ done（timer 到点），seq 单调且同属一个 call。
	rows, err := db.Query(
		`SELECT report_seq, phase, final, call_id FROM report_receipts
		   WHERE work = ? AND source_service = 'timer' ORDER BY report_seq`, work)
	if err != nil {
		t.Fatalf("读 report_receipts 失败: %v", err)
	}
	defer func() { _ = rows.Close() }()
	type receipt struct {
		seq    int64
		phase  string
		final  int
		callID string
	}
	var got []receipt
	for rows.Next() {
		var r receipt
		if err := rows.Scan(&r.seq, &r.phase, &r.final, &r.callID); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Fatalf("补报回执应至少有 accepted + done 两条，实际 %d: %+v", len(got), got)
	}
	if got[0].phase != "accepted" || got[0].final != 0 || got[0].seq != 1 {
		t.Fatalf("第一条补报应是 accepted/非终态/seq=1，实际 %+v", got[0])
	}
	if got[len(got)-1].phase != "done" || got[len(got)-1].final != 1 {
		t.Fatalf("最后一条补报应是 done/终态，实际 %+v", got[len(got)-1])
	}
	for _, r := range got {
		if r.callID != callID {
			t.Fatalf("补报应指向同一个 call %s，实际 %+v", callID, r)
		}
	}

	// memory 门面对外仍可用（会话进程没被测试的只读连接打扰）。
	memClient := dialSPV2(t, memURL)
	if rp := callJumpFrom(t, memClient, "e2e-boss", "memory", "history",
		map[string]any{"conversation_id": "e2e-call-ledger-conv", "limit": 5}, ctx); !rp.OK {
		t.Fatalf("账本写入后 memory.history 失败: %s", rp.Error)
	}
}
