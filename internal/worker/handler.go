package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	headerTaskID     = "X-Task-Id"
	headerTaskType   = "X-Task-Type"
	headerJobID      = "X-Job-Id"
	headerRetryCount = "X-Retry-Count"
	headerError      = "X-Error"
)

func (w *Worker) handleMessage(ctx context.Context, msg *natsbus.Message) error {
	start := time.Now()

	taskID := getHeader(msg.Headers, headerTaskID)
	taskType := getHeader(msg.Headers, headerTaskType)
	jobID := getHeader(msg.Headers, headerJobID)
	retryCount, _ := strconv.Atoi(getHeader(msg.Headers, headerRetryCount))
	tenantID := msg.Metadata.TenantID

	// Use a sentinel for metrics emitted before taskType is validated — this
	// keeps error counters routable in dashboards even on malformed messages.
	metricTaskType := taskType
	if metricTaskType == "" {
		metricTaskType = "unknown"
	}

	if tenantID == "" {
		w.logger.Error("message missing tenant ID: tenant ID must be a 3-64 character lowercase alphanumeric string with hyphens")
		w.recordError(ctx, metricTaskType, "missing_tenant")
		return nil
	}

	ctx, err := tenant.WithTenant(ctx, tenantID)
	if err != nil {
		w.logger.Error("invalid tenant ID: value failed validation (must be 3-64 char lowercase alphanumeric with hyphens)",
			"error", err,
		)
		w.recordError(ctx, metricTaskType, "invalid_tenant")
		return nil
	}

	ctx, span := telemetry.StartSpan(w.tracer, ctx, "worker.ExecuteTask")
	defer span.End()

	span.SetAttributes(
		attribute.String("task.id", taskID),
		attribute.String("task.type", taskType),
		attribute.String("job.id", jobID),
		attribute.String("tenant.id", tenantID),
		attribute.Int("retry.count", retryCount),
	)

	if taskID == "" || taskType == "" {
		w.recordError(ctx, taskType, "invalid_message")
		span.RecordError(ErrInvalidMessage)
		span.SetStatus(codes.Error, "missing required headers")
		if jobID != "" && tenantID != "" {
			w.publishErrorResult(ctx, tenantID, natsbus.TaskType(taskType), jobID, taskID, "invalid_message")
		}
		return nil
	}

	w.logger.Debug("processing task",
		"task_id", taskID,
		"task_type", taskType,
		"job_id", jobID,
		"tenant_id", tenantID,
	)

	payload := &structpb.Struct{}
	if unmarshalErr := proto.Unmarshal(msg.Data, payload); unmarshalErr != nil {
		w.logger.Error("failed to unmarshal payload",
			slog.String("task_id", taskID),
			slog.String("error", unmarshalErr.Error()),
		)
		w.handleTaskError(ctx, span, tenantID, natsbus.TaskType(taskType), jobID, taskID,
			ErrInvalidPayload, "invalid_payload")
		return nil
	}

	var resultPayload *structpb.Struct
	var taskErr error

	switch natsbus.TaskType(taskType) {
	case natsbus.TaskClassify, natsbus.TaskRelate, natsbus.TaskRequires, natsbus.TaskArtifacts:
		resultPayload, taskErr = w.handleCompletion(ctx, payload, tenantID, jobID)
	case natsbus.TaskEmbed:
		resultPayload, taskErr = w.handleEmbedding(ctx, payload, tenantID, jobID)
	default:
		// Rune-safe truncation to avoid splitting multi-byte UTF-8 sequences.
		runes := []rune(taskType)
		truncated := taskType
		if len(runes) > 64 {
			truncated = string(runes[:64])
		}
		w.handleTaskError(ctx, span, tenantID, natsbus.TaskType(taskType), jobID, taskID,
			fmt.Errorf("%w: %s", ErrUnsupportedTaskType, truncated), "unsupported_task_type")
		return nil
	}

	if taskErr != nil {
		w.handleTaskError(ctx, span, tenantID, natsbus.TaskType(taskType), jobID, taskID, taskErr, errorCategory(taskErr))
		return nil
	}

	// Record telemetry before publishing the result. The task processing
	// is complete at this point; publishing is delivery, not work. Ending
	// the span before publish ensures consumers that observe the result
	// also observe a completed span and recorded metrics (the deferred
	// span.End above becomes a safe no-op).
	duration := float64(time.Since(start).Milliseconds())
	span.SetStatus(codes.Ok, "")
	w.recordSuccess(ctx, taskType, duration)
	span.End()

	if err := w.publishResult(ctx, tenantID, natsbus.TaskType(taskType), jobID, taskID, resultPayload); err != nil {
		w.logger.Error("failed to publish result",
			"task_id", taskID,
			"error", err,
		)
		w.recordError(ctx, taskType, "publish_error")
		return nil
	}

	return nil
}

