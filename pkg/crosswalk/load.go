package crosswalk

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// PairQuery selects the SEMANTIC_MATCH verdicts between two catalogs.
type PairQuery struct {
	// A and B are catalog IDs. Edges are read a->b and b->a, once when equal.
	A, B string
	// Jobs maps a job ID to its completion time. Edges of other jobs are ignored.
	Jobs map[string]time.Time
	// Stage is results.StageAnalyzer or results.StageSynthesized.
	Stage string
	// IncludeNonViable keeps synthesized edges with is_viable=false.
	IncludeNonViable bool
	// IncludeNoRelationship keeps NO_RELATIONSHIP edges, which are dropped by default.
	IncludeNoRelationship bool
}

// EdgeVerdict is one edge's verdict with the edge itself and its stored
// direction. Verdict.Reversed is true when that direction is B to A.
type EdgeVerdict struct {
	Verdict
	Edge     graphdb.Edge
	SourceID string // stored edge source node
	TargetID string // stored edge target node
}

// PairEdges holds every verdict on one (A control, B control) pair. A and B
// are in the query's A-to-B reading whatever the stored direction.
type PairEdges struct {
	A, B     graphdb.Node
	Verdicts []EdgeVerdict
	// CritiqueNotes are the distinct critique notes of the pair's edges, in
	// the order the edges were read.
	CritiqueNotes []string
}

// LoadPair reads the verdicts between catalogs q.A and q.B and groups them by
// control pair. It also returns how many edges were skipped because an endpoint
// control is gone (no ID or no published identifier), which happens when a
// catalog was re-imported or emptied after the edge was written. Pairs are
// sorted by (A.ID, B.ID) and each pair's verdicts by (JobID, SourceID), so the
// result does not depend on graph read order.
func LoadPair(ctx context.Context, g graphdb.GraphDB, tenant string, q PairQuery) (pairs []PairEdges, skipped int64, err error) {
	directions := []struct{ src, tgt string }{{q.A, q.B}}
	if q.A != q.B {
		directions = append(directions, struct{ src, tgt string }{q.B, q.A})
	}

	type pairKey struct{ a, b string }
	byPair := map[pairKey]*PairEdges{}
	for _, d := range directions {
		rq := graphdb.RelationshipQuery{
			EdgeLabel:        results.SemanticMatchEdgeLabel,
			SourceProperties: map[string]any{"catalog_id": d.src},
			TargetProperties: map[string]any{"catalog_id": d.tgt},
		}
		// Edges written before the stage property existed are analyzer edges,
		// which an equality filter on the property cannot match.
		if q.Stage == results.StageSynthesized {
			rq.Properties = map[string]any{results.StageProperty: q.Stage}
		}
		rels, err := g.QueryRelationships(ctx, tenant, rq)
		if err != nil {
			return nil, 0, fmt.Errorf("query %s edges from %s to %s: %w", results.SemanticMatchEdgeLabel, d.src, d.tgt, err)
		}
		reversed := d.src != q.A
		for _, rel := range rels {
			completedAt, listed := q.Jobs[propString(rel.Edge.Properties, "job_id")]
			if !listed || EdgeStage(rel.Edge) != q.Stage {
				continue
			}
			// Synthesis rejects some pairs but still stores them; like
			// results export, serve only the viable ones unless asked.
			if q.Stage == results.StageSynthesized && !q.IncludeNonViable && !propBool(rel.Edge.Properties, "is_viable") {
				continue
			}
			typ := ParseRelationshipType(propString(rel.Edge.Properties, "relationship_type"))
			if typ == pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP && !q.IncludeNoRelationship {
				continue
			}
			aNode, bNode := rel.Source, rel.Target
			if reversed {
				aNode, bNode = bNode, aNode
			}
			if !readableControl(aNode) || !readableControl(bNode) {
				skipped++
				continue
			}
			key := pairKey{aNode.ID, bNode.ID}
			pe := byPair[key]
			if pe == nil {
				pe = &PairEdges{A: aNode, B: bNode}
				byPair[key] = pe
			}
			pe.Verdicts = append(pe.Verdicts, EdgeVerdict{
				Verdict: Verdict{
					JobID:       propString(rel.Edge.Properties, "job_id"),
					CompletedAt: completedAt,
					Type:        typ,
					Confidence:  rel.Edge.Confidence,
					Reversed:    reversed,
				},
				Edge:     rel.Edge,
				SourceID: rel.Source.ID,
				TargetID: rel.Target.ID,
			})
			if note := propString(rel.Edge.Properties, results.CritiqueNoteProperty); note != "" && !slices.Contains(pe.CritiqueNotes, note) {
				pe.CritiqueNotes = append(pe.CritiqueNotes, note)
			}
		}
	}

	pairs = make([]PairEdges, 0, len(byPair))
	for _, pe := range byPair {
		slices.SortStableFunc(pe.Verdicts, func(x, y EdgeVerdict) int {
			return cmp.Or(cmp.Compare(x.JobID, y.JobID), cmp.Compare(x.SourceID, y.SourceID))
		})
		pairs = append(pairs, *pe)
	}
	slices.SortFunc(pairs, func(x, y PairEdges) int {
		return cmp.Or(cmp.Compare(x.A.ID, y.A.ID), cmp.Compare(x.B.ID, y.B.ID))
	})
	return pairs, skipped, nil
}

