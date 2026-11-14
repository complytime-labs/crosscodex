package critique

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	analyzerName = results.CritiqueAnalyzer
	promptName   = "critique"
)

// MaxJustificationChars bounds each proposing-panel justification rendered
// into the critique prompt, in runes after hardening with
// intanalyzer.PromptText. The number of justifications is bounded by the
// proposing panel's size.
const MaxJustificationChars = 600

// ErrInvalidVerdict reports a proposed verdict the critique cannot review: an
// unknown kind, or the same (kind, pair) listed twice. Both mean the stored
// relationship or requires result is corrupt.
var ErrInvalidVerdict = errors.New("invalid proposed verdict")

// critiqueGuidance is the static guidance in the system prompt. It keeps the
// critic sceptical: the burden is on the verdict, not on the critic.
const critiqueGuidance = `Your job is to catch verdicts that are wrong. Uphold a verdict only when
the requirements themselves support it; a confident justification is not
evidence.

Sharing a topic, keywords or a control family is not a relationship and not a
dependency. Before answering, name the specific obligation the verdict rests
on:
- For a relationship verdict: what implementing the target does toward the
  source's obligation, and whether the stated relationship type fits it.
- For a prerequisite (requires) verdict: what the source cannot do without the
  target already in place.

Sanity-check it: would an auditor accept evidence of the target as evidence
toward the source (relationship), or agree the source is impaired without the
target (requires)? If you cannot name the obligation, the justifications only
cite shared vocabulary, or the check fails, answer REJECT. Otherwise answer
UPHOLD.`

// CritiqueAnalyzer reviews the verdicts the relationship and requires panels
// accepted, with an independent panel that answers UPHOLD or REJECT.
type CritiqueAnalyzer struct {
	llm       llmclient.Client
	prompts   prompt.Registry
	verdicts  VerdictProvider
	cfg       config.CritiqueConfig
	dependsOn []string
	logger    *slog.Logger
	tracer    trace.Tracer

	// Metrics (optional, nil-safe)
	verdictCounter metric.Int64Counter
	duration       metric.Float64Histogram
}

var (
	_ analyzer.Analyzer[*pb.Control] = (*CritiqueAnalyzer)(nil)
	_ analyzer.Cacheable             = (*CritiqueAnalyzer)(nil)
	_ analyzer.Panelled              = (*CritiqueAnalyzer)(nil)
)

