package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// healthcheckAddr converts a server bind address (e.g. ":50051" or
// "0.0.0.0:50051") into the loopback HTTPS /healthz URL the in-container
// probe dials.
func healthcheckAddr(serverAddr string) string {
	_, port, found := strings.Cut(serverAddr, ":")
	if !found {
		port = serverAddr
	}
	return fmt.Sprintf("https://127.0.0.1:%s/healthz", port)
}

// probeHealthURL issues an HTTPS GET to target and returns nil only when the
// response status is 200. tlsCfg supplies the CA trust (and optional client
// cert; the gateway listener uses VerifyClientCertIfGiven, so a client cert
// is accepted but not required).
func probeHealthURL(ctx context.Context, target string, tlsCfg *tls.Config) error {
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return fmt.Errorf("build healthcheck request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: unhealthy status %d", resp.StatusCode)
	}
	return nil
}

// runHealthcheck loads config, builds the probe URL and TLS trust, and returns
// a process exit code (0 healthy, 1 unhealthy or misconfigured).
func runHealthcheck() int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := config.NewLoader().Load(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: load config: %v\n", err)
		return 1
	}
	tlsCfg, err := tlsconfig.BuildTLSConfig(ctx, cfg.TLS, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: build TLS config: %v\n", err)
		return 1
	}
	if err := probeHealthURL(ctx, healthcheckAddr(cfg.Server.Addr), tlsCfg); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	return 0
}
