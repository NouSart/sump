package server

import (
	"context"
	"errors"
	"io"
	"os"
)

// RemoteCheckReport records protocol reachability and reviewed tool contracts.
// It never claims that business execution or execution permissions were tested.
type RemoteCheckReport struct {
	ProtocolVersion string        `json:"protocol_version"`
	OK              bool          `json:"ok"`
	Checks          []ImportCheck `json:"checks"`
}

// CheckRemoteConfig does not start SUMP, listen on ports or invoke tools. It uses
// the same pinned configuration, secrets and outgoing transport as the service.
func CheckRemoteConfig(ctx context.Context, cfg Config) (RemoteCheckReport, error) {
	report := RemoteCheckReport{ProtocolVersion: protocolVersion}
	if err := cfg.defaults(); err != nil {
		return report, err
	}
	policy, err := readPolicy(cfg.ConfigPath, BuiltinBindings())
	if err != nil {
		return report, err
	}
	if len(policy.Imports) == 0 {
		return report, errors.New("no explicit imports configured; nothing to check")
	}
	secrets, err := readCheckSecrets(cfg.SecretFile)
	if err != nil {
		return report, err
	}
	imports, err := NewImports(policy.Remotes, policy.Imports, cfg.CallTimeout, cfg.MaxOutputBytes, func(key string) string { return secrets[key] })
	if err != nil {
		return report, err
	}
	defer imports.Close()
	report.Checks = imports.Check(ctx)
	report.OK = len(report.Checks) == len(policy.Imports)
	for _, check := range report.Checks {
		report.OK = report.OK && check.OK
	}
	return report, nil
}

// Secrets stay in memory for this check only. Error text never contains their
// values or raw JSON. This reads the service's existing version-1 file format.
func readCheckSecrets(path string) (map[string]string, error) {
	if path == "-" {
		return nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot read MCP secret file")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
	if err != nil || len(raw) > 128<<10 {
		return nil, errors.New("MCP secret file unreadable or exceeds 128 KiB")
	}
	var stored struct {
		Version int               `json:"version"`
		Secrets map[string]string `json:"secrets"`
	}
	if StrictJSON(raw, &stored) != nil || stored.Version != 1 {
		return nil, errors.New("invalid version-1 MCP secret file")
	}
	return stored.Secrets, nil
}
