package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/gateway/backend"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/internal/results"
	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	pkigen "github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
	"github.com/golang-migrate/migrate/v4"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type embeddedTLSPaths struct {
	CACert     string
	ClientCert string
	ClientKey  string
	ServerCert string
	ServerKey  string
}

func pkiPaths(pkiDir string) embeddedTLSPaths {
	return embeddedTLSPaths{
		CACert:     filepath.Join(pkiDir, "ca.pem"),
		ClientCert: filepath.Join(pkiDir, "client.pem"),
		ClientKey:  filepath.Join(pkiDir, "client-key.pem"),
		ServerCert: filepath.Join(pkiDir, "server.pem"),
		ServerKey:  filepath.Join(pkiDir, "server-key.pem"),
	}
}

// ensurePKI makes pkiDir hold the embedded daemon's TLS PKI, generating a
// development PKI only when needed. Concurrent CLI invocations are safe:
// testcerts.Ensure serializes them with a lock beside pkiDir, and the set is
// swapped in whole, so none sees a mixed or partial set. A set the CLI did not
// generate (no .fingerprint) is used if valid and never replaced.
func ensurePKI(pkiDir string) error {
	_, err := testcerts.Ensure(pkiDir, testcerts.EnsureOptions{
		// The CLI dials localhost:<port>, so that is the one SAN a set needs.
		Verify: testcerts.VerifyOptions{
			DNSNames:     []string{"localhost"},
			MinRemaining: 24 * time.Hour,
		},
		Generate: func() (*testcerts.PKI, error) {
			bundle, err := pkigen.GenerateDevPKI(
				pkigen.WithOrganization("CrossCodex Embedded"),
				pkigen.WithValidDuration(365*24*time.Hour),
				pkigen.WithDNSNames("localhost"),
				pkigen.WithIPs(net.IPv4(127, 0, 0, 1), net.IPv6loopback),
			)
			if err != nil {
				return nil, fmt.Errorf("generate embedded PKI: %w", err)
			}
			return &testcerts.PKI{
				CACert:     bundle.CA.CertPEM,
				CAKey:      bundle.CA.KeyPEM,
				ServerCert: bundle.Server.CertPEM,
				ServerKey:  bundle.Server.KeyPEM,
				ClientCert: bundle.Client.CertPEM,
				ClientKey:  bundle.Client.KeyPEM,
			}, nil
		},
		// Private keys live here; keep the directory owner-only.
		DirMode:       0o700,
		KeepUnmanaged: true,
	})
	if err != nil {
		return fmt.Errorf("embedded PKI in %s: %w", pkiDir, err)
	}
	return nil
}

func connectClientWithTLS(endpoint string, paths embeddedTLSPaths) (crosscodexv1connect.GatewayServiceClient, crosscodexv1connect.AdminServiceClient, error) {
	caCert, err := os.ReadFile(paths.CACert)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA cert: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, nil, fmt.Errorf("failed to parse CA cert")
	}

	clientCert, err := tls.LoadX509KeyPair(paths.ClientCert, paths.ClientKey)
	if err != nil {
		return nil, nil, fmt.Errorf("load client cert: %w", err)
	}

	tlsCfg := &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientCert},
		MinVersion:   tls.VersionTLS12, // DevSkim: ignore DS112852 - TLS 1.2 minimum floor is intentional; Go negotiates highest available
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}

	baseURL := "https://" + endpoint
	return crosscodexv1connect.NewGatewayServiceClient(httpClient, baseURL),
		crosscodexv1connect.NewAdminServiceClient(httpClient, baseURL),
		nil
}

type noopAuditEmitter struct{}

func (noopAuditEmitter) EmitAuthEvent(_ context.Context, _ *authn.AuthEvent) error { return nil }

const (
	// embeddedTenantID is the single tenant used in embedded mode. It is the
	// auth default tenant (see embeddedAuthRegistry) and is provisioned at
	// startup so tenant-scoped writes satisfy the tenants foreign key.
	embeddedTenantID          = "embedded"
	embeddedTenantDisplayName = "Embedded Single-Node"
)

