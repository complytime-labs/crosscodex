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
	"github.com/complytime-labs/crosscodex/pkg/catalogversion"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the CatalogService Connect interface.
// It orchestrates parsing, persistence, embeddings, and search, and announces
// stored catalogs so the graph role can project them.
type Service struct {
	parser            oscal.Parser
	assembler         oscal.Assembler
	store             Store
	vectors           vectordb.VectorDB
	embedder          oscal.Embedder
	bus               natsbus.Client
	storage           storage.Provider
	tracer            trace.Tracer
	meter             metric.Meter
	logger            *slog.Logger
	catalogsParsed    metric.Int64Counter
	controlsExtracted metric.Int64Counter
	textlessControls  metric.Int64Counter
	parseDuration     metric.Int64Histogram
	searchDuration    metric.Int64Histogram
	retention         Retention
	retireOps         metric.Int64Counter
	retiredVersions   metric.Int64Counter
	retireDuration    metric.Int64Histogram
	structurer        oscal.Structurer
	structureOpts     oscal.StructureOptions
	structuredTotal   metric.Int64Counter
}

// ServiceOption configures a Service.
type ServiceOption func(*Service)

// NewService creates a new CatalogService with the given options.
// Required: WithParser, WithStore, WithStorage.
// Optional: WithAssembler, WithVectorDB, WithEmbedder, WithBus, WithRetention,
// WithStructurer, WithStructuring, WithTracer, WithMeter, WithLogger.
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

		s.textlessControls, err = s.meter.Int64Counter("crosscodex.catalog.textless_controls")
		if err != nil {
			s.logger.Warn("failed to create textlessControls counter", "error", err)
		}

		s.parseDuration, err = s.meter.Int64Histogram("oscal.parse.duration_ms")
		if err != nil {
			s.logger.Warn("failed to create parseDuration histogram", "error", err)
		}

		s.searchDuration, err = s.meter.Int64Histogram("catalog.search.duration_ms")
		if err != nil {
			s.logger.Warn("failed to create searchDuration histogram", "error", err)
		}

		s.retireOps, err = s.meter.Int64Counter("crosscodex.catalog.retire.total")
		if err != nil {
			s.logger.Warn("failed to create retireOps counter", "error", err)
		}

		s.retiredVersions, err = s.meter.Int64Counter("crosscodex.catalog.retire.versions")
		if err != nil {
			s.logger.Warn("failed to create retiredVersions counter", "error", err)
		}

		s.retireDuration, err = s.meter.Int64Histogram("crosscodex.catalog.retire.duration_ms")
		if err != nil {
			s.logger.Warn("failed to create retireDuration histogram", "error", err)
		}

		s.structuredTotal, err = s.meter.Int64Counter("crosscodex.catalog.structured.total")
		if err != nil {
			s.logger.Warn("failed to create structuredTotal counter", "error", err)
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

// internalError turns an error the client cannot fix into the Connect error
// an RPC returns. Store and storage errors carry SQL, SQLSTATE codes, table
// names and file paths that must not reach a client (CWE-209), so the client
// gets a fixed message pointing at the trace and the full error is logged
// here, once, because the log is then the only record of it.
func (s *Service) internalError(ctx context.Context, method string, err error) *connect.Error {
	traceID := telemetry.TraceIDFromContext(ctx)
	s.logger.ErrorContext(ctx, "catalog RPC failed", "method", method, "trace_id", traceID, "error", err)
	if traceID == "" {
		return connect.NewError(connect.CodeInternal, fmt.Errorf(
			"internal catalog error; the server logged the cause. Retry the request, and if it keeps failing, give an operator the time of the failure and the method name (%s)", method))
	}
	return connect.NewError(connect.CodeInternal, fmt.Errorf(
		"internal catalog error; the server logged the cause under trace %s. Retry the request, and if it keeps failing, give that trace ID to an operator", traceID))
}

// ParseCatalog parses a catalog document from object storage, persists its
// controls, and generates embeddings. It does not write the graph: once a
// non-empty control set is stored it publishes the catalog parsed event, and
// the graph role projects Control nodes from the stored rows. A failed publish
// fails the import with CodeUnavailable so the caller re-imports rather than
// leaving controls that never reach the graph.
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

	// Version arguments are validated before any storage read: they reach
	// listings, reports and prompts.
	slug := req.Msg.GetDocumentSlug()
	explicitLabel := req.Msg.GetVersionLabel()
	switch {
	case slug != "":
		if err := ValidateDocumentSlug(slug); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if explicitLabel != "" {
			if err := ValidateVersionLabel(explicitLabel); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
		}
		if s.store == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(
				"store not configured: a document version cannot be recorded. Configure the catalog store, or import without document_slug"))
		}
	case explicitLabel != "":
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(
			"version_label requires document_slug: a label names a version of a document. Set document_slug, or omit version_label"))
	}

	// Get document from storage
	if s.storage == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("storage provider not configured"))
	}

	reader, err := s.storage.Get(ctx, req.Msg.GetDocumentId())
	if err != nil {
		// Only a missing object is the caller's to act on; any other storage
		// error can carry a filesystem path or an S3 SDK message (CWE-209).
		if errors.Is(err, storage.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf(
				"document %q not found in storage. Submit the document again", req.Msg.GetDocumentId()))
		}
		return nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("get document: %w", err))
	}
	defer reader.Close()

	// Create provenance tracker with tee reader
	prov, teeReader, err := oscal.NewProvenance(reader)
	if err != nil {
		return nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("create provenance: %w", err))
	}

	// Read all data
	data, err := io.ReadAll(teeReader)
	if err != nil {
		return nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("read document: %w", err))
	}

	// Detect actual format from content, decoding the document once for
	// detection and for its metadata (title, version).
	doc := oscal.DetectCatalogJSON(data)
	detectedIsOSCAL := doc.IsCatalog

	// Validate format hint against detected format
	var isOSCAL bool
	switch requestFormat := req.Msg.GetFormat(); requestFormat {
	case crosscodexv1.CatalogFormat_CATALOG_FORMAT_OSCAL:
		if !detectedIsOSCAL {
			return nil, connect.NewError(connect.CodeInvalidArgument, UnsupportedFormatError(requestFormat, data))
		}
		isOSCAL = true
		prov.Format = FormatOSCAL
	case crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA:
		if detectedIsOSCAL {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New("format=CATALOG_FORMAT_GEMARA specified but content appears to be OSCAL JSON"))
		}
		isOSCAL = false
		prov.Format = FormatGemara
	default:
		// CATALOG_FORMAT_UNSPECIFIED or unknown: auto-detect
		isOSCAL = detectedIsOSCAL
		if isOSCAL {
			prov.Format = FormatOSCAL
		} else {
			prov.Format = FormatGemara
		}
	}

	var tree *oscal.Tree
	switch {
	case isOSCAL:
		// Parse via Parser
		if s.parser == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("parser not configured"))
		}
		tree, err = s.parser.Parse(ctx, bytes.NewReader(data))
		if err != nil {
			// A schema that cannot load is server misconfiguration whose error
			// names the schema's path; only a parse failure is the caller's.
			if errors.Is(err, oscal.ErrSchemaLoad) {
				return nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("parse OSCAL: %w", err))
			}
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("parse OSCAL: %v", err))
		}
	case s.structurer != nil && req.Msg.GetFormat() != crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA && IsIngestionOutput(data):
		// Auto-detected crosscodex-ingestion output: a converted document.
		// Without a structurer (ingestion.address unset) the same JSON is a
		// caller's upload, not conversion output, and takes the default.
		var cerr *connect.Error
		tree, cerr = s.structureIngestion(ctx, prov, req.Msg.GetDocumentId(), data)
		if cerr != nil {
			return nil, cerr
		}
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, UnsupportedFormatError(req.Msg.GetFormat(), data))
	}

	// Validate the tree
	if err := validateTree(tree); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("validation failed: %v", err))
	}

	items := tree.Items

	// Generate catalog_id from content hash
	catalogID := generateCatalogID(prov.ContentHash, tenantID)

	// Derive catalog name from OSCAL metadata.title if not provided
	catalogName := req.Msg.GetCatalogName()
	if catalogName == "" && isOSCAL {
		catalogName = doc.MetadataString(OSCALTitleKey)
	}
	// The catalog's own version, so a crosswalk shows the exact versions it
	// mapped. Only OSCAL documents carry one; any other format records "".
	var catalogVersion string
	if isOSCAL {
		catalogVersion = doc.MetadataString(OSCALVersionKey)
	}

	var (
		version      string
		versionLabel string
		supersededID string
		changeReport *crosscodexv1.ChangeReport
	)

	// Persist catalog metadata if store configured
	if s.store != nil {
		catalogRecord := CatalogRecord{
			CatalogID:        catalogID,
			TenantID:         tenantID,
			Name:             catalogName,
			Version:          catalogVersion,
			SourceType:       SourceTypeDocument,
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

		// The catalog row, its containers and its controls go in one
		// transaction, so a failed import stores nothing.
		containerRecords := make([]ContainerRecord, len(tree.Containers))
		for i, c := range tree.Containers {
			containerRecords[i] = ContainerRecord{
				TenantID:   tenantID,
				CatalogID:  catalogID,
				Key:        c.Key,
				Identifier: c.Identifier,
				Title:      c.Title,
				Label:      c.Label,
				Prose:      c.Prose,
				ParentKey:  c.ParentKey,
			}
		}
		controlRecords := make([]ControlRecord, len(items))
		for i, item := range items {
			controlRecords[i] = ControlRecord{
				TenantID:     tenantID,
				ControlID:    fmt.Sprintf("%s/%s", catalogID, item.ID),
				CatalogID:    catalogID,
				Identifier:   item.ID,
				Title:        item.Title,
				Statement:    item.Text,
				Class:        item.Class,
				ParentID:     item.ParentID,
				GroupID:      item.GroupID,
				ContainerKey: item.ContainerKey,
				Props:        item.Props,
				CreatedAt:    time.Now().UTC(),
			}
		}

		if slug == "" {
			if err := s.store.UpsertCatalogWithTree(ctx, catalogRecord, containerRecords, controlRecords); err != nil {
				return nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("store catalog: %w", err))
			}
		} else {
			catalogRecord.DocumentSlug = slug
			var verr *connect.Error
			version, versionLabel, supersededID, changeReport, verr = s.importVersion(ctx, catalogRecord, containerRecords, controlRecords, explicitLabel, doc.MetadataString(OSCALVersionKey))
			if verr != nil {
				return nil, verr
			}
		}

		// The graph role treats an empty projection as an error, so a catalog
		// with no controls is never announced.
		if s.bus != nil && len(items) > 0 {
			if err := s.publishParsed(ctx, tenantID, catalogID, len(items)); err != nil {
				s.logger.ErrorContext(ctx, "queue catalog graph projection failed", "catalog_id", catalogID, "error", err)
				return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf(
					"catalog %s was stored but its graph projection could not be queued: the event bus is unavailable. Re-import the same document to retry", catalogID))
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

	// Store the audit record if a bus is configured. The catalog is already
	// stored, so a failure here reports that and says re-import is safe (the
	// upsert is idempotent) rather than claiming nothing was written.
	if s.bus != nil {
		auditEvent := []byte(fmt.Sprintf(`{"event":"catalog_parsed","catalog_id":%q,"tenant_id":%q,"control_count":%d,"timestamp":%q}`,
			catalogID, tenantID, len(items), time.Now().UTC().Format(time.RFC3339)))

		subject, err := natsbus.CatalogAuditSubject(tenantID, catalogID)
		if err != nil {
			return nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("build catalog audit subject: %w", err))
		}
		if err := s.bus.PublishDurable(ctx, subject, auditEvent, nil); err != nil {
			s.logger.ErrorContext(ctx, "store catalog audit record failed", "catalog_id", catalogID, "error", err)
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf(
				"catalog %s was stored but its audit record was not: the audit stream did not acknowledge it. Re-import the same document to retry; re-import is safe and replaces the stored rows", catalogID))
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
	if s.textlessControls != nil {
		s.textlessControls.Add(ctx, int64(len(tree.Textless)))
	}

	textless := make([]*crosscodexv1.TextlessControl, len(tree.Textless))
	for i, tc := range tree.Textless {
		textless[i] = &crosscodexv1.TextlessControl{Identifier: tc.Identifier, PartNames: tc.PartNames}
	}

	return connect.NewResponse(&crosscodexv1.ParseCatalogResponse{
		CatalogId:           catalogID,
		Status:              crosscodexv1.JobStatus_JOB_STATUS_COMPLETED,
		TextlessControls:    textless,
		Version:             version,
		VersionLabel:        versionLabel,
		SupersededCatalogId: supersededID,
		ChangeReport:        changeReport,
	}), nil
}

// importVersion stores catalog as a version of catalog.DocumentSlug and
// returns the version's display string, its stored label, the catalog it
// superseded and the control differences against it. catalog.VersionLabel is resolved here:
// explicitLabel (already validated), else oscalVersion when it is a valid
// label, else "<date> · <hash6>". An OSCAL version comes from the untrusted
// document, so an invalid one falls back rather than blocking the import.
//
// The superseded catalog is read back from the stored row after the upsert,
// not from a pre-read of the document's current version, so a concurrent import
// cannot make it stale. Content that already carried this slug before the call
// is an identical re-import: it keeps its place and reports nothing superseded.
func (s *Service) importVersion(ctx context.Context, catalog CatalogRecord, containers []ContainerRecord, controls []ControlRecord, explicitLabel, oscalVersion string) (string, string, string, *crosscodexv1.ChangeReport, *connect.Error) {
	existing, err := s.store.GetCatalog(ctx, catalog.CatalogID)
	if err != nil && !errors.Is(err, ErrCatalogNotFound) {
		return "", "", "", nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("read catalog before versioning: %w", err))
	}
	alreadyInDocument := existing != nil && existing.DocumentSlug == catalog.DocumentSlug
	if alreadyInDocument && explicitLabel != "" && explicitLabel != existing.VersionLabel {
		return "", "", "", nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%w: this content is already version %q of document %q, so label %q was not applied and nothing was written; relabelling is not supported. Re-import without --version-label (or with %q), or import changed content to create a new version",
			ErrVersionLabelConflict, existing.VersionLabel, catalog.DocumentSlug, explicitLabel, existing.VersionLabel))
	}

	if ValidateVersionLabel(oscalVersion) != nil {
		oscalVersion = ""
	}
	catalog.VersionLabel = ResolveVersionLabel(explicitLabel, oscalVersion, catalog.CreatedAt, catalog.ContentHash)

	if err := s.store.UpsertCatalogVersion(ctx, catalog, containers, controls); err != nil {
		if errors.Is(err, ErrCatalogInOtherDocument) {
			return "", "", "", nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return "", "", "", nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("store catalog version: %w", err))
	}

	stored, err := s.store.GetCatalog(ctx, catalog.CatalogID)
	if err != nil || stored == nil {
		return "", "", "", nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("read back catalog version %s: %w", catalog.CatalogID, errors.Join(err, ErrCatalogNotFound)))
	}
	version := catalogversion.Format(stored.DocumentSlug, stored.VersionLabel, stored.CreatedAt, stored.ContentHash)
	if alreadyInDocument || stored.SupersedesCatalogID == "" {
		return version, stored.VersionLabel, "", nil, nil
	}

	previous, err := s.store.ListControlsForCatalog(ctx, stored.SupersedesCatalogID)
	if err != nil {
		return "", "", "", nil, s.internalError(ctx, "ParseCatalog", fmt.Errorf("list controls of superseded catalog: %w", err))
	}
	diff := DiffControls(previous, controls)
	return version, stored.VersionLabel, stored.SupersedesCatalogID, &crosscodexv1.ChangeReport{
		Added: diff.Added, Removed: diff.Removed, Changed: diff.Changed, IdsUnstable: diff.IDsUnstable,
	}, nil
}

