package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the CatalogService Connect interface.
// It orchestrates parsing, persistence, graph construction, embeddings, and search.
type Service struct {
	parser            oscal.Parser
	structurer        oscal.Structurer
	assembler         oscal.Assembler
	store             Store
	graph             graphdb.GraphDB
	vectors           vectordb.VectorDB
	embedder          oscal.Embedder
	bus               natsbus.Client
	storage           storage.Provider
	tracer            trace.Tracer
	meter             metric.Meter
	logger            *slog.Logger
	catalogsParsed    metric.Int64Counter
	controlsExtracted metric.Int64Counter
	parseDuration     metric.Int64Histogram
	searchDuration    metric.Int64Histogram
}

// ServiceOption configures a Service.
type ServiceOption func(*Service)

// NewService creates a new CatalogService with the given options.
// Required: WithParser, WithStore, WithStorage.
// Optional: WithStructurer, WithAssembler, WithGraphDB, WithVectorDB, WithEmbedder, WithBus, WithTracer, WithMeter, WithLogger.
func NewService(opts ...ServiceOption) *Service {
	s := &Service{
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}

	// Initialize metrics if meter is configured
	if s.meter != nil {
		var err error
		s.catalogsParsed, err = s.meter.Int64Counter("oscal.catalogs.parsed.total")
		if err != nil {
			s.logger.Warn("failed to create catalogsParsed counter", "error", err)
		}

		s.controlsExtracted, err = s.meter.Int64Counter("oscal.controls.extracted.total")
		if err != nil {
			s.logger.Warn("failed to create controlsExtracted counter", "error", err)
		}

		s.parseDuration, err = s.meter.Int64Histogram("oscal.parse.duration_ms")
		if err != nil {
			s.logger.Warn("failed to create parseDuration histogram", "error", err)
		}

		s.searchDuration, err = s.meter.Int64Histogram("catalog.search.duration_ms")
		if err != nil {
			s.logger.Warn("failed to create searchDuration histogram", "error", err)
		}
	}

	return s
}

// WithParser sets the OSCAL parser.
func WithParser(p oscal.Parser) ServiceOption {
	return func(s *Service) {
		s.parser = p
	}
}

// WithStructurer sets the structurer for non-OSCAL content.
func WithStructurer(st oscal.Structurer) ServiceOption {
	return func(s *Service) {
		s.structurer = st
	}
}

// WithAssembler sets the assembler.
func WithAssembler(a oscal.Assembler) ServiceOption {
	return func(s *Service) {
		s.assembler = a
	}
}

// WithStore sets the catalog store.
func WithStore(st Store) ServiceOption {
	return func(s *Service) {
		s.store = st
	}
}

// WithGraphDB sets the graph database.
func WithGraphDB(g graphdb.GraphDB) ServiceOption {
	return func(s *Service) {
		s.graph = g
	}
}

// WithVectorDB sets the vector database.
func WithVectorDB(v vectordb.VectorDB) ServiceOption {
	return func(s *Service) {
		s.vectors = v
	}
}

// WithEmbedder sets the embedder.
func WithEmbedder(e oscal.Embedder) ServiceOption {
	return func(s *Service) {
		s.embedder = e
	}
}

// WithBus sets the NATS event bus.
func WithBus(b natsbus.Client) ServiceOption {
	return func(s *Service) {
		s.bus = b
	}
}

// WithStorage sets the object storage provider.
func WithStorage(sp storage.Provider) ServiceOption {
	return func(s *Service) {
		s.storage = sp
	}
}

// WithTracer sets the OpenTelemetry tracer.
func WithTracer(t trace.Tracer) ServiceOption {
	return func(s *Service) {
		s.tracer = t
	}
}

// WithMeter sets the OpenTelemetry meter.
func WithMeter(m metric.Meter) ServiceOption {
	return func(s *Service) {
		s.meter = m
	}
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) ServiceOption {
	return func(s *Service) {
		s.logger = l
	}
}

