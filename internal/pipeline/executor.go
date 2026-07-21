package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// jobExecution carries attestation state through the execution phases.
type jobExecution struct {
	layout *attestation.SignedLayout
	links  []*attestation.SignedLink
}

// executeJob is the main lifecycle method. Called as a goroutine from CreateJob and Start.
func (s *Service) executeJob(ctx context.Context, tenantID, jobID string) {
	defer s.wg.Done()

	start := time.Now()
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.ExecuteJob")
	defer span.End()
	span.SetAttributes(
		attribute.String("job.id", jobID),
		attribute.String("tenant.id", tenantID),
	)

	defer func() {
		s.mu.Lock()
		delete(s.running, jobID)
		s.mu.Unlock()
		if s.jobDuration != nil {
			s.jobDuration.Record(ctx, float64(time.Since(start).Milliseconds()))
		}
	}()

	// Set tenant in context for downstream RLS.
	ctx, err := tenant.WithTenant(ctx, tenantID)
	if err != nil {
		s.failJob(ctx, tenantID, jobID, fmt.Errorf("setting tenant context: %w", err))
		return
	}

	// Phase 1: Load job and update to running.
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		s.failJob(ctx, tenantID, jobID, fmt.Errorf("loading job: %w", err))
		return
	}

	if err := s.store.UpdateJobStatus(ctx, jobID, JobStatusRunning, nil); err != nil {
		s.failJob(ctx, tenantID, jobID, fmt.Errorf("updating job status: %w", err))
		return
	}
	if err := s.publishJobState(ctx, tenantID, jobID, JobStatusRunning); err != nil {
		s.logger.WarnContext(ctx, "failed to publish job state", "job_id", jobID, "error", err)
	}

	// Load existing stages (for resume).
	stages, err := s.store.GetStages(ctx, jobID)
	if err != nil {
		s.failJob(ctx, tenantID, jobID, fmt.Errorf("loading stages: %w", err))
		return
	}
	completed := completedStageSet(stages)

	// Attestation state for this job execution.
	exec := &jobExecution{}

	// Phase 2: Analysis.
	if err := s.runAnalysis(ctx, job, completed, exec); err != nil {
		if ctx.Err() != nil {
			s.cancelJob(ctx, tenantID, jobID)
			return
		}
		s.failJob(ctx, tenantID, jobID, err)
		return
	}

	// Phase 3: Synthesis.
	if !completed["synthesis"] {
		if err := s.runSynthesis(ctx, tenantID, jobID, exec); err != nil {
			if ctx.Err() != nil {
				s.cancelJob(ctx, tenantID, jobID)
				return
			}
			s.failJob(ctx, tenantID, jobID, err)
			return
		}
	}

	// Phase 4: Graph.
	if !completed["graph"] {
		if err := s.runGraph(ctx, tenantID, jobID, exec); err != nil {
			if ctx.Err() != nil {
				s.cancelJob(ctx, tenantID, jobID)
				return
			}
			s.failJob(ctx, tenantID, jobID, err)
			return
		}
	}

	// Phase 5: Attestation finalization.
	if err := s.finalizeAttestation(ctx, job, exec); err != nil {
		s.logger.WarnContext(ctx, "attestation finalization failed",
			"job_id", jobID, "error", err)
		// Non-fatal: job still completes.
	}

	// Done.
	if err := s.store.UpdateJobStatus(ctx, jobID, JobStatusCompleted, nil); err != nil {
		s.logger.ErrorContext(ctx, "failed to mark job completed",
			"job_id", jobID, "error", err)
	}
	if err := s.publishJobState(ctx, tenantID, jobID, JobStatusCompleted); err != nil {
		s.logger.WarnContext(ctx, "failed to publish job state", "job_id", jobID, "error", err)
	}
	if s.jobCounter != nil {
		s.jobCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("status", string(JobStatusCompleted))))
	}
}

