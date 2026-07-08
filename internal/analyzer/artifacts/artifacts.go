package artifacts

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
	analyzerName = "artifacts"
	promptName   = "artifacts"
)

// Static guidance text ported from OllamaCrosswalker prompts/artifact_base.yaml.
const extractionGuidance = `Extract the concrete, observable artifacts that this requirement demands or clearly
implies. An artifact is something that must EXIST (a document, configuration, role,
process) for the requirement to be satisfied.

Before extracting, reason through these steps:
1. What action does the requirement mandate? (e.g., define, enforce, review, maintain)
2. What object must that action produce or operate on? (e.g., a policy document,
   a technical configuration, a recurring review process)
3. Is there an explicit or implied frequency? (e.g., annual, quarterly, continuous)
4. Is there an explicit or implied owner role? (e.g., CISO, system administrator)

Rules:
- Only extract artifacts that are EXPLICITLY demanded or CLEARLY implied by the
  requirement text. Do not invent artifacts from general compliance knowledge.
- Section headers, scope descriptions, and preamble text that contain no actionable
  obligation should produce ARTIFACTS: NONE.
- Each artifact must have exactly one type from the taxonomy below.
- Use short, descriptive names (3-8 words). Do not repeat the full requirement text.
- If a field is not stated or implied, use NONE for that field.`

// artifactTypeDefinitions is the static type taxonomy text.
const artifactTypeDefinitions = `- POLICY: Governance document establishing rules, principles, or intent
- PROCEDURE: Step-by-step operational instructions for carrying out an activity
- PLAN: Forward-looking document defining scope, schedule, or approach
- REPORT: Periodic output documenting findings, status, or assessment results
- RECORD: Evidence that a specific event or action occurred
- CONFIGURATION: Technical setting with a verifiable state on a system
- MECHANISM: Technical capability or control that must be present and operational
- ROLE: Organizational position or responsibility that must be assigned
- PROCESS: Recurring activity with a defined frequency or trigger`

// outputFormat is the expected LLM output format.
const outputFormat = `For each artifact, output a block separated by --- delimiters.
If the requirement demands no artifacts, respond with: ARTIFACTS: NONE

ARTIFACT_NAME: <short descriptive name>
ARTIFACT_TYPE: <POLICY|PROCEDURE|PLAN|REPORT|RECORD|CONFIGURATION|MECHANISM|ROLE|PROCESS>
FREQUENCY: <frequency or NONE>
OWNER_ROLE: <role or NONE>
DESCRIPTION: <one sentence describing the artifact>`

// ArtifactsAnalyzer extracts observable artifacts from compliance controls
// using multi-sample LLM panel voting with fuzzy deduplication.
type ArtifactsAnalyzer struct {
	llm     llmclient.Client
	prompts prompt.Registry
	cfg     config.ArtifactsConfig
	tracer  trace.Tracer

	extractionCounter metric.Int64Counter // optional, nil-safe
}

// Compile-time check that ArtifactsAnalyzer implements Analyzer[*pb.Control].
var _ analyzer.Analyzer[*pb.Control] = (*ArtifactsAnalyzer)(nil)