// ParseCatalog parses a catalog document from object storage, persists controls, builds graph, generates embeddings.
func (s *Service) ParseCatalog(ctx context.Context, req *connect.Request[crosscodexv1.ParseCatalogRequest]) (*connect.Response[crosscodexv1.ParseCatalogResponse], error) {
	start := time.Now()

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "catalog.ParseCatalog",
			trace.WithAttributes(attribute.String("document.id", req.Msg.GetDocumentId())))
		defer span.End()
	}

	// Validate tenant context
	if req.Msg.GetTenantContext() == nil || req.Msg.GetTenantContext().GetTenantId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_context.tenant_id is required"))
	}

	// Validate document_id
	if req.Msg.GetDocumentId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("document_id is required"))
	}

	tenantID := req.Msg.GetTenantContext().GetTenantId()

	// Set tenant context
	var err error
	ctx, err = tenant.WithTenant(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant_id: %v", err))
	}

	// Get document from storage
	if s.storage == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("storage provider not configured"))
	}

	reader, err := s.storage.Get(ctx, req.Msg.GetDocumentId())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("document not found: %v", err))
	}
	defer reader.Close()

	// Create provenance tracker with tee reader
	prov, teeReader, err := oscal.NewProvenance(reader)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create provenance: %v", err))
	}

	// Read all data
	data, err := io.ReadAll(teeReader)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read document: %v", err))
	}

	// Detect if OSCAL JSON
	isOSCAL := isOSCALJSON(data)

	var items []oscal.ControlItem
	if isOSCAL {
		// Parse via Parser
		if s.parser == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("parser not configured"))
		}
		items, err = s.parser.Parse(ctx, bytes.NewReader(data))
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("parse OSCAL: %v", err))
		}
	} else {
		// Structure via Structurer
		if s.structurer == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("structurer not configured"))
		}
		// For structurer, we need a StructuredDoc - for now, just fail gracefully
		// In a real implementation, we'd parse the document into sections first
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("non-OSCAL structuring not yet implemented in service layer"))
	}

	// Validate items
	if err := validateItems(items); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("validation failed: %v", err))
	}

	// Generate catalog_id from content hash
	catalogID := generateCatalogID(prov.ContentHash, tenantID)

	// Persist catalog metadata if store configured
	if s.store != nil {
		catalogRecord := CatalogRecord{
			CatalogID:        catalogID,
			TenantID:         tenantID,
			Name:             req.Msg.GetCatalogName(),
			Version:          "",
			SourceType:       "document",
			ObjectPath:       req.Msg.GetDocumentId(),
			CreatedAt:        time.Now().UTC(),
			SourceURI:        prov.SourceURI,
			ContentHash:      prov.ContentHash,
			ContentSize:      prov.ContentSize,
			Format:           prov.Format,
			OutputHash:       prov.OutputHash,
			ExtractorName:    prov.ExtractorName,
			ExtractorVersion: prov.ExtractorVersion,
		}

		if err := s.store.UpsertCatalog(ctx, catalogRecord); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store catalog: %v", err))
		}

		// Persist controls
		controlRecords := make([]ControlRecord, len(items))
		for i, item := range items {
			controlRecords[i] = ControlRecord{
				TenantID:   tenantID,
				ControlID:  fmt.Sprintf("%s/%s", catalogID, item.ID),
				CatalogID:  catalogID,
				Identifier: item.ID,
				Title:      item.Title,
				Statement:  item.Text,
				Class:      item.Class,
				ParentID:   item.ParentID,
				GroupID:    item.GroupID,
				Props:      item.Props,
				CreatedAt:  time.Now().UTC(),
			}
		}

		if err := s.store.UpsertControls(ctx, controlRecords); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store controls: %v", err))
		}
	}

	// Build graph if configured
	if s.graph != nil {
		for _, item := range items {
			node := graphdb.Node{
				ID:    fmt.Sprintf("%s/%s", catalogID, item.ID),
				Label: "Control",
				Properties: map[string]interface{}{
					"tenant_id":  tenantID,
					"catalog_id": catalogID,
					"identifier": item.ID,
					"title":      item.Title,
					"statement":  item.Text,
					"class":      item.Class,
				},
			}

			if err := s.graph.CreateNode(ctx, tenantID, node); err != nil {
				s.logger.Warn("create graph node failed", "control_id", item.ID, "error", err)
			}

			// Create PARENT_OF edge if parent exists
			if item.ParentID != "" {
				sourceID := fmt.Sprintf("%s/%s", catalogID, item.ParentID)
				targetID := fmt.Sprintf("%s/%s", catalogID, item.ID)
				edge := graphdb.Edge{
					ID:    fmt.Sprintf("%s::parent_of::%s", sourceID, targetID),
					Label: "PARENT_OF",
				}

				if err := s.graph.CreateEdge(ctx, tenantID, sourceID, targetID, edge); err != nil {
					s.logger.Warn("create graph edge failed", "from", item.ParentID, "to", item.ID, "error", err)
				}
			}
		}
	}

	// Generate and store embeddings if configured
	if s.embedder != nil && s.vectors != nil {
		texts := make([]string, len(items))
		for i, item := range items {
			texts[i] = oscal.CleanForEmbedding(item.Text)
		}

		embeddings, err := s.embedder.Embed(ctx, texts)
		if err != nil {
			s.logger.Warn("generate embeddings failed", "error", err)
		} else {
			for i, item := range items {
				if i < len(embeddings) {
					emb := vectordb.Embedding{
						CatalogID: catalogID,
						ControlID: item.ID,
						Model:     "default",
						Vector:    embeddings[i],
						Metadata:  map[string]interface{}{"identifier": item.ID, "title": item.Title},
					}

					if err := s.vectors.StoreEmbedding(ctx, tenantID, emb); err != nil {
						s.logger.Warn("store embedding failed", "control_id", item.ID, "error", err)
					}
				}
			}
		}
	}

	// Emit audit event if bus configured
	if s.bus != nil {
		auditEvent := []byte(fmt.Sprintf(`{"event":"catalog_parsed","catalog_id":%q,"tenant_id":%q,"control_count":%d,"timestamp":%q}`,
			catalogID, tenantID, len(items), time.Now().UTC().Format(time.RFC3339)))

		if err := s.bus.Publish(ctx, "audit.catalog.parsed", auditEvent); err != nil {
			s.logger.Warn("publish audit event failed", "error", err)
		}
	}

	// Record metrics
	if s.parseDuration != nil {
		s.parseDuration.Record(ctx, time.Since(start).Milliseconds(),
			metric.WithAttributes(attribute.String("format", prov.Format)))
	}
	if s.catalogsParsed != nil {
		s.catalogsParsed.Add(ctx, 1)
	}
	if s.controlsExtracted != nil {
		s.controlsExtracted.Add(ctx, int64(len(items)))
	}

	return connect.NewResponse(&crosscodexv1.ParseCatalogResponse{
		CatalogId: catalogID,
		Status:    crosscodexv1.JobStatus_JOB_STATUS_COMPLETED,
	}), nil
}

