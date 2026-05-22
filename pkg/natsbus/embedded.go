package natsbus

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

// embeddedServer wraps an in-process NATS server with JetStream enabled.
type embeddedServer struct {
	server *natsserver.Server
	logger *slog.Logger
}

// startEmbedded starts an in-process NATS server with JetStream.
// The server listens on a random port and stores JetStream data in storeDir.
func startEmbedded(cfg config.NATSEmbeddedConfig, logger *slog.Logger) (*embeddedServer, error) {
	storeDir := resolveStoreDir(cfg)

	opts := &natsserver.Options{
		Host:               "127.0.0.1",
		Port:               -1, // Random available port
		NoLog:              true,
		NoSigs:             true,
		JetStream:          true,
		StoreDir:           storeDir,
		JetStreamMaxMemory: -1,
		JetStreamMaxStore:  -1,
	}

	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("creating embedded NATS server: %w: %w", ErrEmbeddedStart, err)
	}

	ns.Start()

	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		return nil, fmt.Errorf("embedded NATS server not ready after 10s: %w", ErrEmbeddedStart)
	}

	logger.Info("embedded NATS server started",
		"addr", ns.ClientURL(),
		"store_dir", storeDir,
	)

	return &embeddedServer{
		server: ns,
		logger: logger,
	}, nil
}

// clientURL returns the URL to connect to the embedded server.
func (e *embeddedServer) clientURL() string {
	return e.server.ClientURL()
}

// shutdown stops the embedded server and waits for shutdown.
func (e *embeddedServer) shutdown() {
	e.server.Shutdown()
	e.server.WaitForShutdown()
	e.logger.Info("embedded NATS server stopped")
}

// resolveStoreDir returns the JetStream storage directory, using the
// configured value or falling back to XDG_STATE_HOME.
func resolveStoreDir(cfg config.NATSEmbeddedConfig) string {
	if cfg.StoreDir != "" {
		return cfg.StoreDir
	}
	return xdgNATSStateDir()
}

// isEmbeddedMode returns true when the URL is empty, indicating
// an embedded NATS server should be started.
func isEmbeddedMode(url string) bool {
	return url == ""
}
