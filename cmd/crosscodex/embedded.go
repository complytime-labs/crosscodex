package main

import (
	"bytes"
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
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	pkigen "github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
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

func ensurePKI(pkiDir string) error {
	if err := testcerts.VerifyDir(pkiDir); err == nil {
		return nil
	}

	bundle, err := pkigen.GenerateDevPKI(
		pkigen.WithOrganization("CrossCodex Embedded"),
		pkigen.WithValidDuration(365*24*time.Hour),
		pkigen.WithDNSNames("localhost"),
		pkigen.WithIPs(net.IPv4(127, 0, 0, 1), net.IPv6loopback),
	)
	if err != nil {
		return fmt.Errorf("generate embedded PKI: %w", err)
	}

	pki := &testcerts.PKI{
		CACert:     bundle.CA.CertPEM,
		CAKey:      bundle.CA.KeyPEM,
		ServerCert: bundle.Server.CertPEM,
		ServerKey:  bundle.Server.KeyPEM,
		ClientCert: bundle.Client.CertPEM,
		ClientKey:  bundle.Client.KeyPEM,
	}

	if err := pki.WriteToDir(pkiDir); err != nil {
		return fmt.Errorf("write embedded PKI: %w", err)
	}

	// Lock down the directory
	if err := os.Chmod(pkiDir, 0o700); err != nil {
		return fmt.Errorf("chmod PKI directory: %w", err)
	}

	if err := testcerts.WriteFingerprint(pkiDir); err != nil {
		return fmt.Errorf("write PKI fingerprint: %w", err)
	}

	return nil
}

func connectClientWithTLS(endpoint string, paths embeddedTLSPaths) (crosscodexv1connect.GatewayServiceClient, error) {
	caCert, err := os.ReadFile(paths.CACert)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA cert")
	}

	clientCert, err := tls.LoadX509KeyPair(paths.ClientCert, paths.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
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

	return crosscodexv1connect.NewGatewayServiceClient(
		httpClient,
		"https://"+endpoint,
	), nil
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

	return authn.NewRegistry(
		noopAuditEmitter{},
		[]authn.Authenticator{x509Auth},
	)
}

type dbAdminBackend struct {
	pool dbpkg.Pool
}

func (b *dbAdminBackend) HealthCheck(ctx context.Context, _ *connectrpc.Request[pb.HealthCheckRequest]) (*connectrpc.Response[pb.HealthCheckResponse], error) {
	healthStatus, err := b.pool.Health(ctx)
	if err != nil {
		return connectrpc.NewResponse(&pb.HealthCheckResponse{
			Status: pb.HealthStatus_HEALTH_STATUS_UNHEALTHY,
		}), nil
	}
	if !healthStatus.Connected {
		return connectrpc.NewResponse(&pb.HealthCheckResponse{
			Status: pb.HealthStatus_HEALTH_STATUS_UNHEALTHY,
		}), nil
	}
	return connectrpc.NewResponse(&pb.HealthCheckResponse{
		Status: pb.HealthStatus_HEALTH_STATUS_HEALTHY,
	}), nil
}

// localIngestionBackend stores raw document content to local storage.
type localIngestionBackend struct {
	storage storage.Provider
}

func (b *localIngestionBackend) ConvertDocument(ctx context.Context, req *connectrpc.Request[pb.ConvertDocumentRequest]) (*connectrpc.Response[pb.ConvertDocumentResponse], error) {
	src, ok := req.Msg.Source.(*pb.ConvertDocumentRequest_Content)
	if !ok {
		return nil, connectrpc.NewError(connectrpc.CodeUnimplemented, errors.New("only inline content is supported; source_uri is not implemented"))
	}
	content := src.Content

	key := fmt.Sprintf("documents/%s.json", uuid.New().String())
	if err := b.storage.Put(ctx, key, bytes.NewReader(content)); err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInternal, fmt.Errorf("store document: %w", err))
	}

	return connectrpc.NewResponse(&pb.ConvertDocumentResponse{
		DocumentId: key,
		Status:     pb.JobStatus_JOB_STATUS_COMPLETED,
	}), nil
}

