package embedding

import (
	"context"
	"encoding/json"
	"errors"
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
	opts ...Option,
) *EmbeddingAnalyzer {
	a := &EmbeddingAnalyzer{
		llm:     llm,
		vectors: vectors,
		store:   store,
		cfg:     cfg,
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

// GenerateWork produces one task per control per model. Section controls and
// controls with a blank statement are skipped with a pre-built result.
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

	// Auto-skip sections (non-leaf), withdrawn controls and controls with a blank
	// statement.
	// Blank statements are withdrawn or placeholder controls (e.g. CSF 2.0
	// ID.BE). Without an ancestor their text is empty and the worker rejects
	// the task as invalid_payload (retried 3x); with one the text is just
	// "[ancestor]", which says nothing about the control. Candidate
	// generation and coverage already exclude them, so nothing reads their
	// vectors.
	if input.GetParts()["class"] == oscal.ClassSection || input.GetParts()[oscal.PropStatus] == oscal.PropStatusWithdrawn || strings.TrimSpace(input.GetStatement()) == "" {
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

var _ analyzer.Cacheable = (*EmbeddingAnalyzer)(nil)

// CacheModels implements analyzer.Cacheable: one embedding call per model
// in analysis.embedding.models.
func (a *EmbeddingAnalyzer) CacheModels() []analyzer.ModelUse {
	uses := make([]analyzer.ModelUse, 0, len(a.cfg.Models))
	for _, m := range a.cfg.Models {
		uses = append(uses, analyzer.ModelUse{Alias: m, Kind: analyzer.ModelEmbedding})
	}
	return uses
}

// CacheIdentity implements analyzer.Cacheable from the task's payload; a
// skipped control's pre-built task is never cached.
func (a *EmbeddingAnalyzer) CacheIdentity(task analyzer.Task) (analyzer.CacheRequest, bool) {
	return intanalyzer.PayloadCacheRequest(task, "control_id", "control_id")
}

// CacheableResult implements analyzer.Cacheable: a result is stored only
// when it carries exactly one vector of at least one number, the one
// Aggregate stores for the control.
func (a *EmbeddingAnalyzer) CacheableResult(result *structpb.Struct) error {
	vectors := result.GetFields()["embeddings"].GetListValue().GetValues()
	if len(vectors) != 1 {
		return fmt.Errorf("the result carries %d vectors, not exactly one vector", len(vectors))
	}
	values := vectors[0].GetListValue().GetValues()
	if len(values) == 0 {
		return errors.New("the result's vector has no dimensions")
	}
	for i, v := range values {
		if _, isNumber := v.GetKind().(*structpb.Value_NumberValue); !isNumber {
			return fmt.Errorf("dimension %d of the result's vector is not a number", i)
		}
	}
	return nil
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

	// Each embedding is stored under its control's own catalog: one pass
	// covers both catalogs of a job that maps one onto another. The run's
	// catalog is the fallback only for a run that records no per-control
	// catalogs; when it records them, a control missing from them fails
	// closed rather than being filed under the source catalog.
	runCatalogID, _ := analyzer.CatalogIDFromContext(ctx)
	perControl := analyzer.HasControlCatalogs(ctx)

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
		catalogID, ok := analyzer.ControlCatalogFromContext(ctx, controlID)
		if !ok {
			if perControl {
				return nil, fmt.Errorf("embedding.Aggregate: control %s has no catalog in this analysis run's control catalogs: "+
					"its embedding would be filed under the wrong catalog, so none of this run's embeddings were stored", controlID)
			}
			catalogID = runCatalogID
		}
		batch = append(batch, vectordb.Embedding{
			CatalogID: catalogID,
			ControlID: controlID,
			Model:     model,
			Vector:    vector,
		})
	}

	if a.vectors != nil && len(batch) > 0 {
		for _, e := range batch {
			if e.CatalogID == "" {
				return nil, fmt.Errorf("embedding.Aggregate: catalog ID required in context to persist the embedding of control %s: "+
					"neither the control nor the analysis run names its catalog", e.ControlID)
			}
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