func embeddedAuthRegistry() (*authn.Registry, error) {
	x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
		SingleTenant:  true,
		DefaultTenant: embeddedTenantID,
	})
	if err != nil {
		return nil, fmt.Errorf("create X.509 authenticator: %w", err)
	}

	tracer, meter := telemetry.Instrumentation("pkg/authn")
	return authn.NewRegistry(
		noopAuditEmitter{},
		[]authn.Authenticator{x509Auth},
		authn.WithTelemetry(tracer, meter),
	)
}

// localPipelineBackend serves job reads from the pipeline store but refuses
// to create, retry or cancel jobs. Embedded mode has no executor, NATS or
// workers, so a stored job would stay PENDING forever (#212). Analysis runs
// only under crosscodexd.
type localPipelineBackend struct {
	store pipeline.Store
}

// needsDaemon is why embedded mode refuses to create or retry a job.
const needsDaemon = `analysis needs the crosscodexd daemon: embedded mode stores and inspects catalogs but cannot run analysis. ` +
	`Start the crosscodexd daemon, point the CLI at it with "crosscodex config set cli.endpoint <url>", and rerun`

func (b *localPipelineBackend) CreateJob(context.Context, *connectrpc.Request[pb.CreateJobRequest]) (*connectrpc.Response[pb.CreateJobResponse], error) {
	return nil, connectrpc.NewError(connectrpc.CodeFailedPrecondition, errors.New(needsDaemon))
}

// RetryJob refuses like CreateJob: a retried job would run nowhere.
func (b *localPipelineBackend) RetryJob(context.Context, *connectrpc.Request[pb.RetryJobRequest]) (*connectrpc.Response[pb.RetryJobResponse], error) {
	return nil, connectrpc.NewError(connectrpc.CodeFailedPrecondition, errors.New(needsDaemon))
}

func (b *localPipelineBackend) GetJob(ctx context.Context, req *connectrpc.Request[pb.GetJobRequest]) (*connectrpc.Response[pb.GetJobResponse], error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, errors.New("missing tenant context"))
	}

	job, err := b.store.GetJob(ctx, req.Msg.GetJobId())
	if err != nil {
		if errors.Is(err, pipeline.ErrNotFound) {
			return nil, connectrpc.NewError(connectrpc.CodeNotFound, errors.New("job not found"))
		}
		return nil, connectrpc.NewError(connectrpc.CodeInternal, fmt.Errorf("get job: %w", err))
	}

	if job.TenantID != tenantID {
		return nil, connectrpc.NewError(connectrpc.CodeNotFound, errors.New("job not found"))
	}

	stages, err := b.store.GetStages(ctx, req.Msg.GetJobId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInternal, fmt.Errorf("get stages: %w", err))
	}

	return connectrpc.NewResponse(&pb.GetJobResponse{Job: localJobToProto(job, stages)}), nil
}

func (b *localPipelineBackend) ListJobs(ctx context.Context, req *connectrpc.Request[pb.ListJobsRequest]) (*connectrpc.Response[pb.ListJobsResponse], error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, errors.New("missing tenant context"))
	}

	var pageSize int32 = 50
	var pageToken string
	if req.Msg.Options != nil && req.Msg.Options.Pagination != nil {
		if req.Msg.Options.Pagination.PageSize > 0 {
			pageSize = req.Msg.Options.Pagination.PageSize
		}
		pageToken = req.Msg.Options.Pagination.PageToken
	}

	offset := 0
	if pageToken != "" {
		if _, err := fmt.Sscanf(pageToken, "%d", &offset); err != nil {
			offset = 0
		}
	}

	filter := pipeline.JobFilter{
		Limit:  int(pageSize),
		Offset: offset,
	}
	if req.Msg.Status != pb.JobStatus_JOB_STATUS_UNSPECIFIED {
		filter.Status = localJobStatusFromProto(req.Msg.Status)
	}

	jobs, total, err := b.store.ListJobs(ctx, tenantID, filter)
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInternal, fmt.Errorf("list jobs: %w", err))
	}

	pbJobs := make([]*pb.PipelineJob, len(jobs))
	for i, job := range jobs {
		stages, err := b.store.GetStages(ctx, job.JobID)
		if err != nil {
			stages = nil
		}
		pbJobs[i] = localJobToProto(job, stages)
	}

	nextPageToken := ""
	if int64(filter.Offset+filter.Limit) < total {
		nextPageToken = fmt.Sprintf("%d", filter.Offset+filter.Limit)
	}

	return connectrpc.NewResponse(&pb.ListJobsResponse{
		Jobs: pbJobs,
		PageInfo: &pb.PageInfo{
			NextPageToken: nextPageToken,
			TotalCount:    total,
		},
	}), nil
}

