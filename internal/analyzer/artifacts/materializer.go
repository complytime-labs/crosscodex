package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	artifactLabel     = "Artifact"
	artifactTypeLabel = "ArtifactType"
	demandsLabel      = "DEMANDS"
	isTypeLabel       = "IS_TYPE"
	determinationType = "llm_panel"
	storagePrefix     = "analysis/artifacts/"
)

// GraphMaterializer creates graph nodes and edges from stored control results.
type GraphMaterializer struct {
	graph  graphdb.GraphDB
	store  storage.Provider
	cfg    config.ArtifactsConfig
	tracer trace.Tracer

	nodeCounter metric.Int64Counter
	edgeCounter metric.Int64Counter
}

// NewGraphMaterializer creates a materializer with the given dependencies.
func NewGraphMaterializer(graph graphdb.GraphDB, store storage.Provider, cfg config.ArtifactsConfig, opts ...MaterializerOption) *GraphMaterializer {
	m := &GraphMaterializer{
		graph: graph,
		store: store,
		cfg:   cfg,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// MaterializerOption configures the GraphMaterializer.
type MaterializerOption func(*GraphMaterializer)

// WithMaterializerTelemetry enables tracing and metrics for the materializer.
func WithMaterializerTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) MaterializerOption {
	return func(m *GraphMaterializer) {
		if tp != nil {
			m.tracer = tp.Tracer("crosscodex/internal/analyzer/artifacts")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			m.nodeCounter, _ = meter.Int64Counter(
				"artifacts.nodes.materialized",
				metric.WithDescription("Total artifact graph nodes materialized"),
			)
			m.edgeCounter, _ = meter.Int64Counter(
				"artifacts.edges.materialized",
				metric.WithDescription("Total artifact graph edges materialized"),
			)
		}
	}
}

// Materialize reads all control results for a job from object storage and
// creates corresponding graph nodes and edges.
//
// IMPORTANT: This method must only be called once per jobID. Re-materializing
// the same job will create duplicate Artifact nodes and edges because the
// GraphDB interface uses CREATE semantics, not MERGE. Idempotent re-materialization
// requires future MERGE support in the graphdb interface. ArtifactType nodes are
// handled idempotently via create-and-ignore-duplicate logic in ensureArtifactTypeNodes.
func (m *GraphMaterializer) Materialize(ctx context.Context, tenantID, jobID string) error {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return fmt.Errorf("materializer.Materialize: %w", err)
	}

	ctx, span := m.startSpan(ctx, "materializer.Materialize")
	defer span.End()

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job_id", jobID),
	)

	// Step 1: Create static ArtifactType nodes (check-then-create).
	if err := m.ensureArtifactTypeNodes(ctx, tenantID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	// Step 2: Read and process control results.
	prefix := tenantID + "/" + storagePrefix + jobID + "/"
	objects, err := m.store.List(ctx, prefix)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("materializer.Materialize: listing results: %w", err)
	}

	controlCount := 0
	artifactCount := 0
	for _, obj := range objects {
		reader, err := m.store.Get(ctx, obj.Key)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("materializer.Materialize: reading %s: %w", obj.Key, err)
		}

		data, err := io.ReadAll(reader)
		if closeErr := reader.Close(); closeErr != nil {
			slog.WarnContext(ctx, "materializer.Materialize: closing reader",
				"key", obj.Key, "error", closeErr)
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("materializer.Materialize: reading body %s: %w", obj.Key, err)
		}

		var cr ControlResult
		if err := json.Unmarshal(data, &cr); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("materializer.Materialize: parsing %s: %w", obj.Key, err)
		}

		for _, art := range cr.Artifacts {
			if err := m.materializeArtifact(ctx, tenantID, jobID, cr.ControlID, art); err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				return err
			}
			artifactCount++
		}
		controlCount++
	}

	span.SetAttributes(
		attribute.Int("control.count", controlCount),
		attribute.Int("artifact.count", artifactCount),
	)
	span.SetStatus(codes.Ok, "")
	return nil
}

func (m *GraphMaterializer) ensureArtifactTypeNodes(ctx context.Context, tenantID string) error {
	now := time.Now()
	for _, at := range AllArtifactTypes() {
		id := strings.ToLower(at.String())
		node := graphdb.Node{
			ID:             id,
			Label:          artifactTypeLabel,
			Properties:     map[string]any{"name": at.TitleCase()},
			ValidFrom:      now,
			CreatedBy:      "system",
			CreationMethod: "static",
		}
		err := m.graph.CreateNode(ctx, tenantID, node)
		if err != nil {
			if errors.Is(err, graphdb.ErrNodeExists) {
				continue
			}
			return fmt.Errorf("materializer: creating ArtifactType %s: %w", id, err)
		}
		if m.nodeCounter != nil {
			m.nodeCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("node.label", artifactTypeLabel),
			))
		}
	}
	return nil
}

// artifactID generates a deterministic ID from control ID and artifact content.
func artifactID(controlID string, art ConsensusArtifact) string {
	normalized := intanalyzer.NormalizeArtifactName(art.Name)
	input := normalized + ":" + art.Type.String()
	hash := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%s__art_%x", controlID, hash[:4])
}

func (m *GraphMaterializer) materializeArtifact(ctx context.Context, tenantID, jobID, controlID string, art ConsensusArtifact) error {
	now := time.Now()
	artID := artifactID(controlID, art)

	// Create Artifact node.
	props := map[string]any{
		"name":             art.Name,
		"frequency":        art.Frequency,
		"owner_role":       art.OwnerRole,
		"description":      art.Description,
		"confidence":       art.Confidence,
		"dedup_generation": 0,
	}
	for k, v := range art.Properties {
		props["prop_"+k] = v
	}

	node := graphdb.Node{
		ID:             artID,
		Label:          artifactLabel,
		Properties:     props,
		ValidFrom:      now,
		CreatedBy:      jobID,
		CreationMethod: determinationType,
	}
	if err := m.graph.CreateNode(ctx, tenantID, node); err != nil {
		return fmt.Errorf("materializer: creating Artifact %s: %w", artID, err)
	}
	if m.nodeCounter != nil {
		m.nodeCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("node.label", artifactLabel),
		))
	}

	// Create DEMANDS edge: (Control) -> (Artifact).
	demandsEdge := graphdb.Edge{
		Label:             demandsLabel,
		DeterminedBy:      jobID,
		DeterminationType: determinationType,
		Confidence:        art.Confidence,
		ValidFrom:         now,
	}
	if err := m.graph.CreateEdge(ctx, tenantID, controlID, artID, demandsEdge); err != nil {
		return fmt.Errorf("materializer: creating DEMANDS edge %s->%s: %w", controlID, artID, err)
	}

	// Create IS_TYPE edge: (Artifact) -> (ArtifactType).
	typeID := strings.ToLower(art.Type.String())
	isTypeEdge := graphdb.Edge{
		Label:             isTypeLabel,
		DeterminedBy:      jobID,
		DeterminationType: determinationType,
		Confidence:        1.0,
		ValidFrom:         now,
	}
	if err := m.graph.CreateEdge(ctx, tenantID, artID, typeID, isTypeEdge); err != nil {
		return fmt.Errorf("materializer: creating IS_TYPE edge %s->%s: %w", artID, typeID, err)
	}

	if m.edgeCounter != nil {
		m.edgeCounter.Add(ctx, 2, metric.WithAttributes(
			attribute.String("edge.label", "DEMANDS+IS_TYPE"),
		))
	}

	return nil
}

func (m *GraphMaterializer) startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if m.tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return m.tracer.Start(ctx, name)
}
