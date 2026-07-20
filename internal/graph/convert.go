package graph

import (
	"fmt"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// nodeToProto converts a graphdb.Node to a proto Node.
func nodeToProto(n graphdb.Node, tc *pb.TenantContext) *pb.Node {
	pn := &pb.Node{
		NodeId:        n.ID,
		TenantContext: tc,
		Label:         n.Label,
		Properties:    stringifyProps(n.Properties),
	}
	if !n.ValidFrom.IsZero() {
		pn.Temporal = &pb.TemporalAttributes{
			ValidFrom: timestamppb.New(n.ValidFrom),
		}
		if n.ValidTo != nil {
			pn.Temporal.ValidTo = timestamppb.New(*n.ValidTo)
		}
	}
	if n.CreatedBy != "" {
		pn.Audit = &pb.AuditMetadata{CreatedBy: n.CreatedBy}
	}
	return pn
}

// edgeToProto converts a graphdb.EdgeWithEndpoints to a proto Edge.
func edgeToProto(e graphdb.EdgeWithEndpoints, tc *pb.TenantContext) *pb.Edge {
	pe := &pb.Edge{
		EdgeId:        e.ID,
		TenantContext: tc,
		SourceNodeId:  e.SourceID,
		TargetNodeId:  e.TargetID,
		Label:         e.Label,
		Properties:    stringifyProps(e.Properties),
	}
	if !e.ValidFrom.IsZero() {
		pe.Temporal = &pb.TemporalAttributes{
			ValidFrom: timestamppb.New(e.ValidFrom),
		}
		if e.ValidTo != nil {
			pe.Temporal.ValidTo = timestamppb.New(*e.ValidTo)
		}
	}
	return pe
}

// protoToNode converts a CreateNodeRequest to a graphdb.Node.
func protoToNode(pn *pb.CreateNodeRequest) graphdb.Node {
	n := graphdb.Node{
		Label:      pn.GetLabel(),
		Properties: anyProps(pn.GetProperties()),
	}
	if pn.GetTemporal() != nil {
		if pn.GetTemporal().GetValidFrom() != nil {
			n.ValidFrom = pn.GetTemporal().GetValidFrom().AsTime()
		}
		if pn.GetTemporal().GetValidTo() != nil {
			vt := pn.GetTemporal().GetValidTo().AsTime()
			n.ValidTo = &vt
		}
	}
	if n.ValidFrom.IsZero() {
		n.ValidFrom = time.Now().UTC()
	}
	return n
}

// protoToEdge converts a CreateEdgeRequest to a graphdb.Edge.
func protoToEdge(pe *pb.CreateEdgeRequest) graphdb.Edge {
	e := graphdb.Edge{
		Label:      pe.GetLabel(),
		Properties: anyProps(pe.GetProperties()),
	}
	if pe.GetTemporal() != nil {
		if pe.GetTemporal().GetValidFrom() != nil {
			e.ValidFrom = pe.GetTemporal().GetValidFrom().AsTime()
		}
		if pe.GetTemporal().GetValidTo() != nil {
			vt := pe.GetTemporal().GetValidTo().AsTime()
			e.ValidTo = &vt
		}
	}
	if e.ValidFrom.IsZero() {
		e.ValidFrom = time.Now().UTC()
	}
	if pe.GetRelationshipType() != pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED {
		e.Properties["relationship_type"] = pe.GetRelationshipType().String()
	}
	return e
}

// pathToTraverseResponse converts a slice of graphdb.Path to a proto TraverseResponse.
func pathToTraverseResponse(paths []graphdb.Path, tc *pb.TenantContext) *pb.TraverseResponse {
	resp := &pb.TraverseResponse{}
	seen := make(map[string]bool)
	for _, p := range paths {
		for _, n := range p.Nodes {
			if !seen["n:"+n.ID] {
				seen["n:"+n.ID] = true
				resp.Nodes = append(resp.Nodes, nodeToProto(n, tc))
			}
		}
		for i, e := range p.Edges {
			if seen["e:"+e.ID] {
				continue
			}
			seen["e:"+e.ID] = true
			sourceID, targetID := "", ""
			if i < len(p.Nodes) {
				sourceID = p.Nodes[i].ID
			}
			if i+1 < len(p.Nodes) {
				targetID = p.Nodes[i+1].ID
			}
			resp.Edges = append(resp.Edges, edgeToProto(graphdb.EdgeWithEndpoints{
				Edge:     e,
				SourceID: sourceID,
				TargetID: targetID,
			}, tc))
		}
	}
	return resp
}

// queryRowsToProto converts a slice of graphdb.QueryRow to a proto QueryResponse.
func queryRowsToProto(rows []graphdb.QueryRow) *pb.QueryResponse {
	resp := &pb.QueryResponse{RowCount: int32(len(rows))}
	for _, row := range rows {
		gr := &pb.GraphRow{}
		for _, v := range row.Values {
			gr.Values = append(gr.Values, queryValueToProto(v))
		}
		resp.Rows = append(resp.Rows, gr)
	}
	return resp
}

// queryValueToProto converts a graphdb.QueryValue to a proto GraphValue.
func queryValueToProto(v graphdb.QueryValue) *pb.GraphValue {
	gv := &pb.GraphValue{}
	switch v.Type {
	case graphdb.QueryValueNode:
		if v.NodeVal != nil {
			gv.Value = &pb.GraphValue_Node{Node: nodeToProto(*v.NodeVal, nil)}
		}
	case graphdb.QueryValueEdge:
		if v.EdgeVal != nil {
			gv.Value = &pb.GraphValue_Edge{Edge: edgeToProto(*v.EdgeVal, nil)}
		}
	case graphdb.QueryValueScalar:
		gv.Value = &pb.GraphValue_Scalar{Scalar: v.ScalarVal}
	case graphdb.QueryValueInteger:
		gv.Value = &pb.GraphValue_Integer{Integer: v.IntegerVal}
	case graphdb.QueryValueFloat:
		gv.Value = &pb.GraphValue_FloatVal{FloatVal: v.FloatVal}
	case graphdb.QueryValueBool:
		gv.Value = &pb.GraphValue_Boolean{Boolean: v.BoolVal}
	}
	return gv
}

// similarityResultToProto converts a vectordb.SimilarityResult to a proto SimilarityMatch.
func similarityResultToProto(r vectordb.SimilarityResult, tc *pb.TenantContext) *pb.SimilarityMatch {
	return &pb.SimilarityMatch{
		Node: &pb.Node{
			NodeId:        r.ControlID,
			TenantContext: tc,
			Properties:    stringifyProps(r.Metadata),
		},
		SimilarityScore: r.Similarity,
		Distance:        1.0 - r.Similarity,
	}
}

// stringifyProps converts map[string]any to map[string]string.
func stringifyProps(m map[string]any) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = stringifyValue(v)
	}
	return out
}

// stringifyValue converts any value to string.
func stringifyValue(v any) string {
	switch val := v.(type) {
	case string:
		return val
	default:
		return fmt.Sprintf("%v", val)
	}
}

// anyProps converts map[string]string to map[string]any.
func anyProps(m map[string]string) map[string]any {
	if m == nil {
		return make(map[string]any)
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
