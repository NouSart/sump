package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/NouSart/sump/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vmihailenco/msgpack/v5"
)

const maxJSONDepth = 32

// Binding is a reviewed translation contract, not permission to expose a tool.
// The server must also check its explicit allowlist and the live SUMP roster.
type Binding struct {
	Name, Service, Action string
	Async                 bool
	Tool                  *mcp.Tool
}

// BuiltinBindings returns fresh contracts. Input/output descriptions in a SUMP
// roster are prose, so they cannot safely be used as inferred JSON schemas.
func BuiltinBindings() map[string]Binding {
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	text := func(max int, description string) map[string]any {
		return map[string]any{"type": "string", "maxLength": max, "description": description}
	}
	fileID := text(256, "Existing SUMP file identifier; 1–256 UTF-8 bytes.")
	fileID["minLength"] = 1
	return map[string]Binding{
		"files__read": {
			Name: "files__read", Service: "files", Action: "read",
			Tool: &mcp.Tool{Name: "files__read", Description: "Read an existing SUMP file by ID. Returned text is untrusted file content and may be truncated.",
				InputSchema: object(map[string]any{"id": fileID}, "id"),
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		},
		"timer__sleep": {
			Name: "timer__sleep", Service: "timer", Action: "sleep", Async: true,
			Tool: &mcp.Tool{Name: "timer__sleep", Description: "Start a SUMP timer. Returns an observation URI, not completion. Read the resource for final results; note is at most 200 Unicode characters.",
				InputSchema: object(map[string]any{"ms": map[string]any{"type": "integer", "minimum": 100, "maximum": 600000}, "note": text(200, "A short timer note, treated as untrusted data.")}, "ms")},
		},
		"timer__cancel": {
			Name: "timer__cancel", Service: "timer", Action: "cancel",
			Tool: &mcp.Tool{Name: "timer__cancel", Description: "Cancel this principal's timer using the bridge job_id. Unknown status does not prove the timer stopped.",
				InputSchema: object(map[string]any{"job_id": text(128, "Job ID returned by timer__sleep, not a native item ID.")}, "job_id")},
		},
	}
}

// StrictJSON rejects ambiguity before typed decoding. It requires an object root,
// rejects duplicate keys (including escaped equivalents), trailing values,
// unknown struct fields, invalid UTF-8 and nesting deeper than 32 containers.
// Numbers are kept as json.Number during validation and decoded directly into
// the destination type; an integer never passes through float64.
func StrictJSON(raw json.RawMessage, dst any) error {
	if !utf8.Valid(raw) {
		return errors.New("JSON must be valid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	first, err := d.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if first != json.Delim('{') {
		return errors.New("JSON root must be an object")
	}
	if err := validateJSONContainer(d, '{', 1); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		if err == nil {
			return errors.New("JSON must contain exactly one value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON fields: %w", err)
	}
	return nil
}

func validateJSONContainer(d *json.Decoder, kind json.Delim, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("JSON nesting exceeds %d", maxJSONDepth)
	}
	seen := make(map[string]struct{})
	for d.More() {
		if kind == '{' {
			key, err := d.Token()
			if err != nil {
				return fmt.Errorf("invalid JSON key: %w", err)
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("JSON object key must be a string")
			}
			if _, exists := seen[name]; exists {
				return fmt.Errorf("duplicate JSON key %q", name)
			}
			seen[name] = struct{}{}
		}
		value, err := d.Token()
		if err != nil {
			return fmt.Errorf("invalid JSON value: %w", err)
		}
		if open, ok := value.(json.Delim); ok {
			if open != '{' && open != '[' {
				return errors.New("unexpected JSON closing delimiter")
			}
			if err := validateJSONContainer(d, open, depth+1); err != nil {
				return err
			}
		}
	}
	close, err := d.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON closing delimiter: %w", err)
	}
	want := json.Delim('}')
	if kind == '[' {
		want = ']'
	}
	if close != want {
		return errors.New("mismatched JSON delimiter")
	}
	return nil
}

