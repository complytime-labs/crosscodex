package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// materialize dispatches to the appropriate handler based on analyzer name.
func (s *Service) materialize(ctx context.Context, tenantID, analyzer, jobID string, data []byte) error {
	start := time.Now()
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "graph.materialize."+analyzer)
	defer span.End()
	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("analyzer", analyzer),
		attribute.String("job.id", jobID),
	)

	var err error
	switch analyzer {
	case "relationship":
		err = s.materializeRelationship(ctx, tenantID, jobID, data)
	case "requires":
		err = s.materializeRequires(ctx, tenantID, jobID, data)
	case "artifacts":
		err = s.materializeArtifacts(ctx, tenantID, jobID, data)
	case "classify":
		err = s.materializeClassify(ctx, tenantID, jobID, data)
	case "embed":
		err = s.materializeEmbed(ctx, tenantID, jobID, data)
	default:
		s.logger.InfoContext(ctx, "no graph handler for analyzer", "analyzer", analyzer)
		span.SetStatus(codes.Ok, "no handler")
		return nil
	}

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	s.recordMaterialize(ctx, analyzer, start)
	return err
}

// semanticMatchResult represents a single relationship analysis result.
type semanticMatchResult struct {
	SourceID         string            `json:"source_id"`
	TargetID         string            `json:"target_id"`
	RelationshipType string            `json:"relationship_type"`
	Confidence       float64           `json:"confidence"`
	Properties       map[string]string `json:"properties"`
}

func (s *Service) materializeRelationship(ctx context.Context, tenantID, jobID string, data []byte) error {
	var results []semanticMatchResult
	if err := json.Unmarshal(data, &results); err != nil {
		return fmt.Errorf("unmarshal relationship results: %w", err)
	}

	for _, r := range results {
		edge := graphdb.Edge{
			ID:        fmt.Sprintf("%s_%s_%s", jobID, r.SourceID, r.TargetID),
			Label:     "SEMANTIC_MATCH",
			ValidFrom: time.Now().UTC(),
			Properties: map[string]any{
				"relationship_type": r.RelationshipType,
				"job_id":            jobID,
			},
			Confidence: r.Confidence,
		}
		for k, v := range r.Properties {
			edge.Properties[k] = v
		}

		err := s.graph.CreateEdge(ctx, tenantID, r.SourceID, r.TargetID, edge)
		if err != nil && !isNodeExists(err) {
			return fmt.Errorf("create SEMANTIC_MATCH edge %s->%s: %w", r.SourceID, r.TargetID, err)
		}
	}
	return nil
}

// requiresResult represents a single requires analysis result.
type requiresResult struct {
	SourceID   string   `json:"source_id"`
	TargetID   string   `json:"target_id"`
	Confidence float64  `json:"confidence"`
	Unanimous  bool     `json:"unanimous"`
	ValidVotes int      `json:"valid_votes"`
	TotalVotes int      `json:"total_votes"`
	Models     []string `json:"models"`
}

func (s *Service) materializeRequires(ctx context.Context, tenantID, jobID string, data []byte) error {
	var results []requiresResult
	if err := json.Unmarshal(data, &results); err != nil {
		return fmt.Errorf("unmarshal requires results: %w", err)
	}

	for _, r := range results {
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

		if err := s.graph.CreateRequiresEdge(ctx, tenantID, reqEdge); err != nil && !isNodeExists(err) {
			return fmt.Errorf("create REQUIRES edge %s->%s: %w", r.SourceID, r.TargetID, err)
		}
	}
	return nil
}

// artifactResult represents artifact analysis output.
type artifactResult struct {
	ControlID string     `json:"control_id"`
	Artifacts []artifact `json:"artifacts"`
}

type artifact struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	Frequency  string  `json:"frequency"`
	OwnerRole  string  `json:"owner_role"`
	Confidence float64 `json:"confidence"`
}

func (s *Service) materializeArtifacts(ctx context.Context, tenantID, jobID string, data []byte) error {
	var results []artifactResult
	if err := json.Unmarshal(data, &results); err != nil {
		return fmt.Errorf("unmarshal artifact results: %w", err)
	}

	now := time.Now().UTC()
	for _, r := range results {
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
				ID:        fmt.Sprintf("%s_demands_%s", r.ControlID, artID),
				Label:     "DEMANDS",
				ValidFrom: now,
				Properties: map[string]any{
					"job_id": jobID,
				},
			}
			if err := s.graph.CreateEdge(ctx, tenantID, r.ControlID, artID, demandsEdge); err != nil && !isNodeExists(err) {
				return fmt.Errorf("create DEMANDS edge %s->%s: %w", r.ControlID, artID, err)
			}

			// IS_TYPE edge: Artifact -> ArtifactType.
			isTypeEdge := graphdb.Edge{
				ID:        fmt.Sprintf("%s_is_type_%s", artID, a.Type),
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

func (s *Service) recordMaterialize(ctx context.Context, analyzer string, start time.Time) {
	if s.materializeLatency != nil {
		s.materializeLatency.Record(ctx, float64(time.Since(start).Milliseconds()),
			metric.WithAttributes(attribute.String("analyzer", analyzer)))
	}
}
