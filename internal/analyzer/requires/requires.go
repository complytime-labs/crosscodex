package requires

import (
	"context"
	"fmt"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
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
// The prompt messages are not included in the payload -- workers re-resolve
// the prompt using the prompt_name, prompt_version, and content_hash fields.
// content_hash enables workers to verify consistency after re-resolution.
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
		sourceText := intanalyzer.TruncateText(input.GetStatement(), a.cfg.MaxSourceChars)
		targetText := intanalyzer.TruncateText(cfg.Parameters["target_text_"+pair.TargetControlID], a.cfg.MaxTargetChars)

		vars := map[string]string{
			"requires_guidance":      requiresGuidance,
			"dependency_definitions": dependencyDefinitions,
			"few_shot_examples":      fewShotStr,
			"source_id":              pair.SourceControlID,
			"target_id":              pair.TargetControlID,
			"source_text":            sourceText,
			"target_text":            targetText,
			"source_framework":       cfg.Parameters["source_framework"],
			"target_framework":       cfg.Parameters["target_framework"],
			"source_type":            cfg.Parameters["source_type_"+pair.SourceControlID],
			"source_level":           cfg.Parameters["source_level_"+pair.SourceControlID],
			"source_ancestor":        cfg.Parameters["source_ancestor_"+pair.SourceControlID],
			"target_type":            cfg.Parameters["target_type_"+pair.TargetControlID],
			"target_level":           cfg.Parameters["target_level_"+pair.TargetControlID],
			"target_ancestor":        cfg.Parameters["target_ancestor_"+pair.TargetControlID],
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

// Aggregate groups completed task results by pair and produces a summary
// output with metadata. The consensus computation, per-pair JSON writes,
// and NATS event publishing are pipeline-layer concerns handled by the
// orchestrator after calling Aggregate -- matching the relationship pattern
// where Aggregate stays lightweight.
func (a *RequiresAnalyzer) Aggregate(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "requires.Aggregate")
	defer span.End()

	var (
		totalCount int
		errorCount int
	)

	for _, r := range results {
		totalCount++
		if r.Error != nil {
			errorCount++
		}
	}

	// Resolve prompt version for metadata.
	promptVersion := ""
	spec, err := a.prompts.Resolve(ctx, promptName)
	if err == nil {
		promptVersion = spec.Version
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
	span.SetStatus(codes.Ok, "")

	return &analyzer.Output{
		AnalyzerName: analyzerName,
		Data:         nil, // Individual results are in the TaskResult slice
		Metadata:     metadata,
	}, nil
}
