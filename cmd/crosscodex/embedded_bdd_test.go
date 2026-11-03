package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	connectrpc "connectrpc.com/connect"
	"go.opentelemetry.io/otel"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/gateway/backend"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("ensurePKI", func() {
	It("generates a complete PKI in an empty directory", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")

		Expect(ensurePKI(pkiDir)).To(Succeed())

		// Directory must be 0700
		info, err := os.Stat(pkiDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o700)))

		// All 6 PEM files must exist
		for _, name := range []string{"ca.pem", "ca-key.pem", "server.pem", "server-key.pem", "client.pem", "client-key.pem"} {
			path := filepath.Join(pkiDir, name)
			_, err := os.Stat(path)
			Expect(err).NotTo(HaveOccurred(), "missing: "+name)
		}

		// Key files must be 0600
		for _, name := range []string{"ca-key.pem", "server-key.pem", "client-key.pem"} {
			info, err := os.Stat(filepath.Join(pkiDir, name))
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)), name+" should be 0600")
		}

		// Fingerprint must exist
		_, err = os.Stat(filepath.Join(pkiDir, ".fingerprint"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("reuses existing valid certs", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")

		Expect(ensurePKI(pkiDir)).To(Succeed())

		// Record fingerprint
		fp1, err := os.ReadFile(filepath.Join(pkiDir, ".fingerprint"))
		Expect(err).NotTo(HaveOccurred())

		// Second call should reuse
		Expect(ensurePKI(pkiDir)).To(Succeed())

		fp2, err := os.ReadFile(filepath.Join(pkiDir, ".fingerprint"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fp2)).To(Equal(string(fp1)))
	})

	It("regenerates when certs are corrupted", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")

		Expect(ensurePKI(pkiDir)).To(Succeed())

		fp1, err := os.ReadFile(filepath.Join(pkiDir, ".fingerprint"))
		Expect(err).NotTo(HaveOccurred())

		// Corrupt a cert file
		Expect(os.WriteFile(filepath.Join(pkiDir, "ca.pem"), []byte("corrupted"), 0o644)).To(Succeed())

		// Should regenerate
		Expect(ensurePKI(pkiDir)).To(Succeed())

		fp2, err := os.ReadFile(filepath.Join(pkiDir, ".fingerprint"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fp2)).NotTo(Equal(string(fp1)))
	})
})

var _ = Describe("connectClientWithTLS", func() {
	It("returns an error when cert files do not exist", func() {
		paths := embeddedTLSPaths{
			CACert:     "/nonexistent/ca.pem",
			ClientCert: "/nonexistent/client.pem",
			ClientKey:  "/nonexistent/client-key.pem",
			ServerCert: "/nonexistent/server.pem",
			ServerKey:  "/nonexistent/server-key.pem",
		}
		client, _, err := connectClientWithTLS("localhost:0", paths)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("read CA cert"))
		Expect(client).To(BeNil())
	})

	It("returns an error when CA cert is invalid PEM", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")
		Expect(ensurePKI(pkiDir)).To(Succeed())

		paths := pkiPaths(pkiDir)
		Expect(os.WriteFile(paths.CACert, []byte("not-valid-pem"), 0o644)).To(Succeed())

		client, _, err := connectClientWithTLS("localhost:0", paths)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to parse CA cert"))
		Expect(client).To(BeNil())
	})

	It("returns an error when client cert/key pair is invalid", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")
		Expect(ensurePKI(pkiDir)).To(Succeed())

		paths := pkiPaths(pkiDir)
		Expect(os.WriteFile(paths.ClientCert, []byte("not-a-cert"), 0o644)).To(Succeed())

		client, _, err := connectClientWithTLS("localhost:0", paths)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("load client cert"))
		Expect(client).To(BeNil())
	})

	It("creates a non-nil client with valid certs", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")
		Expect(ensurePKI(pkiDir)).To(Succeed())

		paths := pkiPaths(pkiDir)
		client, _, err := connectClientWithTLS("localhost:0", paths)
		Expect(err).NotTo(HaveOccurred())
		Expect(client).NotTo(BeNil())
	})
})

var _ = Describe("embeddedAuthRegistry", func() {
	It("creates a registry that authenticates with embedded client certs", func() {
		dir := GinkgoT().TempDir()
		pkiDir := filepath.Join(dir, "pki")
		Expect(ensurePKI(pkiDir)).To(Succeed())

		registry, err := embeddedAuthRegistry()
		Expect(err).NotTo(HaveOccurred())
		Expect(registry).NotTo(BeNil())

		// Load and parse client cert to build TLS state
		paths := pkiPaths(pkiDir)
		certPEM, err := os.ReadFile(paths.ClientCert)
		Expect(err).NotTo(HaveOccurred())

		block, _ := pem.Decode(certPEM)
		Expect(block).NotTo(BeNil())
		leaf, err := x509.ParseCertificate(block.Bytes)
		Expect(err).NotTo(HaveOccurred())

		req := &authn.Request{
			Method: authn.AuthMethodMTLS,
			TLSState: &tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{leaf},
			},
		}

		identity, err := registry.Authenticate(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(identity.TenantID).To(Equal("embedded"))
		Expect(identity.Roles).To(ContainElement("admin"))
		Expect(identity.Method).To(Equal(authn.AuthMethodMTLS))
	})
})

