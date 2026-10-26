package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/QAQ-awa-QAQ/sump/mcp/server"
)

func main() {
	var cfg server.Config
	var checkRemotes bool
	flag.StringVar(&cfg.Listen, "addr", "127.0.0.1:9902", "SUMP WebSocket listen address")
	flag.StringVar(&cfg.Center, "center", "ws://127.0.0.1:9000/ws", "settings center WebSocket URL")
	flag.StringVar(&cfg.HTTP.Listen, "http-addr", "127.0.0.1:9903", "MCP Streamable HTTP listen address")
	flag.StringVar(&cfg.ConfigPath, "config", "data/mcp.json", "explicit imports and exports configuration")
	flag.StringVar(&cfg.SecretFile, "secret-file", "data/mcp-secret.json", "service-owned secret file")
	flag.BoolVar(&cfg.HTTP.AllowRemote, "allow-remote", false, "explicitly allow a non-loopback MCP listener; TLS and token required")
	flag.StringVar(&cfg.HTTP.TLSCertFile, "tls-cert", "", "PEM TLS certificate chain; required with tls-key for direct remote access")
	flag.StringVar(&cfg.HTTP.TLSKeyFile, "tls-key", "", "PEM TLS private key; never send or commit this file")
	flag.BoolVar(&checkRemotes, "check-remotes", false, "check configured remote authentication and tool contracts without invoking tools or starting SUMP")
	flag.Func("allowed-hosts", "comma-separated exact HTTP Host authorities for an explicit remote deployment", func(value string) error {
		for _, host := range strings.Split(value, ",") {
			cfg.HTTP.AllowedHosts = append(cfg.HTTP.AllowedHosts, strings.TrimSpace(host))
		}
		return nil
	})
	flag.IntVar(&cfg.MaxCalls, "max-calls", 8, "maximum simultaneous work calls")
	flag.IntVar(&cfg.MaxCallsPerPrincipal, "max-principal-calls", 2, "maximum simultaneous work calls per authenticated HTTP principal or internal SUMP caller")
	flag.IntVar(&cfg.MaxOutputBytes, "max-output-bytes", 128<<10, "maximum serialized MCP result size")
	flag.Int64Var(&cfg.HTTP.MaxRequestBytes, "max-request-bytes", 256<<10, "maximum HTTP request size")
	flag.IntVar(&cfg.HTTP.MaxRequests, "max-http-requests", 32, "maximum simultaneous HTTP requests, including event streams")
	flag.Int64Var(&cfg.MaxMessageBytes, "max-message-bytes", 1<<20, "maximum incoming SUMP WebSocket message size")
	flag.DurationVar(&cfg.CallTimeout, "call-timeout", 10*time.Second, "maximum synchronous backend wait or total check-remotes deadline")
	flag.DurationVar(&cfg.AsyncTimeout, "async-timeout", 2*time.Minute, "async import wait; exported jobs become unknown after this interval")
	flag.DurationVar(&cfg.DeliveryTimeout, "delivery-timeout", 5*time.Second, "maximum final-report delivery wait")
	flag.DurationVar(&cfg.JobTTL, "job-ttl", 15*time.Minute, "volatile observation retention; must exceed async-timeout")
	flag.IntVar(&cfg.MaxJobs, "max-jobs", 128, "maximum retained export observations, including final results")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if checkRemotes {
		report, err := server.CheckRemoteConfig(ctx, cfg)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			log.Fatal("cannot write remote check result")
		}
		if !report.OK {
			os.Exit(1)
		}
		return
	}
	s, err := server.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err = s.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	log.Printf("MCP HTTP listening at %s (Bearer authentication required)", s.HTTPURL())
	<-ctx.Done()
}
