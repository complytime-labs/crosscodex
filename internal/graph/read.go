package graph

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"go.opentelemetry.io/otel/attribute"
)

// GetNode retrieves a single node by ID.
func (s *Service) GetNode(ctx context.Context, req *connect.Request[pb.GetNodeRequest]) (*connect.Response[pb.GetNodeResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.GetNode")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "GetNode", start, connect.CodeOf(err))
		return nil, err
	}

	if req.Msg.GetNodeId() == "" {
		s.recordRPC(ctx, "GetNode", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("node_id is required"))
	}

	node, err := s.graph.GetNode(ctx, tenantID, req.Msg.GetNodeId())
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "GetNode", start, code)
		return nil, s.rpcError(ctx, "GetNode", code, err)
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	s.recordRPC(ctx, "GetNode", start, connect.Code(0))
	return connect.NewResponse(&pb.GetNodeResponse{Node: nodeToProto(*node, tc)}), nil
}

// GetEdge retrieves a single edge by ID, including source and target node IDs.
func (s *Service) GetEdge(ctx context.Context, req *connect.Request[pb.GetEdgeRequest]) (*connect.Response[pb.GetEdgeResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.GetEdge")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "GetEdge", start, connect.CodeOf(err))
		return nil, err
	}

	if req.Msg.GetEdgeId() == "" {
		s.recordRPC(ctx, "GetEdge", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("edge_id is required"))
	}

	edge, err := s.graph.GetEdge(ctx, tenantID, req.Msg.GetEdgeId())
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "GetEdge", start, code)
		return nil, s.rpcError(ctx, "GetEdge", code, err)
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	s.recordRPC(ctx, "GetEdge", start, connect.Code(0))
	return connect.NewResponse(&pb.GetEdgeResponse{Edge: edgeToProto(*edge, tc)}), nil
}

// Traverse performs graph traversal from a starting node.
func (s *Service) Traverse(ctx context.Context, req *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.Traverse")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "Traverse", start, connect.CodeOf(err))
		return nil, err
	}

	if req.Msg.GetStartNodeId() == "" {
		s.recordRPC(ctx, "Traverse", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("start_node_id is required"))
	}

	direction, ok := protoDirectionToString(req.Msg.GetDirection())
	if !ok {
		s.recordRPC(ctx, "Traverse", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unknown traversal direction %d: it is not a defined TraversalDirection, so the traversal was refused rather than guessed; "+
				"use TRAVERSAL_DIRECTION_OUTBOUND, TRAVERSAL_DIRECTION_INBOUND or TRAVERSAL_DIRECTION_BOTH, or leave direction unset for outbound",
				req.Msg.GetDirection()))
	}

	query := graphdb.TraversalQuery{
		StartNode:  req.Msg.GetStartNodeId(),
		Direction:  direction,
		EdgeLabels: req.Msg.GetEdgeLabels(),
		MaxDepth:   int(req.Msg.GetMaxDepth()),
	}
	if req.Msg.GetAsOf() != nil {
		t := req.Msg.GetAsOf().AsTime()
		query.AsOf = &t
	}

	paths, err := s.graph.Traverse(ctx, tenantID, query)
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "Traverse", start, code)
		return nil, s.rpcError(ctx, "Traverse", code, err)
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	s.recordRPC(ctx, "Traverse", start, connect.Code(0))
	return connect.NewResponse(pathToTraverseResponse(paths, tc)), nil
}

// Query executes a read-only openCypher query. Admin-only.
func (s *Service) Query(ctx context.Context, req *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.Query")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "Query", start, connect.CodeOf(err))
		return nil, err
	}

	if err := s.requireAdmin(ctx); err != nil {
		s.recordRPC(ctx, "Query", start, connect.CodePermissionDenied)
		return nil, err
	}

	if req.Msg.GetCypher() == "" {
		s.recordRPC(ctx, "Query", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cypher is required"))
	}

	rows, err := s.graph.ExecuteQuery(ctx, tenantID, req.Msg.GetCypher(), req.Msg.GetParameters())
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "Query", start, code)
		return nil, s.rpcError(ctx, "Query", code, err)
	}

	s.recordRPC(ctx, "Query", start, connect.Code(0))
	return connect.NewResponse(queryRowsToProto(rows)), nil
}

