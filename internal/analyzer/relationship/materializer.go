package relationship

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	edgeLabel         = "SEMANTIC_MATCH"
	determinationType = "llm_panel"
	storagePrefix     = "analysis/relationship/"
)

// GraphMaterializer creates graph edges from stored pair results, so a job's
// SEMANTIC_MATCH edges can be rebuilt from object storage via Materialize().
// This follows the Graph Backend Portability principle: no package outside
// pkg/graphdb imports AGE-specific types.
//
// Nothing in production calls it yet. The live SEMANTIC_MATCH writer is
// internal/graph (materializeRelationship in subscriber_handlers.go), which
// consumes relationship result events from NATS and writes a different
// payload. The two use different edge ID kinds, so wiring this materializer
// into a graph that internal/graph also writes adds separate edges instead of
// colliding with them (see "Graph writers" in docs/dev/design-principles.md).
type GraphMaterializer struct {
	graph  graphdb.GraphDB
	store  storage.Provider
	cfg    config.RelationshipConfig
	tracer trace.Tracer

	// Metrics (optional, nil-safe)
	edgeCounter metric.Int64Counter
}

// NewGraphMaterializer creates a materializer with the given dependencies.
func NewGraphMaterializer(graph graphdb.GraphDB, store storage.Provider, cfg config.RelationshipConfig, opts ...MaterializerOption) *GraphMaterializer {
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
			m.tracer = tp.Tracer("crosscodex/internal/analyzer/relationship")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			m.edgeCounter, _ = meter.Int64Counter(
				"relationship.edges.materialized",
				metric.WithDescription("Total graph edges materialized from pair results"),
			)
		}
	}
}

// Materialize reads all pair results for a job from object storage and
// creates corresponding graph edges. This is an on-demand rebuild that
// can reconstruct the graph from the source-of-truth storage.
//
// Pairs are written one CreateEdge call at a time, so Materialize is not
// atomic: an error returns immediately and leaves the edges already written
// in place. Re-running the same job is safe and completes the rebuild,
// because each SEMANTIC_MATCH edge ID is derived from the job and the pair:
// graphdb.ErrEdgeExists means the edge is already there, so it counts as
// success (it is not returned, and not counted as a newly created edge).
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

	// Storage paths are tenant-scoped per project convention: <tenant>/<path>.
	prefix := tenantID + "/" + storagePrefix + jobID + "/"
	objects, err := m.store.List(ctx, prefix)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("materializer.Materialize: listing pair results: %w", err)
	}

	pairCount := 0
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

		var pair PairResult
		if err := json.Unmarshal(data, &pair); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("materializer.Materialize: parsing %s: %w", obj.Key, err)
		}

		// The ID is derived from the job and the pair, so re-running a job is
		// rejected as ErrEdgeExists instead of duplicating the edge. The kind
		// differs from internal/graph's "semantic-match", whose edges carry a
		// different payload, so neither writer silently keeps the other's edge.
		edge := graphdb.Edge{
			ID:                graphdb.DerivedID("semantic-match-rebuild", jobID, pair.SourceControlID, pair.TargetControlID),
			Label:             edgeLabel,
			DeterminedBy:      jobID,
			DeterminationType: determinationType,
			Confidence:        pair.Consensus.ConfidenceFraction,
			ValidFrom:         time.Now(),
			Properties: map[string]any{
				"relationship_type": pair.Consensus.Relationship.String(),
				"contribution_type": pair.Consensus.ContributionType.String(),
				"unanimous":         pair.Consensus.Unanimous,
				"valid_vote_count":  pair.Consensus.ValidVoteCount,
				"similarity_score":  float64(pair.SimilarityScore),
			},
		}

		err = m.graph.CreateEdge(ctx, tenantID, pair.SourceControlID, pair.TargetControlID, edge)
		switch {
		case errors.Is(err, graphdb.ErrEdgeExists):
			// An edge with this derived ID already exists.
		case err != nil:
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("materializer.Materialize: creating edge %s--%s: %w",
				pair.SourceControlID, pair.TargetControlID, err)
		case m.edgeCounter != nil:
			m.edgeCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("relationship_type", pair.Consensus.Relationship.String()),
			))
		}
		pairCount++
	}

	span.SetAttributes(attribute.Int("pair.count", pairCount))
	span.SetStatus(codes.Ok, "")
	return nil
}

// AgeOff is intentionally absent. Temporal edge pruning will be added
// when the graph backend exposes edge deletion by temporal predicate.
// See: Graph Backend Portability in docs/dev/design-principles.md.

// startSpan creates a tracing span if a tracer is configured, otherwise
// returns the context and a no-op span.
func (m *GraphMaterializer) startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if m.tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return m.tracer.Start(ctx, name)
}
