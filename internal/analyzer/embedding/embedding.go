package embedding

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
			TaskID:   fmt.Sprintf("%s-%s", analyzerName, controlID),
			TaskType: analyzerName,
			Payload:  result,
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
// metadata counts (total, embedded, skipped, error). Task-level errors are
// counted in metadata rather than returned as an aggregate error.
func (a *EmbeddingAnalyzer) Aggregate(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "embedding.Aggregate")
	defer span.End()

	embeddedCount, skippedCount, errorCount := intanalyzer.CountResults(results)

	total := len(results)

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

	return &analyzer.Output{
		AnalyzerName: analyzerName,
		Data:         nil,
		Metadata:     metadata,
	}, nil
}