// SimilaritySearch finds the stored controls nearest a query vector in the
// named catalogs, under one embedding model. The control_id form searches
// with the control's stored embedding, in its own catalog unless catalog_ids
// names others, and leaves the control itself out of the results. Every
// catalog must have embeddings under the model: one that has none fails the
// whole search rather than returning a partial result.
func (s *Service) SimilaritySearch(ctx context.Context, req *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.SimilaritySearch")
	defer span.End()

	msg := req.Msg
	fail := func(err error) (*connect.Response[pb.SimilaritySearchResponse], error) {
		s.recordRPC(ctx, "SimilaritySearch", start, connect.CodeOf(err))
		return nil, err
	}

	tenantID, err := s.extractTenant(ctx, msg.GetTenantContext())
	if err != nil {
		return fail(err)
	}

	if s.vectors == nil {
		return fail(connect.NewError(connect.CodeFailedPrecondition, errors.New("vector search not configured")))
	}

	catalogs, err := distinctCatalogs(msg.GetCatalogIds(), s.maxSimilarityCatalogs)
	if err != nil {
		return fail(err)
	}

	limit := int(msg.GetLimit())
	if limit <= 0 {
		limit = 10
	}
	if limit > s.maxSimilarityLimit {
		return fail(connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"limit %d is too large: a search returns at most %d results per catalog (graph.max_similarity_limit); lower limit to %d or less",
			limit, s.maxSimilarityLimit, s.maxSimilarityLimit)))
	}

	var (
		form   string
		vector []float32
		model  = msg.GetModel()
		// self is the query control, excluded from its own catalog's results.
		// It stays zero in the query_embedding form, and a zero CatalogID
		// never equals a validated, non-empty target catalog.
		self CatalogControl
		// fetch is the per-catalog result count. The control_id form asks for
		// one more, so dropping the control itself still leaves limit results.
		fetch = limit
	)
	switch q := msg.GetQuery().(type) {
	case *pb.SimilaritySearchRequest_QueryEmbedding:
		form = "query_embedding"
		vector = q.QueryEmbedding.GetEmbeddings()
		if err := checkEmbeddingQuery(vector, catalogs, model); err != nil {
			return fail(err)
		}
	case *pb.SimilaritySearchRequest_ControlId:
		form = "control_id"
		if q.ControlId == "" {
			return fail(connect.NewError(connect.CodeInvalidArgument, errors.New("control_id is required")))
		}
		var emb vectordb.Embedding
		self, emb, err = s.controlEmbedding(ctx, tenantID, q.ControlId, model)
		if err != nil {
			return fail(err)
		}
		vector, model = emb.Vector, emb.Model
		if len(catalogs) == 0 {
			catalogs = []string{self.CatalogID}
		}
		fetch = limit + 1
	default:
		return fail(connect.NewError(connect.CodeInvalidArgument, errors.New("query_embedding or control_id is required")))
	}
	span.SetAttributes(
		attribute.String("similarity.form", form),
		attribute.String("similarity.model", model),
		attribute.Int("similarity.catalogs", len(catalogs)))

	var matches []catalogMatch
	for _, catalog := range catalogs {
		results, err := s.vectors.FindSimilar(ctx, tenantID, vectordb.FindSimilarQuery{
			CatalogID: catalog,
			Model:     model,
			Vector:    vector,
			Limit:     fetch,
		})
		if errors.Is(err, vectordb.ErrModelNotFound) {
			// Identical for a missing catalog and another tenant's: RLS and the
			// tenant filter make them indistinguishable, so nothing leaks.
			return fail(connect.NewError(connect.CodeNotFound, fmt.Errorf(
				"catalog %q has no embeddings under model %q", catalog, model)))
		}
		if err != nil {
			return fail(s.rpcError(ctx, "SimilaritySearch", mapVectorError(err), err))
		}
		for _, r := range results {
			if catalog == self.CatalogID && r.ControlID == self.ControlID {
				continue
			}
			matches = append(matches, catalogMatch{catalogID: catalog, result: r})
		}
	}
	slices.SortFunc(matches, func(a, b catalogMatch) int {
		return cmp.Or(
			cmp.Compare(b.result.Similarity, a.result.Similarity),
			cmp.Compare(a.catalogID, b.catalogID),
			cmp.Compare(a.result.ControlID, b.result.ControlID))
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	resp := &pb.SimilaritySearchResponse{}
	for _, m := range matches {
		resp.Matches = append(resp.Matches, similarityResultToProto(m.result, m.catalogID, tc))
	}
	s.recordRPC(ctx, "SimilaritySearch", start, connect.Code(0))
	return connect.NewResponse(resp), nil
}

// catalogMatch is one similarity result with the catalog it was found in.
type catalogMatch struct {
	catalogID string
	result    vectordb.SimilarityResult
}

// distinctCatalogs validates a SimilaritySearch catalog list and returns it
// without duplicates, in first-occurrence order. Every entry must be
// non-empty, and the list may name at most limit distinct catalogs.
func distinctCatalogs(ids []string, limit int) ([]string, error) {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for i, id := range ids {
		if id == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
				"catalog_ids[%d] is empty: every entry must name a catalog", i))
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > limit {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"catalog_ids names %d distinct catalogs, more than the limit of %d (graph.max_similarity_catalogs); split the search into requests of at most %d catalogs each",
			len(out), limit, limit))
	}
	return out, nil
}