// New creates an ArtifactsAnalyzer with the given dependencies.
func New(llm llmclient.Client, prompts prompt.Registry, cfg config.ArtifactsConfig, opts ...Option) *ArtifactsAnalyzer {
	a := &ArtifactsAnalyzer{
		llm:     llm,
		prompts: prompts,
		cfg:     cfg,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

func (a *ArtifactsAnalyzer) Name() string      { return analyzerName }
func (a *ArtifactsAnalyzer) DependsOn() []string { return []string{} }
func (a *ArtifactsAnalyzer) ResultSchema() proto.Message {
	return &pb.AnalysisResult{}
}

// GenerateWork produces one Task per model x sample for the given control.
// Sections (class "compliance-section") are auto-skipped.
func (a *ArtifactsAnalyzer) GenerateWork(ctx context.Context, input *pb.Control, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "artifacts.GenerateWork")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("artifacts.GenerateWork: %w", err)
	}

	// Skip sections.
	if input.GetParts()["class"] == "compliance-section" {
		taskID := fmt.Sprintf("%s-%s-skip", analyzerName, input.GetIdentifier())
		payload, _ := structpb.NewStruct(map[string]interface{}{
			"control_id": input.GetIdentifier(),
			"skipped":    "true",
		})
		span.SetStatus(codes.Ok, "")
		return []analyzer.Task{{
			TaskID:   taskID,
			TaskType: analyzerName,
			Payload:  payload,
		}}, nil
	}

	// Resolve prompt spec.
	spec, err := a.prompts.Resolve(ctx, promptName)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("artifacts.GenerateWork: resolving prompt %q: %w", promptName, err)
	}

	fewShotStr := intanalyzer.FormatFewShotExamples(spec.FewShot)
	reqText := intanalyzer.TruncateText(input.GetStatement(), a.cfg.MaxTextChars)

	vars := map[string]string{
		"extraction_guidance":      extractionGuidance,
		"artifact_type_definitions": artifactTypeDefinitions,
		"few_shot_examples":        fewShotStr,
		"output_format":            outputFormat,
		"control_id":               input.GetIdentifier(),
		"framework":                cfg.Parameters["framework"],
		"ancestor_title":           cfg.Parameters["ancestor_title_" + input.GetIdentifier()],
		"requirement_text":         reqText,
	}

	systemMsg, err := prompt.SubstitutePlaceholders(spec.Templates.System, vars)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("artifacts.GenerateWork: substituting system template for %s: %w",
			input.GetIdentifier(), err)
	}

	userMsg, err := prompt.SubstitutePlaceholders(spec.Templates.User, vars)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("artifacts.GenerateWork: substituting user template for %s: %w",
			input.GetIdentifier(), err)
	}

	messages := []llmclient.ChatMessage{
		{Role: llmclient.RoleSystem, Content: systemMsg},
		{Role: llmclient.RoleUser, Content: userMsg},
	}
	contentHash := llmclient.ContentHash(messages)

	temperature := a.cfg.SamplingTemperature
	if a.cfg.SamplesPerModel <= 1 {
		temperature = 0.0
	}

	var tasks []analyzer.Task
	for _, model := range a.cfg.Models {
		for s := 0; s < a.cfg.SamplesPerModel; s++ {
			taskID := fmt.Sprintf("%s-%s-%s-s%d",
				analyzerName, input.GetIdentifier(), model, s)

			payload, err := structpb.NewStruct(map[string]interface{}{
				"control_id":    input.GetIdentifier(),
				"model":         model,
				"sample_index":  float64(s),
				"temperature":   temperature,
				"max_tokens":    float64(a.cfg.MaxTokens),
				"prompt_name":   spec.Name,
				"prompt_version": spec.Version,
				"content_hash":  contentHash,
			})
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				return nil, fmt.Errorf("artifacts.GenerateWork: building payload for %s: %w", taskID, err)
			}

			tasks = append(tasks, analyzer.Task{
				TaskID:   taskID,
				TaskType: analyzerName,
				Payload:  payload,
			})
		}
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("control.id", input.GetIdentifier()),
		attribute.Int("task.count", len(tasks)),
	)
	span.SetStatus(codes.Ok, "")
	return tasks, nil
}

// Aggregate groups completed task results and produces a summary output.
// Consensus computation and graph materialization are pipeline-layer concerns.
func (a *ArtifactsAnalyzer) Aggregate(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "artifacts.Aggregate")
	defer span.End()

	var totalCount, errorCount int
	for _, r := range results {
		totalCount++
		if r.Error != nil {
			errorCount++
		}
	}

	promptVersion := ""
	if a.prompts != nil {
		spec, err := a.prompts.Resolve(ctx, promptName)
		if err == nil {
			promptVersion = spec.Version
		}
	}

	metadata := map[string]string{
		"total_count":    strconv.Itoa(totalCount),
		"error_count":    strconv.Itoa(errorCount),
		"prompt_name":    promptName,
		"prompt_version": promptVersion,
	}

	if a.extractionCounter != nil {
		a.extractionCounter.Add(ctx, int64(totalCount), metric.WithAttributes(
			attribute.String("artifacts.result", "processed"),
		))
	}

	span.SetAttributes(
		attribute.Int("artifacts.total", totalCount),
		attribute.Int("artifacts.errors", errorCount),
	)
	span.SetStatus(codes.Ok, "")

	return &analyzer.Output{
		AnalyzerName: analyzerName,
		Data:         nil,
		Metadata:     metadata,
	}, nil
}