// New creates a CritiqueAnalyzer. llm is unused by the analyzer itself (the
// workers call the models) and kept for parity with the other analyzers.
func New(llm llmclient.Client, prompts prompt.Registry, verdicts VerdictProvider, cfg config.CritiqueConfig, opts ...Option) *CritiqueAnalyzer {
	a := &CritiqueAnalyzer{
		llm:      llm,
		prompts:  prompts,
		verdicts: verdicts,
		cfg:      cfg,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Name returns "critique".
func (a *CritiqueAnalyzer) Name() string { return analyzerName }

// DependsOn returns the analyzers set by WithDependencies: the enabled ones
// among relationship and requires.
func (a *CritiqueAnalyzer) DependsOn() []string { return slices.Clone(a.dependsOn) }

// ResultSchema returns an empty AnalysisResult for type registration.
func (a *CritiqueAnalyzer) ResultSchema() proto.Message { return &pb.AnalysisResult{} }

// Panel implements analyzer.Panelled: the aliases in analysis.critique.models
// and analysis.critique.allow_shared_panel_models.
func (a *CritiqueAnalyzer) Panel() analyzer.Panel {
	return analyzer.Panel{Aliases: slices.Clone(a.cfg.Models), AllowShared: a.cfg.AllowSharedPanelModels}
}

// CacheModels implements analyzer.Cacheable: one completion model per panel
// member in analysis.critique.models.
func (a *CritiqueAnalyzer) CacheModels() []analyzer.ModelUse {
	uses := make([]analyzer.ModelUse, 0, len(a.cfg.Models))
	for _, m := range a.cfg.Models {
		uses = append(uses, analyzer.ModelUse{Alias: m, Kind: analyzer.ModelCompletion})
	}
	return uses
}

// CacheIdentity implements analyzer.Cacheable from the task's payload. The
// verdict kind, the proposed verdict and the justifications are in the
// rendered messages, so they are part of the content hash.
func (a *CritiqueAnalyzer) CacheIdentity(task analyzer.Task) (analyzer.CacheRequest, bool) {
	return intanalyzer.PayloadCacheRequest(task, "source_control_id", "target_control_id")
}

// CacheableResult implements analyzer.Cacheable: a result is stored only
// when ParseCritique reads a valid vote from it.
func (a *CritiqueAnalyzer) CacheableResult(result *structpb.Struct) error {
	raw, ok := intanalyzer.ExtractResponseText(result)
	if !ok {
		return errors.New("the result has no response text")
	}
	if !ParseCritique(raw).Valid() {
		return errors.New("the critique response does not parse: it has no VERDICT: UPHOLD or REJECT line or no REASON line")
	}
	return nil
}

// GenerateWork produces one task per proposed verdict, panel model and
// sample, with task ID "critique-<kind>-<source>--<target>-<model>-s<N>".
// It runs once per job in the pipeline's candidate-dependent pass, so input
// is unused. The verdicts come from the VerdictProvider. Each pair's control
// texts come from cfg.Parameters ("source_text_<id>", "target_text_<id>",
// "source_context_<id>", "source_title_<id>" and the target_ equivalents).
//
// It returns an error wrapping intanalyzer.ErrMissingControlText for a pair
// whose statement is missing or empty, and one wrapping ErrInvalidVerdict for
// an unknown kind or a duplicate (kind, pair). Either way it produces no
// tasks.
func (a *CritiqueAnalyzer) GenerateWork(ctx context.Context, _ *pb.Control, cfg analyzer.AnalyzerConfig) (tasks []analyzer.Task, err error) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "critique.GenerateWork")
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		a.recordDuration(ctx, "generate", start)
		span.End()
	}()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("critique.GenerateWork: %w", err)
	}
	jobID := cfg.Parameters["job_id"]
	if jobID == "" {
		return nil, errors.New("critique.GenerateWork: job_id is required in AnalyzerConfig.Parameters")
	}
	verdicts, err := a.verdicts.Verdicts(ctx, tenantID, jobID)
	if err != nil {
		return nil, fmt.Errorf("critique.GenerateWork: fetching proposed verdicts: %w", err)
	}
	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job.id", jobID),
		attribute.Int("verdict.count", len(verdicts)),
		attribute.Int("model.count", len(a.cfg.Models)),
	)
	if len(verdicts) == 0 {
		return nil, nil
	}

	spec, err := a.prompts.Resolve(ctx, promptName)
	if err != nil {
		return nil, fmt.Errorf("critique.GenerateWork: resolving prompt %q: %w", promptName, err)
	}
	temperature := a.cfg.SamplingTemperature
	if a.cfg.SamplesPerModel <= 1 {
		temperature = 0.0
	}

	seen := make(map[string]bool, len(verdicts))
	for _, v := range verdicts {
		pair := v.SourceControlID + "--" + v.TargetControlID
		if v.Kind != KindRelationship && v.Kind != KindRequires {
			return nil, fmt.Errorf("critique.GenerateWork: pair %s has verdict kind %q, not %q or %q: %w",
				pair, v.Kind, KindRelationship, KindRequires, ErrInvalidVerdict)
		}
		if seen[v.Kind+"|"+pair] {
			return nil, fmt.Errorf("critique.GenerateWork: the %s verdict of pair %s is listed twice, so the stored %s result is corrupt; "+
				"re-run the job: %w", v.Kind, pair, v.Kind, ErrInvalidVerdict)
		}
		seen[v.Kind+"|"+pair] = true

		messages, err := a.render(spec, v, cfg.Parameters)
		if err != nil {
			return nil, fmt.Errorf("critique.GenerateWork: %s verdict of pair %s: %w", v.Kind, pair, err)
		}
		contentHash := llmclient.ContentHash(messages)

		for _, model := range a.cfg.Models {
			for s := 0; s < a.cfg.SamplesPerModel; s++ {
				taskID := fmt.Sprintf("%s-%s-%s--%s-%s-s%d", analyzerName, v.Kind, v.SourceControlID, v.TargetControlID, model, s)
				_, taskSpan := telemetry.StartSpan(a.tracer, ctx, "critique.task")
				taskSpan.SetAttributes(
					attribute.String("tenant.id", tenantID),
					attribute.String("task.id", taskID),
					attribute.String("critique.kind", v.Kind),
					attribute.String("model", model),
					attribute.Int("sample", s),
				)
				payload, err := structpb.NewStruct(map[string]interface{}{
					"kind":              v.Kind,
					"source_control_id": v.SourceControlID,
					"target_control_id": v.TargetControlID,
					"model":             model,
					"sample_index":      float64(s),
					"temperature":       temperature,
					"max_tokens":        float64(a.cfg.MaxTokens),
					"prompt_name":       spec.Name,
					"prompt_version":    spec.Version,
					"content_hash":      contentHash,
					"messages":          intanalyzer.MessagesForPayload(messages),
				})
				if err != nil {
					taskSpan.RecordError(err)
					taskSpan.SetStatus(codes.Error, err.Error())
					taskSpan.End()
					return nil, fmt.Errorf("critique.GenerateWork: building payload for %s: %w", taskID, err)
				}
				taskSpan.End()
				tasks = append(tasks, analyzer.Task{TaskID: taskID, TaskType: analyzerName, Payload: payload})
			}
		}
	}
	span.SetAttributes(attribute.Int("task.count", len(tasks)))
	return tasks, nil
}