// checkEmbeddingQuery validates the query_embedding form, which has no
// control to take a catalog or model from. Cosine similarity is undefined for
// a zero vector (pgvector returns NaN), and a NaN or ±Inf component makes
// every score meaningless, so both are rejected.
func checkEmbeddingQuery(vector []float32, catalogs []string, model string) error {
	if len(catalogs) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New(
			"query_embedding search needs catalog_ids: name the catalogs whose embeddings to search"))
	}
	if model == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New(
			"query_embedding search needs model: name the embedding model that produced the vector"))
	}
	if len(vector) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New(
			"query_embedding is empty: send the vector to search with"))
	}
	nonZero := false
	for i, x := range vector {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
				"query_embedding must be finite and non-zero: component %d is %v", i, x))
		}
		nonZero = nonZero || x != 0
	}
	if !nonZero {
		return connect.NewError(connect.CodeInvalidArgument, errors.New(
			"query_embedding must be finite and non-zero: every component is 0, and similarity to a zero vector is undefined"))
	}
	return nil
}

// controlEmbedding looks up controlID and the stored embedding to search
// with. model picks among the control's stored embeddings; empty means its
// only one. Errors are Connect errors carrying only the caller's own IDs.
func (s *Service) controlEmbedding(ctx context.Context, tenantID, controlID, model string) (CatalogControl, vectordb.Embedding, error) {
	if s.controls == nil {
		return CatalogControl{}, vectordb.Embedding{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("control lookup not configured"))
	}
	control, err := s.controls.GetCatalogControl(ctx, controlID)
	if errors.Is(err, ErrControlNotFound) {
		return CatalogControl{}, vectordb.Embedding{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("control %q not found", controlID))
	}
	if err != nil {
		return CatalogControl{}, vectordb.Embedding{}, s.rpcError(ctx, "SimilaritySearch", connect.CodeInternal, err)
	}
	stored, err := s.vectors.GetEmbeddings(ctx, tenantID, control.CatalogID, control.ControlID)
	if errors.Is(err, vectordb.ErrNotFound) {
		return CatalogControl{}, vectordb.Embedding{}, connect.NewError(connect.CodeNotFound, fmt.Errorf(
			"control %q has no stored embedding; run an analysis that includes its catalog to embed it", controlID))
	}
	if err != nil {
		return CatalogControl{}, vectordb.Embedding{}, s.rpcError(ctx, "SimilaritySearch", mapVectorError(err), err)
	}

	models := make([]string, len(stored))
	for i, e := range stored {
		models[i] = e.Model
	}
	switch i := slices.Index(models, model); {
	case model == "" && len(stored) == 1:
		return control, stored[0], nil
	case model == "":
		return CatalogControl{}, vectordb.Embedding{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"control %q has embeddings under several models (%s): set model to the one to search under", controlID, strings.Join(models, ", ")))
	case i < 0:
		return CatalogControl{}, vectordb.Embedding{}, connect.NewError(connect.CodeNotFound, fmt.Errorf(
			"control %q has no embedding under model %q; its stored models are %s: set model to one of them", controlID, model, strings.Join(models, ", ")))
	default:
		return control, stored[i], nil
	}
}

