package server

import (
	"errors"
	"io"
	"log"
	"os"
	"time"
)

// Config contains startup-only transport and resource limits. The local policy
// file contains reviewed exports and imports; it never contains credentials.
type Config struct {
	Listen, Center, ConfigPath, SecretFile         string
	HTTP                                           HTTPOptions
	Logger                                         *log.Logger
	MaxCalls, MaxCallsPerPrincipal, MaxOutputBytes int
	MaxMessageBytes                                int64
	CallTimeout, ReloadInterval                    time.Duration
	AsyncTimeout, DeliveryTimeout, JobTTL          time.Duration
	MaxJobs                                        int
}

func (c *Config) defaults() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:9902"
	}
	if c.Center == "" {
		c.Center = "ws://127.0.0.1:9000/ws"
	}
	if c.HTTP.Listen == "" {
		c.HTTP.Listen = "127.0.0.1:9903"
	}
	if c.ConfigPath == "" {
		c.ConfigPath = "data/mcp.json"
	}
	if c.SecretFile == "" {
		c.SecretFile = "data/mcp-secret.json"
	}
	if c.Logger == nil {
		c.Logger = log.Default()
	}
	if c.MaxCalls == 0 {
		c.MaxCalls = 8
	}
	if c.MaxCallsPerPrincipal == 0 {
		c.MaxCallsPerPrincipal = min(2, c.MaxCalls)
	}
	if c.MaxOutputBytes == 0 {
		c.MaxOutputBytes = 128 << 10
	}
	if c.MaxMessageBytes == 0 {
		c.MaxMessageBytes = 1 << 20
	}
	if c.CallTimeout == 0 {
		c.CallTimeout = 10 * time.Second
	}
	if c.ReloadInterval == 0 {
		c.ReloadInterval = time.Second
	}
	if c.AsyncTimeout == 0 {
		c.AsyncTimeout = 2 * time.Minute
	}
	if c.DeliveryTimeout == 0 {
		c.DeliveryTimeout = 5 * time.Second
	}
	if c.JobTTL == 0 {
		c.JobTTL = 15 * time.Minute
	}
	if c.MaxJobs == 0 {
		c.MaxJobs = 128
	}
	if c.AsyncTimeout < 0 || c.DeliveryTimeout < 0 || c.JobTTL <= c.AsyncTimeout || c.MaxJobs < 1 {
		return errors.New("invalid async timeout, observation lifetime or capacity")
	}
	if c.MaxCalls < 1 || c.MaxCallsPerPrincipal < 1 || c.MaxCallsPerPrincipal > c.MaxCalls || c.MaxOutputBytes < 4096 || c.MaxMessageBytes < 4096 || c.CallTimeout < 0 || c.ReloadInterval < 100*time.Millisecond {
		return errors.New("invalid MCP concurrency, byte or time limits")
	}
	return nil
}

type exportPolicy struct {
	Exports []string       `json:"exports"`
	Remotes []RemoteConfig `json:"remotes,omitempty"`
	Imports []ImportConfig `json:"imports,omitempty"`
}

func readPolicy(path string, bindings map[string]Binding) (*exportPolicy, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return &exportPolicy{}, nil
	}
	if err != nil {
		return nil, errors.New("cannot open MCP policy file")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(raw) > 64<<10 {
		return nil, errors.New("MCP policy file exceeds 64 KiB or cannot be read")
	}
	var p exportPolicy
	if err := decodeFields(raw, &p, "exports", "remotes", "imports"); err != nil {
		return nil, errors.New("invalid MCP binding configuration")
	}
	if len(p.Exports) > len(bindings) {
		return nil, errors.New("too many MCP exports")
	}
	seen := make(map[string]bool)
	for _, name := range p.Exports {
		if _, ok := bindings[name]; !ok {
			return nil, errors.New("unknown MCP export")
		}
		if seen[name] {
			return nil, errors.New("duplicate MCP export")
		}
		seen[name] = true
	}
	if err := ValidateImportConfigs(p.Remotes, p.Imports); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *exportPolicy) allows(name string) bool {
	for _, allowed := range p.Exports {
		if name == allowed {
			return true
		}
	}
	return false
}
