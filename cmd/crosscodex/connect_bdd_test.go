package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

// fakeHealthClient answers Health with a fixed response or error; every
// other GatewayServiceClient method panics through the nil embed.
type fakeHealthClient struct {
	crosscodexv1connect.GatewayServiceClient
	resp *pb.HealthCheckResponse
	err  error
}

func (f *fakeHealthClient) Health(context.Context, *connectrpc.Request[pb.HealthRequest]) (*connectrpc.Response[pb.HealthCheckResponse], error) {
	if f.err != nil {
		return nil, f.err
	}
	return connectrpc.NewResponse(f.resp), nil
}

func dependencyHealth(status pb.HealthStatus, message string) *pb.HealthCheckResponse {
	return &pb.HealthCheckResponse{
		Status: status,
		Services: map[string]*pb.ServiceHealth{
			"database": {ServiceName: "database", Status: status, Message: message},
		},
	}
}

var _ = Describe("Connection Management", func() {
	Describe("healthCheck", func() {
		It("passes only when the daemon reports HEALTHY", func() {
			client := &fakeHealthClient{resp: dependencyHealth(pb.HealthStatus_HEALTH_STATUS_HEALTHY, "database reachable")}
			Expect(healthCheck(context.Background(), client)).To(Succeed())
		})

		DescribeTable("fails closed on any other status and names the failing dependency",
			func(status pb.HealthStatus) {
				client := &fakeHealthClient{resp: dependencyHealth(status, "database unreachable")}
				err := healthCheck(context.Background(), client)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("database: database unreachable"))
			},
			Entry("unhealthy", pb.HealthStatus_HEALTH_STATUS_UNHEALTHY),
			Entry("degraded", pb.HealthStatus_HEALTH_STATUS_DEGRADED),
			Entry("unspecified", pb.HealthStatus_HEALTH_STATUS_UNSPECIFIED),
		)

		It("fails closed on a non-healthy status with no dependency detail", func() {
			client := &fakeHealthClient{resp: &pb.HealthCheckResponse{Status: pb.HealthStatus_HEALTH_STATUS_UNHEALTHY}}
			err := healthCheck(context.Background(), client)
			Expect(err).To(MatchError(ContainSubstring("HEALTH_STATUS_UNHEALTHY")))
		})

		It("fails when the Health RPC errors", func() {
			client := &fakeHealthClient{err: connectrpc.NewError(connectrpc.CodeUnavailable, errors.New("connection refused"))}
			Expect(healthCheck(context.Background(), client)).To(MatchError(ContainSubstring("connection refused")))
		})
	})

	Describe("xdgStateDir", func() {
		It("uses XDG_STATE_HOME when set", func() {
			original := os.Getenv("XDG_STATE_HOME")
			defer os.Setenv("XDG_STATE_HOME", original)
			os.Setenv("XDG_STATE_HOME", "/tmp/test-state")
			Expect(xdgStateDir()).To(Equal("/tmp/test-state/crosscodex"))
		})

		It("falls back to ~/.local/state when XDG_STATE_HOME is empty", func() {
			original := os.Getenv("XDG_STATE_HOME")
			defer os.Setenv("XDG_STATE_HOME", original)
			os.Unsetenv("XDG_STATE_HOME")
			home, _ := os.UserHomeDir()
			Expect(xdgStateDir()).To(Equal(filepath.Join(home, ".local", "state", "crosscodex")))
		})
	})

	Describe("PID file", func() {
		var tmpDir string

		BeforeEach(func() {
			var err error
			tmpDir, err = os.MkdirTemp("", "crosscodex-test-*")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			os.RemoveAll(tmpDir)
		})

		It("round-trips port through PID file", func() {
			path := filepath.Join(tmpDir, "daemon.pid")
			Expect(writePIDFile(path, 12345)).To(Succeed())
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(12345))
			Expect(alive).To(BeTrue())
		})

		It("returns zero port for missing file", func() {
			path := filepath.Join(tmpDir, "nonexistent.pid")
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(0))
			Expect(alive).To(BeFalse())
		})

		It("returns zero for single-line file", func() {
			path := filepath.Join(tmpDir, "bad.pid")
			Expect(os.WriteFile(path, []byte("12345"), 0o600)).To(Succeed())
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(0))
			Expect(alive).To(BeFalse())
		})

		It("returns zero for non-numeric PID", func() {
			path := filepath.Join(tmpDir, "bad.pid")
			Expect(os.WriteFile(path, []byte("notanumber\n50051\n"), 0o600)).To(Succeed())
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(0))
			Expect(alive).To(BeFalse())
		})

		It("returns zero for non-numeric port", func() {
			path := filepath.Join(tmpDir, "bad.pid")
			Expect(os.WriteFile(path, []byte("12345\nnotaport\n"), 0o600)).To(Succeed())
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(0))
			Expect(alive).To(BeFalse())
		})

		It("returns false alive for dead process", func() {
			path := filepath.Join(tmpDir, "dead.pid")
			Expect(os.WriteFile(path, []byte("999999999\n50051\n"), 0o600)).To(Succeed())
			port, alive := readPIDFile(path)
			Expect(port).To(Equal(50051))
			Expect(alive).To(BeFalse())
		})
	})

	Describe("needsConnection", func() {
		DescribeTable("returns expected result for command paths",
			func(cmdPath string, expected bool) {
				Expect(needsConnection(cmdPath)).To(Equal(expected))
			},
			// Single-word local commands (no prefix stripping needed)
			Entry("version", "version", false),
			Entry("help", "help", false),
			Entry("completion", "completion", false),

			// Multi-word local commands (as returned by cmd.CommandPath())
			Entry("crosscodex config show", "crosscodex config show", false),
			Entry("crosscodex config get", "crosscodex config get", false),
			Entry("crosscodex catalog validate", "crosscodex catalog validate", false),
			Entry("crosscodex prompt list", "crosscodex prompt list", false),
			Entry("crosscodex completion bash", "crosscodex completion bash", false),
			Entry("crosscodex project init", "crosscodex project init", false),
			// Remote commands (need daemon connection)
			Entry("crosscodex catalog import", "crosscodex catalog import", true),
			Entry("crosscodex catalog list", "crosscodex catalog list", true),
			Entry("crosscodex run start", "crosscodex run start", true),
			Entry("crosscodex results summary", "crosscodex results summary", true),

			// Unknown commands return true
			Entry("nonexistent", "nonexistent", true),
			Entry("crosscodex unknown", "crosscodex unknown", true),
		)
	})

	Describe("resolveEndpoint", func() {
		It("prefers explicit flag value", func() {
			endpoint := resolveEndpoint("flag-value:9090", "env-value:9090", "config-value:9090")
			Expect(endpoint).To(Equal("flag-value:9090"))
		})

		It("falls back to env when flag is empty", func() {
			endpoint := resolveEndpoint("", "env-value:9090", "config-value:9090")
			Expect(endpoint).To(Equal("env-value:9090"))
		})

		It("falls back to config when flag and env are empty", func() {
			endpoint := resolveEndpoint("", "", "config-value:9090")
			Expect(endpoint).To(Equal("config-value:9090"))
		})

		It("returns default when all are empty", func() {
			endpoint := resolveEndpoint("", "", "")
			Expect(endpoint).To(Equal("localhost:50051"))
		})
	})
})

// crosscodexd's split-role NATS check must not leak into config loading: the
// CLI shares the daemon's configuration and starts its embedded daemon as
// role all regardless of the configured role.
var _ = Describe("loadConfig with a split role and no nats.url", func() {
	It("loads, so local commands are not blocked", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		GinkgoT().Setenv("CROSSCODEX_ROLE", "gateway")
		GinkgoT().Setenv("CROSSCODEX_NATS_URL", "")

		state := &cliState{}
		Expect(loadConfig(state, "")).To(Succeed())
		Expect(state.fullCfg.Role).To(Equal("gateway"))
		Expect(state.cfg).NotTo(BeNil())
	})
})