func (w *Worker) handleCompletion(ctx context.Context, payload *structpb.Struct, tenantID, jobID string) (*structpb.Struct, error) {
	req, err := extractCompletionRequest(payload, tenantID, jobID)
	if err != nil {
		return nil, err
	}

	tenantCfg := w.cfg.LLM.ForTenant(tenantID)
	if req.Model == "" {
		req.Model = tenantCfg.DefaultModel
	}

	llmStart := time.Now()
	resp, err := w.llm.Complete(ctx, req)
	llmDuration := float64(time.Since(llmStart).Milliseconds())
	if w.llmLatency != nil {
		w.llmLatency.Record(ctx, llmDuration, metric.WithAttributes(
			attribute.String("task_type", "completion"),
			attribute.String("model", req.Model),
		))
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLLMCall, err)
	}

	return buildCompletionResult(resp)
}

func (w *Worker) handleEmbedding(ctx context.Context, payload *structpb.Struct, tenantID, jobID string) (*structpb.Struct, error) {
	req, err := extractEmbeddingRequest(payload, tenantID, jobID)
	if err != nil {
		return nil, err
	}

	tenantCfg := w.cfg.LLM.ForTenant(tenantID)
	if req.Model == "" {
		req.Model = tenantCfg.EmbeddingModel
	}

	llmStart := time.Now()
	resp, err := w.llm.Embed(ctx, req)
	llmDuration := float64(time.Since(llmStart).Milliseconds())
	if w.llmLatency != nil {
		w.llmLatency.Record(ctx, llmDuration, metric.WithAttributes(
			attribute.String("task_type", "embedding"),
			attribute.String("model", req.Model),
		))
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLLMCall, err)
	}

	return buildEmbeddingResult(resp)
}

func (w *Worker) publishResult(ctx context.Context, tenantID string, taskType natsbus.TaskType, jobID, taskID string, payload *structpb.Struct) error {
	subject, err := natsbus.ResultSubject(tenantID, taskType, jobID)
	if err != nil {
		return fmt.Errorf("building result subject: %w", err)
	}

	data, err := proto.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling result: %w", err)
	}

	headers := map[string][]string{
		headerTaskID: {taskID},
	}

	return w.bus.PublishWithHeaders(ctx, subject, data, headers)
}

func (w *Worker) publishErrorResult(ctx context.Context, tenantID string, taskType natsbus.TaskType, jobID, taskID, errorMsg string) {
	subject, err := natsbus.ResultSubject(tenantID, taskType, jobID)
	if err != nil {
		w.logger.Error("failed to build error result subject",
			"tenant_id", tenantID,
			"error", err,
		)
		return
	}

	headers := map[string][]string{
		headerTaskID: {taskID},
		headerError:  {errorMsg},
	}

	if err := w.bus.PublishWithHeaders(ctx, subject, nil, headers); err != nil {
		w.logger.Error("failed to publish error result",
			"task_id", taskID,
			"error", err,
		)
	}
}

func (w *Worker) handleTaskError(ctx context.Context, span trace.Span, tenantID string, taskType natsbus.TaskType, jobID, taskID string, err error, category string) {
	// Log only the sanitized category — not err.Error() — to prevent upstream
	// LLM API responses (which may include credentials or prompt fragments)
	// from leaking into structured logs shipped to SIEMs.
	w.logger.Error("task failed",
		slog.String("task_id", taskID),
		slog.String("task_type", string(taskType)),
		slog.String("error_category", category),
	)
	// RecordError captures the full error chain in the span's error event
	// (exported as a structured event, not the status description).
	span.RecordError(err)
	// SetStatus uses the sanitized category so that telemetry backends
	// (Jaeger, OTLP) do not receive raw upstream error messages.
	span.SetStatus(codes.Error, category)
	w.recordError(ctx, string(taskType), category)
	w.publishErrorResult(ctx, tenantID, taskType, jobID, taskID, category)
}

func (w *Worker) recordSuccess(ctx context.Context, taskType string, durationMS float64) {
	if w.taskCounter != nil {
		w.taskCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("task_type", taskType),
			attribute.String("status", "success"),
		))
	}
	if w.taskDuration != nil {
		w.taskDuration.Record(ctx, durationMS, metric.WithAttributes(
			attribute.String("task_type", taskType),
		))
	}
}

func (w *Worker) recordError(ctx context.Context, taskType, category string) {
	if w.errorCounter != nil {
		w.errorCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("task_type", taskType),
			attribute.String("error_category", category),
		))
	}
}

func errorCategory(err error) string {
	switch {
	case errors.Is(err, ErrInvalidPayload):
		return "invalid_payload"
	case errors.Is(err, ErrLLMCall):
		return "llm_error"
	case errors.Is(err, ErrTenantConfig):
		return "tenant_config_error"
	default:
		return "unknown"
	}
}

func getHeader(headers map[string][]string, key string) string {
	vals := headers[key]
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}
