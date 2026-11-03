package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// jobExecution carries attestation state through the execution phases.
// attestor is nil when the job's tenant does not attest jobs; every
// attestation step is then skipped.
type jobExecution struct {
	attestor *Attestor
	// subject binds every layout step and link to this job: its URI names
	// the tenant and job, its digest the job's stored config.
	subject attestation.Artifact
	dag     *analyzer.DAG
	layout  *attestation.SignedLayout
	links   []*attestation.SignedLink
}

// executeJob is the main lifecycle method. Called as a goroutine from
// CreateJob, RetryJob and Start, with rj, the entry that registered this run
// in s.running.
func (s *Service) executeJob(ctx context.Context, rj *runningJob, tenantID, jobID string) {
	defer s.wg.Done()

	start := time.Now()
	// Runs after every other deferred call, the panic recovery's writes
	// included: once the entry is gone a retry may start a new run of the
	// job, so this run must have finished writing. It releases only this
	// run's own context and entry, never a newer run's.
	defer func() {
		s.mu.Lock()
		rj.cancel(nil)
		if s.running[jobID] == rj {
			delete(s.running, jobID)
		}
		s.mu.Unlock()
		if s.jobDuration != nil {
			s.jobDuration.Record(ctx, float64(time.Since(start).Milliseconds()))
		}
	}()

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
	exec := &jobExecution{attestor: s.attestors.forTenant(tenantID), subject: jobSubject(job)}

	// Phase 2: Analysis and its attestation. The stored links of the
	// pipeline steps an earlier run completed are verified first, before
	// this run signs and stores anything, so a job whose stored attestation
	// cannot be trusted fails with every object as it was. Then analysis
	// runs, and a link is signed for every analyzer over its stored result.
	for _, phase := range []func() error{
		func() error { return s.loadStoredLinks(ctx, job, completed, exec) },
		func() error { return s.runAnalysis(ctx, job, stages, completed, exec) },
		func() error { return s.attestAnalyzers(ctx, job, exec) },
	} {
		if err := phase(); err != nil {
			if ctx.Err() != nil {
				s.interruptJob(ctx, tenantID, jobID)
				return
			}
			s.failJob(ctx, tenantID, jobID, err)
			return
		}
	}

	// Phase 3: Synthesis.
	if !completed["synthesis"] {
		if err := s.runSynthesis(ctx, tenantID, jobID, job, exec); err != nil {
			if ctx.Err() != nil {
				s.interruptJob(ctx, tenantID, jobID)
				return
			}
			s.failJob(ctx, tenantID, jobID, err)
			return
		}
	}

	// Phase 4: Graph.
	if !completed["graph"] {
		if err := s.runGraph(ctx, job, exec); err != nil {
			if ctx.Err() != nil {
				s.interruptJob(ctx, tenantID, jobID)
				return
			}
			s.failJob(ctx, tenantID, jobID, err)
			return
		}
	}
	// runGraph records a failed stage write as a warning, so a cancel while
	// the graph stage ran returns nil with the stage still running.
	if ctx.Err() != nil {
		s.interruptJob(ctx, tenantID, jobID)
		return
	}

	// Phase 5: Attestation finalization. Completed is immutable, so the job
	// is written completed only after this: a bundle lost to a cancel or a
	// shutdown here could never be stored afterwards. A cancel ends the job
	// cancelled; a shutdown leaves it running, and the resume finalizes it
	// again from the links this run stored. A failure fails the job: an
	// attested job never completes without its bundle. Every stage has
	// completed, so retrying the job from its failure only finalizes again.
	if err := s.finalizeAttestation(ctx, job, exec); err != nil {
		if ctx.Err() != nil {
			s.interruptJob(ctx, tenantID, jobID)
			return
		}
		s.failJob(ctx, tenantID, jobID, err)
		return
	}

	// Done. Claim the completed transition under the lock CancelJob takes,
	// so a cancel either happened first (the job ends cancelled) or is
	// refused; the write then runs on a context the cancellation of the
	// job's own does not reach.
	if !s.claimCompletion(ctx, rj) {
		s.interruptJob(ctx, tenantID, jobID)
		return
	}
	doneCtx, cancelDone := terminalWriteContext(ctx)
	defer cancelDone()
	if err := s.store.UpdateJobStatus(doneCtx, jobID, JobStatusCompleted, nil); err != nil {
		s.logger.ErrorContext(ctx, "failed to mark job completed",
			"job_id", jobID, "error", err)
	}
	if err := s.publishJobState(doneCtx, tenantID, jobID, JobStatusCompleted); err != nil {
		s.logger.WarnContext(ctx, "failed to publish job state", "job_id", jobID, "error", err)
	}
	if s.jobCounter != nil {
		s.jobCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("status", string(JobStatusCompleted))))
	}
}

