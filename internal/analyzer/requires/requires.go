package requires

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/consensus"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	analyzerName = "requires"
	promptName   = "requires"
)

// requiresGuidance is the static guidance text embedded in the system prompt.
// Directs the LLM to evaluate prerequisite (REQUIRES) relationships between
// compliance controls based on operational dependency, not surface vocabulary.
const requiresGuidance = `Focus on OPERATIONAL DEPENDENCY between requirements, not shared vocabulary.
A prerequisite relationship means one control MUST be implemented before another
can be effectively implemented or assessed. Surface vocabulary overlap (e.g.,
"access", "audit", "policy") does NOT imply a prerequisite dependency.

Before classifying, reason through these steps:
1. What specific obligation does the SOURCE control impose?
2. What specific obligation does the TARGET control impose?
3. Does implementing the SOURCE require the TARGET to already be in place?
4. Could the SOURCE be meaningfully implemented without the TARGET?

The core test: if an organisation has NOT implemented the target control, is
the source control impossible or significantly impaired to implement? If yes,
the target is a prerequisite. If the source can stand alone, use NO_DEPENDENCY.

Choosing between dependency types:
- REQUIRES: the source control cannot be meaningfully implemented or assessed
  without the target control already being in place. The target is a hard
  operational prerequisite.
  Test: "If the target were removed, would the source become unimplementable
  or fundamentally incomplete?" If YES -> REQUIRES.
- BENEFITS_FROM: the target control improves or strengthens the source, but
  the source can still be implemented without it. The target is a soft
  dependency.
  Test: "Would the source be weakened but still functional without the target?"
  If YES -> BENEFITS_FROM.
- NO_DEPENDENCY: no operational prerequisite relationship exists. The controls
  may share vocabulary or be in the same domain but can be implemented
  independently.`

// dependencyDefinitions is the static dependency type definitions text.
const dependencyDefinitions = `Dependency Definitions:
REQUIRES: Hard prerequisite. The source control cannot be meaningfully
  implemented or assessed without the target already in place. Removing the
  target makes the source unimplementable or fundamentally incomplete.
BENEFITS_FROM: Soft dependency. The target strengthens or improves the source
  implementation, but the source remains functional without it.
NO_DEPENDENCY: No operational prerequisite. Controls can be implemented
  independently regardless of shared vocabulary or domain adjacency.`

// RequiresAnalyzer classifies prerequisite dependency relationships between
// pairs of compliance controls using multi-sample LLM panel voting.
type RequiresAnalyzer struct {
	llm        llmclient.Client
	prompts    prompt.Registry
	candidates CandidateProvider
	cfg        config.RequiresConfig
	consensus  *consensus.Computer
	logger     *slog.Logger
	tracer     trace.Tracer

	// Metrics (optional, nil-safe)
	voteCounter      metric.Int64Counter
	consensusLatency metric.Float64Histogram
	pairCounter      metric.Int64Counter
}

// Compile-time check that RequiresAnalyzer implements Analyzer[*pb.Control].
var _ analyzer.Analyzer[*pb.Control] = (*RequiresAnalyzer)(nil)

