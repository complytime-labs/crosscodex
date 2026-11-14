package graph

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// materializeFunc writes one analyzer's results for a job into the graph.
type materializeFunc func(ctx context.Context, tenantID, jobID string, data []byte) error

// handlerFor returns the materializer for an analyzer's results, or false
// when the graph has nothing to build from that stage (e.g. the pipeline's own
// graph stage).
func (s *Service) handlerFor(analyzer string) (materializeFunc, bool) {
	switch analyzer {
	case "relationship":
		return s.materializeRelationship, true
	case "requires":
		return s.materializeRequires, true
	case results.CritiqueAnalyzer:
		return s.materializeCritique, true
	case "artifacts":
		return s.materializeArtifacts, true
	case results.SynthesisStage:
		return s.materializeSynthesis, true
	case "classify":
		return s.materializeClassify, true
	case "embedding":
		return s.materializeEmbed, true
	default:
		return nil, false
	}
}

// materialize runs the analyzer's handler under a span and records its duration.
func (s *Service) materialize(ctx context.Context, handle materializeFunc, tenantID, analyzer, jobID string, data []byte) error {
	start := time.Now()
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "graph.materialize."+analyzer)
	defer span.End()
	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("analyzer", analyzer),
		attribute.String("job.id", jobID),
	)

	err := handle(ctx, tenantID, jobID, data)

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	s.recordMaterialize(ctx, analyzer, start)
	return err
}

func (s *Service) materializeRelationship(ctx context.Context, tenantID, jobID string, data []byte) error {
	var matches []results.SemanticMatchResult
	if err := json.Unmarshal(data, &matches); err != nil {
		return fmt.Errorf("unmarshal relationship results: %w", err)
	}
	return s.writeSemanticMatches(ctx, tenantID, jobID, matches, results.Verdicts{})
}

// writeSemanticMatches writes one SEMANTIC_MATCH edge per relationship result
// that verdicts does not veto. An edge whose pair has a critique carries its
// vote counts and note (see critiqueProperties).
func (s *Service) writeSemanticMatches(ctx context.Context, tenantID, jobID string, matches []results.SemanticMatchResult, verdicts results.Verdicts) error {
	for _, r := range matches {
		if verdicts.Vetoed(r.SourceID, r.TargetID) {
			continue
		}
		edge := graphdb.Edge{
			ID:        graphdb.DerivedID(results.SemanticMatchEdgeIDKind, jobID, r.SourceID, r.TargetID),
			Label:     results.SemanticMatchEdgeLabel,
			ValidFrom: time.Now().UTC(),
			Properties: map[string]any{
				"relationship_type":   r.RelationshipType,
				"job_id":              jobID,
				results.StageProperty: results.StageAnalyzer,
			},
			Confidence: r.Confidence,
		}
		for k, v := range r.Properties {
			edge.Properties[k] = v
		}
		if c, ok := verdicts.Result(r.SourceID, r.TargetID); ok {
			critiqueProperties(c, edge.Properties)
		}

		err := s.graph.CreateEdge(ctx, tenantID, r.SourceID, r.TargetID, edge)
		if err != nil && !isNodeExists(err) && !s.skipMissingEndpoint(ctx, tenantID, "relationship", results.SemanticMatchEdgeLabel, r.SourceID, r.TargetID, err) {
			return fmt.Errorf("create SEMANTIC_MATCH edge %s->%s: %w", r.SourceID, r.TargetID, err)
		}
	}
	return nil
}

// critiqueProperties sets c's vote counts and, when it has one, its note on an
// edge's properties.
func critiqueProperties(c results.CritiqueResult, props map[string]any) {
	props[results.CritiqueUpheldProperty] = c.Upheld
	props[results.CritiqueRejectedProperty] = c.Rejected
	if note := c.Note(); note != "" {
		props[results.CritiqueNoteProperty] = note
	}
}

func (s *Service) materializeRequires(ctx context.Context, tenantID, jobID string, data []byte) error {
	var reqResults []results.RequiresResult
	if err := json.Unmarshal(data, &reqResults); err != nil {
		return fmt.Errorf("unmarshal requires results: %w", err)
	}
	return s.writeRequires(ctx, tenantID, jobID, reqResults, results.Verdicts{})
}