// pairAnalyzerParameters returns the AnalyzerConfig parameters the
// candidate-dependent analyzers (requires, relationship) read when building
// prompts. Those analyzers run once per job with no per-control input, so
// every control's statement, ancestor title and classification is passed
// under both the source_ and target_ prefixes; a control can be either end of
// a pair. A missing or partial classify output is tolerated (classify can be
// disabled): controls without a classification simply get no type or level
// keys. Only a classify output that does not parse fails the job, since
// prompts would otherwise silently lose classifications that were produced.
func pairAnalyzerParameters(jobID string, controls []*pb.Control, classify *analyzer.Output) (map[string]string, error) {
	params := map[string]string{"job_id": jobID}
	for _, c := range controls {
		id := c.GetControlId()
		ancestor := c.GetParts()["ancestor_title"]
		for _, end := range []string{"source_", "target_"} {
			params[end+"text_"+id] = c.GetStatement()
			params[end+"ancestor_"+id] = ancestor
		}
	}
	if classify == nil || len(classify.ResultData) == 0 {
		return params, nil
	}
	var classified []results.ClassifyResult
	if err := json.Unmarshal(classify.ResultData, &classified); err != nil {
		return nil, fmt.Errorf("reading classify results for pair analyzers: %w", err)
	}
	for _, r := range classified {
		for _, end := range []string{"source_", "target_"} {
			params[end+"type_"+r.ControlID] = r.Type
			params[end+"level_"+r.ControlID] = r.Level
		}
	}
	return params, nil
}

