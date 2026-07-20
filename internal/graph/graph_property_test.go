package graph_test

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

func TestNodeConversionRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		id := rapid.StringMatching(`[a-z][a-z0-9-]{1,10}`).Draw(t, "id")
		label := rapid.SampledFrom([]string{"Control", "Catalog", "Artifact"}).Draw(t, "label")
		now := time.Now().UTC().Truncate(time.Millisecond)

		original := graphdb.Node{
			ID:        id,
			Label:     label,
			ValidFrom: now,
			Properties: map[string]any{
				"key": rapid.String().Draw(t, "prop"),
			},
		}

		tc := &pb.TenantContext{TenantId: "test-tenant"}
		proto := graph.ExportNodeToProto(original, tc)

		if proto.GetNodeId() != original.ID {
			t.Fatalf("ID mismatch: got %s, want %s", proto.GetNodeId(), original.ID)
		}
		if proto.GetLabel() != original.Label {
			t.Fatalf("Label mismatch: got %s, want %s", proto.GetLabel(), original.Label)
		}
		if proto.GetTenantContext().GetTenantId() != tc.TenantId {
			t.Fatalf("Tenant mismatch: got %s, want %s", proto.GetTenantContext().GetTenantId(), tc.TenantId)
		}
		if proto.GetTemporal() == nil {
			t.Fatal("Temporal is nil")
		}
		if !proto.GetTemporal().GetValidFrom().AsTime().Equal(now) {
			t.Fatalf("ValidFrom mismatch: got %v, want %v", proto.GetTemporal().GetValidFrom().AsTime(), now)
		}
	})
}

func TestEdgeConversionRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		id := rapid.StringMatching(`[a-z][a-z0-9-]{1,10}`).Draw(t, "id")
		sourceID := rapid.StringMatching(`[a-z][a-z0-9-]{1,10}`).Draw(t, "source_id")
		targetID := rapid.StringMatching(`[a-z][a-z0-9-]{1,10}`).Draw(t, "target_id")
		label := rapid.SampledFrom([]string{"MAPS_TO", "REQUIRES", "IMPLEMENTS"}).Draw(t, "label")
		now := time.Now().UTC().Truncate(time.Millisecond)

		original := graphdb.EdgeWithEndpoints{
			Edge: graphdb.Edge{
				ID:        id,
				Label:     label,
				ValidFrom: now,
				Properties: map[string]any{
					"weight": "0.95",
				},
			},
			SourceID: sourceID,
			TargetID: targetID,
		}

		tc := &pb.TenantContext{TenantId: "test-tenant"}
		proto := graph.ExportEdgeToProto(original, tc)

		if proto.GetEdgeId() != original.ID {
			t.Fatalf("ID mismatch: got %s, want %s", proto.GetEdgeId(), original.ID)
		}
		if proto.GetLabel() != original.Label {
			t.Fatalf("Label mismatch: got %s, want %s", proto.GetLabel(), original.Label)
		}
		if proto.GetSourceNodeId() != sourceID {
			t.Fatalf("SourceID mismatch: got %s, want %s", proto.GetSourceNodeId(), sourceID)
		}
		if proto.GetTargetNodeId() != targetID {
			t.Fatalf("TargetID mismatch: got %s, want %s", proto.GetTargetNodeId(), targetID)
		}
		if proto.GetTemporal() == nil {
			t.Fatal("Temporal is nil")
		}
		if !proto.GetTemporal().GetValidFrom().AsTime().Equal(now) {
			t.Fatalf("ValidFrom mismatch: got %v, want %v", proto.GetTemporal().GetValidFrom().AsTime(), now)
		}
	})
}

func TestProtoToNodeConversion(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		label := rapid.SampledFrom([]string{"Control", "Catalog", "Artifact"}).Draw(t, "label")
		propValue := rapid.String().Draw(t, "prop_value")

		req := &pb.CreateNodeRequest{
			Label: label,
			Properties: map[string]string{
				"test_key": propValue,
			},
		}

		node := graph.ExportProtoToNode(req)

		if node.Label != label {
			t.Fatalf("Label mismatch: got %s, want %s", node.Label, label)
		}
		if node.Properties["test_key"] != propValue {
			t.Fatalf("Property mismatch: got %v, want %s", node.Properties["test_key"], propValue)
		}
		if node.ValidFrom.IsZero() {
			t.Fatal("ValidFrom not set")
		}
	})
}

