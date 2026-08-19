package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
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
		if r := recover(); r != nil {
			// A panic in this detached goroutine would otherwise crash the
			// whole daemon and leave the job in 'running', crash-looping on
			// every resume. Recover, mark the job failed, and stay up.
			failCtx := ctx
			if tctx, terr := tenant.WithTenant(context.Background(), tenantID); terr == nil {
				failCtx = tctx
			}
			s.logger.ErrorContext(failCtx, "recovered from panic during job execution",
				"job_id", jobID, "panic", r, "stack", string(debug.Stack()))
			span.RecordError(fmt.Errorf("panic during job execution: %v", r))
			span.SetStatus(codes.Error, "panic during job execution")
			s.failJob(failCtx, tenantID, jobID, fmt.Errorf("panic during job execution: %v", r))
		}
	}()

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
		if err := s.runSynthesis(ctx, tenantID, jobID, job, exec); err != nil {
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

	// Determine incomplete analyzer names (not in completed set) and
	// completed ones (for pre-seeding CompletedOutputs on resume, below).
	dagOrder := dag.Order()
	var incompleteAnalyzers, completedAnalyzers []string
	for _, name := range dagOrder {
		if completed[name] {
			completedAnalyzers = append(completedAnalyzers, name)
		} else {
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

	// Partition analyzers into two passes. Candidate-dependent analyzers
	// (requires/relationship) need candidate data (requires_candidates /
	// relationship_candidates) materialized before their GenerateWork runs, so
	// they run in a second pass after candidate generation. Everything else
	// runs first.
	var prePass, postPass []string
	for _, name := range incompleteAnalyzers {
		if candidateDependentAnalyzers[name] {
			postPass = append(postPass, name)
		} else {
			prePass = append(prePass, name)
		}
	}

	// AnalyzerConfig is shared across both passes: every real analyzer's
	// GenerateWork needs job_id (requires/relationship read it from
	// cfg.Parameters["job_id"] and fail fast if absent; the others ignore it).
	analyzerConfig := make(map[string]analyzer.AnalyzerConfig, len(dagOrder))
	for _, name := range dagOrder {
		analyzerConfig[name] = analyzer.AnalyzerConfig{Parameters: map[string]string{"job_id": job.JobID}}
	}

	// Catalog-backed jobs get their full control list fanned out per-control;
	// document-backed jobs (ExtractCatalogID ok=false) leave Controls empty,
	// mirroring how candidate generation already treats a missing catalog_id
	// as a legitimate no-op rather than a failure. controls starts as a
	// non-nil empty slice (rather than a nil slice) so that req.Controls in
	// runPass's perControl branch is always non-nil, signaling "per-control
	// mode, zero controls" to the engine instead of falling back to its
	// single-Input path (which expects a *pb.Control, not the
	// *emptypb.Empty passed as req.Input here).
	var catalogID string
	controls := []*pb.Control{}
	if id, ok := ExtractCatalogID(job.Config); ok && s.catalogControls != nil {
		tenantID, terr := tenant.FromContext(ctx)
		if terr != nil {
			return fmt.Errorf("runAnalysis: %w", terr)
		}
		fetched, err := s.catalogControls.Controls(ctx, tenantID, id)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("runAnalysis: fetching catalog controls: %w", err)
		}
		catalogID = id
		controls = fetched
	}

	// allOutputs accumulates analyzer outputs across both passes for the
	// attestation link materials/products below. Seeded from any analyzers
	// already durably completed by a prior process invocation of this job
	// (crash/restart resume) — without this, a later pass's DAG subset that
	// transitively depends on them would re-dispatch and re-execute them,
	// even though they are marked completed, because the engine's dependency
	// gate is seeded solely from CompletedOutputs.
	allOutputs, err := s.store.GetCompletedAnalysisResults(ctx, job.JobID, completedAnalyzers)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("loading completed analysis results for resume: %w", err)
	}

	// runPass executes one engine pass and merges its outputs, failing the
	// analysis stage if any analyzer in the pass errored. perControl selects
	// between the two per-analyzer input shapes: pre-candidate analyzers
	// (classify/embedding/artifacts) need one *pb.Control per task; the
	// candidate-dependent analyzers (requires/relationship) ignore their
	// GenerateWork input entirely and instead read all candidate pairs for
	// the job — so they run with Controls empty and a placeholder *pb.Control
	// Input (any real analyzer registered here is Analyzer[*pb.Control], so
	// Input must type-assert to that, unlike the emptypb.Empty stub tests use).
	runPass := func(names []string, perControl bool, label string) error {
		if len(names) == 0 {
			return nil
		}
		req := analysis.ExecutionRequest{
			JobID:            job.JobID,
			AnalyzerNames:    names,
			Input:            &emptypb.Empty{},
			AnalyzerConfig:   analyzerConfig,
			CompletedOutputs: allOutputs,
		}
		if perControl {
			req.Controls = controls
			req.CatalogID = catalogID
		} else {
			req.Input = &pb.Control{}
		}
		result, err := s.engine.Execute(ctx, req)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("engine.Execute (%s): %w", label, err)
		}
		if len(result.Failed) > 0 {
			failErr := fmt.Errorf("analysis failed: %d analyzers failed: %v", len(result.Failed), result.Failed)
			span.RecordError(failErr)
			span.SetStatus(codes.Error, failErr.Error())
			return failErr
		}
		for k, v := range result.Outputs {
			allOutputs[k] = v
		}
		return nil
	}

	// Pre-candidate pass.
	if err := runPass(prePass, true, "pre-candidate pass"); err != nil {
		return err
	}

	// Candidate generation runs only when candidate-dependent analyzers will
	// follow and a generator is wired. A nil generator (tests / not-yet-wired
	// production) means the post-pass runs against whatever candidates already
	// exist.
	if len(postPass) > 0 && !completed["candidate_generation"] && s.candidateGen != nil {
		if err := s.store.UpdateStageStatus(ctx, job.JobID, "candidate_generation", StageStatusRunning); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("updating candidate_generation stage to running: %w", err)
		}

		tenantID, terr := tenant.FromContext(ctx)
		if terr != nil {
			return fmt.Errorf("candidate generation: %w", terr)
		}

		genErr := s.candidateGen.Generate(ctx, tenantID, job.JobID, job.Config)
		switch {
		case genErr == nil || errors.Is(genErr, ErrNotACatalogJob):
			// A nil error is a real success; ErrNotACatalogJob is a legitimate
			// no-op for document-backed jobs (zero candidates). Both are a
			// successfully terminal stage, and the candidate-dependent pass
			// proceeds either way.
			if err := s.store.UpdateStageStatus(ctx, job.JobID, "candidate_generation", StageStatusCompleted); err != nil {
				s.logger.WarnContext(ctx, "failed to update candidate_generation stage to completed",
					"job_id", job.JobID, "error", err)
			}
		default:
			if err := s.store.UpdateStageError(ctx, job.JobID, "candidate_generation", genErr); err != nil {
				s.logger.WarnContext(ctx, "failed to update candidate_generation stage to failed",
					"job_id", job.JobID, "error", err)
			}
			span.RecordError(genErr)
			span.SetStatus(codes.Error, genErr.Error())
			return fmt.Errorf("candidate generation failed: %w", genErr)
		}
	}

	// Candidate-dependent pass.
	if err := runPass(postPass, false, "candidate-dependent pass"); err != nil {
		return err
	}

	// Populate vote_summaries from requires/relationship results in
	// allOutputs, so synthesis (runSynthesis, below) has rows to rank and
	// update. Always attempted (not gated on postPass having just run):
	// buildVoteSummaryPairs returns an empty pairs slice when neither
	// analyzer's output is present, and WriteVoteSummaries uses
	// INSERT ... ON CONFLICT DO NOTHING, so this is a safe no-op both when
	// no candidate-dependent analyzers exist for this job and when a prior
	// process invocation already durably wrote these rows (resume case).
	tenantID, terr := tenant.FromContext(ctx)
	if terr != nil {
		return fmt.Errorf("runAnalysis: %w", terr)
	}
	pairs, err := buildVoteSummaryPairs(allOutputs)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("runAnalysis: building vote summary pairs: %w", err)
	}
	if err := s.store.WriteVoteSummaries(ctx, tenantID, job.JobID, pairs); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("runAnalysis: writing vote summaries: %w", err)
	}

	// Create link for each completed analyzer.
	for analyzerName, output := range allOutputs {
		// Materials: outputs from dependencies.
		var materials []attestation.Artifact
		for _, dep := range dag.Analyzers() {
			if dep.Name() == analyzerName {
				for _, depName := range dep.DependsOn() {
					if depOutput, ok := allOutputs[depName]; ok {
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
func (s *Service) runSynthesis(ctx context.Context, tenantID, jobID string, job *Job, exec *jobExecution) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.RunSynthesis")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", jobID))

	if err := s.store.UpdateStageStatus(ctx, jobID, "synthesis", StageStatusRunning); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("updating synthesis stage to running: %w", err)
	}
	s.publishStageEvent(ctx, tenantID, jobID, "synthesis", natsbus.StageStarted)

	classifyOut, err := s.store.GetCompletedAnalysisResults(ctx, jobID, []string{"classify", "requires", "relationship"})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("runSynthesis: loading analysis results: %w", err)
	}

	var classifyResults []results.ClassifyResult
	if out, ok := classifyOut["classify"]; ok && len(out.ResultData) > 0 {
		if err := json.Unmarshal(out.ResultData, &classifyResults); err != nil {
			return fmt.Errorf("runSynthesis: unmarshaling classify results: %w", err)
		}
	}
	var requiresResults []results.RequiresResult
	if out, ok := classifyOut["requires"]; ok && len(out.ResultData) > 0 {
		if err := json.Unmarshal(out.ResultData, &requiresResults); err != nil {
			return fmt.Errorf("runSynthesis: unmarshaling requires results: %w", err)
		}
	}
	var relResults []results.SemanticMatchResult
	if out, ok := classifyOut["relationship"]; ok && len(out.ResultData) > 0 {
		if err := json.Unmarshal(out.ResultData, &relResults); err != nil {
			return fmt.Errorf("runSynthesis: unmarshaling relationship results: %w", err)
		}
	}

	// Similarity matrices: one per configured embedding model, read from the
	// same pgEmbeddingsReader candidate generation already uses. Only
	// meaningful for catalog-backed jobs; a document-backed job (no
	// catalog_id) has no embeddings and synthesis proceeds with zero
	// similarity stats (SimilarityCount=0 per pair).
	similarityByModel := make(map[string]similarityMatrix)
	if catalogID, ok := ExtractCatalogID(job.Config); ok && s.embeddingsReader != nil {
		for _, model := range s.embeddingConfig.Models {
			ids, values, err := s.embeddingsReader.SimilarityMatrix(ctx, tenantID, catalogID, model)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				return fmt.Errorf("runSynthesis: reading similarity matrix for model %q: %w", model, err)
			}
			similarityByModel[model] = similarityMatrix{IDs: ids, Values: values}
		}
	}

	inputs, classifications := buildSynthesisInputs(classifyResults, requiresResults, relResults, similarityByModel)

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