// ParseRelationshipType reads a stored relationship type, either the bare name
// ("SUBSET_OF") or the proto enum name. An unknown name yields UNSPECIFIED, so
// the row is reported rather than dropped.
func ParseRelationshipType(s string) pb.RelationshipType {
	if v, ok := pb.RelationshipType_value["RELATIONSHIP_TYPE_"+s]; ok {
		return pb.RelationshipType(v)
	}
	if v, ok := pb.RelationshipType_value[s]; ok {
		return pb.RelationshipType(v)
	}
	return pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED
}

// EdgeStage reads an edge's stage; an edge without the property is an
// analyzer edge.
func EdgeStage(e graphdb.Edge) string {
	if st := propString(e.Properties, results.StageProperty); st != "" {
		return st
	}
	return results.StageAnalyzer
}

// readableControl reports whether a node carries what a crosswalk row needs: an
// ID and the control's published identifier.
func readableControl(n graphdb.Node) bool {
	return n.ID != "" && propString(n.Properties, "identifier") != ""
}

func propBool(props map[string]any, key string) bool {
	b, _ := props[key].(bool)
	return b
}

func propString(props map[string]any, key string) string {
	s, _ := props[key].(string)
	return s
}

// EquivalenceEdge is one EQUIVALENT verdict reduced to the nodes and catalogs
// that decide whether it is one of several.
type EquivalenceEdge struct{ SourceNode, SourceCatalog, TargetNode, TargetCatalog string }

// EquivalenceGroup is a node judged equivalent to two or more nodes of one
// other catalog. With NodeIsSource the node is the edges' source and
// OtherCatalog the targets' catalog; otherwise the node is the target.
type EquivalenceGroup struct {
	Node         string
	NodeIsSource bool
	OtherCatalog string
	Members      []int // indices into the input edges, ascending, len >= 2
}

// EquivalenceGroups finds the one-to-many groups among edges. Groups are
// ordered source groups first, then by node and other catalog.
func EquivalenceGroups(edges []EquivalenceEdge) []EquivalenceGroup {
	type key struct {
		node, otherCatalog string
		isSource           bool
	}
	members := map[key][]int{}
	for i, e := range edges {
		sk := key{e.SourceNode, e.TargetCatalog, true}
		members[sk] = append(members[sk], i)
		tk := key{e.TargetNode, e.SourceCatalog, false}
		members[tk] = append(members[tk], i)
	}
	var groups []EquivalenceGroup
	for k, idx := range members {
		if len(idx) >= 2 {
			groups = append(groups, EquivalenceGroup{Node: k.node, NodeIsSource: k.isSource, OtherCatalog: k.otherCatalog, Members: idx})
		}
	}
	slices.SortFunc(groups, func(x, y EquivalenceGroup) int {
		srcFirst := 0
		if x.NodeIsSource != y.NodeIsSource {
			srcFirst = 1
			if x.NodeIsSource {
				srcFirst = -1
			}
		}
		return cmp.Or(srcFirst, cmp.Compare(x.Node, y.Node), cmp.Compare(x.OtherCatalog, y.OtherCatalog))
	})
	return groups
}