// CancelJob refuses: embedded mode runs no job, so a job that is still
// running is a crosscodexd daemon's. Only that daemon can stop the run;
// writing CANCELLED to the database here would leave it executing, and
// writing, a job recorded as cancelled.
func (b *localPipelineBackend) CancelJob(context.Context, *connectrpc.Request[pb.CancelJobRequest]) (*connectrpc.Response[pb.CancelJobResponse], error) {
	return nil, connectrpc.NewError(connectrpc.CodeFailedPrecondition, errors.New(
		`cancelling a job needs the crosscodexd daemon that runs it: embedded mode runs no job and cannot stop one. `+
			`Point the CLI at the daemon with "crosscodex config set cli.endpoint <url>", and rerun`))
}

// localJobToProto converts a pipeline.Job and its stages to the proto PipelineJob
// message. This mirrors pipeline.jobToProto but lives in the embedded package to
// avoid exporting internal helpers.
func localJobToProto(job *pipeline.Job, stages []*pipeline.Stage) *pb.PipelineJob {
	pbStages := make([]*pb.JobStage, len(stages))
	var completedSteps, failedSteps, cancelledSteps int32
	for i, stage := range stages {
		pbStage := &pb.JobStage{
			StageName:  stage.StageName,
			Status:     localStageStatusToProto(stage.Status),
			RetryCount: int32(stage.RetryCount),
		}
		if stage.StartedAt != nil {
			pbStage.StartedAt = timestamppb.New(*stage.StartedAt)
		}
		if stage.CompletedAt != nil {
			pbStage.CompletedAt = timestamppb.New(*stage.CompletedAt)
		}
		if stage.ErrorMessage != "" {
			pbStage.Error = &pb.Error{Message: stage.ErrorMessage}
		}
		pbStages[i] = pbStage

		switch stage.Status {
		case pipeline.StageStatusCompleted:
			completedSteps++
		case pipeline.StageStatusFailed:
			failedSteps++
		case pipeline.StageStatusCancelled:
			cancelledSteps++
		}
	}

	var cfg pb.JobConfig
	if err := json.Unmarshal(job.Config, &cfg); err != nil {
		cfg = pb.JobConfig{}
	}

	totalSteps := int32(len(stages))
	var completionPct float32
	if totalSteps > 0 {
		completionPct = float32(completedSteps) / float32(totalSteps) * 100.0
	}

	// The recorded message is written for the tenant, as pipeline's
	// jobToProto returns it.
	var jobErr *pb.Error
	if job.ErrorMessage != "" {
		jobErr = &pb.Error{Message: job.ErrorMessage}
	}

	return &pb.PipelineJob{
		JobId:  job.JobID,
		Error:  jobErr,
		Status: localJobStatusToProto(job.Status),
		Config: &cfg,
		Audit: &pb.AuditMetadata{
			CreatedAt: timestamppb.New(job.CreatedAt),
			UpdatedAt: timestamppb.New(job.UpdatedAt),
			CreatedBy: job.CreatedBy,
		},
		Progress: &pb.JobProgress{
			TotalSteps:           totalSteps,
			CompletedSteps:       completedSteps,
			FailedSteps:          failedSteps,
			CancelledSteps:       cancelledSteps,
			CompletionPercentage: completionPct,
		},
		Stages: pbStages,
	}
}

