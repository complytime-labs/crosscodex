package relationship

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
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
	analyzerName = "relationship"
	promptName   = "relationship"
)

// classificationGuidance is the static guidance text embedded in the system prompt.
// Ported from OllamaCrosswalker prompts/base.yaml guidance section.
const classificationGuidance = `Focus on the SEMANTIC INTENT of each requirement, not shared vocabulary.
Two requirements may use identical words (e.g. "audit", "access", "control",
"management", "report") with completely different meanings in different regulatory
contexts. Surface vocabulary overlap does NOT imply a genuine compliance relationship.

Before classifying, reason through these steps:
1. What specific subject domain does the SOURCE address? (e.g., data protection,
   financial reporting, personnel security, physical access, change management)
2. What specific subject domain does the TARGET address?
3. Do both requirements address the SAME subject domain, or merely share vocabulary?
4. If the same domain: does one fully encompass the other, or is there partial overlap?

The core test: if an organisation implements the target requirement, does that
meaningfully contribute to satisfying the source requirement's underlying obligation?
If the answer is no — even if the controls share language — use NO_RELATIONSHIP.

Choosing between the overlap types:
- CONTRIBUTES_TO: both requirements address the SAME subject matter (same type of
  system, actor, data, or risk). Implementing the target measurably contributes to
  the source, but neither fully encompasses the other.
  When CONTRIBUTES_TO, also classify the contribution type:
    INTEGRAL_TO — the source CANNOT be meaningfully satisfied without the target.
      Test: "If this target control did not exist, could the source still be fulfilled?"
      If NO → INTEGRAL_TO.
    EXAMPLE_OF — the target is ONE WAY to partially satisfy the source, but
      alternatives exist.
      Test: "Could the source be satisfied through other controls instead?"
      If YES → EXAMPLE_OF.
- PARTIAL: adjacent or tangentially related, but different subject domains. Shared
  governance vocabulary ("management", "policy", "documentation") is not enough.
- NO_RELATIONSHIP: different domains entirely.

Use CONFLICTS_WITH only when the requirements are in direct operational contradiction —
satisfying one would violate or undermine the other.
Use COMPLEMENTS when two requirements address the same risk through entirely different
mechanisms with no scope overlap — implementing one does not contribute to satisfying
the other, but together they provide complete coverage of the risk area.`

// relationshipDefinitions is the static relationship type definitions text.
// Ported from OllamaCrosswalker prompts/base.yaml relationship_definitions section.
const relationshipDefinitions = `Relationship Definitions:
EQUIVALENT: Same scope and intent; either requirement could satisfy the other.
SUPERSET_OF: Source is broader; target is a specific instance of source.
SUBSET_OF: Source is narrower; source is a specific instance of target.
CONTRIBUTES_TO: Target measurably advances the source's obligation but neither
  fully encompasses the other. Requires shared subject matter — not merely
  shared vocabulary. Sub-classify as INTEGRAL_TO (source cannot be satisfied
  without the target) or EXAMPLE_OF (target is one way among alternatives).
COMPLEMENTS: Same risk domain, different mechanism, no scope overlap; jointly necessary for complete coverage.
PARTIAL: Adjacent domains with tangential connection; shared governance vocabulary
  or adjacent risk area but different subject matter, negligible coverage contribution.
CONFLICTS_WITH: Direct operational contradiction; satisfying one violates or undermines the other.
NO_RELATIONSHIP: Different functional domains or intent; no genuine compliance relationship.`

// RelationshipAnalyzer classifies NIST IR 8477 relationships between pairs of
// compliance controls using multi-sample LLM panel voting.
type RelationshipAnalyzer struct {
	llm        llmclient.Client
	prompts    prompt.Registry
	candidates CandidateProvider
	cfg        config.RelationshipConfig
	tracer     trace.Tracer

	// Metrics (optional, nil-safe)
	voteCounter      metric.Int64Counter
	consensusLatency metric.Float64Histogram
	pairCounter      metric.Int64Counter
}

// Compile-time check that RelationshipAnalyzer implements Analyzer[*pb.Control].
var _ analyzer.Analyzer[*pb.Control] = (*RelationshipAnalyzer)(nil)