// render builds the system and user messages that ask the panel to review v.
// Every value from a catalog or a model is hardened: statements and
// justifications with PromptText, short values with PromptValue, contexts
// with PromptContext. Each stays inside its data tag.
func (a *CritiqueAnalyzer) render(spec *prompt.PromptSpec, v ProposedVerdict, params map[string]string) ([]llmclient.ChatMessage, error) {
	sourceKey := "source_text_" + v.SourceControlID
	targetKey := "target_text_" + v.TargetControlID
	sourceText := intanalyzer.PromptText(params[sourceKey], a.cfg.MaxSourceChars)
	targetText := intanalyzer.PromptText(params[targetKey], a.cfg.MaxTargetChars)
	for _, end := range [][2]string{{sourceKey, sourceText}, {targetKey, targetText}} {
		key, text := end[0], end[1]
		if text != "" {
			continue
		}
		if _, ok := params[key]; ok {
			return nil, fmt.Errorf("parameter %q has no statement text after removing whitespace and format characters; "+
				"give the control a statement in the catalog and re-import it: %w", key, intanalyzer.ErrMissingControlText)
		}
		return nil, fmt.Errorf("no %q parameter: %w", key, intanalyzer.ErrMissingControlText)
	}
	short := func(s string) string { return intanalyzer.PromptValue(s, intanalyzer.MaxPromptValueBytes) }

	var blocks []string
	for _, j := range v.Justifications {
		if h := intanalyzer.PromptText(j, MaxJustificationChars); h != "" {
			blocks = append(blocks, "<justification>\n"+h+"\n</justification>")
		}
	}
	justifications := "(none recorded)"
	if len(blocks) > 0 {
		justifications = strings.Join(blocks, "\n")
	}
	kindLabel := "relationship type"
	if v.Kind == KindRequires {
		kindLabel = "prerequisite: the source requires the target"
	}

	vars := map[string]string{
		"critique_guidance": critiqueGuidance,
		"source_id":         short(v.SourceControlID),
		"target_id":         short(v.TargetControlID),
		"source_text":       sourceText,
		"target_text":       targetText,
		"source_context":    intanalyzer.PromptContext(params["source_context_"+v.SourceControlID], intanalyzer.MaxPromptContextBytes),
		"source_title":      short(params["source_title_"+v.SourceControlID]),
		"target_context":    intanalyzer.PromptContext(params["target_context_"+v.TargetControlID], intanalyzer.MaxPromptContextBytes),
		"target_title":      short(params["target_title_"+v.TargetControlID]),
		"verdict_kind":      kindLabel,
		"proposed_verdict":  short(v.Verdict),
		"justifications":    justifications,
	}
	system, err := prompt.SubstitutePlaceholders(spec.Templates.System, vars)
	if err != nil {
		return nil, fmt.Errorf("substituting system template: %w", err)
	}
	user, err := prompt.SubstitutePlaceholders(spec.Templates.User, vars)
	if err != nil {
		return nil, fmt.Errorf("substituting user template: %w", err)
	}
	return []llmclient.ChatMessage{
		{Role: llmclient.RoleSystem, Content: system},
		{Role: llmclient.RoleUser, Content: user},
	}, nil
}