// publishParsed tells the graph role that catalogID's controls are stored and
// ready to project. The subscriber reads tenant and catalog from the subject
// and content from Postgres; the payload is informational.
func (s *Service) publishParsed(ctx context.Context, tenantID, catalogID string, controlCount int) error {
	subject, err := natsbus.CatalogParsedSubject(tenantID, catalogID)
	if err != nil {
		return fmt.Errorf("build catalog parsed subject: %w", err)
	}
	payload, err := json.Marshal(struct {
		CatalogID    string `json:"catalog_id"`
		ControlCount int    `json:"control_count"`
		Timestamp    string `json:"timestamp"`
	}{catalogID, controlCount, time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return fmt.Errorf("encode catalog parsed event: %w", err)
	}
	if err := s.bus.PublishDurable(ctx, subject, payload, nil); err != nil {
		return fmt.Errorf("store %s: %w", subject, err)
	}
	return nil
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
		if errors.Is(err, ErrCatalogNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf(
				"catalog %q not found. List catalogs to find a valid catalog_id", req.Msg.GetCatalogId()))
		}
		return nil, s.internalError(ctx, "GetCatalog", fmt.Errorf("get catalog: %w", err))
	}

	pb, err := s.catalogToProto(ctx, record, map[string]string{})
	if err != nil {
		return nil, s.internalError(ctx, "GetCatalog", err)
	}
	return connect.NewResponse(&crosscodexv1.GetCatalogResponse{Catalog: pb}), nil
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
		return nil, s.internalError(ctx, "ListCatalogs", fmt.Errorf("list catalogs: %w", err))
	}

	catalogs := make([]*crosscodexv1.Catalog, len(records))
	currentByDoc := map[string]string{}
	for i := range records {
		catalogs[i], err = s.catalogToProto(ctx, &records[i], currentByDoc)
		if err != nil {
			return nil, s.internalError(ctx, "ListCatalogs", err)
		}
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
		if errors.Is(err, ErrControlNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf(
				"control %q not found. Search controls to find a valid control_id", req.Msg.GetControlId()))
		}
		return nil, s.internalError(ctx, "GetControl", fmt.Errorf("get control: %w", err))
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
		return nil, s.internalError(ctx, "SearchControls", fmt.Errorf("search controls: %w", err))
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