// runAnalysis builds the DAG, checks that it runs the analyzers the job's
// stages were created with, determines incomplete analyzers, and executes
// them.
func (s *Service) runAnalysis(ctx context.Context, job *Job, stages []*Stage, completed map[string]bool, exec *jobExecution) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.RunAnalysis")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", job.JobID))

	cats, ok := jobconfig.ExtractCatalogs(job.Config)
	if !ok {
		err := fmt.Errorf("job %s cannot run: %w. Analysis runs over imported catalogs; "+
			"start a new analysis with \"%s\"", job.JobID, ErrMissingCatalogID, newJobCommand(job))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	// Build DAG from registry.
	dag, err := s.registry.BuildDAG(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("building DAG: %w", err)
	}
	if err := checkJobAnalyzers(job, stages, dag.Order()); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	// An attested job's layout is signed and stored before any analyzer
	// runs, so a job that cannot be attested fails without spending work.
	exec.dag = dag
	if exec.attestor != nil {
		layout, err := s.createLayout(ctx, job, exec)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
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

	// AnalyzerConfig is shared across both passes. Every analyzer gets job_id
	// here (requires/relationship read it from cfg.Parameters["job_id"] and
	// fail fast if absent; the others ignore it); the candidate-dependent
	// analyzers' entries are replaced with pairAnalyzerParameters once the
	// pre-candidate pass has produced classify output.
	analyzerConfig := make(map[string]analyzer.AnalyzerConfig, len(dagOrder))
	for _, name := range dagOrder {
		analyzerConfig[name] = analyzer.AnalyzerConfig{Parameters: map[string]string{"job_id": job.JobID}}
	}

	// The catalog's full control list is fanned out per-control. controls
	// starts as a non-nil empty slice (rather than a nil slice) so that
	// req.Controls in runPass's perControl branch is always non-nil,
	// signaling "per-control mode" to the engine instead of falling back to
	// its single-Input path (which expects a *pb.Control, not the
	// *emptypb.Empty passed as req.Input here). A nil catalogControls reader
	// (tests, partial wiring) leaves it empty.
	controls := []*pb.Control{}
	if s.catalogControls != nil {
		tenantID, terr := tenant.FromContext(ctx)
		if terr != nil {
			return fmt.Errorf("runAnalysis: %w", terr)
		}
		fetched, err := s.catalogControls.Controls(ctx, tenantID, cats.SourceID)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("runAnalysis: fetching catalog controls: %w", err)
		}
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
	// the job, taking each pair's control texts from the parameters built by
	// pairAnalyzerParameters — so they run with Controls empty and a
	// placeholder *pb.Control Input (any real analyzer registered here is
	// Analyzer[*pb.Control], so Input must type-assert to that, unlike the
	// emptypb.Empty stub tests use).
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
			req.CatalogID = cats.SourceID
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
		if genErr != nil {
			if err := s.store.UpdateStageError(ctx, job.JobID, "candidate_generation", genErr); err != nil {
				s.logger.WarnContext(ctx, "failed to update candidate_generation stage to failed",
					"job_id", job.JobID, "error", err)
			}
			span.RecordError(genErr)
			span.SetStatus(codes.Error, genErr.Error())
			return fmt.Errorf("candidate generation failed: %w", genErr)
		}
		// A nil error is a real success; the candidate-dependent pass proceeds.
		if err := s.store.UpdateStageStatus(ctx, job.JobID, "candidate_generation", StageStatusCompleted); err != nil {
			s.logger.WarnContext(ctx, "failed to update candidate_generation stage to completed",
				"job_id", job.JobID, "error", err)
		}
	}

	// Candidate-dependent pass.
	if len(postPass) > 0 {
		pairParams, err := pairAnalyzerParameters(job.JobID, controls, allOutputs["classify"])
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("runAnalysis: %w", err)
		}
		for _, name := range postPass {
			analyzerConfig[name] = analyzer.AnalyzerConfig{Parameters: pairParams}
		}
	}
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
	// same pgEmbeddingsReader candidate generation already uses. A nil
	// embeddings reader (tests, partial wiring) leaves them empty, and
	// synthesis proceeds with zero similarity stats (SimilarityCount=0 per
	// pair).
	similarityByModel := make(map[string]similarityMatrix)
	if cats, ok := jobconfig.ExtractCatalogs(job.Config); ok && s.embeddingsReader != nil {
		for _, model := range s.embeddingConfig.Models {
			ids, values, err := s.embeddingsReader.SimilarityMatrix(ctx, tenantID, cats.CatalogIDs(), model)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				return fmt.Errorf("runSynthesis: reading similarity matrix for model %q: %w", model, err)
			}
			similarityByModel[model] = similarityMatrix{IDs: ids, Values: values}
		}
	}

	inputs, classifications, err := buildSynthesisInputs(classifyResults, requiresResults, relResults, similarityByModel)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("runSynthesis: %w", err)
	}

	synthResult, err := s.synthesis.Execute(ctx, jobID, inputs, classifications)
	if err != nil {
		_ = s.store.UpdateStageStatus(ctx, jobID, "synthesis", StageStatusFailed)
		s.publishStageEvent(ctx, tenantID, jobID, "synthesis", natsbus.StageFailed)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("synthesis.Execute: %w", err)
	}

	// The stage completes only once its link is stored, so a completed
	// stage always has one for a later run to read back.
	if exec.attestor != nil {
		if err := s.attestSynthesis(ctx, job, exec, synthResult); err != nil {
			s.failPipelineStage(ctx, job, "synthesis", err)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
	}

	if err := s.store.UpdateStageStatus(ctx, jobID, "synthesis", StageStatusCompleted); err != nil {
		s.logger.WarnContext(ctx, "failed to update synthesis stage to completed", "job_id", jobID, "error", err)
	}
	s.publishStageEvent(ctx, tenantID, jobID, "synthesis", natsbus.StageCompleted)

	span.SetStatus(codes.Ok, "")
	return nil
}