func localJobStatusToProto(js pipeline.JobStatus) pb.JobStatus {
	switch js {
	case pipeline.JobStatusPending:
		return pb.JobStatus_JOB_STATUS_PENDING
	case pipeline.JobStatusRunning:
		return pb.JobStatus_JOB_STATUS_RUNNING
	case pipeline.JobStatusCompleted:
		return pb.JobStatus_JOB_STATUS_COMPLETED
	case pipeline.JobStatusFailed:
		return pb.JobStatus_JOB_STATUS_FAILED
	case pipeline.JobStatusCancelled:
		return pb.JobStatus_JOB_STATUS_CANCELLED
	default:
		return pb.JobStatus_JOB_STATUS_UNSPECIFIED
	}
}

func localJobStatusFromProto(pbStatus pb.JobStatus) pipeline.JobStatus {
	switch pbStatus {
	case pb.JobStatus_JOB_STATUS_PENDING:
		return pipeline.JobStatusPending
	case pb.JobStatus_JOB_STATUS_RUNNING:
		return pipeline.JobStatusRunning
	case pb.JobStatus_JOB_STATUS_COMPLETED:
		return pipeline.JobStatusCompleted
	case pb.JobStatus_JOB_STATUS_FAILED:
		return pipeline.JobStatusFailed
	case pb.JobStatus_JOB_STATUS_CANCELLED:
		return pipeline.JobStatusCancelled
	default:
		return ""
	}
}

func localStageStatusToProto(ss pipeline.StageStatus) pb.JobStatus {
	switch ss {
	case pipeline.StageStatusPending:
		return pb.JobStatus_JOB_STATUS_PENDING
	case pipeline.StageStatusRunning:
		return pb.JobStatus_JOB_STATUS_RUNNING
	case pipeline.StageStatusCompleted:
		return pb.JobStatus_JOB_STATUS_COMPLETED
	case pipeline.StageStatusFailed:
		return pb.JobStatus_JOB_STATUS_FAILED
	case pipeline.StageStatusCancelled:
		return pb.JobStatus_JOB_STATUS_CANCELLED
	default:
		return pb.JobStatus_JOB_STATUS_UNSPECIFIED
	}
}

// embeddedResources holds resources that need cleanup when the embedded daemon stops.
type embeddedResources struct {
	dbPool  dbpkg.Pool
	storage storage.Provider
}

func (r *embeddedResources) close() {
	if r.storage != nil {
		r.storage.Close()
	}
	if r.dbPool != nil {
		r.dbPool.Close()
	}
}