// GetCatalog retrieves catalog metadata by ID.
func (s *Service) GetCatalog(ctx context.Context, req *connect.Request[crosscodexv1.GetCatalogRequest]) (*connect.Response[crosscodexv1.GetCatalogResponse], error) {
	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "catalog.GetCatalog",
			trace.WithAttributes(attribute.String("catalog.id", req.Msg.GetCatalogId())))
		defer span.End()
	}

	// Validate tenant context
	if req.Msg.GetTenantContext() == nil || req.Msg.GetTenantContext().GetTenantId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_context.tenant_id is required"))
	}

	tenantID := req.Msg.GetTenantContext().GetTenantId()

	// Set tenant context
	var err error
	ctx, err = tenant.WithTenant(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant_id: %v", err))
	}

	if s.store == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("store not configured"))
	}

	record, err := s.store.GetCatalog(ctx, req.Msg.GetCatalogId())
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, connect.NewError(connect.CodeNotFound, errors.New(err.Error()))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get catalog: %v", err))
	}

	return connect.NewResponse(&crosscodexv1.GetCatalogResponse{
		Catalog: catalogRecordToProto(record),
	}), nil
}

// ListCatalogs lists catalogs with pagination.
func (s *Service) ListCatalogs(ctx context.Context, req *connect.Request[crosscodexv1.ListCatalogsRequest]) (*connect.Response[crosscodexv1.ListCatalogsResponse], error) {
	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "catalog.ListCatalogs")
		defer span.End()
	}

	// Validate tenant context
	if req.Msg.GetTenantContext() == nil || req.Msg.GetTenantContext().GetTenantId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_context.tenant_id is required"))
	}

	tenantID := req.Msg.GetTenantContext().GetTenantId()

	// Set tenant context
	var err error
	ctx, err = tenant.WithTenant(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant_id: %v", err))
	}

	if s.store == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("store not configured"))
	}

	var pageSize int32
	var pageToken string
	if req.Msg.GetOptions() != nil && req.Msg.GetOptions().GetPagination() != nil {
		pageSize = req.Msg.GetOptions().GetPagination().GetPageSize()
		pageToken = req.Msg.GetOptions().GetPagination().GetPageToken()
	}

	offset := 0
	if pageToken != "" {
		if n, err := fmt.Sscanf(pageToken, "%d", &offset); err != nil || n != 1 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid page_token: %q", pageToken))
		}
	}

	opts := ListOptions{
		Limit:  int(pageSize),
		Offset: offset,
	}

	records, pageInfo, err := s.store.ListCatalogs(ctx, opts)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list catalogs: %v", err))
	}

	catalogs := make([]*crosscodexv1.Catalog, len(records))
	for i, rec := range records {
		catalogs[i] = catalogRecordToProto(&rec)
	}

	return connect.NewResponse(&crosscodexv1.ListCatalogsResponse{
		Catalogs: catalogs,
		PageInfo: &crosscodexv1.PageInfo{
			NextPageToken: fmt.Sprintf("%d", pageInfo.NextOffset),
			TotalCount:    pageInfo.TotalCount,
		},
	}), nil
}

