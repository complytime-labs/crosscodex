package embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

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
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	analyzerName        = "embedding"
	resultTypeEmbedding = "embedding"
)

// EmbeddingAnalyzer generates vector embeddings for compliance controls and
// builds per-model cosine similarity matrices.
type EmbeddingAnalyzer struct {
	llm     llmclient.Client
	vectors vectordb.VectorDB
	store   storage.Provider
	cfg     config.EmbeddingConfig
	relCfg  config.RelationshipConfig
	tracer  trace.Tracer

	// Metrics (optional, nil-safe)
	embedCounter metric.Int64Counter
}

// Compile-time check that EmbeddingAnalyzer implements Analyzer[*pb.Control].
var _ analyzer.Analyzer[*pb.Control] = (*EmbeddingAnalyzer)(nil)

// New creates an EmbeddingAnalyzer with the given dependencies.
func New(
	llm llmclient.Client,
	vectors vectordb.VectorDB,
	store storage.Provider,
	cfg config.EmbeddingConfig,
	relCfg config.RelationshipConfig,
	opts ...Option,
) *EmbeddingAnalyzer {
	a := &EmbeddingAnalyzer{
		llm:     llm,
		vectors: vectors,
		store:   store,
		cfg:     cfg,
		relCfg:  relCfg,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Name returns "embedding".
func (a *EmbeddingAnalyzer) Name() string { return analyzerName }

// DependsOn returns ["classify"] — embedding runs after classification.
func (a *EmbeddingAnalyzer) DependsOn() []string { return []string{"classify"} }

// ResultSchema returns an empty AnalysisResult for type registration.
func (a *EmbeddingAnalyzer) ResultSchema() proto.Message {
	return &pb.AnalysisResult{}
}

// GenerateWork produces one task per control per model. Section controls
// are skipped with a pre-built result (no embedding for non-leaf controls).
func (a *EmbeddingAnalyzer) GenerateWork(ctx context.Context, input *pb.Control, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "embedding.GenerateWork")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("embedding.GenerateWork: %w", err)
	}

	controlID := input.GetControlId()
	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("control.id", controlID),
	)

	// Section auto-skip: no embedding for non-leaf controls.
	if input.GetParts()["class"] == oscal.ClassSection {
		span.SetAttributes(attribute.Bool("embedding.skipped", true))
		if a.embedCounter != nil {
			a.embedCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("embedding.result", "skipped"),
			))
		}
		result := &pb.AnalysisResult{
			ResultId:   fmt.Sprintf("%s-%s", analyzerName, controlID),
			ResultType: resultTypeEmbedding,
			Attributes: map[string]string{
				"control_id": controlID,
				"skipped":    "true",
			},
			Confidence: 1.0,
		}
		return []analyzer.Task{{
			TaskID:         fmt.Sprintf("%s-%s", analyzerName, controlID),
			TaskType:       analyzerName,
			Payload:        result,
			PreBuiltResult: true,
		}}, nil
	}

	// Prepare text for embedding.
	ancestorTitle := input.GetParts()["ancestor_title"]
	text := prepareText(input.GetStatement(), ancestorTitle, a.cfg.MaxChars)
	contentHash := llmclient.ContentHash(text)

	// Produce one task per model.
	tasks := make([]analyzer.Task, 0, len(a.cfg.Models))
	for _, model := range a.cfg.Models {
		taskID := fmt.Sprintf("%s-%s-%s", analyzerName, controlID, model)

		payload, err := structpb.NewStruct(map[string]interface{}{
			"control_id":   controlID,
			"model":        model,
			"text":         text,
			"batch_size":   float64(a.cfg.BatchSize),
			"content_hash": contentHash,
		})
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("embedding.GenerateWork: building payload: %w", err)
		}

		tasks = append(tasks, analyzer.Task{
			TaskID:   taskID,
			TaskType: analyzerName,
			Payload:  payload,
		})

		if a.embedCounter != nil {
			a.embedCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("embedding.result", "pending"),
				attribute.String("embedding.model", model),
			))
		}
	}

	span.SetAttributes(attribute.Int("embedding.task_count", len(tasks)))
	span.SetStatus(codes.Ok, "")
	return tasks, nil
}