// New creates a RequiresAnalyzer with the given dependencies.
func New(llm llmclient.Client, prompts prompt.Registry, candidates CandidateProvider, cfg config.RequiresConfig, opts ...Option) *RequiresAnalyzer {
	a := &RequiresAnalyzer{
		llm:        llm,
		prompts:    prompts,
		candidates: candidates,
		cfg:        cfg,
		consensus: consensus.New(
			consensus.WithThreshold(cfg.ConsensusThreshold),
			consensus.WithMaxErrorRate(cfg.MaxErrorRate),
		),
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Name returns "requires".
func (a *RequiresAnalyzer) Name() string { return analyzerName }

// DependsOn returns ["embedding"] -- requires analysis needs embedding
// similarity results to determine candidate prerequisite pairs.
func (a *RequiresAnalyzer) DependsOn() []string { return []string{"embedding"} }

// ResultSchema returns an empty AnalysisResult for type registration.
func (a *RequiresAnalyzer) ResultSchema() proto.Message {
	return &pb.AnalysisResult{}
}

// GenerateWork produces one Task per candidate pair per model per sample.
// Each task carries LLM parameters in a structpb.Struct payload for the worker.
// The rendered prompt messages travel in the payload's "messages" field,
// alongside prompt_name, prompt_version and their content_hash.
//
// input is unused: the pipeline runs this candidate-dependent pass once per
// job, so each pair's texts come from cfg.Parameters ("source_text_<id>",
// "target_text_<id>"), as do its classification ("source_type_<id>",
// "source_level_<id>", "source_context_<id>", "source_title_<id>" and the target_ equivalents).
// A pair whose source or target text is missing or empty returns an error
// wrapping intanalyzer.ErrMissingControlText and produces no tasks.
func (a *RequiresAnalyzer) GenerateWork(ctx context.Context, input *pb.Control, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "requires.GenerateWork")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("requires.GenerateWork: %w", err)
	}

	jobID := cfg.Parameters["job_id"]
	if jobID == "" {
		err := fmt.Errorf("requires.GenerateWork: job_id is required in AnalyzerConfig.Parameters")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	pairs, err := a.candidates.Candidates(ctx, tenantID, jobID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("requires.GenerateWork: fetching candidates: %w", err)
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.Int("candidate.count", len(pairs)),
		attribute.Int("model.count", len(a.cfg.Models)),
	)

	if len(pairs) == 0 {
		span.SetStatus(codes.Ok, "")
		return nil, nil
	}

	// Resolve prompt spec.
	spec, err := a.prompts.Resolve(ctx, promptName)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("requires.GenerateWork: resolving prompt %q: %w", promptName, err)
	}

	// Format few-shot examples inline in the user template.
	fewShotStr := intanalyzer.FormatFewShotExamples(spec.FewShot)

	// Determine temperature.
	temperature := a.cfg.SamplingTemperature
	if a.cfg.SamplesPerModel <= 1 {
		temperature = 0.0
	}

	// Build tasks.
	var tasks []analyzer.Task
	for _, pair := range pairs {
		// The candidate-dependent pass has no per-control input: the pipeline
		// passes every control's text as parameters.
		sourceKey := "source_text_" + pair.SourceControlID
		targetKey := "target_text_" + pair.TargetControlID
		// Every catalog value is uploader- or OSCAL-controlled, so each is
		// hardened: format characters removed, UTF-8 repaired, whitespace
		// collapsed, tag delimiters escaped. Statements go through
		// PromptText with their end's limit. Hardening comes before the
		// blank check, so a statement of only invisible characters counts
		// as missing.
		sourceText := intanalyzer.PromptText(cfg.Parameters[sourceKey], a.cfg.MaxSourceChars)
		targetText := intanalyzer.PromptText(cfg.Parameters[targetKey], a.cfg.MaxTargetChars)
		var missingKey string
		switch {
		case sourceText == "":
			missingKey = sourceKey
		case targetText == "":
			missingKey = targetKey
		}
		if missingKey != "" {
			reason := fmt.Sprintf("no %q parameter", missingKey)
			if _, ok := cfg.Parameters[missingKey]; ok {
				reason = fmt.Sprintf("parameter %q has no statement text after removing whitespace and format characters; "+
					"give the control a statement in the catalog and re-import it", missingKey)
			}
			err := fmt.Errorf("requires.GenerateWork: pair %s--%s: %s: %w",
				pair.SourceControlID, pair.TargetControlID, reason, intanalyzer.ErrMissingControlText)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
		// IDs, framework names, types, levels and titles are short
		// values: PromptValue caps each at MaxPromptValueBytes, so a catalog
		// name cannot carry a statement-sized payload.
		short := func(v string) string { return intanalyzer.PromptValue(v, intanalyzer.MaxPromptValueBytes) }

		vars := map[string]string{
			"requires_guidance":      requiresGuidance,
			"dependency_definitions": dependencyDefinitions,
			"few_shot_examples":      fewShotStr,
			"source_id":              short(pair.SourceControlID),
			"target_id":              short(pair.TargetControlID),
			"source_text":            sourceText,
			"target_text":            targetText,
			// Keyed per control: in a bidirectional crosswalk either end of a
			// pair can come from either catalog.
			"source_framework": short(cfg.Parameters["source_framework_"+pair.SourceControlID]),
			"target_framework": short(cfg.Parameters["target_framework_"+pair.TargetControlID]),
			"source_type":      short(cfg.Parameters["source_type_"+pair.SourceControlID]),
			"source_level":     short(cfg.Parameters["source_level_"+pair.SourceControlID]),
			"source_context":   intanalyzer.PromptContext(cfg.Parameters["source_context_"+pair.SourceControlID], intanalyzer.MaxPromptContextBytes),
			"source_title":     short(cfg.Parameters["source_title_"+pair.SourceControlID]),
			"target_type":      short(cfg.Parameters["target_type_"+pair.TargetControlID]),
			"target_level":     short(cfg.Parameters["target_level_"+pair.TargetControlID]),
			"target_context":   intanalyzer.PromptContext(cfg.Parameters["target_context_"+pair.TargetControlID], intanalyzer.MaxPromptContextBytes),
			"target_title":     short(cfg.Parameters["target_title_"+pair.TargetControlID]),
		}

		systemMsg, err := prompt.SubstitutePlaceholders(spec.Templates.System, vars)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("requires.GenerateWork: substituting system template for %s--%s: %w",
				pair.SourceControlID, pair.TargetControlID, err)
		}

		userMsg, err := prompt.SubstitutePlaceholders(spec.Templates.User, vars)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("requires.GenerateWork: substituting user template for %s--%s: %w",
				pair.SourceControlID, pair.TargetControlID, err)
		}

		messages := []llmclient.ChatMessage{
			{Role: llmclient.RoleSystem, Content: systemMsg},
			{Role: llmclient.RoleUser, Content: userMsg},
		}
		contentHash := llmclient.ContentHash(messages)

		for _, model := range a.cfg.Models {
			for s := 0; s < a.cfg.SamplesPerModel; s++ {
				taskID := fmt.Sprintf("%s-%s--%s-%s-s%d",
					analyzerName, pair.SourceControlID, pair.TargetControlID, model, s)

				payload, err := structpb.NewStruct(map[string]interface{}{
					"source_control_id": pair.SourceControlID,
					"target_control_id": pair.TargetControlID,
					"model":             model,
					"sample_index":      float64(s),
					"temperature":       temperature,
					"max_tokens":        float64(a.cfg.MaxTokens),
					"prompt_name":       spec.Name,
					"prompt_version":    spec.Version,
					"content_hash":      contentHash,
					"aggregate_score":   pair.AggregateScore,
					"messages":          intanalyzer.MessagesForPayload(messages),
				})
				if err != nil {
					span.RecordError(err)
					span.SetStatus(codes.Error, err.Error())
					return nil, fmt.Errorf("requires.GenerateWork: building payload for %s: %w", taskID, err)
				}

				tasks = append(tasks, analyzer.Task{
					TaskID:   taskID,
					TaskType: analyzerName,
					Payload:  payload,
				})
			}
		}
	}

	span.SetAttributes(attribute.Int("task.count", len(tasks)))
	span.SetStatus(codes.Ok, "")
	return tasks, nil
}