// TemporalQuery executes a temporal point-in-time query. Admin-only.
func (s *Service) TemporalQuery(ctx context.Context, req *connect.Request[pb.TemporalQueryRequest]) (*connect.Response[pb.QueryResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.TemporalQuery")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "TemporalQuery", start, connect.CodeOf(err))
		return nil, err
	}

	if err := s.requireAdmin(ctx); err != nil {
		s.recordRPC(ctx, "TemporalQuery", start, connect.CodePermissionDenied)
		return nil, err
	}

	if req.Msg.GetCypher() == "" {
		s.recordRPC(ctx, "TemporalQuery", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cypher is required"))
	}
	if req.Msg.GetAsOf() == nil {
		s.recordRPC(ctx, "TemporalQuery", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("as_of is required"))
	}

	// Inject as_of timestamp into parameters so the caller's Cypher can reference it via $as_of.
	// The caller is responsible for writing temporal filtering logic in their query.
	params := req.Msg.GetParameters()
	if params == nil {
		params = make(map[string]string)
	}
	params["as_of"] = graphdb.FormatTime(req.Msg.GetAsOf().AsTime())

	rows, err := s.graph.ExecuteQuery(ctx, tenantID, req.Msg.GetCypher(), params)
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "TemporalQuery", start, code)
		return nil, s.rpcError(ctx, "TemporalQuery", code, err)
	}

	s.recordRPC(ctx, "TemporalQuery", start, connect.Code(0))
	return connect.NewResponse(queryRowsToProto(rows)), nil
}

// requireAdmin checks the identity in context for admin role.
func (s *Service) requireAdmin(ctx context.Context) error {
	identity := identityFromContext(ctx)
	if identity == nil {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	if err := authn.RequireRole(*identity, authn.RoleAdmin); err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("admin access required: %v", err))
	}
	return nil
}

// identityFromContext extracts the authn.Identity from context.
// Uses the shared context helper from pkg/authn.
func identityFromContext(ctx context.Context) *authn.Identity {
	return authn.IdentityFromContext(ctx)
}

// protoDirectionToString converts a proto TraversalDirection to the graphdb
// direction string. UNSPECIFIED means outbound (proto3 zero value); ok is
// false for an undefined enum number, which the caller must reject rather
// than silently traverse outbound.
func protoDirectionToString(d pb.TraversalDirection) (direction string, ok bool) {
	switch d {
	case pb.TraversalDirection_TRAVERSAL_DIRECTION_UNSPECIFIED,
		pb.TraversalDirection_TRAVERSAL_DIRECTION_OUTBOUND:
		return "outbound", true
	case pb.TraversalDirection_TRAVERSAL_DIRECTION_INBOUND:
		return "inbound", true
	case pb.TraversalDirection_TRAVERSAL_DIRECTION_BOTH:
		return "both", true
	default:
		return "", false
	}
}

// mapGraphError maps graphdb errors to Connect status codes. Node ID and
// label-kind conflicts are FailedPrecondition, not AlreadyExists: the write
// is refused because of what the graph already holds, and graph writers
// treat AlreadyExists-style errors as idempotent success.
func mapGraphError(err error) connect.Code {
	switch {
	case err == nil:
		return connect.Code(0)
	case errors.Is(err, graphdb.ErrNodeNotFound),
		errors.Is(err, graphdb.ErrEdgeNotFound),
		errors.Is(err, graphdb.ErrGraphNotFound):
		return connect.CodeNotFound
	case errors.Is(err, graphdb.ErrInvalidCypher),
		errors.Is(err, graphdb.ErrTenantRequired):
		return connect.CodeInvalidArgument
	case errors.Is(err, graphdb.ErrReadOnlyViolation):
		return connect.CodePermissionDenied
	case errors.Is(err, graphdb.ErrNotSupported):
		return connect.CodeUnimplemented
	case errors.Is(err, graphdb.ErrNodeIDConflict),
		errors.Is(err, graphdb.ErrLabelKindConflict):
		return connect.CodeFailedPrecondition
	default:
		return connect.CodeInternal
	}
}

// mapVectorError maps vectordb errors to Connect status codes.
func mapVectorError(err error) connect.Code {
	switch {
	case err == nil:
		return connect.Code(0)
	case errors.Is(err, vectordb.ErrNotFound),
		errors.Is(err, vectordb.ErrModelNotFound):
		return connect.CodeNotFound
	case errors.Is(err, vectordb.ErrInvalidDimension):
		return connect.CodeInvalidArgument
	default:
		return connect.CodeInternal
	}
}
