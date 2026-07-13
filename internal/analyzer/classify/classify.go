package classify

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

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
	// analyzerName is the unique identifier for this analyzer.
	analyzerName = "classify"

	// promptName is the name of the prompt spec in the prompt registry.
	promptName = "classify"

	// sectionClass matches oscal.ClassSection for auto-classification.
	sectionClass = "compliance-section"

	// resultTypeClassification is the result_type for AnalysisResult.
	resultTypeClassification = "classification"
)

// ClassifyAnalyzer classifies compliance requirements on type and level dimensions.
type ClassifyAnalyzer struct {
	llm     llmclient.Client
	prompts prompt.Registry
	cfg     config.ClassificationConfig
	tracer  trace.Tracer

	// Metrics (optional, nil-safe)
	classifyCounter  metric.Int64Counter
	textLenHistogram metric.Float64Histogram
}

// Compile-time check that ClassifyAnalyzer implements Analyzer[*pb.Control].
var _ analyzer.Analyzer[*pb.Control] = (*ClassifyAnalyzer)(nil)

// New creates a ClassifyAnalyzer with the given dependencies.
func New(llm llmclient.Client, prompts prompt.Registry, cfg config.ClassificationConfig, opts ...Option) *ClassifyAnalyzer {
	a := &ClassifyAnalyzer{
		llm:     llm,
		prompts: prompts,
		cfg:     cfg,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Name returns "classify".
func (a *ClassifyAnalyzer) Name() string { return analyzerName }

// DependsOn returns nil -- classification has no upstream dependencies.
func (a *ClassifyAnalyzer) DependsOn() []string { return nil }

// ResultSchema returns an empty AnalysisResult for type registration.
func (a *ClassifyAnalyzer) ResultSchema() proto.Message {
	return &pb.AnalysisResult{}
}

// GenerateWork produces one task per control. Sections are auto-classified
// as None|None without an LLM call. Requirements produce a task with the
// rendered prompt and LLM parameters packed into a structpb.Struct payload.
func (a *ClassifyAnalyzer) GenerateWork(ctx context.Context, input *pb.Control, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "classify.GenerateWork")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("classify.GenerateWork: %w", err)
	}

	controlID := input.GetControlId()
	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("control.id", controlID),
	)

	taskID := fmt.Sprintf("%s-%s", analyzerName, controlID)

	// Section auto-classification: no LLM call needed.
	if input.GetParts()["class"] == sectionClass {
		span.SetAttributes(attribute.Bool("classification.skipped", true))
		if a.classifyCounter != nil {
			a.classifyCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("classify.result", "skipped"),
			))
		}
		result := &pb.AnalysisResult{
			ResultId:   taskID,
			ResultType: resultTypeClassification,
			Attributes: map[string]string{
				"control_id": controlID,
				"type":       TypeNone.String(),
				"level":      LevelNone.String(),
				"skipped":    "true",
			},
			Confidence: 1.0,
		}
		return []analyzer.Task{{
			TaskID:   taskID,
			TaskType: analyzerName,
			Payload:  result,
		}}, nil
	}

	// Sanitize requirement text.
	text := sanitizeText(input.GetStatement(), a.cfg.MaxTextLength)

	if a.textLenHistogram != nil {
		a.textLenHistogram.Record(ctx, float64(utf8.RuneCountInString(text)))
	}

	// Resolve prompt spec, format few-shot examples, substitute placeholders.
	// We use Resolve() + manual substitution instead of Render() because
	// Render() adds few-shot examples as separate user/assistant message pairs.
	// The Python classifier puts examples inline in the system prompt (2 messages
	// total), which is more token-efficient and matches the proven pattern.
	spec, err := a.prompts.Resolve(ctx, promptName)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("classify.GenerateWork: resolving prompt %q: %w", promptName, err)
	}

	fewShotStr := formatFewShotExamples(spec.FewShot)
	vars := map[string]string{
		"requirement":       text,
		"few_shot_examples": fewShotStr,
	}

	systemMsg, err := prompt.SubstitutePlaceholders(spec.Templates.System, vars)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("classify.GenerateWork: substituting system template: %w", err)
	}

	userMsg, err := prompt.SubstitutePlaceholders(spec.Templates.User, vars)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("classify.GenerateWork: substituting user template: %w", err)
	}

	// Compute content hash over the assembled messages for provenance.
	messages := []llmclient.ChatMessage{
		{Role: llmclient.RoleSystem, Content: systemMsg},
		{Role: llmclient.RoleUser, Content: userMsg},
	}
	contentHash := llmclient.ContentHash(messages)

	// Pack LLM call parameters into the payload for the worker.
	temperature := a.cfg.Temperature
	model := a.cfg.Model
	payload, err := structpb.NewStruct(map[string]interface{}{
		"control_id":     controlID,
		"model":          model,
		"temperature":    temperature,
		"max_tokens":     float64(a.cfg.MaxTokens),
		"prompt_name":    spec.Name,
		"prompt_version": spec.Version,
		"content_hash":   contentHash,
		"messages":       intanalyzer.MessagesForPayload(messages),
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("classify.GenerateWork: building payload: %w", err)
	}

	if a.classifyCounter != nil {
		a.classifyCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("classify.result", "pending"),
		))
	}

	span.SetStatus(codes.Ok, "")
	return []analyzer.Task{{
		TaskID:   taskID,
		TaskType: analyzerName,
		Payload:  payload,
	}}, nil
}