// writeRequires writes one REQUIRES edge per requires result that verdicts
// does not veto. An edge whose pair has a critique carries its vote counts and
// note.
func (s *Service) writeRequires(ctx context.Context, tenantID, jobID string, reqResults []results.RequiresResult, verdicts results.Verdicts) error {
	for _, r := range reqResults {
		if verdicts.Vetoed(r.SourceID, r.TargetID) {
			continue
		}
		reqEdge := graphdb.RequiresEdge{
			SourceID:   r.SourceID,
			TargetID:   r.TargetID,
			Confidence: r.Confidence,
			Unanimous:  r.Unanimous,
			ValidVotes: r.ValidVotes,
			TotalVotes: r.TotalVotes,
			Models:     r.Models,
			AnalyzedAt: time.Now().UTC(),
			TenantID:   tenantID,
			JobID:      jobID,
		}
		if c, ok := verdicts.Result(r.SourceID, r.TargetID); ok {
			reqEdge.Critique = &graphdb.EdgeCritique{Upheld: c.Upheld, Rejected: c.Rejected, Note: c.Note()}
		}

		err := s.graph.CreateRequiresEdge(ctx, tenantID, reqEdge)
		if err != nil && !isNodeExists(err) && !s.skipMissingEndpoint(ctx, tenantID, "requires", "REQUIRES", r.SourceID, r.TargetID, err) {
			return fmt.Errorf("create REQUIRES edge %s->%s: %w", r.SourceID, r.TargetID, err)
		}
	}
	return nil
}

// materializeCritique writes the SEMANTIC_MATCH and REQUIRES edges of a job
// that ran the critique stage, whose relationship and requires events the
// pipeline deferred to this one: each verdict the critique did not veto, with
// its critique's vote counts and note. The relationship, requires and
// critique verdicts are applied independently, so a pair vetoed as a
// relationship keeps its REQUIRES edge and the reverse. A pair analyzer the
// job did not run has no stored result (sql.ErrNoRows) and writes nothing.
// Every result is read and decoded before the first write, so an unreadable
// one writes no edge. Writes are idempotent, so a redelivered event adds
// nothing.
func (s *Service) materializeCritique(ctx context.Context, tenantID, jobID string, data []byte) error {
	var crit []results.CritiqueResult
	if err := json.Unmarshal(data, &crit); err != nil {
		return fmt.Errorf("unmarshal critique results: %w", err)
	}
	var matches []results.SemanticMatchResult
	if err := s.resolveStored(ctx, jobID, results.RelationshipAnalyzer, &matches); err != nil {
		return err
	}
	var reqResults []results.RequiresResult
	if err := s.resolveStored(ctx, jobID, results.RequiresAnalyzer, &reqResults); err != nil {
		return err
	}
	if err := s.writeSemanticMatches(ctx, tenantID, jobID, matches, results.Apply(results.RelationshipAnalyzer, crit)); err != nil {
		return err
	}
	return s.writeRequires(ctx, tenantID, jobID, reqResults, results.Apply(results.RequiresAnalyzer, crit))
}

// resolveStored decodes the stored result of analyzer for jobID into v. A job
// without that analyzer's result leaves v unchanged.
func (s *Service) resolveStored(ctx context.Context, jobID, analyzer string, v any) error {
	data, err := s.resolvers.Resolve(ctx, ResourceRef{
		Type: "analysis_result",
		ID:   jobID,
		URI:  fmt.Sprintf("pg://results/%s/%s", jobID, analyzer),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve %s results: %w", analyzer, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal %s results: %w", analyzer, err)
	}
	return nil
}

func (s *Service) materializeArtifacts(ctx context.Context, tenantID, jobID string, data []byte) error {
	var artifactResults []results.ArtifactResult
	if err := json.Unmarshal(data, &artifactResults); err != nil {
		return fmt.Errorf("unmarshal artifact results: %w", err)
	}

	now := time.Now().UTC()
	for _, r := range artifactResults {
		for i, a := range r.Artifacts {
			artID := fmt.Sprintf("%s__art_%d", r.ControlID, i)

			// Create Artifact node.
			artNode := graphdb.Node{
				ID:        artID,
				Label:     "Artifact",
				ValidFrom: now,
				Properties: map[string]any{
					"name":       a.Name,
					"type":       a.Type,
					"frequency":  a.Frequency,
					"owner_role": a.OwnerRole,
					"confidence": a.Confidence,
					"job_id":     jobID,
				},
			}
			if err := s.graph.CreateNode(ctx, tenantID, artNode); err != nil && !isNodeExists(err) {
				return fmt.Errorf("create Artifact node %s: %w", artID, err)
			}

			// Create ArtifactType node (idempotent).
			typeNode := graphdb.Node{
				ID:        "type_" + a.Type,
				Label:     "ArtifactType",
				ValidFrom: now,
				Properties: map[string]any{
					"name": a.Type,
				},
			}
			if err := s.graph.CreateNode(ctx, tenantID, typeNode); err != nil && !isNodeExists(err) {
				return fmt.Errorf("create ArtifactType node %s: %w", a.Type, err)
			}

			// DEMANDS edge: Requirement -> Artifact.
			demandsEdge := graphdb.Edge{
				ID:        graphdb.DerivedID("demands", r.ControlID, artID),
				Label:     "DEMANDS",
				ValidFrom: now,
				Properties: map[string]any{
					"job_id": jobID,
				},
			}
			err := s.graph.CreateEdge(ctx, tenantID, r.ControlID, artID, demandsEdge)
			if err != nil && !isNodeExists(err) && !s.skipMissingEndpoint(ctx, tenantID, "artifacts", "DEMANDS", r.ControlID, artID, err) {
				return fmt.Errorf("create DEMANDS edge %s->%s: %w", r.ControlID, artID, err)
			}

			// IS_TYPE edge: Artifact -> ArtifactType.
			isTypeEdge := graphdb.Edge{
				ID:        graphdb.DerivedID("is-type", artID, a.Type),
				Label:     "IS_TYPE",
				ValidFrom: now,
				Properties: map[string]any{
					"job_id": jobID,
				},
			}
			if err := s.graph.CreateEdge(ctx, tenantID, artID, "type_"+a.Type, isTypeEdge); err != nil && !isNodeExists(err) {
				return fmt.Errorf("create IS_TYPE edge %s->%s: %w", artID, a.Type, err)
			}
		}
	}
	return nil
}