// UnsupportedFormatError explains why a document that failed OSCAL detection
// cannot be imported. Only OSCAL JSON is supported, so the advice depends on
// what was sent: a declared Gemara catalog, OSCAL in XML or YAML, bytes that
// are not valid JSON (including a damaged OSCAL file), another OSCAL model
// (a profile is the common mistake), or other JSON without the OSCAL root key.
func UnsupportedFormatError(format crosscodexv1.CatalogFormat, data []byte) error {
	const supported = "Only OSCAL JSON catalogs can be imported"
	if format == crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA {
		return fmt.Errorf("unsupported catalog format: Gemara catalogs cannot be imported yet. "+
			"%s; convert the document to an OSCAL catalog and import it with format OSCAL", supported)
	}
	// RawMessage validates the syntax without building the document tree.
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		trimmed := bytes.TrimSpace(data)
		if bytes.HasPrefix(trimmed, []byte("<")) {
			return fmt.Errorf("unsupported catalog format: the document is XML. %s; convert it to "+
				"OSCAL JSON (NIST's oscal-cli converts between OSCAL formats) and import the JSON file", supported)
		}
		// YAML may open with comments and a "---" document marker.
		yamlBody := trimmed
		for bytes.HasPrefix(yamlBody, []byte("#")) || bytes.HasPrefix(yamlBody, []byte("---")) {
			_, rest, _ := bytes.Cut(yamlBody, []byte("\n"))
			yamlBody = bytes.TrimSpace(rest)
		}
		if bytes.HasPrefix(yamlBody, []byte(OSCALRootKey+":")) || bytes.HasPrefix(yamlBody, []byte(oscal.ProfileRootKey+":")) {
			return fmt.Errorf("unsupported catalog format: the document is YAML. %s; convert it to "+
				"OSCAL JSON (NIST's oscal-cli converts between OSCAL formats) and import the JSON file", supported)
		}
		return fmt.Errorf("unsupported catalog format: the document is not valid JSON (%v). "+
			"%s; if this is an OSCAL catalog, fix the JSON syntax, otherwise convert it to an "+
			"OSCAL catalog, then import it again", err, supported)
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) == nil {
		if _, ok := root[oscal.ProfileRootKey]; ok {
			return fmt.Errorf("unsupported catalog format: the document is an OSCAL profile, not a catalog. "+
				"Resolve the profile into a catalog (NIST's oscal-cli resolves profiles), or import the "+
				"resolved-profile catalog its publisher provides. %s", supported)
		}
		for _, key := range oscal.ModelRootKeys() {
			if key == oscal.CatalogRootKey || key == oscal.ProfileRootKey {
				continue // a catalog never reaches here; a profile has its own advice above
			}
			if _, ok := root[key]; ok {
				return fmt.Errorf("unsupported catalog format: the document is an OSCAL %q, not a catalog. "+
					"%s; import the catalog whose controls it references instead", key, supported)
			}
		}
	}
	return fmt.Errorf("unsupported catalog format: the document is JSON but has no top-level %q object. "+
		"%s; convert the document to an OSCAL catalog and import it again", OSCALRootKey, supported)
}