var _ = Describe("gateway.PoolAdminBackend", func() {
	It("implements gateway.AdminBackend", func() {
		var _ gateway.AdminBackend = (*gateway.PoolAdminBackend)(nil)
	})
})

var _ gateway.IngestionBackend = (*backend.PassthroughIngestion)(nil)
var _ gateway.PipelineBackend = (*localPipelineBackend)(nil)

// capturingJobStore records the job passed to CreateJob, so a spec can prove
// none was stored; every other pipeline.Store method is unused here and
// panics through the nil embed.
type capturingJobStore struct {
	pipeline.Store
	created *pipeline.Job
}

func (s *capturingJobStore) CreateJob(_ context.Context, job *pipeline.Job) error {
	s.created = job
	return nil
}

var _ = Describe("localPipelineBackend.CreateJob", func() {
	// Safety mechanism: embedded mode has no executor, so a stored job would
	// stay PENDING forever (#212).
	It("refuses with an actionable FailedPrecondition and stores no job", func() {
		ctx, err := tenant.WithTenant(context.Background(), "embedded")
		Expect(err).NotTo(HaveOccurred())
		ctx = tenant.WithUser(ctx, "alice@example.com")
		store := &capturingJobStore{}
		b := &localPipelineBackend{store: store}

		_, err = b.CreateJob(ctx, connectrpc.NewRequest(&pb.CreateJobRequest{
			JobType: pb.JobType_JOB_TYPE_FULL_ANALYSIS,
			Config:  &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1"},
		}))
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("crosscodexd"))
		Expect(err.Error()).To(ContainSubstring("cli.endpoint"))
		Expect(store.created).To(BeNil())
	})
})

// cancelRecordingStore panics on every pipeline.Store method through the
// nil embed except UpdateJobStatus, which it records.
type cancelRecordingStore struct {
	pipeline.Store
	updates []pipeline.JobStatus
}

func (s *cancelRecordingStore) UpdateJobStatus(_ context.Context, _ string, status pipeline.JobStatus, _ error) error {
	s.updates = append(s.updates, status)
	return nil
}

var _ = Describe("localPipelineBackend.CancelJob", func() {
	// Embedded mode runs no job: a job that is still running is a daemon's,
	// which goes on executing whatever the database says. Writing CANCELLED
	// here would leave a daemon writing to a job recorded as cancelled.
	It("refuses with an actionable FailedPrecondition and writes nothing", func() {
		ctx, err := tenant.WithTenant(context.Background(), "embedded")
		Expect(err).NotTo(HaveOccurred())
		store := &cancelRecordingStore{}
		b := &localPipelineBackend{store: store}

		_, err = b.CancelJob(ctx, connectrpc.NewRequest(&pb.CancelJobRequest{JobId: "job-1"}))
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeFailedPrecondition))
		Expect(err.Error()).To(And(ContainSubstring("crosscodexd"), ContainSubstring("cli.endpoint")))
		Expect(store.updates).To(BeEmpty())
	})
})

var _ = Describe("localPipelineBackend.RetryJob", func() {
	// A retried job would stay running forever, as a created one would stay
	// pending: embedded mode has no executor. The store is never touched
	// (its nil embed would panic).
	DescribeTable("refuses with an actionable FailedPrecondition and changes no job",
		func(fromFailure bool) {
			ctx, err := tenant.WithTenant(context.Background(), "embedded")
			Expect(err).NotTo(HaveOccurred())
			b := &localPipelineBackend{store: &capturingJobStore{}}

			_, err = b.RetryJob(ctx, connectrpc.NewRequest(&pb.RetryJobRequest{JobId: "job-1", RetryFromFailure: fromFailure}))
			Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeFailedPrecondition))
			Expect(err.Error()).To(And(ContainSubstring("crosscodexd"), ContainSubstring("cli.endpoint")))
		},
		Entry("from its failure", true),
		Entry("as a new job", false),
	)
})

var _ = Describe("buildEmbeddedService", func() {
	It("returns an error when database DSN is empty", func() {
		cfg := &config.Config{}
		_, _, err := buildEmbeddedService(context.Background(), cfg, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("database not configured"))
	})
})

