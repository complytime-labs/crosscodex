package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/admin"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/internal/feedback"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/gateway/backend"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/internal/results"
	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/retention"
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
// generate (no .fingerprint) is used if valid and never replaced. A set the CLI
// generated also carries the attestation signing key pair; one that predates it
// fails verification and is regenerated whole. A supplied set is TLS-only, so
// it is not required to carry keys (see requireManagedAttestationKeys).
func ensurePKI(pkiDir string) error {
	_, err := testcerts.Ensure(pkiDir, testcerts.EnsureOptions{
		// The CLI dials localhost:<port>, so that is the one SAN a set needs.
		// A generated server cert is serverAuth-only, so the daemon's server
		// key cannot authenticate as the CLI's client; a CLI-generated set with
		// a dual-use cert fails here and is regenerated. A supplied set is
		// exempt (see testcerts.EnsureOptions.KeepUnmanaged).
		Verify: testcerts.VerifyOptions{
			DNSNames:       []string{"localhost"},
			MinRemaining:   24 * time.Hour,
			ServerAuthOnly: true,
			VerifyExtra:    requireManagedAttestationKeys,
		},
		WriteExtra: testcerts.WriteAttestationKeypair,
		Generate: func() (*testcerts.PKI, error) {
			bundle, err := pkigen.GenerateDevPKI(
				pkigen.WithOrganization("CrossCodex Embedded"),
				pkigen.WithValidDuration(365*24*time.Hour),
				pkigen.WithDNSNames("localhost"),
				pkigen.WithIPs(net.IPv4(127, 0, 0, 1), net.IPv6loopback),
				pkigen.WithExtKeyUsage(x509.ExtKeyUsageServerAuth),
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

// requireManagedAttestationKeys verifies the attestation key pair in dir only
// for a set the CLI generated (it has a .fingerprint). A set the operator
// supplied has TLS material only, and Ensure never rewrites it.
func requireManagedAttestationKeys(dir string) error {
	fingerprint, err := testcerts.ReadFingerprint(dir)
	if err != nil {
		return err
	}
	if fingerprint == "" {
		return nil
	}
	return testcerts.VerifyAttestationKeypair(dir)
}

// defaultAttestationKeyPaths points attestation at the key pair ensurePKI
// provisions in pkiDir when attestation is enabled and the user set neither
// path. A user-set path is never overridden, so a bad one still fails loudly
// where the keys are loaded.
func defaultAttestationKeyPaths(cfg *config.Config, pkiDir string) {
	a := &cfg.Attestation
	if !a.Enabled || a.PrivateKeyPath != "" || a.PublicKeyPath != "" {
		return
	}
	a.PrivateKeyPath = filepath.Join(pkiDir, testcerts.AttestationKeyFile)
	a.PublicKeyPath = filepath.Join(pkiDir, testcerts.AttestationPubFile)
}

func connectClientWithTLS(endpoint string, paths embeddedTLSPaths) (crosscodexv1connect.GatewayServiceClient, crosscodexv1connect.AdminServiceClient, crosscodexv1connect.EdgeCleaningServiceClient, error) {
	caCert, err := os.ReadFile(paths.CACert)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read CA cert: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, nil, nil, fmt.Errorf("failed to parse CA cert")
	}

	clientCert, err := tls.LoadX509KeyPair(paths.ClientCert, paths.ClientKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load client cert: %w", err)
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
		crosscodexv1connect.NewEdgeCleaningServiceClient(httpClient, baseURL),
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

// globLiteral escapes the glob metacharacters authn.X509Match patterns
// (filepath.Match) interpret, so a subject value matches only itself.
var globLiteral = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)

// embeddedAuthRegistry grants admin and reviewer of embeddedTenantID to the
// client certificate at clientCertPath, the one the CLI presents, matched by
// its exact common name and organization. The embedded daemon has a single
// local user, so the separation between administering and reviewing that the
// crosscodexd daemon enforces through auth.x509_mappings does not apply here:
// the one user votes and reads the review queue. Any other certificate the embedded CA
// signs, including the daemon's own server certificate, gets no identity.
// The subject is read from the certificate, so a PKI set the operator
// supplied works too.
func embeddedAuthRegistry(clientCertPath string) (*authn.Registry, error) {
	certPEM, err := os.ReadFile(clientCertPath)
	if err != nil {
		return nil, fmt.Errorf("read embedded client certificate: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("embedded client certificate %s is not PEM. Remove the PKI directory so the CLI generates a new set, or supply a valid one", clientCertPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse embedded client certificate %s: %w", clientCertPath, err)
	}
	// An empty CN pattern matches every CN, so without one the mapping could
	// admit other certificates the CA signs.
	if cert.Subject.CommonName == "" {
		return nil, fmt.Errorf("embedded client certificate %s has no common name, so it cannot be told apart from other certificates its CA signs. Supply one with a common name, or remove the PKI directory so the CLI generates a new set", clientCertPath)
	}
	match := authn.X509Match{CN: globLiteral.Replace(cert.Subject.CommonName)}
	if len(cert.Subject.Organization) > 0 {
		match.Organization = globLiteral.Replace(cert.Subject.Organization[0])
	}

	x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
		SingleTenant:  true,
		DefaultTenant: embeddedTenantID,
		Mappings:      []authn.X509Mapping{{Match: match, Roles: []string{authn.RoleAdmin, authn.RoleReviewer}}},
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
// only under crosscodexd. It lists a catalog's controls through the readers
// the daemon's pipeline uses, and renders prompts from them, the prompt
// layers and the analysis settings of the CLI's configuration.
type localPipelineBackend struct {
	store           pipeline.Store
	catalogNames    pipeline.CatalogNameReader
	catalogControls pipeline.CatalogControlsReader
	analysis        config.AnalysisConfig
	logger          *slog.Logger

	// The prompt registry is built from promptCfg on the first
	// RenderPrompt, not at startup: only prompt render reads the layers,
	// so a layer that does not load must not stop every embedded command.
	// A registry set beforehand (specs) is used as is.
	promptCfg   config.PromptConfig
	promptsOnce sync.Once
	prompts     prompt.Registry
	promptsErr  error
}

// RenderPrompt answers as the daemon's pipeline does, through the same
// pipeline.ServeRenderPrompt. Rendering calls no LLM, so it needs no daemon.
func (b *localPipelineBackend) RenderPrompt(ctx context.Context, req *connectrpc.Request[pb.RenderPromptRequest]) (*connectrpc.Response[pb.RenderPromptResponse], error) {
	b.promptsOnce.Do(func() {
		if b.prompts == nil {
			b.prompts, b.promptsErr = prompt.NewRegistry(b.promptCfg)
		}
	})
	if b.promptsErr != nil {
		return nil, connectrpc.NewError(connectrpc.CodeFailedPrecondition, fmt.Errorf(
			"cannot render the prompt: the configured prompt layers do not load: %w. "+
				`Fix prompt.layer_paths or prompt.layers in your configuration, check them with "crosscodex prompt layer", and rerun`, b.promptsErr))
	}
	return pipeline.ServeRenderPrompt(ctx, pipeline.RenderPromptSources{
		Prompts: b.prompts, Analysis: &b.analysis, CatalogNames: b.catalogNames,
		CatalogControls: b.catalogControls, Jobs: b.store, Logger: b.logger,
	}, req)
}

// ListCatalogControls answers as the daemon's pipeline does, through the
// same pipeline.ServeCatalogControls.
func (b *localPipelineBackend) ListCatalogControls(ctx context.Context, req *connectrpc.Request[pb.ListCatalogControlsRequest]) (*connectrpc.Response[pb.ListCatalogControlsResponse], error) {
	return pipeline.ServeCatalogControls(ctx, b.catalogNames, b.catalogControls, b.logger, req)
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

	progress, err := b.store.GetStageProgress(ctx, req.Msg.GetJobId())
	if err != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInternal, fmt.Errorf("get stage progress: %w", err))
	}

	pbJob := localJobToProto(job, stages)
	// Embedded mode runs no analysis, so progress rows exist only for jobs a
	// daemon ran against this database; they are shown as the daemon shows them.
	for i, stage := range stages {
		if p, ok := progress[stage.StageName]; ok {
			pbJob.Stages[i].Progress = pipeline.StageProgressToProto(p)
		}
	}
	// The panel names the models behind a stage. As the daemon does, an
	// unreadable record is logged and shown as not recorded, with no models,
	// so run status still works; an OSCAL export then refuses the job. Unlike
	// the daemon, a stage with no stored record gets no panel at all, where the
	// daemon shows "not recorded" for a completed panel stage.
	for i, stage := range stages {
		panel, err := pipeline.PanelProvenanceFromRecord(stage.PanelRecord)
		if err != nil {
			slog.WarnContext(ctx, "a stored panel record is unreadable, so run status shows it as not recorded",
				"job_id", job.JobID, "stage", stage.StageName, "error", err)
			panel = &pb.PanelProvenance{Independence: pipeline.PanelNotRecorded}
		}
		pbJob.Stages[i].Panel = panel
	}

	return connectrpc.NewResponse(&pb.GetJobResponse{Job: pbJob}), nil
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

// embeddedNoPurge is why the embedded daemon's AdminService refuses every
// retention scan that is not a dry run (admin.WithoutPurge).
const embeddedNoPurge = "the embedded daemon has no purge_user connection or audit stream, so it cannot archive or purge. " +
	"Rerun with --dry-run, or run the scan against crosscodexd (role all or gateway) configured with database.purge_dsn"

// buildEmbeddedService builds the embedded daemon's gateway service and its
// AdminService handler (holds, stats and dry-run scans; see embeddedNoPurge).
func buildEmbeddedService(ctx context.Context, cfg *config.Config, clientCertPath string, logger *slog.Logger) (*gateway.Service, crosscodexv1connect.AdminServiceHandler, *embeddedResources, error) {
	if cfg.Database.DSN == "" {
		return nil, nil, nil, fmt.Errorf("database not configured for embedded mode\n\n" +
			"  Set a database DSN:\n" +
			// secretlint-disable-next-line @secretlint/secretlint-rule-database-connection-string -- error-message example DSN with dev-only credentials (user: postgres, password: integration, host: localhost)
			"    crosscodex config set database.dsn \"postgres://postgres:integration@localhost:15432/crosscodex_test?sslmode=disable\"\n\n" +
			"  Or start the development database:\n" +
			"    task dev:up")
	}

	if logger == nil {
		logger = slog.Default()
	}

	// Catalog import is the embedded daemon's only signer. A key pair that
	// does not load refuses startup before the database is touched.
	attTracer, attMeter := telemetry.Instrumentation("pkg/attestation")
	attestors, err := pipeline.NewAttestors(cfg.Attestation, cfg.TLS.FIPS.Enabled, attTracer, attMeter)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("embedded daemon: %w", err)
	}
	retentionPolicy, err := retention.NewPolicy(cfg.Retention)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("embedded daemon: build retention policy: %w", err)
	}

	resources := &embeddedResources{}

	// Run migrations
	migrator, err := dbpkg.NewMigrator(cfg.Database.DSN)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create migrator: %w", err)
	}
	if err := migrator.Up(ctx); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		migrator.Close()
		return nil, nil, nil, fmt.Errorf("run migrations: %w", err)
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
		return nil, nil, nil, fmt.Errorf("connect to database: %w", err)
	}
	resources.dbPool = pool

	// Verify required extensions
	if err := pool.VerifyExtensions(ctx); err != nil {
		resources.close()
		return nil, nil, nil, fmt.Errorf("verify database extensions: %w", err)
	}

	// Provision the embedded tenant so tenant-scoped writes (catalogs, jobs, …)
	// satisfy the tenants foreign key. Runs on the owner pool, which bypasses
	// the tenants RLS policy (see pkg/db.EnsureTenant). Idempotent across restarts.
	if err := dbpkg.EnsureTenant(ctx, pool, embeddedTenantID, embeddedTenantDisplayName); err != nil {
		resources.close()
		return nil, nil, nil, fmt.Errorf("provision embedded tenant: %w", err)
	}

	// Create local storage
	dataDir := embeddedDataDir()
	storageTracer, storageMeter := telemetry.Instrumentation("pkg/storage")
	localStorage, err := storage.NewLocal(dataDir, "embedded", storage.WithLocalTelemetry(storageTracer, storageMeter))
	if err != nil {
		resources.close()
		return nil, nil, nil, fmt.Errorf("create local storage: %w", err)
	}
	resources.storage = localStorage

	tenantConn := dbpkg.NewTenantPool(pool)

	// Create catalog service (satisfies gateway.CatalogBackend)
	catalogStore := catalog.NewPGStore(tenantConn)
	oscalTracer, _ := telemetry.Instrumentation("pkg/oscal")
	parser := oscal.NewParser("", oscal.WithParserTracer(oscalTracer),
		oscal.WithRequirementPartNames(cfg.Catalog.RequirementPartNames))
	catalogTracer, catalogMeter := telemetry.Instrumentation("internal/catalog")
	catalogOpts := []catalog.ServiceOption{
		catalog.WithParser(parser),
		catalog.WithStructuring(cfg.Catalog.Structuring),
		catalog.WithStore(catalogStore),
		catalog.WithStorage(localStorage),
		catalog.WithLogger(logger),
		catalog.WithTracer(catalogTracer),
		catalog.WithMeter(catalogMeter),
	}
	// Only a configured ingestion service produces converted documents;
	// without one, ingestion-shaped JSON is an upload like any other.
	if cfg.Ingestion.Address != "" {
		// No Completer: the LLM tiers never run, so a converted document the
		// heading, table and pattern tiers cannot structure fails closed.
		catalogOpts = append(catalogOpts,
			catalog.WithStructurer(oscal.NewStructurer(nil, nil, oscal.WithStructurerTracer(oscalTracer))))
	}
	catalogSvc := catalog.NewService(catalogOpts...)

	// Ingestion stores OSCAL JSON to local storage unchanged and, when
	// ingestion.address is set, converts other documents through the
	// ingestion service. The embedded daemon ignores storage.objects and
	// keeps objects under dataDir, so that is the directory the ingestion
	// service must share.
	ingestionTracer, ingestionMeter := telemetry.Instrumentation("internal/gateway/backend")
	localIngestion, err := backend.NewIngestion(ctx, cfg.Ingestion, cfg.TLS,
		config.ObjectStorageConfig{Backend: string(storage.ProviderTypeLocal), BasePath: dataDir}, localStorage,
		backend.WithIngestionClientTelemetry(ingestionTracer, ingestionMeter),
		backend.WithIngestionClientLogger(logger))
	if err != nil {
		resources.close()
		return nil, nil, nil, fmt.Errorf("embedded daemon: %w", err)
	}

	// Create local pipeline backend (records jobs, no execution)
	pipelineStore := pipeline.NewPGStore(tenantConn, pool,
		pipeline.WithStoreTelemetry(otel.GetTracerProvider(), otel.GetMeterProvider()))
	localPipeline := &localPipelineBackend{
		store: pipelineStore,
		catalogNames: pipeline.NewPGCatalogNameReader(tenantConn,
			pipeline.WithPGCatalogNameReaderTelemetry(otel.GetTracerProvider(), otel.GetMeterProvider())),
		catalogControls: pipeline.NewPGCatalogControlsReader(tenantConn),
		analysis:        cfg.Analysis,
		logger:          logger,
		// Built on first render; see localPipelineBackend.
		promptCfg: cfg.Prompt,
	}

	resultsTracer, resultsMeter := telemetry.Instrumentation("internal/results")
	mappings := results.NewService(
		results.NewPGStore(tenantConn,
			results.WithStoreTelemetry(otel.GetTracerProvider(), otel.GetMeterProvider())),
		results.WithLogger(logger),
		results.WithTelemetry(resultsTracer, resultsMeter),
	)

	// Create auth registry
	registry, err := embeddedAuthRegistry(clientCertPath)
	if err != nil {
		resources.close()
		return nil, nil, nil, fmt.Errorf("create auth registry: %w", err)
	}

	// Votes and the review queue only: without feedback.WithDeterminations
	// the service refuses determinations with FailedPrecondition, since they retire
	// graph edges and the embedded daemon has no graph.
	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	feedbackSvc := feedback.NewService(
		feedback.NewPGStore(tenantConn, feedback.WithStoreTelemetry(tp, mp)),
		cfg.Feedback, feedback.WithTelemetry(tp, mp))

	// Holds and collectors use the retention pool, as in role all: it reaches
	// a deleted tenant's data as retention must. WithoutPurge refuses every
	// non-dry-run scan before an engine is built; the engine's nil scan lock
	// would refuse one again. No audit stream runs here, so there is no
	// stream drift to report.
	retentionConn := dbpkg.NewRetentionTenantPool(pool)
	holds := retention.NewPgHoldStore(retentionConn)
	engineFor := func(tenantID string, policy retention.Policy) (*retention.Engine, error) {
		collectors := []retention.Collector{
			retention.NewDBCollector(retentionConn),
			retention.NewCatalogCollector(retentionConn),
			retention.NewObjectCollector(localStorage, retention.ClassAttestation, tenantID),
		}
		return retention.NewEngine(collectors, holds, nil, nil, nil, policy, nil), nil
	}
	adminSvc := admin.NewService(engineFor, retentionPolicy, holds,
		func(context.Context) ([]retention.StreamDrift, error) { return nil, nil },
		admin.WithoutPurge(embeddedNoPurge))

	// Build gateway service with real backends
	gatewayTracer, gatewayMeter := telemetry.Instrumentation("internal/gateway")
	svc := gateway.NewService(
		gateway.WithAuthn(registry),
		gateway.WithCatalogBackend(catalogSvc),
		gateway.WithIngestionBackend(localIngestion),
		gateway.WithPipelineBackend(localPipeline),
		gateway.WithMappingsBackend(mappings),
		gateway.WithCandidatesBackend(mappings),
		gateway.WithFeedbackBackend(feedbackSvc),
		gateway.WithAdminBackend(gateway.NewPoolAdminBackend(pool)),
		gateway.WithAttestor(func(tenantID string) attestation.Generator {
			if att := attestors.ForTenant(tenantID); att != nil {
				return att.Generator
			}
			return nil
		}, localStorage),
		gateway.WithMaxUploadSize(cfg.Server.MaxUploadSize),
		gateway.WithLogger(logger),
		gateway.WithTelemetry(gatewayTracer, gatewayMeter),
	)

	return svc, adminSvc, resources, nil
}

func embeddedDataDir() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "crosscodex", "data")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "crosscodex", "data")
}