// New creates a RelationshipAnalyzer with the given dependencies.
func New(llm llmclient.Client, prompts prompt.Registry, candidates CandidateProvider, cfg config.RelationshipConfig, opts ...Option) *RelationshipAnalyzer {
	a := &RelationshipAnalyzer{
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

// Name returns "relationship".
func (a *RelationshipAnalyzer) Name() string { return analyzerName }

// DependsOn returns ["embedding"] — relationship analysis requires embedding
// similarity results to determine candidate pairs.
func (a *RelationshipAnalyzer) DependsOn() []string { return []string{"embedding"} }

// ResultSchema returns an empty AnalysisResult for type registration.
func (a *RelationshipAnalyzer) ResultSchema() proto.Message {
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
// "source_level_<id>", "source_ancestor_<id>" and the target_ equivalents).
// A pair whose source or target text is missing or empty returns an error
// wrapping intanalyzer.ErrMissingControlText and produces no tasks.
func (a *RelationshipAnalyzer) GenerateWork(ctx context.Context, input *pb.Control, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "relationship.GenerateWork")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("relationship.GenerateWork: %w", err)
	}

	jobID := cfg.Parameters["job_id"]
	if jobID == "" {
		err := fmt.Errorf("relationship.GenerateWork: job_id is required in AnalyzerConfig.Parameters")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	pairs, err := a.candidates.Candidates(ctx, tenantID, jobID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("relationship.GenerateWork: fetching candidates: %w", err)
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
		return nil, fmt.Errorf("relationship.GenerateWork: resolving prompt %q: %w", promptName, err)
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
			err := fmt.Errorf("relationship.GenerateWork: pair %s--%s: %s: %w",
				pair.SourceControlID, pair.TargetControlID, reason, intanalyzer.ErrMissingControlText)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
		// IDs, framework names, types, levels and ancestor titles are short
		// values: PromptValue caps each at MaxPromptValueBytes, so a catalog
		// name cannot carry a statement-sized payload.
		short := func(v string) string { return intanalyzer.PromptValue(v, intanalyzer.MaxPromptValueBytes) }

		vars := map[string]string{
			"classification_guidance":  classificationGuidance,
			"relationship_definitions": relationshipDefinitions,
			"few_shot_examples":        fewShotStr,
			"source_id":                short(pair.SourceControlID),
			"target_id":                short(pair.TargetControlID),
			"source_text":              sourceText,
			"target_text":              targetText,
			// Keyed per control: in a bidirectional crosswalk either end of a
			// pair can come from either catalog.
			"source_framework": short(cfg.Parameters["source_framework_"+pair.SourceControlID]),
			"target_framework": short(cfg.Parameters["target_framework_"+pair.TargetControlID]),
			"source_type":      short(cfg.Parameters["source_type_"+pair.SourceControlID]),
			"source_level":     short(cfg.Parameters["source_level_"+pair.SourceControlID]),
			"source_ancestor":  short(cfg.Parameters["source_ancestor_"+pair.SourceControlID]),
			"target_type":      short(cfg.Parameters["target_type_"+pair.TargetControlID]),
			"target_level":     short(cfg.Parameters["target_level_"+pair.TargetControlID]),
			"target_ancestor":  short(cfg.Parameters["target_ancestor_"+pair.TargetControlID]),
		}

		systemMsg, err := prompt.SubstitutePlaceholders(spec.Templates.System, vars)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("relationship.GenerateWork: substituting system template for %s--%s: %w",
				pair.SourceControlID, pair.TargetControlID, err)
		}

		userMsg, err := prompt.SubstitutePlaceholders(spec.Templates.User, vars)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("relationship.GenerateWork: substituting user template for %s--%s: %w",
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
					"similarity_score":  float64(pair.SimilarityScore),
					"messages":          intanalyzer.MessagesForPayload(messages),
				})
				if err != nil {
					span.RecordError(err)
					span.SetStatus(codes.Error, err.Error())
					return nil, fmt.Errorf("relationship.GenerateWork: building payload for %s: %w", taskID, err)
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

var _ analyzer.Cacheable = (*RelationshipAnalyzer)(nil)

// CacheModels implements analyzer.Cacheable: one completion model per panel
// member in analysis.relationship.models.
func (a *RelationshipAnalyzer) CacheModels() []analyzer.ModelUse {
	uses := make([]analyzer.ModelUse, 0, len(a.cfg.Models))
	for _, m := range a.cfg.Models {
		uses = append(uses, analyzer.ModelUse{Alias: m, Kind: analyzer.ModelCompletion})
	}
	return uses
}

// CacheIdentity implements analyzer.Cacheable from the task's payload,
// whose sample_index keeps each voter's identity distinct. similarity_score
// is not sent to the model, so it is not part of the identity.
func (a *RelationshipAnalyzer) CacheIdentity(task analyzer.Task) (analyzer.CacheRequest, bool) {
	return intanalyzer.PayloadCacheRequest(task, "source_control_id", "target_control_id")
}

// CacheableResult implements analyzer.Cacheable: a result is stored only
// when ParseResponse finds a known relationship in its response, the vote
// Aggregate counts.
func (a *RelationshipAnalyzer) CacheableResult(result *structpb.Struct) error {
	raw, ok := intanalyzer.ExtractResponseText(result)
	if !ok {
		return errors.New("the result has no response text")
	}
	if ParseResponse(raw).ParseStatus != ParseOK {
		return errors.New("the relationship response does not parse: it names no known RELATIONSHIP")
	}
	return nil
}

// Aggregate groups completed task results by source/target pair, computes
// consensus for each pair via the pure ComputeConsensus function (every pair
// with at least one parseable vote gets a result — there is no vote-count
// gate here, unlike requires.Aggregate), and JSON-encodes the surviving
// pairs into Output.ResultData. Pairs whose consensus is RelNoRelationship
// are omitted: the graph subscriber materializes a graph edge for every
// entry in ResultData unconditionally, so a "no relationship" pair must
// never reach it. Persisting ResultData to Postgres and publishing the
// resulting NATS events remain pipeline-layer concerns handled by the
// orchestrator after calling Aggregate.
func (a *RelationshipAnalyzer) Aggregate(ctx context.Context, taskResults []analyzer.TaskResult) (*analyzer.Output, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "relationship.Aggregate")
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
			attribute.String("relationship.result", "processed"),
		))
	}

	span.SetAttributes(
		attribute.Int("relationship.total", totalCount),
		attribute.Int("relationship.errors", errorCount),
	)

	groups := make(map[string]map[string]*Vote) // pairKey -> voteKey -> Vote
	pairIDs := make(map[string][2]string)       // pairKey -> [source, target]
	var order []string

	for _, r := range taskResults {
		if r.Error != nil {
			continue
		}
		source, target, model, sample, ok := intanalyzer.ParsePairTaskID(r.TaskID, analyzerName, a.cfg.Models)
		if !ok {
			continue
		}
		raw, ok := intanalyzer.ExtractResponseText(r.Result)
		if !ok {
			continue
		}
		vote := ParseResponse(raw)
		vote.VoteKey = r.TaskID
		vote.Model = model
		vote.SampleIndex = sample

		key := source + "--" + target
		if _, exists := groups[key]; !exists {
			groups[key] = make(map[string]*Vote)
			pairIDs[key] = [2]string{source, target}
			order = append(order, key)
		}
		groups[key][r.TaskID] = vote
	}

	sort.Strings(order)
	var pairResults []results.SemanticMatchResult
	for _, key := range order {
		ids := pairIDs[key]
		c := ComputeConsensus(groups[key])
		if c.Relationship == RelNoRelationship {
			continue // no relationship found: nothing to persist for this pair
		}
		pairResults = append(pairResults, results.SemanticMatchResult{
			SourceID: ids[0], TargetID: ids[1],
			RelationshipType: c.Relationship.String(),
			Confidence:       c.ConfidenceFraction,
			Properties: map[string]string{
				results.SemanticMatchContributionTypeKey: c.ContributionType.String(),
				results.SemanticMatchUnanimousKey:        strconv.FormatBool(c.Unanimous),
				"valid_vote_count":                       strconv.Itoa(c.ValidVoteCount),
			},
		})
	}

	resultData, err := json.Marshal(pairResults)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("relationship.Aggregate: marshaling result data: %w", err)
	}

	span.SetStatus(codes.Ok, "")

	return &analyzer.Output{
		AnalyzerName: analyzerName,
		Data:         nil, // Individual results are in the TaskResult slice
		Metadata:     metadata,
		ResultData:   resultData,
	}, nil
}