var _ analyzer.Cacheable = (*RequiresAnalyzer)(nil)

// CacheModels implements analyzer.Cacheable: one completion model per panel
// member in analysis.requires.models.
func (a *RequiresAnalyzer) CacheModels() []analyzer.ModelUse {
	uses := make([]analyzer.ModelUse, 0, len(a.cfg.Models))
	for _, m := range a.cfg.Models {
		uses = append(uses, analyzer.ModelUse{Alias: m, Kind: analyzer.ModelCompletion})
	}
	return uses
}

// CacheIdentity implements analyzer.Cacheable from the task's payload,
// whose sample_index keeps each voter's identity distinct. aggregate_score
// is not sent to the model, so it is not part of the identity.
func (a *RequiresAnalyzer) CacheIdentity(task analyzer.Task) (analyzer.CacheRequest, bool) {
	return intanalyzer.PayloadCacheRequest(task, "source_control_id", "target_control_id")
}

// CacheableResult implements analyzer.Cacheable: a result is stored only
// when ParseResponse reads a YES or NO decision from its response, the
// vote Aggregate counts.
func (a *RequiresAnalyzer) CacheableResult(result *structpb.Struct) error {
	raw, ok := intanalyzer.ExtractResponseText(result)
	if !ok {
		return errors.New("the result has no response text")
	}
	if ParseResponse("", raw).Decision == nil {
		return errors.New("the requires response does not parse: it has no REQUIRES: YES or NO decision")
	}
	return nil
}