// localPipelineBackend records pipeline jobs via the pipeline store but does
// not execute them. This is the embedded-mode equivalent of pipeline.Service
// without DAG execution, NATS, or telemetry.
type localPipelineBackend struct {
	store pipeline.Store
}

func (b *localPipelineBackend) CreateJob(ctx context.Context, req *connectrpc.Request[pb.CreateJobRequest]) (*connectrpc.Response[pb.CreateJobResponse], error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, errors.New("missing tenant context"))
	}

	if req.Msg.Config == nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, errors.New("config is required"))
	}

	configBytes, err := json.Marshal(req.Msg.Config)
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, fmt.Errorf("marshal config: %w", err))
	}

	jobID := uuid.New().String()
	now := time.Now()
	job := &pipeline.Job{
		JobID:     jobID,
		TenantID:  tenantID,
		Status:    pipeline.JobStatusPending,
		Config:    configBytes,
		CreatedBy: tenantID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := b.store.CreateJob(ctx, job); err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInternal, fmt.Errorf("create job: %w", err))
	}

	return connectrpc.NewResponse(&pb.CreateJobResponse{
		JobId:  jobID,
		Status: pb.JobStatus_JOB_STATUS_PENDING,
	}), nil
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

func (b *localPipelineBackend) CancelJob(ctx context.Context, req *connectrpc.Request[pb.CancelJobRequest]) (*connectrpc.Response[pb.CancelJobResponse], error) {
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

	if err := b.store.UpdateJobStatus(ctx, req.Msg.GetJobId(), pipeline.JobStatusCancelled, nil); err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInternal, fmt.Errorf("cancel job: %w", err))
	}

	return connectrpc.NewResponse(&pb.CancelJobResponse{Cancelled: true}), nil
}

// localJobToProto converts a pipeline.Job and its stages to the proto PipelineJob
// message. This mirrors pipeline.jobToProto but lives in the embedded package to
// avoid exporting internal helpers.
func localJobToProto(job *pipeline.Job, stages []*pipeline.Stage) *pb.PipelineJob {
	pbStages := make([]*pb.JobStage, len(stages))
	var completedSteps, failedSteps int32
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

	return &pb.PipelineJob{
		JobId:  job.JobID,
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
	pool, err := dbpkg.NewPool(poolCfg)
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
	localStorage, err := storage.NewLocal(dataDir, "embedded")
	if err != nil {
		resources.close()
		return nil, nil, fmt.Errorf("create local storage: %w", err)
	}
	resources.storage = localStorage

	// Create catalog service (satisfies gateway.CatalogBackend)
	catalogStore := catalog.NewPGStore(pool)
	parser := oscal.NewParser("")
	catalogSvc := catalog.NewService(
		catalog.WithParser(parser),
		catalog.WithStore(catalogStore),
		catalog.WithStorage(localStorage),
		catalog.WithLogger(logger),
	)

	// Create local ingestion backend (stores raw content to local storage)
	localIngestion := &localIngestionBackend{storage: localStorage}

	// Create local pipeline backend (records jobs, no execution)
	tenantConn := dbpkg.NewTenantPool(pool)
	pipelineStore := pipeline.NewPGStore(tenantConn, pool)
	localPipeline := &localPipelineBackend{store: pipelineStore}

	// Create auth registry
	registry, err := embeddedAuthRegistry()
	if err != nil {
		resources.close()
		return nil, nil, fmt.Errorf("create auth registry: %w", err)
	}

	// Build gateway service with real backends
	svc := gateway.NewService(
		gateway.WithAuthn(registry),
		gateway.WithCatalogBackend(catalogSvc),
		gateway.WithIngestionBackend(localIngestion),
		gateway.WithPipelineBackend(localPipeline),
		gateway.WithAdminBackend(&dbAdminBackend{pool: pool}),
		gateway.WithMaxUploadSize(cfg.Server.MaxUploadSize),
		gateway.WithLogger(logger),
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
