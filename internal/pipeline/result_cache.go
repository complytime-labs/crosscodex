package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// modelProbeText is what the probe sends: a completion asks for one token
// in reply, an embedding embeds it. Only resp.Model and resp.Deployment are
// read.
const modelProbeText = "crosscodex result cache model probe. Reply with OK."

// maxObservedModelBytes bounds the model name and the deployment ID a probe
// accepts from the gateway. Both reach every cache key and the tenant's run
// status, so a longer or unprintable one leaves the alias uncached.
const maxObservedModelBytes = 256

// errProbeNotWired reports WithResultCache with a nil probe client.
var errProbeNotWired = errors.New("pipeline: the result cache is wired without a model probe client")

// prepareResultCache resolves job's use of the result cache (#243) for the
// analyzers in names, in DAG order. In order:
//   - without WithResultCache, the cache is off (Mode "");
//   - with analysis.result_cache.enabled false for the tenant, it is
//     ModeDisabled, which makes no cache call and probes nothing;
//   - when the tenant does not attest jobs, it is off (Mode ""): nothing
//     can be checkpointed, so nothing could ever be trusted;
//   - with the job's stored no_cache (cats, read from job.Config, the only
//     source of ModeBypassed) it is ModeBypassed, and otherwise ModeEnabled.
//
// In the last two modes it probes, once per distinct resolved alias and
// kind, the model behind every alias the cacheable analyzers declare. A
// probe that fails stops the job before any analysis call: it returns a
// StageError of ErrLLMUnavailable, recorded on the first analyzer of names
// using the alias. A probe that names no usable model or deployment leaves
// that alias uncached, with a fixed reason. So does one that names only the
// alias as the model and no deployment, when llm.model_revisions has no
// entry for the alias: a change of the model behind the alias would go
// unnoticed (a LiteLLM gateway echoes the alias, and names the model behind
// it only in its deployment header).
func (s *Service) prepareResultCache(ctx context.Context, job *Job, cats jobconfig.Catalogs, exec *jobExecution,
	names []string, controls []*pb.Control) (analysis.CacheSettings, error) {
	if !s.cacheWired {
		return analysis.CacheSettings{}, nil
	}
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return analysis.CacheSettings{}, fmt.Errorf("result cache: %w", err)
	}
	tcfg := s.cacheCfg.ForTenant(tenantID)
	if !tcfg.Enabled {
		return analysis.CacheSettings{Mode: resultcache.ModeDisabled}, nil
	}
	if exec.attestor == nil {
		return analysis.CacheSettings{}, nil
	}
	if s.probe == nil {
		return analysis.CacheSettings{}, errProbeNotWired
	}
	controlCatalogs := make(map[string]string, len(controls))
	for _, c := range controls {
		controlCatalogs[c.GetControlId()] = c.GetCatalogId()
	}
	settings := analysis.CacheSettings{
		Mode:            resultcache.ModeEnabled,
		LookupBatchSize: tcfg.LookupBatchSize,
		ControlCatalogs: controlCatalogs,
		NoCacheCommand:  noCacheCommand(job),
	}
	exec.checkpoints = s.newCacheCheckpoints(job, exec.attestor, exec.dag.Order())
	settings.Checkpoints = exec.checkpoints
	if cats.NoCache {
		settings.Mode = resultcache.ModeBypassed
	}

	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.ProbeModels")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenantID), attribute.String("job.id", job.JobID),
		attribute.String("result_cache.mode", string(settings.Mode)))

	llm := s.llmCfg.ForTenant(tenantID)
	settings.Models = map[analyzer.ModelUse]analysis.CacheModel{}
	probed := map[analyzer.ModelUse]analysis.CacheModel{}
	for _, name := range names {
		ra, err := s.registry.Get(name)
		if err != nil {
			return analysis.CacheSettings{}, fmt.Errorf("result cache: analyzer %q: %w", name, err)
		}
		c, ok := analyzer.AsCacheable(ra)
		if !ok {
			continue
		}
		for _, use := range c.CacheModels() {
			if _, done := settings.Models[use]; done {
				continue
			}
			// An empty alias resolves exactly as the worker resolves it.
			resolved, field := use.Alias, ""
			if resolved == "" {
				resolved, field = llm.DefaultModel, "llm.default_model"
				if use.Kind == analyzer.ModelEmbedding {
					resolved, field = llm.EmbeddingModel, "llm.embedding_model"
				}
			}
			if resolved == "" {
				settings.Models[use] = analysis.CacheModel{
					Reason: fmt.Sprintf("no %s model is configured (%s is empty)", use.Kind, field)}
				continue
			}
			key := analyzer.ModelUse{Alias: resolved, Kind: use.Kind}
			if cm, ok := probed[key]; ok {
				settings.Models[use] = cm
				continue
			}
			cm, err := s.probeModel(ctx, tenantID, job.JobID, key)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "model probe failed")
				// A cancelled job or a stopping daemon is not a gateway
				// failure: the executor ends the job as cancelled or
				// interrupted from the context's error.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return analysis.CacheSettings{}, fmt.Errorf("result cache: probing alias %q stopped: %w", resolved, ctxErr)
				}
				return analysis.CacheSettings{}, s.llmUnavailable(ctx, job, name, resolved, tenantID, err)
			}
			probed[key] = cm
			settings.Models[use] = cm
		}
	}
	span.SetStatus(codes.Ok, "")
	return settings, nil
}