// Aggregate groups completed task results by source/target pair, computes
// consensus for each pair via a.consensus (skipping pairs that don't clear
// the configured vote/error thresholds), and JSON-encodes the surviving
// "requires" pairs into Output.ResultData. Persisting ResultData to Postgres
// and publishing the resulting NATS events remain pipeline-layer concerns
// handled by the orchestrator after calling Aggregate.
func (a *RequiresAnalyzer) Aggregate(ctx context.Context, taskResults []analyzer.TaskResult) (*analyzer.Output, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "requires.Aggregate")
	defer span.End()

	var (
		totalCount int
		errorCount int
	)

	for _, r := range taskResults {
		totalCount++
		if r.Error != nil {
			errorCount++
		}
	}

	// Resolve prompt version for metadata. Tolerate a nil prompts registry
	// (e.g. in unit tests exercising Aggregate in isolation) the same way a
	// resolution error is tolerated below.
	promptVersion := ""
	if a.prompts != nil {
		if spec, err := a.prompts.Resolve(ctx, promptName); err == nil {
			promptVersion = spec.Version
		}
	}

	metadata := map[string]string{
		"total_count":    strconv.Itoa(totalCount),
		"error_count":    strconv.Itoa(errorCount),
		"prompt_name":    promptName,
		"prompt_version": promptVersion,
	}

	if a.pairCounter != nil {
		a.pairCounter.Add(ctx, int64(totalCount), metric.WithAttributes(
			attribute.String("requires.result", "processed"),
		))
	}

	span.SetAttributes(
		attribute.Int("requires.total", totalCount),
		attribute.Int("requires.errors", errorCount),
	)

	// Group per-pair votes and compute consensus. A pair with too few valid
	// votes (consensus.ErrInsufficientVotes) is skipped rather than failing
	// the whole aggregation -- one noisy pair shouldn't sink every other
	// pair's result. Any other error from Compute is unexpected and fails
	// Aggregate outright.
	type pairGroup struct {
		source, target string
		votes          []consensus.Vote
		models         map[string]bool
	}
	groups := make(map[string]*pairGroup)
	var order []string

	for _, r := range taskResults {
		if r.Error != nil {
			continue
		}
		source, target, model, _, ok := intanalyzer.ParsePairTaskID(r.TaskID, analyzerName, a.cfg.Models)
		if !ok {
			continue
		}
		raw, ok := intanalyzer.ExtractResponseText(r.Result)
		if !ok {
			continue
		}
		key := source + "--" + target
		g, exists := groups[key]
		if !exists {
			g = &pairGroup{source: source, target: target, models: map[string]bool{}}
			groups[key] = g
			order = append(order, key)
		}
		g.votes = append(g.votes, ParseResponse(r.TaskID, raw))
		g.models[model] = true
	}

	sort.Strings(order)
	var pairResults []results.RequiresResult
	for _, key := range order {
		g := groups[key]
		res, err := a.consensus.Compute(g.votes)
		if err != nil {
			var insufficient *consensus.ErrInsufficientVotes
			if errors.As(err, &insufficient) {
				a.logger.WarnContext(ctx, "requires: skipping pair with insufficient votes",
					"source_id", g.source, "target_id", g.target, "error", err)
				continue
			}
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("requires.Aggregate: computing consensus for %s--%s: %w", g.source, g.target, err)
		}
		if !res.Decision || !a.consensus.MeetsThreshold(res) {
			continue // consensus says no prerequisite relationship, or confidence doesn't clear the configured threshold: nothing to persist for this pair
		}
		models := make([]string, 0, len(g.models))
		for m := range g.models {
			models = append(models, m)
		}
		sort.Strings(models)
		pairResults = append(pairResults, results.RequiresResult{
			SourceID: g.source, TargetID: g.target,
			Confidence: res.ConfidenceFraction, Unanimous: res.Unanimous,
			ValidVotes: res.ValidVoteCount, TotalVotes: res.TotalVoteCount, Models: models,
		})
	}

	resultData, err := json.Marshal(pairResults)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("requires.Aggregate: marshaling result data: %w", err)
	}

	span.SetStatus(codes.Ok, "")

	return &analyzer.Output{
		AnalyzerName: analyzerName,
		Data:         nil, // Individual results are in the TaskResult slice
		Metadata:     metadata,
		ResultData:   resultData,
	}, nil
}
