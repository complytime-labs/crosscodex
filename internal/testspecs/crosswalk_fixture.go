package testspecs

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// Catalog IDs of the crosswalk fixture.
const (
	CrosswalkCatA = "cat-a"
	CrosswalkCatB = "cat-b"
	// CrosswalkCatHub links A to B transitively: see SeedCrosswalkGraph.
	CrosswalkCatHub = "cat-hub"
)

// CrosswalkEpoch is the ValidFrom of every fixture node and edge; job
// completion times are offsets from it.
var CrosswalkEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// AddCrosswalkNode creates a Control node in catalog; identifier "" omits the
// identifier property.
func AddCrosswalkNode(ctx context.Context, g graphdb.GraphDB, tenantID, id, catalog, identifier string) error {
	props := map[string]any{"catalog_id": catalog, "title": "title " + id, "statement": "statement " + id}
	if identifier != "" {
		props["identifier"] = identifier
	}
	return g.CreateNode(ctx, tenantID, graphdb.Node{ID: id, Label: "Control", Properties: props, ValidFrom: CrosswalkEpoch})
}

// LinkCrosswalk stores a SEMANTIC_MATCH edge for job; stage "" omits the stage
// property, as edges written before it existed do.
func LinkCrosswalk(ctx context.Context, g graphdb.GraphDB, tenantID, job, src, tgt, stage string, typ pb.RelationshipType, conf float64) error {
	props := map[string]any{"relationship_type": typ.String()[len("RELATIONSHIP_TYPE_"):], "job_id": job}
	if stage != "" {
		props[results.StageProperty] = stage
	}
	if stage == results.StageSynthesized {
		props["is_viable"] = true
	}
	return g.CreateEdge(ctx, tenantID, src, tgt, graphdb.Edge{
		ID: graphdb.DerivedID("test-edge", job, src, tgt, stage), Label: results.SemanticMatchEdgeLabel,
		Properties: props, ValidFrom: CrosswalkEpoch, Confidence: conf,
	})
}

// CrosswalkJobs returns the fixture's jobs by ID. j1 and j2 run A to B, j3
// runs B to A, "running" stands for an unfinished job the gateway would not
// select, "bi" is bidirectional, "hubA" runs A to the hub catalog and "hubB"
// runs the hub catalog to B.
func CrosswalkJobs() map[string]*pb.CrosswalkJob {
	job := func(id, src, tgt string, bidirectional bool, hours time.Duration) *pb.CrosswalkJob {
		return &pb.CrosswalkJob{JobId: id, SourceCatalogId: src, TargetCatalogId: tgt,
			Bidirectional: bidirectional, CompletedAt: timestamppb.New(CrosswalkEpoch.Add(hours * time.Hour))}
	}
	jobs := []*pb.CrosswalkJob{
		job("j1", CrosswalkCatA, CrosswalkCatB, false, 1),
		job("j2", CrosswalkCatA, CrosswalkCatB, false, 2),
		job("j3", CrosswalkCatB, CrosswalkCatA, false, 3),
		job("running", CrosswalkCatA, CrosswalkCatB, false, 4),
		job("bi", CrosswalkCatA, CrosswalkCatB, true, 5),
		job("hubA", CrosswalkCatA, CrosswalkCatHub, false, 6),
		job("hubB", CrosswalkCatHub, CrosswalkCatB, false, 7),
	}
	byID := make(map[string]*pb.CrosswalkJob, len(jobs))
	for _, j := range jobs {
		byID[j.JobId] = j
	}
	return byID
}

// SeedCrosswalkGraph writes the crosswalk fixture into tenantID's existing
// graph: catalog A has AC-1 (parent of AC-1.a) and AC-2, catalog B has SC-1 and
// SC-2, and synthesized SEMANTIC_MATCH edges for j1, j2, j3 (reversed) and
// "running" (see CrosswalkJobs). Catalog cat-hub has HB-1 and HB-2 and the
// two-hop paths A to hub to B: AC-1.a is a subset of both, and each is a
// subset of SC-1 (derived twice, beside its direct row); AC-1.a is a subset of
// HB-1, which is a superset of SC-2 (type unknown); AC-2 is equivalent to HB-1
// (derived to SC-1 and SC-2). It lets the memdriver and agedriver specs
// assert the same rows from identical input.
func SeedCrosswalkGraph(ctx context.Context, g graphdb.GraphDB, tenantID string) error {
	for _, n := range [][3]string{
		{"a-parent", CrosswalkCatA, "AC-1"}, {"a1", CrosswalkCatA, "AC-1.a"}, {"a2", CrosswalkCatA, "AC-2"},
		{"b1", CrosswalkCatB, "SC-1"}, {"b2", CrosswalkCatB, "SC-2"},
		{"h1", CrosswalkCatHub, "HB-1"}, {"h2", CrosswalkCatHub, "HB-2"},
	} {
		if err := AddCrosswalkNode(ctx, g, tenantID, n[0], n[1], n[2]); err != nil {
			return fmt.Errorf("seed node %s: %w", n[0], err)
		}
	}
	if err := g.CreateEdge(ctx, tenantID, "a-parent", "a1", graphdb.Edge{
		ID: "parent-1", Label: "PARENT_OF", ValidFrom: CrosswalkEpoch}); err != nil {
		return fmt.Errorf("seed parent edge: %w", err)
	}
	syn := results.StageSynthesized
	for _, l := range []struct {
		job, src, tgt string
		typ           pb.RelationshipType
		conf          float64
	}{
		{"j1", "a1", "b1", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, 0.7},
		{"j2", "a1", "b1", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, 0.8},
		{"j3", "b1", "a1", pb.RelationshipType_RELATIONSHIP_TYPE_SUPERSET_OF, 0.9},
		{"j1", "a2", "b2", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT, 0.6},
		{"j3", "b2", "a2", pb.RelationshipType_RELATIONSHIP_TYPE_CONTRIBUTES_TO, 0.5},
		{"running", "a2", "b1", pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL, 0.99},
		{"hubA", "a1", "h1", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, 0.8},
		{"hubA", "a1", "h2", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, 0.6},
		{"hubA", "a2", "h1", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT, 0.9},
		{"hubB", "h1", "b1", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, 0.5},
		{"hubB", "h2", "b1", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, 0.5},
		{"hubB", "h1", "b2", pb.RelationshipType_RELATIONSHIP_TYPE_SUPERSET_OF, 0.5},
	} {
		if err := LinkCrosswalk(ctx, g, tenantID, l.job, l.src, l.tgt, syn, l.typ, l.conf); err != nil {
			return fmt.Errorf("seed edge %s %s->%s: %w", l.job, l.src, l.tgt, err)
		}
	}
	return nil
}
