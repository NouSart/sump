package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// All keys and trust roots are test-local. Nothing is installed in Windows or
// added to the process-global transport/root store.
func httpTLSFiles(t *testing.T) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SUMP HTTP test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "loopback test"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(ca)
	return
}

func httpTLSClient(t *testing.T, config *tls.Config) *http.Client {
	t.Helper()
	transport := &http.Transport{TLSClientConfig: config}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

func TestHTTPTLSConfiguration(t *testing.T) {
	certFile, keyFile, _ := httpTLSFiles(t)
	_, wrongKey, _ := httpTLSFiles(t)
	invalid := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(invalid, []byte("invalid certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []HTTPOptions{
		{TLSCertFile: certFile}, {TLSKeyFile: keyFile},
		{TLSCertFile: invalid, TLSKeyFile: keyFile},
		{TLSCertFile: certFile, TLSKeyFile: wrongKey},
		{TLSCertFile: certFile, TLSKeyFile: filepath.Join(t.TempDir(), "missing.key")},
		{Listen: "192.0.2.1:9903", AllowRemote: true},
		{Listen: "0.0.0.0:0", TLSCertFile: certFile, TLSKeyFile: keyFile},
	} {
		if _, err := NewHTTPServer(opts, nil, httpTestVerifier); err == nil {
			t.Fatalf("invalid TLS options accepted: %+v", opts)
		}
	}
	// Configuration only: never bind a public interface in this test.
	h, err := NewHTTPServer(HTTPOptions{Listen: "0.0.0.0:0", AllowRemote: true, TLSCertFile: certFile, TLSKeyFile: keyFile}, nil, httpTestVerifier)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("TLS 1.2 minimum missing")
	}
}

func TestRemoteTLSRequiresSecretBeforeBinding(t *testing.T) {
	certFile, keyFile, _ := httpTLSFiles(t)
	center := gatewayPeer(t, func(env protocol.Envelope) (any, error) {
		if env.Type == protocol.TypeRegister {
			return protocol.RosterPayload{Revision: 1}, nil
		}
		return protocol.SettingsResult{}, nil
	})
	dir := t.TempDir()
	s, err := New(Config{
		Listen: "127.0.0.1:0", Center: center,
		ConfigPath: filepath.Join(dir, "absent-policy.json"), SecretFile: "-",
		Logger: log.New(io.Discard, "", 0),
		HTTP:   HTTPOptions{Listen: "192.0.2.1:9903", AllowRemote: true, TLSCertFile: certFile, TLSKeyFile: keyFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// This documentation-only address must never be bound: missing credentials
	// must be rejected before HTTP starts, even with valid TLS and opt-in.
	if err := s.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "provisioned http.token") {
		t.Fatalf("missing ingress secret not rejected before listen: %v", err)
	}
	if s.HTTPURL() != "" {
		t.Fatal("uncredentialed remote listener started")
	}
}

func TestHTTPTLSWireAndAccessChecks(t *testing.T) {
	certFile, keyFile, roots := httpTLSFiles(t)
	h := startHTTPTest(t, HTTPOptions{TLSCertFile: certFile, TLSKeyFile: keyFile}, nil)
	if !strings.HasPrefix(h.URL(), "https://") {
		t.Fatalf("TLS URL: %q", h.URL())
	}
	client := httpTLSClient(t, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	for _, tc := range []struct {
		name string
		edit func(*http.Request)
		want int
	}{
		{"authorized", func(*http.Request) {}, http.StatusOK},
		{"invalid_token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }, http.StatusUnauthorized},
		{"foreign_host", func(r *http.Request) { r.Host = "attacker.example" }, http.StatusForbidden},
		{"foreign_origin", func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := newHTTPRequest(t.Context(), h, http.MethodPost, httpBody("tools/list", nil))
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(r)
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want || resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
				t.Fatalf("unexpected response: %d %s", resp.StatusCode, body)
			}
			if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Mcp-Session-Id") != "" {
				t.Fatal("TLS changed stateless/private response policy")
			}
		})
	}
}

func TestHTTPTLSRejectsUntrustedPeerNameAndPlaintext(t *testing.T) {
	certFile, keyFile, roots := httpTLSFiles(t)
	h := startHTTPTest(t, HTTPOptions{TLSCertFile: certFile, TLSKeyFile: keyFile}, nil)
	for _, tc := range []struct {
		name   string
		config *tls.Config
	}{
		{"unknown_ca", &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}},
		{"wrong_server_name", &tls.Config{RootCAs: roots, ServerName: "wrong.example", MinVersion: tls.VersionTLS12}},
		{"obsolete_tls", &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := newHTTPRequest(t.Context(), h, http.MethodPost, httpBody("tools/list", nil))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := httpTLSClient(t, tc.config).Do(r)
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil {
				t.Fatal("invalid TLS trust/version accepted")
			}
		})
	}
	r, err := newHTTPRequest(t.Context(), h, http.MethodPost, httpBody("tools/list", nil))
	if err != nil {
		t.Fatal(err)
	}
	r.URL.Scheme = "http"
	resp, err := httpTLSClient(t, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}).Do(r)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil && resp.StatusCode == http.StatusOK {
		t.Fatal("TLS endpoint accepted plaintext MCP request")
	}
}

func TestHTTPTLSCertificateLoadedBeforeStart(t *testing.T) {
	certFile, keyFile, roots := httpTLSFiles(t)
	h, err := NewHTTPServer(HTTPOptions{Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile}, nil, httpTestVerifier)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	// Removing only test-local files proves Start uses the loaded snapshot.
	if err := os.Remove(certFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	r, err := newHTTPRequest(ctx, h, http.MethodPost, httpBody("tools/list", nil))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpTLSClient(t, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preloaded certificate not used: %d", resp.StatusCode)
	}
}