// attestSynthesis signs and stores the synthesis link: its materials are
// every analyzer's product, and its product is the synthesis result.
func (s *Service) attestSynthesis(ctx context.Context, job *Job, exec *jobExecution, result *synthesis.ExecuteResult) error {
	analyzers := make(map[string]bool, len(exec.dag.Order()))
	for _, name := range exec.dag.Order() {
		analyzers[name] = true
	}
	var materials []attestation.Artifact
	for _, link := range exec.links {
		if analyzers[link.Step] {
			materials = append(materials, link.Products...)
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("job %s: the synthesis result could not be encoded for its attestation link: %w: %w. %s",
			job.JobID, err, ErrAttestationSigning, retryHint(job))
	}
	link, err := s.createLink(ctx, job, exec, "synthesis", materials,
		[]attestation.Artifact{{URI: "synthesis.output", Digest: contentDigest(encoded)}})
	if err != nil {
		return err
	}
	exec.links = append(exec.links, link)
	return nil
}

// failPipelineStage records that stage failed with err and publishes it, so
// retrying the job from its failure resumes at stage. A cancelled or stopped
// job is left to interruptJob.
func (s *Service) failPipelineStage(ctx context.Context, job *Job, stage string, err error) {
	if ctx.Err() != nil {
		return
	}
	if uerr := s.store.UpdateStageError(ctx, job.JobID, stage, err); uerr != nil {
		s.logger.WarnContext(ctx, "failed to record the stage failure", "job_id", job.JobID, "stage", stage, "error", uerr)
	}
	s.publishStageEvent(ctx, job.TenantID, job.JobID, stage, natsbus.StageFailed)
}

// runGraph updates stage status and publishes completion event to NATS.
// The graph service subscribes asynchronously.
func (s *Service) runGraph(ctx context.Context, job *Job, exec *jobExecution) error {
	tenantID, jobID := job.TenantID, job.JobID
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.RunGraph")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", jobID))

	if err := s.store.UpdateStageStatus(ctx, jobID, "graph", StageStatusRunning); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("updating graph stage to running: %w", err)
	}
	s.publishStageEvent(ctx, tenantID, jobID, "graph", natsbus.StageStarted)

	// The graph link's material is the synthesis product; like synthesis,
	// the stage completes only once its link is stored.
	if exec.attestor != nil {
		var materials []attestation.Artifact
		for _, link := range exec.links {
			if link.Step == "synthesis" {
				materials = link.Products
			}
		}
		link, err := s.createLink(ctx, job, exec, "graph", materials, nil)
		if err != nil {
			s.failPipelineStage(ctx, job, "graph", err)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
		exec.links = append(exec.links, link)
	}

	// Graph service subscribes to stage.completed events and materializes asynchronously.
	if err := s.store.UpdateStageStatus(ctx, jobID, "graph", StageStatusCompleted); err != nil {
		s.logger.WarnContext(ctx, "failed to update graph stage to completed", "job_id", jobID, "error", err)
	}
	s.publishStageEvent(ctx, tenantID, jobID, "graph", natsbus.StageCompleted)

	span.SetStatus(codes.Ok, "")
	return nil
}

// endRunningStages applies end to every stage of a job that has just been
// recorded as ended (failed or cancelled) and is still running, so no stage
// of a job that will never resume claims to be running. Failures are logged:
// the job's own status is already recorded.
//
// It also runs from executeJob's panic recovery (through failJob), where the
// store that just panicked may panic again; a panic here is logged instead of
// crashing the daemon.
func (s *Service) endRunningStages(ctx context.Context, jobID string, end func(stage string) error) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.ErrorContext(ctx, "job ended, but ending its running stages panicked; they keep status \"running\"",
				"job_id", jobID, "panic", r)
		}
	}()
	stages, err := s.store.GetStages(ctx, jobID)
	if err != nil {
		s.logger.ErrorContext(ctx, "job ended, but its stages could not be read to end the running ones; "+
			"they keep status \"running\". The job itself is recorded as ended and does not resume",
			"job_id", jobID, "error", err)
		return
	}
	for _, stage := range stages {
		if stage.Status != StageStatusRunning {
			continue
		}
		if err := end(stage.StageName); err != nil {
			s.logger.ErrorContext(ctx, "job ended, but a running stage could not be ended; "+
				"it keeps status \"running\". The job itself is recorded as ended and does not resume",
				"job_id", jobID, "stage", stage.StageName, "error", err)
		}
	}
}