// GetControl retrieves a single control by ID.
func (s *Service) GetControl(ctx context.Context, req *connect.Request[crosscodexv1.GetControlRequest]) (*connect.Response[crosscodexv1.GetControlResponse], error) {
	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "catalog.GetControl",
			trace.WithAttributes(attribute.String("control.id", req.Msg.GetControlId())))
		defer span.End()
	}

	// Validate tenant context
	if req.Msg.GetTenantContext() == nil || req.Msg.GetTenantContext().GetTenantId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_context.tenant_id is required"))
	}

	tenantID := req.Msg.GetTenantContext().GetTenantId()

	// Set tenant context
	var err error
	ctx, err = tenant.WithTenant(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant_id: %v", err))
	}

	if s.store == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("store not configured"))
	}

	record, err := s.store.GetControl(ctx, req.Msg.GetControlId())
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, connect.NewError(connect.CodeNotFound, errors.New(err.Error()))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get control: %v", err))
	}

	return connect.NewResponse(&crosscodexv1.GetControlResponse{
		Control: controlRecordToProto(record),
	}), nil
}

// SearchControls performs full-text and semantic search on controls.
func (s *Service) SearchControls(ctx context.Context, req *connect.Request[crosscodexv1.SearchControlsRequest]) (*connect.Response[crosscodexv1.SearchControlsResponse], error) {
	searchStart := time.Now()

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "catalog.SearchControls",
			trace.WithAttributes(attribute.String("query", req.Msg.GetQuery())))
		defer span.End()
	}

	// Validate tenant context
	if req.Msg.GetTenantContext() == nil || req.Msg.GetTenantContext().GetTenantId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_context.tenant_id is required"))
	}

	tenantID := req.Msg.GetTenantContext().GetTenantId()

	// Set tenant context
	var err error
	ctx, err = tenant.WithTenant(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant_id: %v", err))
	}

	if s.store == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("store not configured"))
	}

	var pageSize int32
	var pageToken string
	if req.Msg.GetOptions() != nil && req.Msg.GetOptions().GetPagination() != nil {
		pageSize = req.Msg.GetOptions().GetPagination().GetPageSize()
		pageToken = req.Msg.GetOptions().GetPagination().GetPageToken()
	}

	offset := 0
	if pageToken != "" {
		if n, err := fmt.Sscanf(pageToken, "%d", &offset); err != nil || n != 1 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid page_token: %q", pageToken))
		}
	}

	query := SearchQuery{
		Query:      req.Msg.GetQuery(),
		CatalogIDs: req.Msg.GetCatalogIds(),
		Limit:      int(pageSize),
		Offset:     offset,
	}

	// Full-text search via Store
	ftRecords, pageInfo, err := s.store.SearchControls(ctx, query)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("search controls: %v", err))
	}

	// Semantic search via VectorDB if available
	var semanticResults []vectordb.SimilarityResult
	if s.vectors != nil && s.embedder != nil && req.Msg.GetQuery() != "" {
		// Generate query embedding
		embeddings, err := s.embedder.Embed(ctx, []string{oscal.CleanForEmbedding(req.Msg.GetQuery())})
		if err == nil && len(embeddings) > 0 {
			// For each catalog ID, perform semantic search
			for _, catID := range req.Msg.GetCatalogIds() {
				findQuery := vectordb.FindSimilarQuery{
					CatalogID: catID,
					Model:     "default",
					Vector:    embeddings[0],
					Limit:     query.Limit,
				}

				results, err := s.vectors.FindSimilar(ctx, tenantID, findQuery)
				if err != nil {
					s.logger.Warn("semantic search failed", "catalog_id", catID, "error", err)
					continue
				}
				semanticResults = append(semanticResults, results...)
			}
		}
	}

	// Merge results
	mergedRecords := mergeResults(ftRecords, semanticResults)

	controls := make([]*crosscodexv1.Control, len(mergedRecords))
	for i, rec := range mergedRecords {
		controls[i] = controlRecordToProto(&rec)
	}

	// Record search metrics
	if s.searchDuration != nil {
		mode := "fulltext"
		if len(semanticResults) > 0 {
			mode = "hybrid"
		}
		s.searchDuration.Record(ctx, time.Since(searchStart).Milliseconds(),
			metric.WithAttributes(attribute.String("search.mode", mode)))
	}

	return connect.NewResponse(&crosscodexv1.SearchControlsResponse{
		Controls: controls,
		PageInfo: &crosscodexv1.PageInfo{
			NextPageToken: fmt.Sprintf("%d", pageInfo.NextOffset),
			TotalCount:    pageInfo.TotalCount,
		},
	}), nil
}