// Aggregate combines completed task results into a single output with metadata.
func (a *ClassifyAnalyzer) Aggregate(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "classify.Aggregate")
	defer span.End()

	classifiedCount, skippedCount, errorCount := intanalyzer.CountResults(results)

	total := len(results)

	// Resolve prompt version for metadata.
	promptVersion := ""
	spec, err := a.prompts.Resolve(ctx, promptName)
	if err == nil {
		promptVersion = spec.Version
	}

	metadata := map[string]string{
		"total_count":      strconv.Itoa(total),
		"classified_count": strconv.Itoa(classifiedCount),
		"skipped_count":    strconv.Itoa(skippedCount),
		"error_count":      strconv.Itoa(errorCount),
		"prompt_name":      promptName,
		"prompt_version":   promptVersion,
	}

	if a.classifyCounter != nil {
		if classifiedCount > 0 {
			a.classifyCounter.Add(ctx, int64(classifiedCount), metric.WithAttributes(
				attribute.String("classify.result", "classified"),
			))
		}
		if errorCount > 0 {
			a.classifyCounter.Add(ctx, int64(errorCount), metric.WithAttributes(
				attribute.String("classify.result", "error"),
			))
		}
	}

	span.SetAttributes(
		attribute.Int("classify.total", total),
		attribute.Int("classify.classified", classifiedCount),
		attribute.Int("classify.skipped", skippedCount),
		attribute.Int("classify.errors", errorCount),
	)
	span.SetStatus(codes.Ok, "")

	return &analyzer.Output{
		AnalyzerName: analyzerName,
		Data:         nil, // Individual results are in the TaskResult slice
		Metadata:     metadata,
	}, nil
}

// sanitizeText truncates requirement text and replaces newlines with spaces,
// matching the Python classifier behavior. Truncation operates on rune
// boundaries to avoid producing invalid UTF-8.
func sanitizeText(text string, maxLen int) string {
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	if maxLen > 0 && utf8.RuneCountInString(text) > maxLen {
		runes := []rune(text)
		text = string(runes[:maxLen])
	}
	return text
}

// formatFewShotExamples formats few-shot examples into inline text for
// the system prompt, matching the Python classifier pattern.
func formatFewShotExamples(examples []prompt.FewShotExample) string {
	if len(examples) == 0 {
		return ""
	}
	var b strings.Builder
	for _, ex := range examples {
		fmt.Fprintf(&b, "Example: \"%s\" -> %s\n", ex.Input, ex.Output)
	}
	return b.String()
}