// vote is one critic's answer for a reviewed verdict.
type vote struct {
	critique Critique
	failed   string // why the vote does not count; "" for a valid vote
}

// panelSlot is a vote's place in the panel: its model's index in
// analysis.critique.models and its sample number.
type panelSlot struct{ model, sample int }

// reviewed collects the votes of one (kind, pair).
type reviewed struct {
	kind, source, target string
	votes                map[panelSlot]vote
}

// Aggregate groups task results by (kind, pair) and decides each pair. It
// counts UPHOLD and REJECT votes. Task errors, unparseable responses and
// panel votes with no result count as Failed. A pair is vetoed when Rejected
// / (Upheld + Rejected) >= consensus_threshold. When the pair has no valid
// vote, or Failed / panel size exceeds max_error_rate, its critique failed:
// it is kept (Vetoed false). Each result's Outcome is upheld, split (kept
// with at least one REJECT), vetoed or failed. Output.ResultData is a JSON
// []results.CritiqueResult sorted by kind, source and target. Reasons hold one
// entry per panel vote, in panel order (models as configured, then sample
// number), each labelled "<model> s<N>". Metadata error_count counts the task
// errors of results attributed to a panel vote.
func (a *CritiqueAnalyzer) Aggregate(ctx context.Context, taskResults []analyzer.TaskResult) (out *analyzer.Output, err error) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "critique.Aggregate")
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		a.recordDuration(ctx, "aggregate", start)
		span.End()
	}()

	groups := map[string]*reviewed{}
	errorCount := 0
	for _, r := range taskResults {
		id, ok := a.parseTaskID(r.TaskID)
		if !ok || id.sample >= a.cfg.SamplesPerModel {
			a.logger.WarnContext(ctx, "critique: ignoring a result whose task ID is not a critique panel vote", "task_id", r.TaskID)
			continue
		}
		key := id.kind + "|" + id.source + "--" + id.target
		g := groups[key]
		if g == nil {
			g = &reviewed{kind: id.kind, source: id.source, target: id.target, votes: map[panelSlot]vote{}}
			groups[key] = g
		}
		slot := panelSlot{model: slices.Index(a.cfg.Models, id.model), sample: id.sample}
		if _, dup := g.votes[slot]; dup {
			continue
		}
		var v vote
		switch raw, hasText := intanalyzer.ExtractResponseText(r.Result); {
		case r.Error != nil:
			errorCount++
			v.failed = "the critique task failed"
		case !hasText:
			v.failed = "the critique result has no response text"
		default:
			v.critique = ParseCritique(raw)
			if !v.critique.Valid() {
				v.failed = "the response has no VERDICT: UPHOLD or REJECT line or no REASON line"
			}
		}
		g.votes[slot] = v
	}

	critiques := make([]results.CritiqueResult, 0, len(groups))
	counts := map[string]int{}
	for _, g := range groups {
		res := a.decide(g)
		counts[res.Outcome]++
		if a.verdictCounter != nil {
			a.verdictCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("outcome", res.Outcome), attribute.String("critique.kind", g.kind)))
		}
		critiques = append(critiques, res)
	}
	slices.SortFunc(critiques, func(x, y results.CritiqueResult) int {
		return cmp.Or(cmp.Compare(x.Kind, y.Kind), cmp.Compare(x.SourceControlID, y.SourceControlID),
			cmp.Compare(x.TargetControlID, y.TargetControlID))
	})

	resultData, err := json.Marshal(critiques)
	if err != nil {
		return nil, fmt.Errorf("critique.Aggregate: marshaling result data: %w", err)
	}
	promptVersion := ""
	if a.prompts != nil {
		if spec, err := a.prompts.Resolve(ctx, promptName); err == nil {
			promptVersion = spec.Version
		}
	}
	span.SetAttributes(
		attribute.Int("critique.total", len(taskResults)),
		attribute.Int("critique.errors", errorCount),
		attribute.Int("critique.upheld", counts[results.CritiqueUpheld]),
		attribute.Int("critique.split", counts[results.CritiqueSplit]),
		attribute.Int("critique.vetoed", counts[results.CritiqueVetoed]),
		attribute.Int("critique.failed", counts[results.CritiqueFailed]),
	)
	return &analyzer.Output{
		AnalyzerName: analyzerName,
		Metadata: map[string]string{
			"total_count":    strconv.Itoa(len(taskResults)),
			"error_count":    strconv.Itoa(errorCount),
			"upheld_count":   strconv.Itoa(counts[results.CritiqueUpheld]),
			"split_count":    strconv.Itoa(counts[results.CritiqueSplit]),
			"vetoed_count":   strconv.Itoa(counts[results.CritiqueVetoed]),
			"failed_count":   strconv.Itoa(counts[results.CritiqueFailed]),
			"prompt_name":    promptName,
			"prompt_version": promptVersion,
		},
		ResultData: resultData,
	}, nil
}