// runAnalysis builds the DAG, determines incomplete analyzers, and executes them.
func (s *Service) runAnalysis(ctx context.Context, job *Job, completed map[string]bool, exec *jobExecution) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.RunAnalysis")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", job.JobID))

	// Build DAG from registry.
	dag, err := s.registry.BuildDAG(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("building DAG: %w", err)
	}

	// Create attestation layout before analysis starts.
	layout, err := s.createLayout(ctx, job, dag)
	if err != nil {
		s.logger.WarnContext(ctx, "failed to create attestation layout",
			"job_id", job.JobID, "error", err)
		// Non-fatal: continue without attestation.
	} else {
		exec.layout = layout
	}

	// Determine incomplete analyzer names (not in completed set).
	dagOrder := dag.Order()
	var incompleteAnalyzers []string
	for _, name := range dagOrder {
		if !completed[name] {
			incompleteAnalyzers = append(incompleteAnalyzers, name)
		}
	}

	// All complete = skip (resume case).
	if len(incompleteAnalyzers) == 0 {
		span.SetStatus(codes.Ok, "all analyzers already completed")
		return nil
	}

	span.SetAttributes(attribute.Int("incomplete.count", len(incompleteAnalyzers)))

	// Parse config from job.
	var jobConfig map[string]interface{}
	if err := json.Unmarshal(job.Config, &jobConfig); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("unmarshaling job config: %w", err)
	}

	// Build ExecutionRequest.
	req := analysis.ExecutionRequest{
		JobID:         job.JobID,
		AnalyzerNames: incompleteAnalyzers,
		Input:         &emptypb.Empty{},
	}

	// Execute via engine.
	result, err := s.engine.Execute(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("engine.Execute: %w", err)
	}

	// Check result.Failed for errors.
	if len(result.Failed) > 0 {
		err := fmt.Errorf("analysis failed: %d analyzers failed: %v", len(result.Failed), result.Failed)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	// Create link for each completed analyzer.
	for analyzerName, output := range result.Outputs {
		// Materials: outputs from dependencies.
		var materials []attestation.Artifact
		for _, dep := range dag.Analyzers() {
			if dep.Name() == analyzerName {
				for _, depName := range dep.DependsOn() {
					if depOutput, ok := result.Outputs[depName]; ok {
						digest := computeDigest(depOutput)
						materials = append(materials, attestation.Artifact{
							URI:    depName + ".output",
							Digest: digest,
						})
					}
				}
				break
			}
		}

		// Products: this analyzer's output.
		digest := computeDigest(output)
		products := []attestation.Artifact{
			{URI: analyzerName + ".output", Digest: digest},
		}

		link, err := s.createLink(ctx, job, analyzerName, materials, products)
		if err != nil {
			s.logger.WarnContext(ctx, "failed to create link for analyzer",
				"job_id", job.JobID, "analyzer", analyzerName, "error", err)
			// Non-fatal: continue.
		} else {
			exec.links = append(exec.links, link)
		}
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

// runSynthesis updates stage status and calls the synthesis executor.
func (s *Service) runSynthesis(ctx context.Context, tenantID, jobID string, exec *jobExecution) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.RunSynthesis")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", jobID))

	if err := s.store.UpdateStageStatus(ctx, jobID, "synthesis", StageStatusRunning); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("updating synthesis stage to running: %w", err)
	}
	s.publishStageEvent(ctx, tenantID, jobID, "synthesis", natsbus.StageStarted)

	var inputs []synthesis.SynthesisInput
	classifications := make(map[string]synthesis.Classification)

	synthResult, err := s.synthesis.Execute(ctx, jobID, inputs, classifications)
	if err != nil {
		_ = s.store.UpdateStageStatus(ctx, jobID, "synthesis", StageStatusFailed)
		s.publishStageEvent(ctx, tenantID, jobID, "synthesis", natsbus.StageFailed)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("synthesis.Execute: %w", err)
	}

	if err := s.store.UpdateStageStatus(ctx, jobID, "synthesis", StageStatusCompleted); err != nil {
		s.logger.WarnContext(ctx, "failed to update synthesis stage to completed", "job_id", jobID, "error", err)
	}
	s.publishStageEvent(ctx, tenantID, jobID, "synthesis", natsbus.StageCompleted)

	// Create attestation link for synthesis.
	// Materials: analysis outputs (stored in exec.links).
	var materials []attestation.Artifact
	for _, link := range exec.links {
		materials = append(materials, link.Products...)
	}

	// Products: synthesis output (if any).
	var products []attestation.Artifact
	if synthResult != nil {
		digest := computeDigest(synthResult)
		products = append(products, attestation.Artifact{
			URI:    "synthesis.output",
			Digest: digest,
		})
	}

	job := &Job{TenantID: tenantID, JobID: jobID}
	link, err := s.createLink(ctx, job, "synthesis", materials, products)
	if err != nil {
		s.logger.WarnContext(ctx, "failed to create synthesis link",
			"job_id", jobID, "error", err)
	} else {
		exec.links = append(exec.links, link)
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

// runGraph updates stage status and publishes completion event to NATS.
// The graph service subscribes asynchronously.
func (s *Service) runGraph(ctx context.Context, tenantID, jobID string, exec *jobExecution) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.RunGraph")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", jobID))

	if err := s.store.UpdateStageStatus(ctx, jobID, "graph", StageStatusRunning); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("updating graph stage to running: %w", err)
	}
	s.publishStageEvent(ctx, tenantID, jobID, "graph", natsbus.StageStarted)

	// Graph service subscribes to stage.completed events and materializes asynchronously.
	if err := s.store.UpdateStageStatus(ctx, jobID, "graph", StageStatusCompleted); err != nil {
		s.logger.WarnContext(ctx, "failed to update graph stage to completed", "job_id", jobID, "error", err)
	}
	s.publishStageEvent(ctx, tenantID, jobID, "graph", natsbus.StageCompleted)

	// Create attestation link for graph.
	// Materials: synthesis output (last link's products).
	var materials []attestation.Artifact
	if len(exec.links) > 0 {
		materials = exec.links[len(exec.links)-1].Products
	}

	var products []attestation.Artifact

	job := &Job{TenantID: tenantID, JobID: jobID}
	link, err := s.createLink(ctx, job, "graph", materials, products)
	if err != nil {
		s.logger.WarnContext(ctx, "failed to create graph link",
			"job_id", jobID, "error", err)
	} else {
		exec.links = append(exec.links, link)
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

// failJob updates status to failed, publishes state event, records metric.
func (s *Service) failJob(ctx context.Context, tenantID, jobID string, err error) {
	s.logger.ErrorContext(ctx, "job failed", "job_id", jobID, "error", err)

	if updateErr := s.store.UpdateJobStatus(ctx, jobID, JobStatusFailed, err); updateErr != nil {
		s.logger.ErrorContext(ctx, "failed to update job status to failed",
			"job_id", jobID, "error", updateErr)
	}

	if err := s.publishJobState(ctx, tenantID, jobID, JobStatusFailed); err != nil {
		s.logger.WarnContext(ctx, "failed to publish job state", "job_id", jobID, "error", err)
	}

	if s.jobCounter != nil {
		s.jobCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("status", string(JobStatusFailed))))
	}
}