var _ = Describe("startEmbeddedDaemon telemetry initialization", func() {
	var stateDir string

	BeforeEach(func() {
		stateDir = GinkgoT().TempDir()
		DeferCleanup(testspecs.IsolateTelemetryGlobals())
	})

	It("initializes telemetry before building the embedded service", func() {
		state := &cliState{fullCfg: &config.Config{}}

		err := startEmbeddedDaemon(context.Background(), state, stateDir, filepath.Join(stateDir, "daemon.pid"))

		Expect(err).To(MatchError(ContainSubstring("database not configured")))
		Expect(otel.GetTextMapPropagator().Fields()).To(ContainElement("traceparent"),
			"telemetry.Init must run before the embedded gateway is built")
	})

	It("refuses to start with an actionable error when observability config is invalid", func() {
		cfg := &config.Config{}
		cfg.Observability.Endpoint = "localhost:4317"
		cfg.Observability.Tracing.Protocol = "carrier-pigeon"
		state := &cliState{fullCfg: cfg}

		err := startEmbeddedDaemon(context.Background(), state, stateDir, filepath.Join(stateDir, "daemon.pid"))

		Expect(err).To(MatchError(ContainSubstring("initialize telemetry")))
		Expect(err).To(MatchError(ContainSubstring(`unsupported tracing protocol "carrier-pigeon"`)))
		Expect(state.daemon).To(BeNil())
		Expect(filepath.Join(stateDir, "pki")).NotTo(BeADirectory(),
			"an invalid observability config must stop startup before PKI bootstrap")
		Expect(otel.GetTextMapPropagator().Fields()).To(BeEmpty())
	})
})

// failedJobStore serves one failed job of tenant "embedded" with no stages;
// every other pipeline.Store method panics through the nil embed.
type failedJobStore struct {
	pipeline.Store
	job *pipeline.Job
}

func (s *failedJobStore) GetJob(context.Context, string) (*pipeline.Job, error) { return s.job, nil }

func (s *failedJobStore) GetStages(context.Context, string) ([]*pipeline.Stage, error) {
	return nil, nil
}

// backendJobClient answers GetJob from an embedded localPipelineBackend, as
// the embedded gateway does for tenant "embedded"; every other
// GatewayServiceClient method panics through the nil embed.
type backendJobClient struct {
	crosscodexv1connect.GatewayServiceClient
	backend *localPipelineBackend
}

func (c *backendJobClient) GetJob(ctx context.Context, req *connectrpc.Request[pb.GetJobRequest]) (*connectrpc.Response[pb.GetJobResponse], error) {
	ctx, err := tenant.WithTenant(ctx, "embedded")
	if err != nil {
		return nil, err
	}
	return c.backend.GetJob(ctx, req)
}

var _ = Describe("run status in embedded mode", func() {
	// The stored message may be a raw one recorded before tenant messages,
	// so it holds an escape sequence and a line break.
	It("shows a failed job's error, cleaned, in text and in JSON", func() {
		store := &failedJobStore{job: &pipeline.Job{JobID: "job-1", TenantID: "embedded", Status: pipeline.JobStatusFailed,
			Config: []byte(`{}`), ErrorMessage: "job job-1 failed\x1b[2J\nfake line"}}
		statusOf := func(args ...string) string {
			cmd := newRunCmd(&cliState{client: &backendJobClient{backend: &localPipelineBackend{store: store}}, daemon: &embeddedDaemon{}})
			cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
			out, _, err := execute(cmd, append([]string{"status", "job-1"}, args...)...)
			Expect(err).NotTo(HaveOccurred())
			return out
		}
		want := intanalyzer.CleanDisplay(store.job.ErrorMessage, maxJobErrorBytes)
		Expect(want).NotTo(ContainSubstring("\x1b"))

		Expect(statusOf()).To(ContainSubstring("Error: " + want + "\n"))
		var got map[string]any
		Expect(json.Unmarshal([]byte(statusOf("--json")), &got)).To(Succeed())
		Expect(got).To(HaveKeyWithValue("error", HaveKeyWithValue("message", want)))
	})
})

var _ = Describe("localJobToProto", func() {
	It("counts completed, failed and cancelled stages apart", func() {
		stages := []*pipeline.Stage{
			{StageName: "classify", Status: pipeline.StageStatusCompleted},
			{StageName: "embedding", Status: pipeline.StageStatusFailed},
			{StageName: "synthesis", Status: pipeline.StageStatusCancelled},
			{StageName: "graph", Status: pipeline.StageStatusPending},
		}
		p := localJobToProto(&pipeline.Job{JobID: "job-1"}, stages).GetProgress()
		Expect([]int32{p.GetTotalSteps(), p.GetCompletedSteps(), p.GetFailedSteps(), p.GetCancelledSteps()}).
			To(Equal([]int32{4, 1, 1, 1}))
	})
})