// Aggregate combines completed embedding results and produces aggregate
// metadata counts (total, embedded, skipped, error), and separately
// JSON-encodes one results.EmbedResult per distinct embedded control into
// Output.ResultData. Per design, graph materialization for embeddings is a
// documented no-op (vectors live in vectordb, not the graph), so this blob
// records only which control IDs were embedded, for readback consistency
// with the other analyzers. GenerateWork emits one TaskID per
// (control, model) as "embedding-<controlID>-<model>", or one per control
// with no model suffix for the section auto-skip path
// ("embedding-<controlID>"); both are stripped via
// intanalyzer.StripTaskIDPrefix and, for the model-suffixed form, a
// configured model name trimmed off the remainder. A control processed
// under multiple models is deduplicated to a single entry. Results with a
// task-level error are omitted from ResultData rather than failing the
// whole stage.
func (a *EmbeddingAnalyzer) Aggregate(ctx context.Context, taskResults []analyzer.TaskResult) (*analyzer.Output, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "embedding.Aggregate")
	defer span.End()

	embeddedCount, skippedCount, errorCount := intanalyzer.CountResults(taskResults)

	total := len(taskResults)

	metadata := map[string]string{
		"total_count":    strconv.Itoa(total),
		"embedded_count": strconv.Itoa(embeddedCount),
		"skipped_count":  strconv.Itoa(skippedCount),
		"error_count":    strconv.Itoa(errorCount),
	}

	if a.embedCounter != nil {
		if embeddedCount > 0 {
			a.embedCounter.Add(ctx, int64(embeddedCount), metric.WithAttributes(
				attribute.String("embedding.result", "embedded"),
			))
		}
		if errorCount > 0 {
			a.embedCounter.Add(ctx, int64(errorCount), metric.WithAttributes(
				attribute.String("embedding.result", "error"),
			))
		}
	}

	span.SetAttributes(
		attribute.Int("embedding.total", total),
		attribute.Int("embedding.embedded", embeddedCount),
		attribute.Int("embedding.skipped", skippedCount),
		attribute.Int("embedding.errors", errorCount),
	)
	span.SetStatus(codes.Ok, "")

	catalogID, hasCatalog := analyzer.CatalogIDFromContext(ctx)

	seen := make(map[string]bool)
	var order []string
	var batch []vectordb.Embedding
	for _, r := range taskResults {
		if r.Error != nil {
			continue
		}
		rest, ok := intanalyzer.StripTaskIDPrefix(r.TaskID, analyzerName)
		if !ok {
			continue
		}
		controlID := rest
		var model string
		for _, m := range a.cfg.Models {
			if trimmed, found := strings.CutSuffix(rest, "-"+m); found {
				controlID = trimmed
				model = m
				break
			}
		}
		if !seen[controlID] {
			seen[controlID] = true
			order = append(order, controlID)
		}

		// Section auto-skip results have no model suffix and no vector
		// payload (r.Result is *pb.AnalysisResult, not *structpb.Struct) —
		// model == "" for them, so this branch naturally excludes them.
		if model == "" || a.vectors == nil {
			continue
		}
		payload, ok := r.Result.(*structpb.Struct)
		if !ok {
			continue
		}
		fields := payload.GetFields()
		embeddingsList := fields["embeddings"].GetListValue().GetValues()
		if len(embeddingsList) == 0 {
			continue
		}
		// GenerateWork sends one text per task, so resp.Data has exactly one row.
		row := embeddingsList[0].GetListValue().GetValues()
		vector := make([]float32, len(row))
		for i, v := range row {
			vector[i] = float32(v.GetNumberValue())
		}
		batch = append(batch, vectordb.Embedding{
			CatalogID: catalogID,
			ControlID: controlID,
			Model:     model,
			Vector:    vector,
		})
	}

	if a.vectors != nil && len(batch) > 0 {
		if !hasCatalog || catalogID == "" {
			return nil, fmt.Errorf("embedding.Aggregate: catalog ID required in context to persist embeddings")
		}
		tenantID, err := tenant.FromContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("embedding.Aggregate: %w", err)
		}
		if err := a.vectors.StoreBatch(ctx, tenantID, batch); err != nil {
			return nil, fmt.Errorf("embedding.Aggregate: storing embeddings: %w", err)
		}
	}

	sort.Strings(order)
	embedResults := make([]results.EmbedResult, len(order))
	for i, controlID := range order {
		embedResults[i] = results.EmbedResult{ControlID: controlID}
	}

	resultData, err := json.Marshal(embedResults)
	if err != nil {
		return nil, fmt.Errorf("embedding.Aggregate: marshaling result data: %w", err)
	}

	return &analyzer.Output{
		AnalyzerName: analyzerName,
		Data:         nil,
		Metadata:     metadata,
		ResultData:   resultData,
	}, nil
}