// decide counts g's votes over the whole panel, in panel order (a panel vote
// with no result is Failed), applies the veto rule and sets the outcome.
func (a *CritiqueAnalyzer) decide(g *reviewed) results.CritiqueResult {
	res := results.CritiqueResult{Kind: g.kind, SourceControlID: g.source, TargetControlID: g.target, Reasons: []string{}}
	for mi, model := range a.cfg.Models {
		for s := range a.cfg.SamplesPerModel {
			label := fmt.Sprintf("%s s%d", model, s)
			v, ok := g.votes[panelSlot{model: mi, sample: s}]
			if !ok {
				v.failed = "no result was returned for this vote"
			}
			countVote(&res, label, v)
		}
	}
	valid := res.Upheld + res.Rejected
	switch {
	case valid == 0 || a.failedOver(res.Failed):
		res.Outcome = results.CritiqueFailed
	case float64(res.Rejected)/float64(valid) >= a.cfg.ConsensusThreshold:
		res.Vetoed, res.Outcome = true, results.CritiqueVetoed
	case res.Rejected > 0:
		res.Outcome = results.CritiqueSplit
	default:
		res.Outcome = results.CritiqueUpheld
	}
	return res
}

// countVote adds vote v, labelled with its model and sample, to res's counts and
// reasons.
func countVote(res *results.CritiqueResult, label string, v vote) {
	switch {
	case v.failed != "":
		res.Failed++
		reason := "FAILED: " + v.failed
		if v.critique.Reason != "" {
			reason += ": " + v.critique.Reason
		}
		res.Reasons = append(res.Reasons, label+" "+reason)
	case v.critique.Verdict == VerdictReject:
		res.Rejected++
		res.Reasons = append(res.Reasons, label+" REJECT: "+v.critique.Reason)
	default:
		res.Upheld++
		res.Reasons = append(res.Reasons, label+" UPHOLD: "+v.critique.Reason)
	}
}

// panelSize is the number of votes each verdict gets.
func (a *CritiqueAnalyzer) panelSize() int { return len(a.cfg.Models) * a.cfg.SamplesPerModel }

// failedOver reports whether failed votes exceed max_error_rate of the panel.
func (a *CritiqueAnalyzer) failedOver(failed int) bool {
	return float64(failed)/float64(max(a.panelSize(), 1)) > a.cfg.MaxErrorRate
}

// voteID is what a critique task ID names.
type voteID struct {
	kind, source, target, model string
	sample                      int
}

// parseTaskID reverses GenerateWork's task ID with the shared pair parser,
// trying each verdict kind as part of the prefix.
func (a *CritiqueAnalyzer) parseTaskID(taskID string) (voteID, bool) {
	for _, k := range []string{KindRelationship, KindRequires} {
		if source, target, model, sample, ok := intanalyzer.ParsePairTaskID(taskID, analyzerName+"-"+k, a.cfg.Models); ok {
			return voteID{kind: k, source: source, target: target, model: model, sample: sample}, true
		}
	}
	return voteID{}, false
}

func (a *CritiqueAnalyzer) recordDuration(ctx context.Context, phase string, start time.Time) {
	if a.duration != nil {
		a.duration.Record(ctx, float64(time.Since(start).Milliseconds()), metric.WithAttributes(attribute.String("phase", phase)))
	}
}