// cancelJob updates status to cancelled, publishes state event.
func (s *Service) cancelJob(ctx context.Context, tenantID, jobID string) {
	s.logger.InfoContext(ctx, "job cancelled", "job_id", jobID)

	if err := s.store.UpdateJobStatus(ctx, jobID, JobStatusCancelled, nil); err != nil {
		s.logger.ErrorContext(ctx, "failed to update job status to cancelled",
			"job_id", jobID, "error", err)
	}

	if err := s.publishJobState(ctx, tenantID, jobID, JobStatusCancelled); err != nil {
		s.logger.WarnContext(ctx, "failed to publish job state", "job_id", jobID, "error", err)
	}
}

// publishStageEvent builds PipelineStageSubject, marshals JSON event, publishes to NATS.
func (s *Service) publishStageEvent(ctx context.Context, tenantID, jobID, stageName string, stage natsbus.Stage) {
	if s.bus == nil {
		return
	}

	subject, err := natsbus.PipelineStageSubject(tenantID, jobID, stage)
	if err != nil {
		s.logger.WarnContext(ctx, "failed to build pipeline stage subject",
			"tenant_id", tenantID, "job_id", jobID, "stage", stage, "error", err)
		return
	}

	event := map[string]interface{}{
		"tenant_id":  tenantID,
		"job_id":     jobID,
		"stage_name": stageName,
		"stage":      string(stage),
		"timestamp":  time.Now().Unix(),
	}

	payload, err := json.Marshal(event)
	if err != nil {
		s.logger.WarnContext(ctx, "failed to marshal stage event", "error", err)
		return
	}

	if err := s.bus.Publish(ctx, subject, payload); err != nil {
		s.logger.WarnContext(ctx, "failed to publish stage event",
			"subject", subject, "error", err)
	}
}

