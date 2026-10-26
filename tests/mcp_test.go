package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpTestVersion = "2026-07-28"
const mcpTestInput = `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`
const mcpTestOutput = `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`

// TestMCPBidirectional uses real center/files/timer/batch/mcp processes. The external lookup
// target is an HTTP fixture, not a mock of SUMP's translator. Ingress is exercised
// with independent raw JSON-RPC requests rather than the bridge's SDK client.
func TestMCPBidirectional(t *testing.T) {
	if os.Getenv("SUMP_MCP_FIXTURE") == "1" {
		serveMCPFixture(t)
		return
	}
	runMCPBidirectional(t, false)
}

// This exercises encrypted traffic between real local processes. It deliberately
// does not claim cross-device or public-network acceptance.
func TestMCPRemoteTLSPreparation(t *testing.T) {
	runMCPBidirectional(t, true)
}

func runMCPBidirectional(t *testing.T, useTLS bool) {
	dir := t.TempDir()
	scheme := "http"
	client := http.DefaultClient
	var certFile, keyFile, caFile string
	if useTLS {
		scheme = "https"
		certFile, keyFile, caFile, client = mcpTLSMaterials(t, dir)
	}
	const token = "mcp-ingress-test-01234567890123456789"
	const remoteToken = "mcp-remote-test-01234567890123456789"
	fixtureAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	callsPath := filepath.Join(dir, "remote-calls.jsonl")
	releasePath := filepath.Join(dir, "release-slow")
	fixtureLog, err := os.Create(filepath.Join(dir, "remote.log"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureProcess := exec.Command(os.Args[0], "-test.run=^TestMCPBidirectional$", "-test.timeout=120s")
	fixtureProcess.Env = append(os.Environ(), "SUMP_MCP_FIXTURE=1", "SUMP_MCP_FIXTURE_ADDR="+fixtureAddr, "SUMP_MCP_FIXTURE_TOKEN="+remoteToken, "SUMP_MCP_FIXTURE_CALLS="+callsPath)
	fixtureProcess.Env = append(fixtureProcess.Env, "SUMP_MCP_FIXTURE_RELEASE="+releasePath)
	if useTLS {
		fixtureProcess.Env = append(fixtureProcess.Env, "SUMP_MCP_FIXTURE_CERT="+certFile, "SUMP_MCP_FIXTURE_KEY="+keyFile)
	}
	fixtureProcess.Stdout = fixtureLog
	fixtureProcess.Stderr = fixtureLog
	if err = fixtureProcess.Start(); err != nil {
		fixtureLog.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixtureProcess.Process.Kill(); _ = fixtureProcess.Wait(); _ = fixtureLog.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, e := net.DialTimeout("tcp", fixtureAddr, 100*time.Millisecond)
		if e == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("external fixture process did not start")
		}
		time.Sleep(25 * time.Millisecond)
	}

	policy := map[string]any{
		"exports": []string{"files__read", "timer__sleep", "timer__cancel"},
		"remotes": []any{map[string]any{"id": "catalog", "url": scheme + "://" + fixtureAddr + "/mcp", "token_required": true}, map[string]any{"id": "offline", "url": fmt.Sprintf("http://127.0.0.1:%d/mcp", freePort(t))}},
		"imports": []any{map[string]any{"action": "offline_lookup", "remote": "offline", "tool": "lookup", "input_schema": json.RawMessage(mcpTestInput), "output_schema": json.RawMessage(mcpTestOutput)}, map[string]any{"action": "lookup", "remote": "catalog", "tool": "lookup", "description": "Look up a fixture value", "input_schema": json.RawMessage(mcpTestInput), "output_schema": json.RawMessage(mcpTestOutput)}},
	}
	if useTLS {
		policy["remotes"].([]any)[0].(map[string]any)["ca_file"] = caFile
	}
	policy["imports"] = append(policy["imports"].([]any), map[string]any{"action": "async_lookup", "remote": "catalog", "tool": "lookup", "async": true, "input_schema": json.RawMessage(mcpTestInput), "output_schema": json.RawMessage(mcpTestOutput)})
	policyFile := filepath.Join(dir, "mcp.json")
	writePolicy := func() {
		t.Helper()
		raw, _ := json.Marshal(policy)
		if err := os.WriteFile(policyFile, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writePolicy()
	var bridgeBinary string
	start := func(name string, args ...string) {
		t.Helper()
		bin := buildServiceNamed(t, "github.com/QAQ-awa-QAQ/sump/"+name, "sump-"+name)
		if name == "mcp" {
			bridgeBinary = bin
			if useTLS {
				args = append(args, "-tls-cert", certFile, "-tls-key", keyFile)
			}
		}
		logPath := filepath.Join(dir, name+".log")
		logFile, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err = cmd.Start(); err != nil {
			_ = logFile.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = logFile.Close()
			if t.Failed() {
				raw, _ := os.ReadFile(logPath)
				t.Logf("%s: %s", name, raw)
			}
		})
	}
	addr := func() string { return fmt.Sprintf("127.0.0.1:%d", freePort(t)) }
	center, files, bridge, httpAddr, timer, batch := addr(), addr(), addr(), addr(), addr(), addr()
	centerURL := "ws://" + center + "/ws"
	secretFile := filepath.Join(dir, "mcp-secret.json")
	start("settings-center", "-addr", center, "-settings", filepath.Join(dir, "settings.json"))
	waitWSReady(t, centerURL, 15*time.Second)
	start("files", "-addr", files, "-center", centerURL, "-db", filepath.Join(dir, "files.db"), "-dir", filepath.Join(dir, "files"))
	start("timer", "-addr", timer, "-center", centerURL)
	start("mcp", "-addr", bridge, "-center", centerURL, "-http-addr", httpAddr, "-config", policyFile, "-secret-file", secretFile)
	waitWSReady(t, "ws://"+bridge+"/ws", 15*time.Second)
	start("batch", "-addr", batch, "-center", centerURL, "-db", filepath.Join(dir, "batch.db"))
	waitWSReady(t, "ws://"+batch+"/ws", 15*time.Second)
	obs, rec := registerObserver(t, centerURL)
	probe := startReportProbe(t, centerURL)
	for _, name := range []string{"files", "timer", "batch", "mcp", "e2e-observer"} {
		waitRosterHas(t, obs, name, 15*time.Second)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for key, value := range map[string]string{"http.token": token, "remote.catalog.token": remoteToken} {
		view := findServiceSetting(t, listSettings(t, obs, "mcp"), "mcp", key)
		if !view.Secret || view.Value != "" || view.Default != "" {
			t.Fatalf("secret declaration leaked: %s", key)
		}
		rp := callJump(t, obs, "settings-center", "set_setting", protocol.SetSettingPayload{Service: "mcp", Key: key, Value: value}, ctx)
		if !rp.OK || bytes.Contains(rp.Data, []byte(value)) {
			t.Fatalf("secret update rejected or leaked: %s", key)
		}
		if event := waitSetEvent(t, rec, "mcp", key, 5*time.Second); !event.OK || event.Value != "" {
			t.Fatalf("secret event failed/leaked: %s", key)
		}
		waitSecretFlag(t, obs, "mcp", key, true, 5*time.Second)
	}
	// Batch admission is separate from Tool and Async. The translator has not
	// declared imported capabilities batchable: an accepted async receipt must
	// never be counted by batch as a completed remote operation.
	batchClient, err := protocol.Dial("ws://"+batch+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer batchClient.Close()
	batchInput, err := protocol.EncodePayload(map[string]any{"query": "must-not-execute"})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"lookup", "async_lookup"} {
		rp := callJump(t, batchClient, "batch", "run", protocol.BatchRunPayload{
			Service: "mcp", Action: action,
			Items: []protocol.BatchItem{{Input: batchInput}},
		}, ctx)
		if rp.OK || !strings.Contains(rp.Error, "BATCH_NOT_BATCHABLE") {
			t.Fatalf("batch accepted undeclared MCP capability %s: %+v", action, rp)
		}
	}
	if calls, err := os.ReadFile(callsPath); err != nil || len(calls) != 0 {
		t.Fatalf("batch rejection executed a remote tool: %s %v", calls, err)
	}
	if useTLS {
		// The offline binding makes exit status nonzero, while both catalog
		// contracts must pass. Discovery must not execute even one business call.
		check := exec.CommandContext(ctx, bridgeBinary, "-check-remotes", "-config", policyFile, "-secret-file", secretFile, "-call-timeout", "2s")
		check.Dir = dir
		output, checkErr := check.CombinedOutput()
		var diagnosis struct {
			OK     bool `json:"ok"`
			Checks []struct {
				Remote string `json:"remote"`
				OK     bool   `json:"ok"`
			} `json:"checks"`
		}
		if checkErr == nil || json.Unmarshal(output, &diagnosis) != nil || diagnosis.OK || len(diagnosis.Checks) != 3 {
			t.Fatalf("preflight result: %s %v", output, checkErr)
		}
		for _, item := range diagnosis.Checks {
			if item.OK != (item.Remote == "catalog") {
				t.Fatalf("wrong preflight result: %s", output)
			}
		}
		if calls, _ := os.ReadFile(callsPath); len(calls) != 0 {
			t.Fatal("preflight executed a tool")
		}
		if bytes.Contains(output, []byte(remoteToken)) || bytes.Contains(output, []byte(token)) {
			t.Fatal("preflight leaked credential")
		}
	}
	endpoint := scheme + "://" + httpAddr + "/mcp"
	// No initialization or session ID is sent on the external request path.
	rawCall := func(method string, params map[string]any) map[string]any {
		t.Helper()
		params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": mcpTestVersion, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "raw-e2e", "version": "1"}, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "raw-correlation", "method": method, "params": params})
		req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", mcpTestVersion)
		req.Header.Set("Mcp-Method", method)
		if name, ok := params["name"].(string); ok {
			req.Header.Set("Mcp-Name", name)
		}
		if uri, ok := params["uri"].(string); ok && method == "resources/read" {
			req.Header.Set("Mcp-Name", uri)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 256<<10))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if res.StatusCode != http.StatusOK || json.Unmarshal(data, &out) != nil || out["id"] != "raw-correlation" {
			t.Fatalf("invalid raw reply: %d %s", res.StatusCode, data)
		}
		if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Mcp-Session-Id") != "" {
			t.Fatal("unexpected cache/session semantics")
		}
		return out
	}
	tools := rawCall("tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 3 || tools[0].(map[string]any)["name"] != "files__read" || tools[1].(map[string]any)["name"] != "timer__cancel" || tools[2].(map[string]any)["name"] != "timer__sleep" {
		t.Fatalf("imports leaked through exports: %+v", tools)
	}
	fc, err := protocol.Dial("ws://"+files+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()
	const sample = "真实文件 hello 世界"
	savedReply := callJump(t, fc, "files", "save", protocol.FileSavePayload{Filename: "sample.txt", Data: base64.StdEncoding.EncodeToString([]byte(sample))}, ctx)
	if !savedReply.OK {
		t.Fatal(savedReply.Error)
	}
	var saved protocol.FileSaveResult
	if err = protocol.DecodeRaw(savedReply.Data, &saved); err != nil {
		t.Fatal(err)
	}
	read := rawCall("tools/call", map[string]any{"name": "files__read", "arguments": map[string]any{"id": saved.ID}})
	result := read["result"].(map[string]any)
	if result["isError"] == true || result["structuredContent"].(map[string]any)["text"] != sample {
		t.Fatalf("file translation failed: %+v", result)
	}
	bc, err := protocol.Dial("ws://"+bridge+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	callLookup := func(query string) {
		t.Helper()
		raw, _ := protocol.EncodePayload(map[string]any{"query": query})
		env, _ := protocol.NewEnvelope(protocol.TypeJump, "e2e-caller", "mcp", "trace-lookup", protocol.JumpPayload{Action: "lookup", Input: raw})
		reply, err := bc.Call(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
		if reply.ID != env.ID || reply.Trace != env.Trace || reply.To != env.From {
			t.Fatalf("response association lost: %+v", reply)
		}
		var rp protocol.ResponsePayload
		if err = reply.DecodePayload(&rp); err != nil || !rp.OK {
			t.Fatalf("import failed: %+v %v", rp, err)
		}
		var out struct {
			Phase      string         `msgpack:"phase"`
			IsError    bool           `msgpack:"is_error"`
			Structured map[string]any `msgpack:"structured_content"`
		}
		if err = protocol.DecodeRaw(rp.Data, &out); err != nil || out.Phase != "completed" || out.IsError || out.Structured["value"] != "found:"+query {
			t.Fatalf("import output: %+v %v", out, err)
		}
	}
	if failure := callJump(t, bc, "mcp", "offline_lookup", map[string]any{"query": "x"}, ctx); failure.OK || !strings.Contains(failure.Error, "not submitted") {
		t.Fatalf("offline target: %+v", failure)
	}
	callLookup("并发框架")
	live := rawCall("tools/call", map[string]any{"name": "files__read", "arguments": map[string]any{"id": saved.ID}})
	if live["result"].(map[string]any)["isError"] == true {
		t.Fatal("offline import blocked local export")
	}
	if hidden := callJump(t, bc, "mcp", "hidden", map[string]any{}, ctx); hidden.OK {
		t.Fatal("unmapped remote action callable")
	}
	rosterReply := callJump(t, obs, "settings-center", "roster", nil, ctx)
	var roster protocol.RosterPayload
	if err = protocol.DecodeRaw(rosterReply.Data, &roster); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"settings-center": true, "files": true, "timer": true, "batch": true, "mcp": true, "e2e-settings-observer": true, "e2e-observer": true}
	for _, card := range roster.Services {
		if !allowed[card.Name] {
			t.Fatalf("foreign server registered as service: %s", card.Name)
		}
		if card.Name == "mcp" {
			if len(card.Provides) != 3 || card.Provides[0].Action != "async_lookup" || !card.Provides[0].Async || card.Provides[1].Action != "lookup" || card.Provides[2].Action != "offline_lookup" || !card.Provides[1].Tool || card.Provides[1].Async {
				t.Fatalf("wrong import declaration: %+v", card.Provides)
			}
			for _, provide := range card.Provides {
				if provide.Batchable {
					t.Fatalf("import acquired implicit batch permission: %+v", provide)
				}
			}
		}
	}
	// Two independent HTTP requests start timers. Control remains available at
	// the principal's full in-flight budget; one completes and one is canceled.
	startTimer := func(ms int) (string, string) {
		t.Helper()
		out := rawCall("tools/call", map[string]any{"name": "timer__sleep", "arguments": map[string]any{"ms": ms, "note": "M2 integration"}})["result"].(map[string]any)
		if out["isError"] == true {
			t.Fatalf("timer failed: %+v", out)
		}
		data := out["structuredContent"].(map[string]any)
		return data["job_id"].(string), data["resource_uri"].(string)
	}
	_, doneURI := startTimer(100)
	cancelID, cancelURI := startTimer(60000)
	canceled := rawCall("tools/call", map[string]any{"name": "timer__cancel", "arguments": map[string]any{"job_id": cancelID}})["result"].(map[string]any)
	if canceled["isError"] == true || canceled["structuredContent"].(map[string]any)["status"] != "canceled" {
		t.Fatalf("timer cancel: %+v", canceled)
	}
	for uri, phase := range map[string]string{doneURI: "done", cancelURI: "canceled"} {
		limit := time.Now().Add(5 * time.Second)
		for {
			r := rawCall("resources/read", map[string]any{"uri": uri})
			if r["error"] != nil {
				t.Fatalf("resource read: %+v", r)
			}
			text := r["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)["text"].(string)
			var snapshot struct {
				Phase string `json:"phase"`
				Final bool   `json:"final"`
			}
			if json.Unmarshal([]byte(text), &snapshot) != nil {
				t.Fatal(text)
			}
			if snapshot.Final {
				if snapshot.Phase != phase {
					t.Fatal(text)
				}
				break
			}
			if time.Now().After(limit) {
				t.Fatal("timer result did not arrive")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// The remote fixture blocks slow until an explicit release file appears.
	// Receiving fast first proves receipts detach from the handler and independent
	// work is not serialized behind slow, without a latency threshold assertion.
	asyncCall := func(query string) string {
		t.Helper()
		rp := callJump(t, bc, "mcp", "async_lookup", map[string]any{"query": query}, ctx)
		var receipt struct {
			ItemID string `msgpack:"item_id"`
			Phase  string `msgpack:"phase"`
		}
		if !rp.OK || protocol.DecodeRaw(rp.Data, &receipt) != nil || receipt.ItemID == "" || receipt.Phase != "accepted" {
			t.Fatalf("async receipt: %+v %+v", rp, receipt)
		}
		return receipt.ItemID
	}
	slowID := asyncCall("slow")
	fastID := asyncCall("fast")
	fastReport := probe.waitReport(t, "completed", 5*time.Second)
	if fastReport.ItemID != fastID || !fastReport.Final || !strings.Contains(fastReport.Text, "found:fast") {
		t.Fatalf("results serialized or crossed: %+v", fastReport)
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	slowReport := probe.waitReport(t, "completed", 5*time.Second)
	if slowReport.ItemID != slowID || !slowReport.Final || !strings.Contains(slowReport.Text, "found:slow") {
		t.Fatalf("slow result lost/crossed: %+v", slowReport)
	}
	policy["exports"] = []string{}
	writePolicy()
	deadline = time.Now().Add(5 * time.Second)
	for {
		tools = rawCall("tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any)
		if len(tools) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("export revocation did not apply")
		}
		time.Sleep(25 * time.Millisecond)
	}
	denied := rawCall("tools/call", map[string]any{"name": "files__read", "arguments": map[string]any{"id": saved.ID}})
	if denied["result"].(map[string]any)["isError"] != true {
		t.Fatal("revoked export still callable")
	}
	callLookup("still available")
	callsData, err := os.ReadFile(callsPath)
	if err != nil || len(strings.Split(strings.TrimSpace(string(callsData)), "\n")) != 4 {
		t.Fatalf("unexpected target calls: %s %v", callsData, err)
	}
	for _, name := range []string{"settings-center", "files", "timer", "batch", "mcp"} {
		data, _ := os.ReadFile(filepath.Join(dir, name+".log"))
		if strings.Contains(string(data), token) || strings.Contains(string(data), remoteToken) {
			t.Fatalf("credential leaked in %s log", name)
		}
	}
	t.Log("verified external raw MCP -> real files.read; SUMP WS -> external lookup; batch rejects undeclared capabilities without remote execution; explicit exports/imports; secret updates; response association")
}

// This function runs in a separate test-binary process. A file of actual remote
// inputs provides an independent oracle for target dispatch counts.
func serveMCPFixture(t *testing.T) {
	remoteToken := os.Getenv("SUMP_MCP_FIXTURE_TOKEN")
	calls, err := os.OpenFile(os.Getenv("SUMP_MCP_FIXTURE_CALLS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer calls.Close()
	var input, output any
	_ = json.Unmarshal([]byte(mcpTestInput), &input)
	_ = json.Unmarshal([]byte(mcpTestOutput), &output)
	remote := mcp.NewServer(&mcp.Implementation{Name: "catalog-fixture", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{mcpTestVersion}})
	remote.AddTool(&mcp.Tool{Name: "lookup", InputSchema: input, OutputSchema: output}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if _, err := calls.WriteString(string(req.Params.Arguments) + "\n"); err != nil {
			t.Error(err)
		}
		var in struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, err
		}
		if in.Query == "slow" {
			for {
				if _, err := os.Stat(os.Getenv("SUMP_MCP_FIXTURE_RELEASE")); err == nil {
					break
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "found:" + in.Query}}, StructuredContent: map[string]any{"value": "found:" + in.Query}}, nil
	})
	remote.AddTool(&mcp.Tool{Name: "hidden", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		t.Error("unmapped tool called")
		return &mcp.CallToolResult{}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true})
	listener, err := net.Listen("tcp", os.Getenv("SUMP_MCP_FIXTURE_ADDR"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+remoteToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second}
	if cert := os.Getenv("SUMP_MCP_FIXTURE_CERT"); cert != "" {
		err = server.ServeTLS(listener, cert, os.Getenv("SUMP_MCP_FIXTURE_KEY"))
	} else {
		err = server.Serve(listener)
	}
	if err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
