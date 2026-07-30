package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultEndpoint   = "localhost:50051"
	healthRetries     = 5
	healthBaseBackoff = 100 * time.Millisecond
)

type embeddedDaemon struct {
	server    *gateway.Server
	port      int
	pidFile   string
	resources *embeddedResources
}

func (d *embeddedDaemon) stop() {
	if d == nil {
		return
	}
	if d.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.server.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "daemon shutdown error: %v\n", err)
		}
	}
	if d.resources != nil {
		d.resources.close()
	}
	if d.pidFile != "" {
		os.Remove(d.pidFile)
	}
}

func connect(ctx context.Context, state *cliState, flagEndpoint string) error {
	envEndpoint := os.Getenv("CROSSCODEX_ENDPOINT")
	cfgEndpoint := ""
	if state.cfg != nil {
		cfgEndpoint = state.cfg.Endpoint
	}

	endpoint := resolveEndpoint(flagEndpoint, envEndpoint, cfgEndpoint)
	explicitEndpoint := flagEndpoint != "" || envEndpoint != "" || cfgEndpoint != ""

	conn, err := dialGRPC(ctx, endpoint)
	if err == nil {
		state.conn = conn
		state.client = pb.NewGatewayServiceClient(conn)
		if healthCheck(ctx, state.client) {
			return nil
		}
		conn.Close()
	}

	if explicitEndpoint {
		return fmt.Errorf("cannot reach crosscodexd at %s\n\n  Is the daemon running? Check with:\n    crosscodex version\n\n  Or start it manually:\n    crosscodexd", endpoint)
	}

	stateDir := xdgStateDir()
	pidPath := filepath.Join(stateDir, "daemon.pid")
	if port, alive := readPIDFile(pidPath); alive && port > 0 {
		ep := fmt.Sprintf("localhost:%d", port)
		conn, err = dialGRPC(ctx, ep)
		if err == nil {
			state.conn = conn
			state.client = pb.NewGatewayServiceClient(conn)
			if healthCheck(ctx, state.client) {
				return nil
			}
			conn.Close()
		}
	}

	return startEmbeddedDaemon(ctx, state, stateDir, pidPath)
}

func startEmbeddedDaemon(ctx context.Context, state *cliState, stateDir, pidPath string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	if state.fullCfg == nil {
		return fmt.Errorf("configuration not loaded")
	}

	// Bootstrap PKI
	pkiDir := filepath.Join(stateDir, "pki")
	if err := ensurePKI(pkiDir); err != nil {
		return fmt.Errorf("bootstrap PKI: %w", err)
	}
	paths := pkiPaths(pkiDir)

	// Create WARN-level logger to suppress INFO noise
	embeddedLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	// Build service with real backends
	svc, resources, err := buildEmbeddedService(ctx, state.fullCfg, embeddedLogger)
	if err != nil {
		return err
	}

	// Configure TLS for the server
	tlsCfg := config.TLSConfig{
		Mode: "mutual",
		CA:   paths.CACert,
		Cert: paths.ServerCert,
		Key:  paths.ServerKey,
	}

	srv, err := gateway.NewServer(ctx, gateway.ServerConfig{
		GRPCAddr: "localhost:0",
		HTTPAddr: "localhost:0",
		TLS:      tlsCfg,
		Service:  svc,
		Logger:   embeddedLogger,
	})
	if err != nil {
		resources.close()
		return fmt.Errorf("create embedded daemon: %w", err)
	}

	if err := srv.Start(); err != nil {
		resources.close()
		return fmt.Errorf("start embedded daemon: %w", err)
	}

	grpcAddr := srv.GRPCAddr()
	_, portStr, err := splitHostPort(grpcAddr)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		resources.close()
		return fmt.Errorf("parse gRPC address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		resources.close()
		return fmt.Errorf("parse port: %w", err)
	}

	if err := writePIDFile(pidPath, port); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		resources.close()
		return fmt.Errorf("write PID file: %w", err)
	}

	// Connect with mTLS
	var conn *grpc.ClientConn
	for i := range healthRetries {
		time.Sleep(healthBaseBackoff * time.Duration(1<<i))
		conn, err = dialGRPCWithTLS(grpcAddr, paths)
		if err == nil {
			state.conn = conn
			state.client = pb.NewGatewayServiceClient(conn)
			if healthCheck(ctx, state.client) {
				state.daemon = &embeddedDaemon{server: srv, port: port, pidFile: pidPath, resources: resources}
				return nil
			}
			conn.Close()
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	resources.close()
	os.Remove(pidPath)
	return fmt.Errorf("embedded daemon started but failed health check after %d retries", healthRetries)
}

func dialGRPC(_ context.Context, endpoint string) (*grpc.ClientConn, error) {
	return grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}

func healthCheck(ctx context.Context, client pb.GatewayServiceClient) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := client.Health(ctx, &pb.HealthRequest{})
	return err == nil
}

func disconnect(state *cliState) {
	if state.conn != nil {
		state.conn.Close()
		state.conn = nil
		state.client = nil
	}
	if state.daemon != nil {
		state.daemon.stop()
		state.daemon = nil
	}
}

func resolveEndpoint(flag, env, cfg string) string {
	if flag != "" {
		return flag
	}
	if env != "" {
		return env
	}
	if cfg != "" {
		return cfg
	}
	return defaultEndpoint
}

func xdgStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "crosscodex")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "crosscodex")
}

func readPIDFile(path string) (port int, alive bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(lines) < 2 {
		return 0, false
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil {
		return 0, false
	}
	port, err = strconv.Atoi(lines[1])
	if err != nil {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return port, false
	}
	// On Unix, FindProcess always succeeds. Signal 0 tests existence.
	err = proc.Signal(syscall.Signal(0))
	return port, err == nil
}

func writePIDFile(path string, port int) error {
	content := fmt.Sprintf("%d\n%d\n", os.Getpid(), port)
	return os.WriteFile(path, []byte(content), 0o600)
}

func loadConfig(state *cliState, profile string) error {
	loader := config.NewLoader()
	var opts []config.Option
	if profile != "" {
		opts = append(opts, config.WithProfile(profile))
	}
	cfg, err := loader.Load(context.Background(), opts...)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	state.fullCfg = cfg
	clientCfg := cfg.CLIConfig()
	state.cfg = &clientCfg
	return nil
}

var localCommands = map[string]bool{
	"project init":          true,
	"project list":          true,
	"project config":        true,
	"project status":        true,
	"config show":           true,
	"config get":            true,
	"config set":            true,
	"config profiles":       true,
	"catalog validate":      true,
	"results verify":        true,
	"version":               true,
	"completion":            true,
	"completion bash":       true,
	"completion zsh":        true,
	"completion fish":       true,
	"completion powershell": true,
	"help":                  true,
	"prompt list":           true,
	"prompt show":           true,
	"prompt layer":          true,
}

func needsConnection(cmdPath string) bool {
	// Strip root command name prefix (e.g., "crosscodex " from "crosscodex project init")
	if i := strings.Index(cmdPath, " "); i >= 0 {
		cmdPath = cmdPath[i+1:]
	}
	return !localCommands[cmdPath]
}

func splitHostPort(addr string) (host, port string, err error) {
	idx := strings.LastIndex(addr, ":")
	if idx == -1 {
		return "", "", fmt.Errorf("invalid address: %s", addr)
	}
	return addr[:idx], addr[idx+1:], nil
}