// Helper functions

// isOSCALJSON detects if data is OSCAL JSON by checking for "catalog" key.
func isOSCALJSON(data []byte) bool {
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return false
	}
	_, ok := obj["catalog"]
	return ok
}

// validateItems checks for duplicate IDs and self-referential parents within the import.
func validateItems(items []oscal.ControlItem) error {
	seen := make(map[string]bool)
	for _, item := range items {
		if seen[item.ID] {
			return fmt.Errorf("duplicate control ID: %s", item.ID)
		}
		seen[item.ID] = true

		if item.ParentID == item.ID {
			return fmt.Errorf("self-referential parent: %s", item.ID)
		}
	}
	return nil
}

// generateCatalogID generates a deterministic catalog ID from content hash and tenant.
func generateCatalogID(contentHash, tenantID string) string {
	h := sha256.New()
	h.Write([]byte(contentHash))
	h.Write([]byte(tenantID))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// mergeResults deduplicates full-text and semantic results by ControlID.
// Full-text results take precedence; semantic-only results are appended
// as partial records (ControlID + Identifier populated from the similarity result).
func mergeResults(ftRecords []ControlRecord, semanticResults []vectordb.SimilarityResult) []ControlRecord {
	seen := make(map[string]bool, len(ftRecords))
	merged := make([]ControlRecord, 0, len(ftRecords)+len(semanticResults))

	for _, rec := range ftRecords {
		if seen[rec.ControlID] {
			continue
		}
		seen[rec.ControlID] = true
		merged = append(merged, rec)
	}

	for _, sr := range semanticResults {
		if seen[sr.ControlID] {
			continue
		}
		seen[sr.ControlID] = true
		merged = append(merged, ControlRecord{
			ControlID:  sr.ControlID,
			Identifier: sr.ControlID,
		})
	}

	return merged
}

// catalogRecordToProto converts CatalogRecord to proto Catalog.
func catalogRecordToProto(rec *CatalogRecord) *crosscodexv1.Catalog {
	if rec == nil {
		return nil
	}

	return &crosscodexv1.Catalog{
		CatalogId: rec.CatalogID,
		TenantContext: &crosscodexv1.TenantContext{
			TenantId: rec.TenantID,
		},
		Name:    rec.Name,
		Version: rec.Version,
		Audit: &crosscodexv1.AuditMetadata{
			CreatedAt: timestamppb.New(rec.CreatedAt),
		},
		Provenance: &crosscodexv1.ProvenanceMetadata{
			LineageMetadata: map[string]string{
				"source_uri":        rec.SourceURI,
				"content_hash":      rec.ContentHash,
				"content_size":      fmt.Sprintf("%d", rec.ContentSize),
				"format":            rec.Format,
				"output_hash":       rec.OutputHash,
				"extractor_name":    rec.ExtractorName,
				"extractor_version": rec.ExtractorVersion,
			},
		},
	}
}

// controlRecordToProto converts ControlRecord to proto Control.
func controlRecordToProto(rec *ControlRecord) *crosscodexv1.Control {
	if rec == nil {
		return nil
	}

	return &crosscodexv1.Control{
		ControlId:  rec.ControlID,
		CatalogId:  rec.CatalogID,
		Identifier: rec.Identifier,
		Title:      rec.Title,
		Statement:  rec.Statement,
		Parts:      rec.Props,
		Audit: &crosscodexv1.AuditMetadata{
			CreatedAt: timestamppb.New(rec.CreatedAt),
		},
	}
}

// Compile-time interface check.
var _ crosscodexv1connect.CatalogServiceHandler = (*Service)(nil)
