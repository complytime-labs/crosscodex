package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	echopb "github.com/complytime-labs/crosscodex/internal/testgrpc/gen/echo/v1"

	"github.com/complytime-labs/crosscodex/internal/testgrpc"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := flag.String("addr", envOrDefault("ADDR", ":9090"), "bind address")
	tlsCA := flag.String("tls-ca", envOrDefault("TLS_CA", "/certs/ca.pem"), "CA certificate")
	tlsCert := flag.String("tls-cert", envOrDefault("TLS_CERT", "/certs/server.pem"), "server certificate")
	tlsKey := flag.String("tls-key", envOrDefault("TLS_KEY", "/certs/server-key.pem"), "server key")
	dbDSN := flag.String("db-dsn", os.Getenv("TEST_DATABASE_DSN"), "PostgreSQL DSN")
	natsURL := flag.String("nats-url", os.Getenv("TEST_NATS_URL"), "NATS URL")
	natsCA := flag.String("nats-ca", envOrDefault("TEST_NATS_CA", "/certs/ca.pem"), "NATS CA cert")
	natsCert := flag.String("nats-cert", envOrDefault("TEST_NATS_CERT", "/certs/client.pem"), "NATS client cert")
	natsKey := flag.String("nats-key", envOrDefault("TEST_NATS_KEY", "/certs/client-key.pem"), "NATS client key")
	flag.Parse()

	var harnessOpts []testgrpc.Option
	harnessOpts = append(harnessOpts,
		testgrpc.WithAddress(*addr),
		testgrpc.WithTLS(*tlsCA, *tlsCert, *tlsKey),
	)

	var dbPool db.TenantConnection
	if *dbDSN != "" {
		migrator, err := db.NewMigrator(*dbDSN)
		if err != nil {
			return fmt.Errorf("migrator: %w", err)
		}
		if err := migrator.Up(context.Background()); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		if err := migrator.Close(); err != nil {
			log.Printf("migrator close: %v", err)
		}
		log.Println("migrations applied")

		pool, err := db.NewPool(db.PoolConfig{
			DSN:          *dbDSN,
			MaxOpenConns: 5,
		})
		if err != nil {
			return fmt.Errorf("db pool: %w", err)
		}
		defer pool.Close()
		dbPool = db.NewTenantPool(pool)
		harnessOpts = append(harnessOpts, testgrpc.WithDB(dbPool))
		log.Printf("DB connected: %s", redactDSN(*dbDSN))
	}

	var natsClient natsbus.Client
	if *natsURL != "" {
		natsTLSCfg, err := tlsconfig.BuildTLSConfig(context.Background(), config.TLSConfig{
			Mode: "mutual",
			CA:   *natsCA,
			Cert: *natsCert,
			Key:  *natsKey,
		}, "nats")
		if err != nil {
			return fmt.Errorf("nats TLS: %w", err)
		}

		natsCfg := config.NATSConfig{
			URL: *natsURL,
			TLS: true,
			Streams: config.NATSStreamsConfig{
				AuditLLMRetention:    24 * time.Hour,
				AuditEventsRetention: 24 * time.Hour,
			},
		}
		client, err := natsbus.New(natsCfg, natsbus.WithTLSConfig(natsTLSCfg))
		if err != nil {
			return fmt.Errorf("nats: %w", err)
		}
		defer client.Close()
		natsClient = client
		harnessOpts = append(harnessOpts, testgrpc.WithNATS(natsClient))
		log.Printf("NATS connected: %s", *natsURL)
	}

	h, err := testgrpc.NewHarness(harnessOpts...)
	if err != nil {
		return fmt.Errorf("harness: %w", err)
	}

	echoSvc := testgrpc.NewEchoService(dbPool, natsClient)
	h.RegisterService(&echopb.TenantEchoService_ServiceDesc, echoSvc)

	if err := h.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	log.Printf("serving on %s", h.Addr())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh

	log.Println("shutting down...")
	h.Stop()
	return nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// redactDSN strips credentials from a PostgreSQL DSN, returning only
// the host and database name. If parsing fails, returns a static
// placeholder to avoid leaking credentials in logs.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "***"
	}
	return "***@" + u.Host + u.Path
}
