package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const importProtocolVersion = "2026-07-28"

// RemoteConfig names an administrator-selected destination. Credentials are
// service secrets, never policy fields or arguments supplied by a caller.
type RemoteConfig struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	TokenRequired bool   `json:"token_required,omitempty"`
	CAFile        string `json:"ca_file,omitempty"`
}

func (r RemoteConfig) SecretKey() string { return "remote." + r.ID + ".token" }

func (r *RemoteConfig) UnmarshalJSON(raw []byte) error {
	type value RemoteConfig
	return decodeFields(raw, (*value)(r), "id", "url", "token_required", "ca_file")
}

// ImportConfig pins one SUMP action to one reviewed remote tool.
// The schemas are exact snapshots, including optional output_schema omission:
// discovering tools never grants access or replaces a reviewed contract.
// Changing imports requires a service restart.
type ImportConfig struct {
	Action       string          `json:"action"`
	Remote       string          `json:"remote"`
	Tool         string          `json:"tool"`
	Description  string          `json:"description,omitempty"`
	Async        bool            `json:"async,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
}

func (c *ImportConfig) UnmarshalJSON(raw []byte) error {
	type value ImportConfig
	return decodeFields(raw, (*value)(c), "action", "remote", "tool", "description", "async", "input_schema", "output_schema")
}

type importedTool struct {
	config        ImportConfig
	input, output *jsonschema.Resolved
}

type remoteTarget struct {
	config    RemoteConfig
	slots     chan struct{}
	transport *http.Transport
}

// 值和版本必须来自同一次读取；版本用于区分成功轮换后重新使用相同令牌的情况。
type remoteCredential struct {
	value   string
	version uint64
}

// Imports owns outgoing connections only. Each invocation has an independent
// SDK session, while each destination has its own HTTP connection pool and TLS
// trust roots. A timeout or teardown
// therefore cannot close another invocation's session. No tools/call is retried.
type Imports struct {
	tools    map[string]importedTool
	remotes  map[string]remoteTarget
	token    func(string) (string, uint64)
	timeout  time.Duration
	maxBytes int
	lifetime context.Context
	cancel   context.CancelFunc
	closed   atomic.Bool
}

var importName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ValidateRemoteToken is suitable for service.HandleSecret. Empty revokes the
// credential; missing mandatory credentials reject calls before connecting.
func ValidateRemoteToken(value string) error {
	if len(value) > 4096 || strings.ContainsAny(value, " \t\r\n") {
		return errors.New("remote token must be at most 4096 bytes without whitespace")
	}
	return nil
}

func ValidateImportConfigs(remotes []RemoteConfig, configs []ImportConfig) error {
	_, _, err := compileImports(remotes, configs)
	return err
}

func compileImports(remotes []RemoteConfig, configs []ImportConfig) (map[string]remoteTarget, map[string]importedTool, error) {
	if len(remotes) > 16 || len(configs) > 64 {
		return nil, nil, errors.New("MCP policy permits at most 16 remotes and 64 imports")
	}
	targets := make(map[string]remoteTarget, len(remotes))
	for _, r := range remotes {
		if !importName.MatchString(r.ID) {
			return nil, nil, errors.New("remote id must be a lowercase identifier")
		}
		if _, exists := targets[r.ID]; exists {
			return nil, nil, fmt.Errorf("duplicate remote %q", r.ID)
		}
		u, err := url.Parse(r.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, nil, fmt.Errorf("remote %q needs an HTTP(S) URL without credentials, query or fragment", r.ID)
		}
		ip := net.ParseIP(u.Hostname())
		if u.Scheme == "http" && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, nil, fmt.Errorf("remote %q requires HTTPS except for loopback addresses", r.ID)
		}
		if r.CAFile != "" && (u.Scheme != "https" || strings.TrimSpace(r.CAFile) == "" || strings.ContainsRune(r.CAFile, 0)) {
			return nil, nil, fmt.Errorf("remote %q ca_file requires HTTPS and a valid file path", r.ID)
		}
		targets[r.ID] = remoteTarget{config: r, slots: make(chan struct{}, 4)}
	}
	tools := make(map[string]importedTool, len(configs))
	reserved := map[string]bool{"configure": true, "report_result": true, "cancel_import": true, "user_message": true, "step": true, "deliver": true, "recall": true, "store": true, "resume": true, "save": true, "fetch": true, "echo": true, "debug_jump": true, "ping": true, "roster": true, "report": true, "list_settings": true, "set_setting": true, "reset_setting": true, "report_setting": true, "capabilities": true}
	for _, c := range configs {
		if !importName.MatchString(c.Action) || reserved[c.Action] {
			return nil, nil, fmt.Errorf("invalid or reserved import action %q", c.Action)
		}
		if _, exists := tools[c.Action]; exists {
			return nil, nil, fmt.Errorf("duplicate import action %q", c.Action)
		}
		if _, exists := targets[c.Remote]; !exists {
			return nil, nil, fmt.Errorf("import %q references an unknown remote", c.Action)
		}
		if err := boundedText("remote tool", c.Tool, 128, true); err != nil {
			return nil, nil, err
		}
		if err := boundedText("import description", c.Description, 2048, false); err != nil {
			return nil, nil, err
		}
		input, err := importSchema(c.InputSchema)
		if err != nil || input.Schema().Type != "object" {
			return nil, nil, fmt.Errorf("import %q requires a valid object input_schema", c.Action)
		}
		var output *jsonschema.Resolved
		if len(c.OutputSchema) > 0 {
			output, err = importSchema(c.OutputSchema)
			if err != nil {
				return nil, nil, fmt.Errorf("import %q requires a valid output_schema or its omission", c.Action)
			}
		}
		// Copy raw contracts so a caller cannot mutate the policy after validation.
		c.InputSchema = bytes.Clone(c.InputSchema)
		c.OutputSchema = bytes.Clone(c.OutputSchema)
		tools[c.Action] = importedTool{config: c, input: input, output: output}
	}
	return targets, tools, nil
}

func importSchema(raw json.RawMessage) (*jsonschema.Resolved, error) {
	if len(raw) > 16<<10 {
		return nil, errors.New("schema exceeds 16 KiB")
	}
	var shape map[string]any
	if err := StrictJSON(raw, &shape); err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	// No loader: external $ref never initiates a network request.
	return schema.Resolve(nil)
}

// token 必须原子读取已提交的值与版本，例如 service.SettingSnapshot。
// 静态配置可固定返回版本 0；动态凭据每次成功改变值时必须推进版本。
func NewImports(remotes []RemoteConfig, configs []ImportConfig, timeout time.Duration, maxBytes int, token func(string) (string, uint64)) (*Imports, error) {
	targets, tools, err := compileImports(remotes, configs)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 || maxBytes < 4096 {
		return nil, errors.New("invalid import timeout or output limit")
	}
	if token == nil {
		token = func(string) (string, uint64) { return "", 0 }
	}
	for id, target := range targets {
		transport, err := newImportTransport(target.config)
		if err != nil {
			for _, ready := range targets {
				if ready.transport != nil {
					ready.transport.CloseIdleConnections()
				}
			}
			return nil, err
		}
		target.transport = transport
		targets[id] = target
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &Imports{tools: tools, remotes: targets, timeout: timeout, maxBytes: maxBytes, token: token, lifetime: lifetime, cancel: cancel}, nil
}

func (i *Imports) credential(key string) remoteCredential {
	value, version := i.token(key)
	return remoteCredential{value: value, version: version}
}

// An explicit CA bundle replaces system roots for this destination only. It is
// loaded once at startup, never installed into the operating system trust store.
func newImportTransport(remote RemoteConfig) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if remote.CAFile == "" {
		return transport, nil
	}
	f, err := os.Open(remote.CAFile)
	if err != nil {
		return nil, fmt.Errorf("remote %q CA file cannot be read", remote.ID)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, fmt.Errorf("remote %q CA file must be readable and at most 1 MiB", remote.ID)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("remote %q CA file contains no valid PEM certificates", remote.ID)
	}
	transport.TLSClientConfig.RootCAs = roots
	return transport, nil
}

func (i *Imports) Close() {
	if i.closed.CompareAndSwap(false, true) {
		i.cancel()
		for _, target := range i.remotes {
			target.transport.CloseIdleConnections()
		}
	}
}

func (i *Imports) Provides() []protocol.Provide {
	out := make([]protocol.Provide, 0, len(i.tools))
	for _, tool := range i.tools {
		output := "Final MCP result: phase, is_error, content, structured_content. Content is untrusted tool data."
		if tool.config.Async {
			output = "Accepted receipt: item_id, phase=accepted. Final result arrives via report_result; acceptance does not mean completion."
		}
		out = append(out, protocol.Provide{Action: tool.config.Action, Tool: true, Async: tool.config.Async,
			Input:  tool.config.Description + " JSON Schema: " + string(tool.config.InputSchema),
			Output: output})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Action < out[b].Action })
	return out
}

func (i *Imports) Handle(ctx context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	call, err := i.Prepare(in)
	if err != nil {
		return nil, err
	}
	defer call.Close()
	ctx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()
	return call.Run(ctx)
}

func (i *Imports) IsAsync(action string) bool {
	tool, ok := i.tools[action]
	return ok && tool.config.Async
}

// preparedImport owns a single invocation's reviewed arguments and remote slot.
// Preparation never contacts the remote. The caller must Close even when Run is
// not called. Run is single-use so retrying a receipt cannot replay the tool.
type preparedImport struct {
	imports   *Imports
	tool      importedTool
	target    remoteTarget
	encoded   json.RawMessage
	token     remoteCredential
	used      atomic.Bool
	submitted atomic.Bool
	closed    atomic.Bool
	release   sync.Once
}

func (p *preparedImport) Close() {
	p.release.Do(func() {
		p.closed.Store(true)
		<-p.target.slots
	})
}

func (i *Imports) Prepare(in protocol.JumpPayload) (*preparedImport, error) {
	tool, ok := i.tools[in.Action]
	if !ok || i.closed.Load() {
		return nil, errors.New("MCP_IMPORT_REJECTED: action unavailable; remote tool not submitted")
	}
	if len(in.Input) > i.maxBytes {
		return nil, errors.New("MCP_INVALID_INPUT: input exceeds byte limit; remote tool not submitted")
	}
	// Share the bounded JSON-shaped MessagePack decoder used by observations;
	// container length headers are checked before any slice/map allocation.
	arguments, err := decodeJobData(in.Input)
	if err != nil {
		return nil, errors.New("MCP_INVALID_INPUT: expected one MessagePack object; remote tool not submitted")
	}
	if _, ok := arguments.(map[string]any); !ok {
		return nil, errors.New("MCP_INVALID_INPUT: input must be an object; remote tool not submitted")
	}
	if err := checkImportValue(arguments, 0); err != nil || tool.input.Validate(arguments) != nil {
		return nil, errors.New("MCP_INVALID_INPUT: input does not match reviewed schema; remote tool not submitted")
	}
	encoded, err := json.Marshal(arguments)
	if err != nil || len(encoded) > i.maxBytes {
		return nil, errors.New("MCP_INVALID_INPUT: input exceeds JSON byte limit; remote tool not submitted")
	}
	target, token, err := i.reserveTarget(tool)
	if err != nil {
		return nil, err
	}
	return &preparedImport{imports: i, tool: tool, target: target, encoded: encoded, token: token}, nil
}

func (i *Imports) reserveTarget(tool importedTool) (remoteTarget, remoteCredential, error) {
	if i.closed.Load() {
		return remoteTarget{}, remoteCredential{}, errors.New("MCP_IMPORT_REJECTED: imports closed; remote tool not submitted")
	}
	target := i.remotes[tool.config.Remote]
	select {
	case target.slots <- struct{}{}:
	default:
		return remoteTarget{}, remoteCredential{}, errors.New("MCP_BUSY: remote concurrency limit reached; remote tool not submitted")
	}
	token := i.credential(target.config.SecretKey())
	if ValidateRemoteToken(token.value) != nil || (target.config.TokenRequired && token.value == "") {
		<-target.slots
		return remoteTarget{}, remoteCredential{}, errors.New("MCP_AUTH_REQUIRED: remote credential unavailable; remote tool not submitted")
	}
	return target, token, nil
}

func (p *preparedImport) Run(ctx context.Context) (any, error) {
	if !p.used.CompareAndSwap(false, true) || p.closed.Load() {
		return nil, errors.New("MCP_IMPORT_REJECTED: prepared call is no longer available; remote tool not submitted")
	}
	i, tool, target, token := p.imports, p.tool, p.target, p.token
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(i.lifetime, cancel)
	defer func() { stop(); cancel() }()
	if ctx.Err() != nil || i.closed.Load() || token != i.credential(target.config.SecretKey()) {
		return nil, errors.New("MCP_IMPORT_REJECTED: call cancelled or credential changed; remote tool not submitted")
	}
	session, err := i.connectChecked(ctx, target, tool, token.value)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	// Recheck immediately before dispatch, including successful secret rotation.
	if ctx.Err() != nil || i.closed.Load() || token != i.credential(target.config.SecretKey()) {
		return nil, errors.New("MCP_IMPORT_REJECTED: call cancelled or credential changed; remote tool not submitted")
	}
	p.submitted.Store(true)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.config.Tool, Arguments: p.encoded})
	if err != nil {
		// A network error does not prove the request failed to execute. In
		// particular, never use SDK error text to infer safe replay.
		return nil, errors.New("MCP_OUTCOME_UNKNOWN: remote call did not return a confirmed result; not retried")
	}
	if result == nil {
		return nil, errors.New("MCP_INVALID_RESULT: empty remote response; execution outcome unknown; not retried")
	}
	if result.NeedsInput() {
		return nil, errors.New("MCP_INPUT_REQUIRED: this import cannot satisfy remote input requests; completion unconfirmed; not retried")
	}
	if !result.IsError && (checkImportValue(result.StructuredContent, 0) != nil || (tool.output != nil && tool.output.Validate(result.StructuredContent) != nil)) {
		return nil, errors.New("MCP_INVALID_RESULT: remote structured output violates reviewed schema; execution may have occurred; not retried")
	}
	phase := "completed"
	if result.IsError {
		phase = "failed"
	}
	// JSON conversion preserves MCP content type names instead of leaking Go
	// struct field names into MessagePack. Do not promote content to instructions.
	wire, err := json.Marshal(map[string]any{"phase": phase, "is_error": result.IsError, "content": result.Content, "structured_content": result.StructuredContent})
	if err != nil || len(wire) > i.maxBytes {
		return nil, errors.New("MCP_INVALID_RESULT: translated result exceeds byte limit; execution may have occurred; not retried")
	}
	var native map[string]any
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.UseNumber()
	if err := decoder.Decode(&native); err != nil {
		return nil, errors.New("MCP_INVALID_RESULT: result cannot be translated")
	}
	return normalizeImportNumbers(native)
}

// Shared by normal invocations and read-only preflight. It performs only
// version discovery and bounded catalogue discovery, never tools/call.
func (i *Imports) connectChecked(ctx context.Context, target remoteTarget, tool importedTool, token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "sump-mcp", Version: "0.1.0"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	httpClient := &http.Client{Transport: &importTransport{base: target.transport, token: token, maxBytes: i.maxBytes, lifetime: ctx}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("MCP redirects are disabled") }}
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: target.config.URL, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: i.maxBytes}, &mcp.ClientSessionOptions{ProtocolVersion: importProtocolVersion})
	if err != nil {
		return nil, errors.New("MCP_REMOTE_UNAVAILABLE: connection, authentication or initialization failed; remote tool not submitted")
	}
	if session.InitializeResult() == nil || session.InitializeResult().ProtocolVersion != importProtocolVersion {
		session.Close()
		return nil, errors.New("MCP_PROTOCOL_MISMATCH: remote must support 2026-07-28; remote tool not submitted")
	}
	if err := checkRemoteContract(ctx, session, tool); err != nil {
		session.Close()
		return nil, err
	}
	return session, nil
}

func checkRemoteContract(ctx context.Context, session *mcp.ClientSession, tool importedTool) error {
	params := &mcp.ListToolsParams{}
	seen := make(map[string]bool)
	var found *mcp.Tool
	for page := 0; page < 16; page++ {
		list, err := session.ListTools(ctx, params)
		if err != nil || list == nil {
			return errors.New("MCP_DISCOVERY_FAILED: tool catalogue unavailable; remote tool not submitted")
		}
		for _, candidate := range list.Tools {
			if candidate != nil && candidate.Name == tool.config.Tool {
				if found != nil {
					return errors.New("MCP_CONTRACT_MISMATCH: duplicate remote tool; remote tool not submitted")
				}
				found = candidate
			}
		}
		if list.NextCursor == "" {
			if found == nil || !sameImportSchema(tool.config.InputSchema, found.InputSchema) || !sameImportSchema(tool.config.OutputSchema, found.OutputSchema) {
				return errors.New("MCP_CONTRACT_MISMATCH: tool missing or schema changed; remote tool not submitted")
			}
			return nil
		}
		if seen[list.NextCursor] {
			break
		}
		seen[list.NextCursor] = true
		params = &mcp.ListToolsParams{Cursor: list.NextCursor}
	}
	return errors.New("MCP_DISCOVERY_FAILED: tool catalogue pagination limit; remote tool not submitted")
}

func sameImportSchema(local json.RawMessage, remote any) bool {
	if len(local) == 0 {
		return remote == nil
	}
	var expected any
	if json.Unmarshal(local, &expected) != nil {
		return false
	}
	a, errA := json.Marshal(expected)
	b, errB := json.Marshal(remote)
	var actual any
	if errA != nil || errB != nil || json.Unmarshal(b, &actual) != nil {
		return false
	}
	b, errB = json.Marshal(actual)
	return errB == nil && bytes.Equal(a, b)
}

// The SDK currently decodes arbitrary JSON numbers through float64. Refuse
// unsafe integers instead of claiming lossless translation outside that range.
func checkImportValue(value any, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("value nesting too deep")
	}
	switch v := value.(type) {
	case nil, bool, int8, int16, int32, uint8, uint16, uint32:
		return nil
	case string:
		if utf8.ValidString(v) {
			return nil
		}
	case int:
		if v >= -9007199254740991 && v <= 9007199254740991 {
			return nil
		}
	case int64:
		if v >= -9007199254740991 && v <= 9007199254740991 {
			return nil
		}
	case uint:
		if uint64(v) <= 9007199254740991 {
			return nil
		}
	case uint64:
		if v <= 9007199254740991 {
			return nil
		}
	case float32:
		return checkImportValue(float64(v), depth)
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) && (math.Trunc(v) != v || math.Abs(v) <= 9007199254740991) {
			return nil
		}
	case []any:
		for _, item := range v {
			if err := checkImportValue(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for key, item := range v {
			if !utf8.ValidString(key) {
				return errors.New("invalid UTF-8 key")
			}
			if err := checkImportValue(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("value is not supported lossless JSON")
}

func normalizeImportNumbers(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, checkImportValue(n, 0)
		}
		n, err := v.Float64()
		if err != nil {
			return nil, err
		}
		return n, checkImportValue(n, 0)
	case []any:
		for idx, item := range v {
			next, err := normalizeImportNumbers(item)
			if err != nil {
				return nil, err
			}
			v[idx] = next
		}
	case map[string]any:
		for key, item := range v {
			next, err := normalizeImportNumbers(item)
			if err != nil {
				return nil, err
			}
			v[key] = next
		}
	}
	return value, nil
}

type importTransport struct {
	base     http.RoundTripper
	token    string
	maxBytes int
	lifetime context.Context
}

func (t *importTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	stop := func() bool { return false }
	if t.lifetime != nil {
		stop = context.AfterFunc(t.lifetime, cancel)
		if err := t.lifetime.Err(); err != nil {
			stop()
			cancel()
			return nil, err
		}
	}
	cleanup := func() { stop(); cancel() }
	// SDK cancellation notifications use a detached context. They must not
	// keep a completed preflight/invocation alive past its own time budget.
	req = req.Clone(ctx)
	req.Header = req.Header.Clone()
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		cleanup()
		return nil, err
	}
	if resp.ContentLength > int64(t.maxBytes) {
		resp.Body.Close()
		cleanup()
		return nil, errors.New("remote response exceeds byte limit")
	}
	resp.Body = &importBody{ReadCloser: resp.Body, remaining: int64(t.maxBytes), cleanup: cleanup}
	return resp, nil
}

type importBody struct {
	io.ReadCloser
	remaining int64
	cleanup   func()
}

func (r *importBody) Close() error {
	err := r.ReadCloser.Close()
	if r.cleanup != nil {
		r.cleanup()
	}
	return err
}

func (r *importBody) Read(p []byte) (int, error) {
	if r.remaining < 0 {
		return 0, errors.New("remote response exceeds byte limit")
	}
	if int64(len(p)) > r.remaining+1 {
		p = p[:r.remaining+1]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return 0, errors.New("remote response exceeds byte limit")
	}
	return n, err
}