// usableModelName reports whether a probe's model name can join cache keys
// and be shown to the tenant: printable UTF-8 of at most
// maxObservedModelBytes bytes.
func usableModelName(name string) bool {
	return len(name) <= maxObservedModelBytes && utf8.ValidString(name) &&
		strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

// probeModel makes one minimal call for use, whose alias is resolved, and
// returns the alias's model identity: Observed and Deployment set when the
// alias is cached, a fixed Reason when it is not (the rules are on
// prepareResultCache).
func (s *Service) probeModel(ctx context.Context, tenantID, jobID string, use analyzer.ModelUse) (cm analysis.CacheModel, err error) {
	start := time.Now()
	var observed string
	defer func() {
		outcome := "observed"
		switch {
		case err != nil:
			outcome = "failed"
		case observed == "" || !usableModelName(observed):
			outcome = "empty"
		case cm.Observed == "":
			outcome = "uncached"
		}
		attrs := metric.WithAttributes(attribute.String("tenant.id", tenantID), attribute.String("kind", string(use.Kind)),
			attribute.String("outcome", outcome))
		if s.probeOps != nil {
			s.probeOps.Add(ctx, 1, attrs)
		}
		if s.probeDuration != nil {
			s.probeDuration.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
		}
	}()
	var deployment string
	if use.Kind == analyzer.ModelEmbedding {
		resp, err := s.probe.Embed(ctx, &llmclient.EmbeddingRequest{Model: use.Alias, Input: []string{modelProbeText},
			TenantID: tenantID, JobID: jobID})
		if err != nil {
			return analysis.CacheModel{}, err
		}
		observed, deployment = resp.Model, resp.Deployment
	} else {
		zero := 0.0
		resp, err := s.probe.Complete(ctx, &llmclient.CompletionRequest{
			Model:       use.Alias,
			Messages:    []llmclient.ChatMessage{{Role: llmclient.RoleUser, Content: modelProbeText}},
			MaxTokens:   1,
			Temperature: &zero,
			TenantID:    tenantID,
			JobID:       jobID,
			PromptName:  resultcache.ProbePromptName,
		})
		if err != nil {
			return analysis.CacheModel{}, err
		}
		observed, deployment = resp.Model, resp.Deployment
	}
	cm = analysis.CacheModel{Alias: use.Alias, Revision: s.llmCfg.ModelRevisions[use.Alias]}
	switch {
	case observed == "":
		cm.Reason = fmt.Sprintf("the model gateway returned no model name for alias %q, "+
			"so its calls were neither looked up nor stored", use.Alias)
	case !usableModelName(observed):
		cm.Reason = fmt.Sprintf("the model gateway returned a model name for alias %q that is not printable text "+
			"of at most %d bytes, so its calls were neither looked up nor stored", use.Alias, maxObservedModelBytes)
	case !usableModelName(deployment):
		cm.Reason = fmt.Sprintf("the model gateway returned a deployment ID for alias %q that is not printable text "+
			"of at most %d bytes, so its calls were neither looked up nor stored", use.Alias, maxObservedModelBytes)
	case observed == use.Alias && deployment == "" && cm.Revision == "":
		cm.Reason = fmt.Sprintf("the model gateway named only the alias %q as the model and sent no deployment ID, "+
			"so a change of the model behind the alias could not be detected and its calls were neither looked up "+
			"nor stored. To cache them, set llm.model_revisions.%s and change it whenever the model behind the alias changes",
			use.Alias, use.Alias)
	default:
		cm.Observed, cm.Deployment = observed, deployment
	}
	return cm, nil
}

// llmUnavailable logs a failed probe once, with its cause, marks
// analyzerName's stage failed and returns the tenant-facing error, which
// carries no text of the cause (CWE-209). A probe that timed out gets its
// own advice: a server that loads models on demand may still be loading the
// model (#262).
func (s *Service) llmUnavailable(ctx context.Context, job *Job, analyzerName, alias, tenantID string, cause error) error {
	logAttrs := []any{"analyzer", analyzerName, "alias", alias, "tenant_id", tenantID, "job_id", job.JobID,
		"trace_id", telemetry.TraceIDFromContext(ctx), "error", cause}
	err := &analysis.StageError{Analyzer: analyzerName, Sentinel: ErrLLMUnavailable, Cause: cause}
	if netErr, ok := errors.AsType[net.Error](cause); ok && netErr.Timeout() {
		s.logger.ErrorContext(ctx, "the model probe timed out, so the job stopped before any analysis LLM call. "+
			"The model server may still be loading the model: raise llm.probe_timeout to cover a cold load, "+
			"or keep the model loaded on the server", logAttrs...)
		err.Message = fmt.Sprintf(
			"Stage %q stopped: the model gateway did not answer a probe request within the probe timeout, and no analysis "+
				"LLM calls were made (error class llm_unavailable). The model may still be loading on a server that loads "+
				"models on demand: retry this job shortly (crosscodex run retry %s). If it keeps timing out, ask your "+
				"operator to raise llm.probe_timeout or keep the model loaded on the server, and give them %s.",
			analyzerName, job.JobID, analysis.TraceReference(ctx, job.JobID))
	} else {
		s.logger.ErrorContext(ctx, "the model probe failed, so the job stopped before any analysis LLM call. "+
			"Check that the LLM gateway (llm.gateway_url) is reachable and serves the alias", logAttrs...)
		err.Message = fmt.Sprintf(
			"Stage %q stopped: the model gateway did not complete a probe request before analysis, and no analysis LLM calls "+
				"were made (error class llm_unavailable). Retry this job once the gateway is working (crosscodex run retry %s). "+
				"If this persists, give your operator %s.",
			analyzerName, job.JobID, analysis.TraceReference(ctx, job.JobID))
	}
	if uerr := s.store.UpdateStageError(ctx, job.JobID, analyzerName, tenantFailure(ctx, job.JobID, job, err)); uerr != nil {
		s.logger.WarnContext(ctx, "failed to record the model probe failure on the stage",
			"job_id", job.JobID, "analyzer", analyzerName, "error", uerr)
	}
	return err
}