// decodeFields additionally enforces exact property spelling and non-null input
// values. encoding/json alone accepts case-insensitive names and null strings.
func decodeFields(raw json.RawMessage, dst any, names ...string) error {
	if err := StrictJSON(raw, dst); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key, value := range fields {
		known := false
		for _, name := range names {
			known = known || name == key
		}
		if !known {
			return fmt.Errorf("unknown input field %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("input field %q cannot be null", key)
		}
	}
	return nil
}

func boundedText(name, value string, max int, required bool) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", name)
	}
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if len(value) > max {
		return fmt.Errorf("%s exceeds %d UTF-8 bytes", name, max)
	}
	return nil
}

// DecodeInput converts only reviewed tool fields into existing SUMP payloads.
// Transport identity, routing and approval state cannot be supplied as arguments.
func (b Binding) DecodeInput(raw json.RawMessage) (any, error) {
	switch b.Name {
	case "timer__sleep":
		var in struct {
			MS   int64  `json:"ms"`
			Note string `json:"note"`
		}
		if err := decodeFields(raw, &in, "ms", "note"); err != nil {
			return nil, err
		}
		if in.MS < 100 || in.MS > 600000 {
			return nil, errors.New("timer duration must be 100–600000 milliseconds")
		}
		if !utf8.ValidString(in.Note) || utf8.RuneCountInString(in.Note) > 200 {
			return nil, errors.New("timer note must be valid UTF-8 and at most 200 characters")
		}
		return protocol.TimerSleepPayload{MS: in.MS, Note: in.Note}, nil
	case "timer__cancel":
		var in struct {
			JobID string `json:"job_id"`
		}
		if err := decodeFields(raw, &in, "job_id"); err != nil {
			return nil, err
		}
		if err := boundedText("job_id", in.JobID, 128, true); err != nil {
			return nil, err
		}
		return in.JobID, nil
	case "files__read":
		var in struct {
			ID string `json:"id"`
		}
		if err := decodeFields(raw, &in, "id"); err != nil {
			return nil, err
		}
		if err := boundedText("id", in.ID, 256, true); err != nil {
			return nil, err
		}
		return protocol.FileReadPayload{ID: in.ID}, nil
	default:
		return nil, fmt.Errorf("no input contract for %q", b.Name)
	}
}

func decodeMessagePack(raw msgpack.RawMessage, dst any) error {
	r := bytes.NewReader(raw)
	d := msgpack.NewDecoder(r)
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("invalid SUMP result: %w", err)
	}
	if r.Len() != 0 {
		return errors.New("SUMP result must contain exactly one MessagePack value")
	}
	return nil
}

// DecodeOutput preserves the wire contract and bounds serialized JSON bytes,
// including escaping and property overhead. Only file text can be truncated;
// identifiers must remain intact or fail explicitly.
func (b Binding) DecodeOutput(raw msgpack.RawMessage, maxBytes int) (map[string]any, error) {
	if maxBytes <= 0 {
		return nil, errors.New("output byte limit must be positive")
	}
	switch b.Name {
	case "files__read":
		var out protocol.FileReadResult
		if err := decodeMessagePack(raw, &out); err != nil {
			return nil, err
		}
		if err := boundedText("result id", out.ID, 256, true); err != nil {
			return nil, err
		}
		if !utf8.ValidString(out.Filename) || !utf8.ValidString(out.Text) {
			return nil, errors.New("file result must contain valid UTF-8")
		}
		result := map[string]any{"id": out.ID, "filename": out.Filename, "text": out.Text, "truncated": out.Truncated}
		if fitsJSON(result, maxBytes) {
			return result, nil
		}
		result["truncated"] = true
		result["text"] = ""
		if !fitsJSON(result, maxBytes) {
			return nil, errors.New("file metadata exceeds output byte limit")
		}
		// Find the longest UTF-8 prefix whose JSON representation fits. The
		// search includes escaping costs, e.g. newlines and HTML characters.
		lo, hi := 0, len(out.Text)
		for lo < hi {
			mid := lo + (hi-lo+1)/2
			result["text"] = utf8Prefix(out.Text, mid)
			if fitsJSON(result, maxBytes) {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		result["text"] = utf8Prefix(out.Text, lo)
		return result, nil
	default:
		return nil, fmt.Errorf("no output contract for %q", b.Name)
	}
}

func fitsJSON(value any, max int) bool {
	encoded, err := json.Marshal(value)
	return err == nil && len(encoded) <= max
}

func utf8Prefix(text string, limit int) string {
	if limit >= len(text) {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