// completedStageSet returns a set of stage names with status "completed".
func completedStageSet(stages []*Stage) map[string]bool {
	completed := make(map[string]bool)
	for _, stage := range stages {
		if stage.Status == StageStatusCompleted {
			completed[stage.StageName] = true
		}
	}
	return completed
}

// createLayout creates and stores the attestation layout for a job.
func (s *Service) createLayout(ctx context.Context, job *Job, dag *analyzer.DAG) (*attestation.SignedLayout, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.CreateLayout")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", job.JobID))

	// Convert DAG to layout options.
	layoutOpts := s.attConverter.Convert(dag)
	layoutOpts.ExpiresIn = s.attCfg.ExpiryDuration

	// Generate layout.
	layout, err := s.attestor.CreateLayout(ctx, layoutOpts)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("creating layout: %w", err)
	}

	// Store layout.
	path := fmt.Sprintf("attestations/%s/%s/layout.json", job.TenantID, job.JobID)
	reader := bytes.NewReader(layout.Raw)
	if err := s.storage.Put(ctx, path, reader); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("storing layout: %w", err)
	}

	span.SetStatus(codes.Ok, "")
	return layout, nil
}

// createLink creates and stores an attestation link for a pipeline step.
func (s *Service) createLink(ctx context.Context, job *Job, stepName string, materials, products []attestation.Artifact) (*attestation.SignedLink, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.CreateLink")
	defer span.End()
	span.SetAttributes(
		attribute.String("job.id", job.JobID),
		attribute.String("step", stepName),
	)

	// Generate link.
	link, err := s.attestor.CreateLink(ctx, stepName, materials, products)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("creating link: %w", err)
	}

	// Store link.
	path := fmt.Sprintf("attestations/%s/%s/links/%s.json", job.TenantID, job.JobID, stepName)
	reader := bytes.NewReader(link.Raw)
	if err := s.storage.Put(ctx, path, reader); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("storing link: %w", err)
	}

	span.SetStatus(codes.Ok, "")
	return link, nil
}

// finalizeAttestation verifies the attestation chain and stores the bundle.
func (s *Service) finalizeAttestation(ctx context.Context, job *Job, exec *jobExecution) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.FinalizeAttestation")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", job.JobID))

	// Skip if no layout created.
	if exec.layout == nil {
		span.SetStatus(codes.Ok, "no layout to finalize")
		return nil
	}

	// Verify chain.
	if err := s.attestor.VerifyChain(ctx, exec.layout, exec.links); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("verifying attestation chain: %w", err)
	}

	// Build bundle.
	linkRaws := make([]json.RawMessage, len(exec.links))
	for i, link := range exec.links {
		linkRaws[i] = json.RawMessage(link.Raw)
	}

	bundle := map[string]interface{}{
		"layout": json.RawMessage(exec.layout.Raw),
		"links":  linkRaws,
	}

	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("marshaling bundle: %w", err)
	}

	// Store bundle.
	bundlePath := fmt.Sprintf("attestations/%s/%s/bundle.json", job.TenantID, job.JobID)
	reader := bytes.NewReader(bundleBytes)
	if err := s.storage.Put(ctx, bundlePath, reader); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("storing bundle: %w", err)
	}

	// Publish to audit trail.
	auditSubject, err := natsbus.AuditSubject(job.TenantID, natsbus.AuditEvents, job.JobID)
	if err != nil {
		s.logger.WarnContext(ctx, "failed to build audit subject",
			"job_id", job.JobID, "error", err)
		// Non-fatal.
	} else if s.bus != nil {
		if err := s.bus.Publish(ctx, auditSubject, bundleBytes); err != nil {
			s.logger.WarnContext(ctx, "failed to publish attestation bundle to audit trail",
				"job_id", job.JobID, "error", err)
			// Non-fatal.
		}
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

// computeDigest computes SHA-256 hex digest for analyzer outputs or proto messages.
func computeDigest(v interface{}) string {
	var data []byte
	switch t := v.(type) {
	case *analyzer.Output:
		// Digest the underlying proto Data.
		if t.Data != nil {
			marshaled, err := proto.Marshal(t.Data)
			if err == nil {
				data = marshaled
			}
		}
		if data == nil {
			data = []byte(t.AnalyzerName)
		}
	case proto.Message:
		marshaled, err := proto.Marshal(t)
		if err == nil {
			data = marshaled
		}
	}
	if data == nil {
		data = []byte(fmt.Sprintf("%v", v))
	}
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}