// claimCompletion marks rj completing unless its context is already done
// (cancelled or stopped), and reports whether it did. It holds s.mu, which
// CancelJob holds while it cancels, so exactly one of the two decides the
// job's terminal state.
func (s *Service) claimCompletion(ctx context.Context, rj *runningJob) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	rj.completing = true
	return true
}

// terminalWriteTimeout bounds a write of a job's final state made on a
// context detached from the job's own (terminalWriteContext), so a stuck
// database cannot hold up shutdown, which waits for every job goroutine.
const terminalWriteTimeout = 30 * time.Second

// terminalWriteContext returns a context for recording a job's final state:
// it keeps ctx's values (tenant, trace) but not its cancellation, which has
// usually already happened (a cancelled job), and expires after
// terminalWriteTimeout.
func terminalWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
}

// failJob updates status to failed, publishes state event, records metric.
// A job whose context is already done did not fail on its own; it is handed
// to interruptJob instead.
func (s *Service) failJob(ctx context.Context, tenantID, jobID string, err error) {
	if ctx.Err() != nil {
		s.interruptJob(ctx, tenantID, jobID)
		return
	}
	s.logger.ErrorContext(ctx, "job failed", "job_id", jobID, "error", err)

	writeCtx, cancel := terminalWriteContext(ctx)
	defer cancel()

	if updateErr := s.store.UpdateJobStatus(writeCtx, jobID, JobStatusFailed, err); updateErr != nil {
		s.logger.ErrorContext(ctx, "failed to update job status to failed",
			"job_id", jobID, "error", updateErr)
	} else {
		// A failed job is never resumed by Start, so a stage still running (left by
		// a shutdown during an earlier run) will never finish.
		s.endRunningStages(writeCtx, jobID, func(stage string) error {
			return s.store.UpdateStageError(writeCtx, jobID, stage, fmt.Errorf("did not finish: the job failed: %w", err))
		})
	}

	if err := s.publishJobState(writeCtx, tenantID, jobID, JobStatusFailed); err != nil {
		s.logger.WarnContext(ctx, "failed to publish job state", "job_id", jobID, "error", err)
	}

	if s.jobCounter != nil {
		s.jobCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("status", string(JobStatusFailed))))
	}
}