func buildEmbeddedService(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*gateway.Service, *embeddedResources, error) {
	if cfg.Database.DSN == "" {
		return nil, nil, fmt.Errorf("database not configured for embedded mode\n\n" +
			"  Set a database DSN:\n" +
			// secretlint-disable-next-line @secretlint/secretlint-rule-database-connection-string -- error-message example DSN with dev-only credentials (user: postgres, password: integration, host: localhost)
			"    crosscodex config set database.dsn \"postgres://postgres:integration@localhost:15432/crosscodex_test?sslmode=disable\"\n\n" +
			"  Or start the development database:\n" +
			"    task dev:up")
	}

	if logger == nil {
		logger = slog.Default()
	}

	resources := &embeddedResources{}

	// Run migrations
	migrator, err := dbpkg.NewMigrator(cfg.Database.DSN)
	if err != nil {
		return nil, nil, fmt.Errorf("create migrator: %w", err)
	}
	if err := migrator.Up(ctx); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		migrator.Close()
		return nil, nil, fmt.Errorf("run migrations: %w", err)
	}
	migrator.Close()

	// Connect to database
	poolCfg := dbpkg.NewPoolConfigFrom(
		cfg.Database.DSN,
		cfg.Database.GraphDSN,
		cfg.Database.MaxConns,
		cfg.Database.SSLMode,
		cfg.Database.Extensions,
	)
	dbTracer, dbMeter := telemetry.Instrumentation("pkg/db")
	pool, err := dbpkg.NewPool(poolCfg, dbpkg.WithTelemetry(dbTracer, dbMeter))
	if err != nil {
		return nil, nil, fmt.Errorf("connect to database: %w", err)
	}
	resources.dbPool = pool

	// Verify required extensions
	if err := pool.VerifyExtensions(ctx); err != nil {
		resources.close()
		return nil, nil, fmt.Errorf("verify database extensions: %w", err)
	}

	// Provision the embedded tenant so tenant-scoped writes (catalogs, jobs, …)
	// satisfy the tenants foreign key. Runs on the owner pool, which bypasses
	// the tenants RLS policy (see pkg/db.EnsureTenant). Idempotent across restarts.
	if err := dbpkg.EnsureTenant(ctx, pool, embeddedTenantID, embeddedTenantDisplayName); err != nil {
		resources.close()
		return nil, nil, fmt.Errorf("provision embedded tenant: %w", err)
	}

	// Create local storage
	dataDir := embeddedDataDir()
	storageTracer, storageMeter := telemetry.Instrumentation("pkg/storage")
	localStorage, err := storage.NewLocal(dataDir, "embedded", storage.WithLocalTelemetry(storageTracer, storageMeter))
	if err != nil {
		resources.close()
		return nil, nil, fmt.Errorf("create local storage: %w", err)
	}
	resources.storage = localStorage

	tenantConn := dbpkg.NewTenantPool(pool)

	// Create catalog service (satisfies gateway.CatalogBackend)
	catalogStore := catalog.NewPGStore(tenantConn)
	oscalTracer, _ := telemetry.Instrumentation("pkg/oscal")
	parser := oscal.NewParser("", oscal.WithParserTracer(oscalTracer))
	catalogTracer, catalogMeter := telemetry.Instrumentation("internal/catalog")
	catalogSvc := catalog.NewService(
		catalog.WithParser(parser),
		catalog.WithStore(catalogStore),
		catalog.WithStorage(localStorage),
		catalog.WithLogger(logger),
		catalog.WithTracer(catalogTracer),
		catalog.WithMeter(catalogMeter),
	)

	// Create local ingestion backend (stores raw content to local storage)
	localIngestion := backend.NewPassthroughIngestion(localStorage)

	// Create local pipeline backend (records jobs, no execution)
	pipelineStore := pipeline.NewPGStore(tenantConn, pool,
		pipeline.WithStoreTelemetry(otel.GetTracerProvider(), otel.GetMeterProvider()))
	localPipeline := &localPipelineBackend{store: pipelineStore}

	resultsTracer, resultsMeter := telemetry.Instrumentation("internal/results")
	mappings := results.NewService(
		results.NewPGStore(tenantConn,
			results.WithStoreTelemetry(otel.GetTracerProvider(), otel.GetMeterProvider())),
		results.WithLogger(logger),
		results.WithTelemetry(resultsTracer, resultsMeter),
	)

	// Create auth registry
	registry, err := embeddedAuthRegistry()
	if err != nil {
		resources.close()
		return nil, nil, fmt.Errorf("create auth registry: %w", err)
	}

	// Build gateway service with real backends
	gatewayTracer, gatewayMeter := telemetry.Instrumentation("internal/gateway")
	svc := gateway.NewService(
		gateway.WithAuthn(registry),
		gateway.WithCatalogBackend(catalogSvc),
		gateway.WithIngestionBackend(localIngestion),
		gateway.WithPipelineBackend(localPipeline),
		gateway.WithMappingsBackend(mappings),
		gateway.WithCandidatesBackend(mappings),
		gateway.WithAdminBackend(gateway.NewPoolAdminBackend(pool)),
		gateway.WithMaxUploadSize(cfg.Server.MaxUploadSize),
		gateway.WithLogger(logger),
		gateway.WithTelemetry(gatewayTracer, gatewayMeter),
	)

	return svc, resources, nil
}

func embeddedDataDir() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "crosscodex", "data")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "crosscodex", "data")
}
