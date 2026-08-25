package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// llmHTTPClient returns a custom *http.Client whose transport carries the
// mTLS material for the "llm" target when the LLM gateway URL uses https.
// It returns (nil, nil) for any non-https URL so callers fall back to
// llmclient's default plaintext client -- preserving existing behavior for
// non-TLS deployments.
func llmHTTPClient(ctx context.Context, cfg *config.Config) (*http.Client, error) {
	u, err := url.Parse(cfg.LLM.GatewayURL)
	if err != nil || u.Scheme != "https" {
		return nil, nil
	}
	tlsCfg, err := tlsconfig.BuildTLSConfig(ctx, cfg.TLS, "llm")
	if err != nil {
		return nil, fmt.Errorf("build LLM TLS config: %w", err)
	}
	return &http.Client{
		Timeout:   time.Duration(cfg.LLM.Timeout) * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

// natsTLSConfig returns the mTLS *tls.Config for the external-NATS hop when
// NATS runs as a separate TLS-verified server (non-empty URL + tls: true).
// It returns (nil, nil) for embedded mode or plaintext external NATS, so
// natsbus.New keeps its current behavior in those cases.
func natsTLSConfig(ctx context.Context, cfg *config.Config) (*tls.Config, error) {
	if cfg.NATS.URL == "" || !cfg.NATS.TLS {
		return nil, nil
	}
	tlsCfg, err := tlsconfig.BuildTLSConfig(ctx, cfg.TLS, "nats")
	if err != nil {
		return nil, fmt.Errorf("build NATS TLS config: %w", err)
	}
	return tlsCfg, nil
}