func (s *Service) materializeClassify(ctx context.Context, tenantID, jobID string, data []byte) error {
	// Classification updates node properties — handled through property updates
	// on existing Requirement nodes. Requires GetNode + property update, which
	// maps to SupersedeFact (old version) + CreateNode (new version) or a
	// future UpdateNodeProperties method on GraphDB.
	//
	// For now, log and skip — classification data is stored in the relational
	// layer and used by the synthesis service. Graph materialization of
	// classification properties is deferred until UpdateNodeProperties is added
	// to the GraphDB interface.
	s.logger.InfoContext(ctx, "classify materialization deferred", "job_id", jobID)
	return nil
}

func (s *Service) materializeEmbed(ctx context.Context, tenantID, jobID string, data []byte) error {
	// Embedding vectors are stored in pkg/vectordb, not in the graph.
	// The embedding analyzer already stores vectors via the worker.
	// No graph materialization needed — the SimilaritySearch RPC queries
	// vectordb directly.
	s.logger.InfoContext(ctx, "embed materialization skipped (stored in vectordb)", "job_id", jobID)
	return nil
}

func isNodeExists(err error) bool {
	return errors.Is(err, graphdb.ErrNodeExists) || errors.Is(err, graphdb.ErrEdgeExists)
}

// skipMissingEndpoint reports whether err means an edge endpoint node does
// not exist, in which case the caller skips that edge and continues. The
// subscription is core NATS, so failing the event would not redeliver it: it
// would only drop every later edge in the same event. The skip is recorded
// as a metric, a span event and a warning, because the edge stays absent
// until the tenant's graph is rebuilt.
func (s *Service) skipMissingEndpoint(ctx context.Context, tenantID, analyzer, label, sourceID, targetID string, err error) bool {
	if !errors.Is(err, graphdb.ErrNodeNotFound) {
		return false
	}
	if s.edgesSkipped != nil {
		s.edgesSkipped.Add(ctx, 1, metric.WithAttributes(
			attribute.String("analyzer", analyzer),
			attribute.String("edge.label", label),
		))
	}
	trace.SpanFromContext(ctx).AddEvent("edge skipped: endpoint node missing", trace.WithAttributes(
		attribute.String("edge.label", label),
		attribute.String("edge.source_id", sourceID),
		attribute.String("edge.target_id", targetID),
		attribute.String("error.message", err.Error()),
	))
	s.logger.WarnContext(ctx, "skipped graph edge: an endpoint node does not exist in the tenant graph, "+
		"usually because the graph role has not projected that control from the catalog parsed event (check the "+
		"graph role's catalog projection logs) or the analyzer "+
		"emitted an ID that matches no Control node. The edge stays absent until it is rewritten: after fixing the cause, "+
		"re-import the tenant's catalog, which rewrites the controls and republishes the catalog parsed event, "+
		"then start a new analysis of it with \"crosscodex run start --source <catalog-id> --target <catalog-id>\".",
		"tenant", tenantID, "analyzer", analyzer, "edge.label", label,
		"source_id", sourceID, "target_id", targetID, "error", err)
	return true
}

func (s *Service) recordMaterialize(ctx context.Context, analyzer string, start time.Time) {
	if s.materializeLatency != nil {
		s.materializeLatency.Record(ctx, float64(time.Since(start).Milliseconds()),
			metric.WithAttributes(attribute.String("analyzer", analyzer)))
	}
}