func TestProtoToEdgeConversion(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		label := rapid.SampledFrom([]string{"MAPS_TO", "REQUIRES", "IMPLEMENTS"}).Draw(t, "label")
		propValue := rapid.String().Draw(t, "prop_value")

		req := &pb.CreateEdgeRequest{
			Label: label,
			Properties: map[string]string{
				"test_key": propValue,
			},
			RelationshipType: pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT,
		}

		edge := graph.ExportProtoToEdge(req)

		if edge.Label != label {
			t.Fatalf("Label mismatch: got %s, want %s", edge.Label, label)
		}
		if edge.Properties["test_key"] != propValue {
			t.Fatalf("Property mismatch: got %v, want %s", edge.Properties["test_key"], propValue)
		}
		if edge.Properties["relationship_type"] != "RELATIONSHIP_TYPE_EQUIVALENT" {
			t.Fatalf("RelationshipType not stored: got %v", edge.Properties["relationship_type"])
		}
		if edge.ValidFrom.IsZero() {
			t.Fatal("ValidFrom not set")
		}
	})
}

func TestPathToTraverseResponse(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nodeCount := rapid.IntRange(1, 5).Draw(t, "node_count")
		edgeCount := nodeCount - 1
		if edgeCount < 0 {
			edgeCount = 0
		}

		nodes := make([]graphdb.Node, nodeCount)
		edges := make([]graphdb.Edge, edgeCount)

		// Use unique IDs to avoid deduplication
		for i := 0; i < nodeCount; i++ {
			nodes[i] = graphdb.Node{
				ID:        "node-" + rapid.StringN(5, 10, -1).Draw(t, "node_id"),
				Label:     "Control",
				ValidFrom: time.Now().UTC(),
				Properties: map[string]any{
					"index": i,
				},
			}
		}

		for i := 0; i < edgeCount; i++ {
			edges[i] = graphdb.Edge{
				ID:        "edge-" + rapid.StringN(5, 10, -1).Draw(t, "edge_id"),
				Label:     "MAPS_TO",
				ValidFrom: time.Now().UTC(),
				Properties: map[string]any{
					"index": i,
				},
			}
		}

		paths := []graphdb.Path{{Nodes: nodes, Edges: edges}}
		tc := &pb.TenantContext{TenantId: "test-tenant"}

		resp := graph.ExportPathToTraverseResponse(paths, tc)

		if len(resp.Nodes) != nodeCount {
			t.Fatalf("Node count mismatch: got %d, want %d", len(resp.Nodes), nodeCount)
		}
		if len(resp.Edges) != edgeCount {
			t.Fatalf("Edge count mismatch: got %d, want %d", len(resp.Edges), edgeCount)
		}
	})
}

func TestQueryRowsToProto(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		rowCount := rapid.IntRange(0, 10).Draw(t, "row_count")
		rows := make([]graphdb.QueryRow, rowCount)

		for i := 0; i < rowCount; i++ {
			rows[i] = graphdb.QueryRow{
				Values: []graphdb.QueryValue{
					{Type: graphdb.QueryValueScalar, ScalarVal: "test"},
					{Type: graphdb.QueryValueInteger, IntegerVal: int64(i)},
				},
			}
		}

		resp := graph.ExportQueryRowsToProto(rows)

		if resp.RowCount != int32(rowCount) {
			t.Fatalf("RowCount mismatch: got %d, want %d", resp.RowCount, rowCount)
		}
		if len(resp.Rows) != rowCount {
			t.Fatalf("Rows length mismatch: got %d, want %d", len(resp.Rows), rowCount)
		}
		for i, row := range resp.Rows {
			if len(row.Values) != 2 {
				t.Fatalf("Row %d value count mismatch: got %d, want 2", i, len(row.Values))
			}
		}
	})
}

func TestSimilarityResultToProto(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		controlID := rapid.StringMatching(`[a-z][a-z0-9-]{1,10}`).Draw(t, "control_id")
		similarity := rapid.Float32Range(0.0, 1.0).Draw(t, "similarity")

		result := vectordb.SimilarityResult{
			ControlID:  controlID,
			Similarity: similarity,
			Metadata: map[string]any{
				"catalog": "nist-800-53",
			},
		}

		tc := &pb.TenantContext{TenantId: "test-tenant"}
		match := graph.ExportSimilarityResultToProto(result, tc)

		if match.Node.GetNodeId() != controlID {
			t.Fatalf("ControlID mismatch: got %s, want %s", match.Node.GetNodeId(), controlID)
		}
		if match.SimilarityScore != similarity {
			t.Fatalf("Similarity mismatch: got %f, want %f", match.SimilarityScore, similarity)
		}
		if match.Distance != 1.0-similarity {
			t.Fatalf("Distance mismatch: got %f, want %f", match.Distance, 1.0-similarity)
		}
	})
}