// validateTree fails closed on a tree that would store dangling or ambiguous
// structure: duplicate item IDs or container keys, an item whose ParentID or
// ContainerKey names nothing, a container whose ParentKey names no container,
// self-parents, and item IDs containing "/" (the graph's container node IDs
// rely on their absence). Every error names the offender and says to fix the source
// catalog and re-import.
func validateTree(tree *oscal.Tree) error {
	containers := make(map[string]bool, len(tree.Containers))
	for _, c := range tree.Containers {
		if containers[c.Key] {
			return fmt.Errorf("duplicate container key %q: container keys must be unique within a catalog; "+
				"fix the source catalog's group structure and re-import", c.Key)
		}
		containers[c.Key] = true
	}
	for _, c := range tree.Containers {
		if c.ParentKey == c.Key {
			return fmt.Errorf("self-referential container parent: container %q names itself as its parent; "+
				"fix the source catalog's group structure and re-import", c.Key)
		}
		if c.ParentKey != "" && !containers[c.ParentKey] {
			return fmt.Errorf("container %q has unknown parent container %q; "+
				"fix the source catalog's group structure and re-import", c.Key, c.ParentKey)
		}
	}

	items := make(map[string]bool, len(tree.Items))
	for _, item := range tree.Items {
		if strings.Contains(item.ID, "/") {
			return fmt.Errorf("invalid item ID %q: IDs must be OSCAL tokens without \"/\" because graph node IDs are "+
				"\"<catalog>/<id>\" and a slash could collide with a container node; "+
				"fix the source catalog and re-import", item.ID)
		}
		if items[item.ID] {
			return fmt.Errorf("duplicate control ID %q: IDs must be unique across a catalog's controls and statement parts; "+
				"rename one in the source catalog and re-import", item.ID)
		}
		items[item.ID] = true
	}
	for _, item := range tree.Items {
		if item.ParentID == item.ID {
			return fmt.Errorf("self-referential parent: item %q names itself as its parent; "+
				"fix the source catalog and re-import", item.ID)
		}
		if item.ParentID != "" && !items[item.ParentID] {
			return fmt.Errorf("item %q has unknown parent %q; "+
				"fix the source catalog so the parent exists and re-import", item.ID, item.ParentID)
		}
		if item.ContainerKey != "" && !containers[item.ContainerKey] {
			return fmt.Errorf("item %q names unknown container %q; "+
				"fix the source catalog's group structure and re-import", item.ID, item.ContainerKey)
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

// formatStringToEnum maps the stored format string to CatalogFormat enum.
func formatStringToEnum(format string) crosscodexv1.CatalogFormat {
	switch strings.ToLower(format) {
	case FormatOSCAL:
		return crosscodexv1.CatalogFormat_CATALOG_FORMAT_OSCAL
	case FormatGemara:
		return crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA
	default:
		return crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED
	}
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

// catalogToProto converts rec like catalogRecordToProto and sets Current from
// the store. currentByDoc caches each document's current catalog ID so a list
// reads it once per document.
func (s *Service) catalogToProto(ctx context.Context, rec *CatalogRecord, currentByDoc map[string]string) (*crosscodexv1.Catalog, error) {
	pb := catalogRecordToProto(rec)
	if rec == nil || rec.DocumentSlug == "" {
		return pb, nil
	}
	currentID, ok := currentByDoc[rec.DocumentSlug]
	if !ok {
		current, err := s.store.GetDocumentCurrent(ctx, rec.DocumentSlug)
		switch {
		case err == nil:
			currentID = current.CatalogID
		case errors.Is(err, ErrDocumentNotFound):
			// A catalog naming a document that has no current version is not current.
		default:
			return nil, fmt.Errorf("get current version of document %q: %w", rec.DocumentSlug, err)
		}
		currentByDoc[rec.DocumentSlug] = currentID
	}
	pb.Current = currentID == rec.CatalogID
	return pb, nil
}

// catalogRecordToProto converts CatalogRecord to proto Catalog. Current is left
// false: it needs the store (see Service.catalogToProto).
func catalogRecordToProto(rec *CatalogRecord) *crosscodexv1.Catalog {
	if rec == nil {
		return nil
	}

	return &crosscodexv1.Catalog{
		CatalogId: rec.CatalogID,
		TenantContext: &crosscodexv1.TenantContext{
			TenantId: rec.TenantID,
		},
		Name:         rec.Name,
		Format:       formatStringToEnum(rec.Format),
		Version:      rec.Version,
		ControlCount: int32(rec.ControlCount),

		DocumentSlug:        rec.DocumentSlug,
		VersionLabel:        rec.VersionLabel,
		SupersedesCatalogId: rec.SupersedesCatalogID,
		LinkMethod:          rec.LinkMethod,
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