// interruptJob ends a job whose context was cancelled. CancelJob cancels with
// ErrJobCancelled: the job and every stage still running end cancelled, and
// the cancelled state is published. Stage failures caused by the
// cancellation are not recorded (DBStageReporter.ReportStageFailed), so a
// stage that was running is still running here. Any other cause is a
// shutdown: nothing is written, so the job stays running and the next Start
// resumes it from its completed stages.
//
// The job's context is already cancelled, so the writes use
// terminalWriteContext; on the job's own context every one would fail.
func (s *Service) interruptJob(ctx context.Context, tenantID, jobID string) {
	if !errors.Is(context.Cause(ctx), ErrJobCancelled) {
		s.logger.InfoContext(ctx, "job interrupted by shutdown; it stays running and resumes on the next start",
			"job_id", jobID)
		return
	}
	s.logger.InfoContext(ctx, "job cancelled", "job_id", jobID)

	writeCtx, cancel := terminalWriteContext(ctx)
	defer cancel()

	// A job whose status the store refuses to change resumes on the next
	// start, so its stages are left as they are too.
	if err := s.store.UpdateJobStatus(writeCtx, jobID, JobStatusCancelled, nil); err != nil {
		s.logger.ErrorContext(ctx, fmt.Sprintf("could not record that job %s was cancelled: its status stays %q, "+
			"so the pipeline resumes it on its next start. Check the database connection; after the restart, "+
			"cancel it again with \"crosscodex run cancel --yes %s\"", jobID, JobStatusRunning, jobID),
			"job_id", jobID, "error", err)
		return
	}

	s.endRunningStages(writeCtx, jobID, func(stage string) error {
		return s.store.UpdateStageStatus(writeCtx, jobID, stage, StageStatusCancelled)
	})

	if err := s.publishJobState(writeCtx, tenantID, jobID, JobStatusCancelled); err != nil {
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

	// Same shape as internal/analysis/reporter.go publish, so subscribers of
	// crosscodex.pipeline.*.*.stage.* decode analyzer and pipeline stages
	// alike. Unlike reporter.go, a failed pipeline stage carries no error
	// detail.
	event := map[string]interface{}{
		"analyzer":  stageName,
		"job_id":    jobID,
		"stage":     string(stage),
		"timestamp": time.Now().UTC().Format(time.RFC3339),
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

// attestationKey is the object storage key of a job's attestation object
// name: layout.json, links/<step>.json or bundle.json.
func attestationKey(job *Job, name string) string {
	return fmt.Sprintf("attestations/%s/%s/%s", job.TenantID, job.JobID, name)
}

// newJobCommand is the command that starts a new job over job's catalogs,
// in job's direction.
func newJobCommand(job *Job) string {
	cats, ok := jobconfig.ExtractCatalogs(job.Config)
	if !ok {
		return "crosscodex run start --source <source-catalog-id> --target <target-catalog-id>"
	}
	cmd := "crosscodex run start --source " + cats.SourceID + " --target " + cats.TargetID
	if cats.Bidirectional {
		cmd += " --bidirectional"
	}
	return cmd
}

// retryHint names both ways to run a job again after a failure it can
// recover from.
func retryHint(job *Job) string {
	return fmt.Sprintf("Then resume this job: crosscodex run retry --from-failure %s, "+
		"or start a new job: %s", job.JobID, newJobCommand(job))
}

// createLayout signs and stores the attestation layout for a job: a step
// per analyzer, then the pipeline's own steps.
func (s *Service) createLayout(ctx context.Context, job *Job, exec *jobExecution) (*attestation.SignedLayout, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.CreateLayout")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", job.JobID))

	// Convert DAG to layout options, then declare the pipeline's own steps,
	// which are linked too: synthesis consumes every analyzer's output, and
	// graph consumes synthesis's.
	layoutOpts := s.attConverter.Convert(exec.dag)
	layoutOpts.ExpiresIn = exec.attestor.ExpiresIn
	var analyzerOutputs []string
	for _, step := range layoutOpts.Steps {
		analyzerOutputs = append(analyzerOutputs, step.ExpectedProducts...)
	}
	layoutOpts.Steps = append(layoutOpts.Steps,
		attestation.Step{Name: "synthesis", ExpectedMaterials: analyzerOutputs, ExpectedProducts: []string{"synthesis.output"}, Threshold: 1},
		attestation.Step{Name: "graph", ExpectedMaterials: []string{"synthesis.output"}, Threshold: 1},
	)
	// Every step expects the job as a material, binding the layout to it.
	for i := range layoutOpts.Steps {
		layoutOpts.Steps[i].ExpectedMaterials = append([]string{exec.subject.URI}, layoutOpts.Steps[i].ExpectedMaterials...)
	}

	layout, err := exec.attestor.Generator.CreateLayout(ctx, layoutOpts)
	if err != nil {
		err = fmt.Errorf("job %s cannot start: its attestation layout could not be signed: %w: %w. No analyzer ran. "+
			"Fix the attestation key pair (attestation.private_key_path and attestation.public_key_path, or the tenant's "+
			"override) and restart crosscodexd. %s", job.JobID, err, ErrAttestationSigning, retryHint(job))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if err := s.putAttestation(ctx, job, "layout", attestationKey(job, "layout.json"), layout.Raw); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	span.SetStatus(codes.Ok, "")
	return layout, nil
}

// createLink signs and stores an attestation link for a pipeline step.
func (s *Service) createLink(ctx context.Context, job *Job, exec *jobExecution, stepName string, materials, products []attestation.Artifact) (*attestation.SignedLink, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.CreateLink")
	defer span.End()
	span.SetAttributes(
		attribute.String("job.id", job.JobID),
		attribute.String("step", stepName),
	)

	// Every link records the job as a material, binding it to this job.
	materials = append([]attestation.Artifact{exec.subject}, materials...)
	link, err := exec.attestor.Generator.CreateLink(ctx, stepName, materials, products)
	if err != nil {
		err = fmt.Errorf("job %s: the attestation link for step %q could not be signed: %w: %w. "+
			"Fix the attestation key pair and restart crosscodexd. %s", job.JobID, stepName, err, ErrAttestationSigning, retryHint(job))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	what := fmt.Sprintf("link for step %q", stepName)
	if err := s.putAttestation(ctx, job, what, attestationKey(job, "links/"+stepName+".json"), link.Raw); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	span.SetStatus(codes.Ok, "")
	return link, nil
}

// putAttestation stores data at key, retrying a failed write
// Attestors.StorageRetries times, Attestors.StorageRetryDelay apart. what
// names the object in the error. It stops early when ctx is done, and the
// caller then sees ctx.Err() set.
func (s *Service) putAttestation(ctx context.Context, job *Job, what, key string, data []byte) error {
	attempts := 1 + s.attestors.StorageRetries
	var err error
	for attempt := 1; ; attempt++ {
		if err = s.storage.Put(ctx, key, bytes.NewReader(data)); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("storing the attestation %s at %q: %w", what, key, err)
		}
		if attempt == attempts {
			break
		}
		s.logger.WarnContext(ctx, "storing an attestation failed; retrying",
			"job_id", job.JobID, "key", key, "attempt", attempt, "attempts", attempts, "error", err)
		timer := time.NewTimer(s.attestors.StorageRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("storing the attestation %s at %q: %w", what, key, ctx.Err())
		case <-timer.C:
		}
	}
	return fmt.Errorf("job %s: its attestation %s could not be stored at %q after %d attempts: %w: %w. "+
		"Check that object storage (storage.objects.base_path) is writable. %s",
		job.JobID, what, key, attempts, err, ErrAttestationStorage, retryHint(job))
}

// attestAnalyzers signs and stores a link for every analyzer of the job, in
// this run or an earlier one. A link's digests are of the analyzer results
// as stored, which synthesis reads, so a resumed run signs the same digests
// and a link can always be made again from the database.
func (s *Service) attestAnalyzers(ctx context.Context, job *Job, exec *jobExecution) error {
	if exec.attestor == nil {
		return nil
	}
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.AttestAnalyzers")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", job.JobID))

	order := exec.dag.Order()
	stored, err := s.store.GetCompletedAnalysisResults(ctx, job.JobID, order)
	if err != nil {
		err = fmt.Errorf("attesting analyzers: loading their stored results: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	digests := make(map[string]string, len(order))
	for _, name := range order {
		out, ok := stored[name]
		if !ok {
			err := fmt.Errorf("job %s: analyzer %q has no stored result to attest: %w. Start a new job: %s",
				job.JobID, name, ErrAttestationInvalid, newJobCommand(job))
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
		digests[name] = contentDigest(out.ResultData)
	}
	for _, a := range exec.dag.Analyzers() {
		var materials []attestation.Artifact
		for _, dep := range a.DependsOn() {
			materials = append(materials, attestation.Artifact{URI: dep + ".output", Digest: digests[dep]})
		}
		products := []attestation.Artifact{{URI: a.Name() + ".output", Digest: digests[a.Name()]}}
		link, err := s.createLink(ctx, job, exec, a.Name(), materials, products)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
		exec.links = append(exec.links, link)
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// linkedPipelineStages are the stages after analysis that have a link. An
// earlier run that completed one stored its link, which a resumed run reads
// back, since it does not run the stage again. candidate_generation has no
// link: its output reaches the chain through the analyzers that read it.
var linkedPipelineStages = []string{"synthesis", "graph"}

// loadStoredLinks appends to exec the stored, verified link of each
// linkedPipelineStages stage that was completed when this run started. A
// missing or unverifiable link fails the job: the stage's results cannot be
// attested, and running it again would not change what is stored.
func (s *Service) loadStoredLinks(ctx context.Context, job *Job, completed map[string]bool, exec *jobExecution) error {
	if exec.attestor == nil {
		return nil
	}
	for _, step := range linkedPipelineStages {
		if !completed[step] {
			continue
		}
		key := attestationKey(job, "links/"+step+".json")
		rc, err := s.storage.Get(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("job %s: step %q completed in an earlier run, but its attestation link %q is missing: %w. "+
				"Its results cannot be attested; start a new job: %s", job.JobID, step, key, ErrAttestationInvalid, newJobCommand(job))
		}
		if err != nil {
			return fmt.Errorf("job %s: reading the attestation link of step %q at %q: %w: %w. "+
				"Check that object storage (storage.objects.base_path) is readable. %s",
				job.JobID, step, key, err, ErrAttestationStorage, retryHint(job))
		}
		raw, err := io.ReadAll(rc)
		closeErr := rc.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("job %s: reading the attestation link of step %q at %q: %w: %w. "+
				"Check that object storage (storage.objects.base_path) is readable. %s",
				job.JobID, step, key, err, ErrAttestationStorage, retryHint(job))
		}
		verified, err := exec.attestor.Generator.Verify(ctx, raw)
		if err == nil && verified.Step != step {
			err = fmt.Errorf("it is signed for step %q", verified.Step)
		}
		if err == nil && !slices.Contains(verified.Materials, exec.subject) {
			err = fmt.Errorf("it is signed for another job, not %q with config digest %q", exec.subject.URI, exec.subject.Digest)
		}
		if err != nil {
			return fmt.Errorf("job %s: the stored attestation link of step %q at %q does not verify: %w: %w. "+
				"It was modified, or signed with a key pair other than the one configured now; its results cannot be "+
				"attested. Start a new job: %s", job.JobID, step, key, err, ErrAttestationInvalid, newJobCommand(job))
		}
		exec.links = append(exec.links, &attestation.SignedLink{
			Raw:       raw,
			Step:      verified.Step,
			Materials: verified.Materials,
			Products:  verified.Products,
		})
	}
	return nil
}

// finalizeAttestation verifies the job's whole attestation (layout and one
// link per step) and stores it as the job's bundle, then publishes the
// bundle to the audit trail. The publish is best effort: the stored bundle
// is the record.
func (s *Service) finalizeAttestation(ctx context.Context, job *Job, exec *jobExecution) error {
	if exec.attestor == nil {
		return nil
	}
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.FinalizeAttestation")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", job.JobID))

	fail := func(err error) error {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	if _, err := exec.attestor.Generator.VerifyChain(ctx, exec.layout, exec.links, exec.subject); err != nil {
		return fail(fmt.Errorf("job %s: its attestation does not verify: %w: %w. Its results are kept but cannot be "+
			"attested. Check whether objects under %q were modified or deleted, then start a new job: %s",
			job.JobID, err, ErrAttestationInvalid, attestationKey(job, ""), newJobCommand(job)))
	}

	bundle, err := attestation.MarshalBundle(exec.layout, exec.links)
	if err != nil {
		return fail(fmt.Errorf("job %s: its attestation bundle could not be encoded: %w: %w. %s",
			job.JobID, err, ErrAttestationSigning, retryHint(job)))
	}
	if err := s.putAttestation(ctx, job, "bundle", attestationKey(job, "bundle.json"), bundle); err != nil {
		return fail(err)
	}

	auditSubject, err := natsbus.AuditSubject(job.TenantID, natsbus.AuditEvents, job.JobID)
	if err != nil {
		s.logger.WarnContext(ctx, "failed to build audit subject",
			"job_id", job.JobID, "error", err)
	} else if s.bus != nil {
		if err := s.bus.Publish(ctx, auditSubject, bundle); err != nil {
			s.logger.WarnContext(ctx, "failed to publish attestation bundle to audit trail; the bundle is stored",
				"job_id", job.JobID, "error", err)
		}
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

// jobSubject is the artifact that binds a job's attestation to the job:
// "job:<tenant>/<job-id>", with the digest of the job's config as stored.
func jobSubject(job *Job) attestation.Artifact {
	return attestation.Artifact{URI: fmt.Sprintf("job:%s/%s", job.TenantID, job.JobID), Digest: contentDigest(job.Config)}
}

// contentDigest is the "sha256:<hex>" digest an attestation records for data.
func contentDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}
