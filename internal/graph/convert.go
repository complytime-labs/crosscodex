package graph

import (
	"errors"
	"fmt"
	"math"
	"strconv"
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
	if !e.ValidFrom.IsZero() || e.ValidTo != nil || e.DeterminedBy != "" || e.Confidence != 0 {
		pe.Temporal = &pb.TemporalAttributes{
			DeterminedBy: e.DeterminedBy,
			Confidence:   float32(e.Confidence),
		}
		if !e.ValidFrom.IsZero() {
			pe.Temporal.ValidFrom = timestamppb.New(e.ValidFrom)
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
		e.DeterminedBy = pe.GetTemporal().GetDeterminedBy()
		// The proto carries confidence as float32. Widening it directly turns
		// 0.95 into 0.949999988..., which fails thresholds such as >= 0.95,
		// so parse its shortest float32 decimal instead. FormatFloat output
		// always parses, so the error is always nil.
		e.Confidence, _ = strconv.ParseFloat(strconv.FormatFloat(float64(pe.GetTemporal().GetConfidence()), 'g', -1, 32), 64)
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

// reservedKeyHints tells an RPC client what to send instead of a reserved
// property key, in proto field names. graphdb.ReservedPropertyError names Go
// fields, which mean nothing to a client. Every key in
// graphdb.ReservedNodeKeys and graphdb.ReservedEdgeKeys needs an entry.
var reservedKeyHints = map[string]string{
	"id":                 "the server generates the ID; rename the property",
	"valid_from":         "set temporal.valid_from instead",
	"valid_to":           "set temporal.valid_to instead",
	"determined_by":      "set temporal.determined_by instead",
	"confidence":         "set temporal.confidence instead",
	"created_by":         "set internally by the pipeline; rename the property",
	"creation_method":    "set internally by the pipeline; rename the property",
	"determination_type": "set internally by the pipeline; rename the property",
	"supersedes":         "set internally by the pipeline; rename the property",
	"superseded_by":      "call SupersedeFact with superseded_by_job_id instead",
}

// graphErrorMessage returns err's message for an RPC client. A reserved
// property key error is rebuilt from its fields in proto field names, keeping
// the index of a *graphdb.BulkEdgeError; nothing of the driver's Go-field
// wording survives. A missing graph or a read-only violation gets a fixed
// message: a driver reports both from a database error whose text (internal
// graph names, SQLSTATE codes) must not reach a client (CWE-209), and the
// client can act on the fixed text alone.
func graphErrorMessage(err error) string {
	switch {
	case errors.Is(err, graphdb.ErrGraphNotFound):
		return graphdb.ErrGraphNotFound.Error() + ": the tenant's graph does not exist; create the tenant before using its graph"
	case errors.Is(err, graphdb.ErrReadOnlyViolation):
		return graphdb.ErrReadOnlyViolation.Error() + ": the query writes to the graph, but Query and TemporalQuery are read-only; write with CreateNode, CreateEdge, BulkCreateEdges or SupersedeFact instead"
	}
	var rk *graphdb.ReservedPropertyError
	if !errors.As(err, &rk) {
		return err.Error()
	}
	hint, ok := reservedKeyHints[rk.Key]
	if !ok {
		hint = "rename the property"
	}
	msg := fmt.Sprintf("properties[%q] is reserved: %s", rk.Key, hint)
	var be *graphdb.BulkEdgeError
	if errors.As(err, &be) {
		msg = fmt.Sprintf("bulk create edges [%d]: %s", be.Index, msg)
	}
	return msg
}

// checkEdgeConfidence rejects a client-sent temporal.confidence that is not a
// finite number in [0, 1]. ±Inf falls outside the range; NaN needs its own
// test because every comparison with it is false.
func checkEdgeConfidence(pe *pb.CreateEdgeRequest) error {
	c := float64(pe.GetTemporal().GetConfidence())
	if math.IsNaN(c) || c < 0 || c > 1 {
		return fmt.Errorf("temporal.confidence %s is invalid: it must be a finite number from 0 to 1 inclusive. Send a value in [0, 1], or omit it",
			strconv.FormatFloat(c, 'g', -1, 32))
	}
	return nil
}
