package graph

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// catalogSubject matches natsbus.CatalogParsedSubject for every tenant.
const catalogSubject = "crosscodex.catalog.*.*.parsed"

// catalogAnalyzer labels catalog projection in the shared event and
// materialize metrics, alongside the analyzer names.
const catalogAnalyzer = "catalog"

// handleCatalogEvent projects a parsed catalog's controls into Control nodes
// and PARENT_OF edges. The tenant and catalog come from the subject and the
// content from the tenant's controls rows, so the payload is never trusted.
// Core NATS does not redeliver: a failed projection stays absent until the
// catalog is re-imported, which publishes the event again.
func (s *Service) handleCatalogEvent(ctx context.Context, msg *natsbus.Message) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "graph.materialize.catalog")
	defer span.End()

	tenantID, catalogID, err := parseCatalogSubject(msg.Subject)
	if err == nil {
		err = tenant.ValidateTenantID(tenantID)
	}
	if err == nil {
		ctx, err = tenant.WithTenant(ctx, tenantID)
	}
	if err != nil {
		s.logger.ErrorContext(ctx, "invalid catalog event subject", "subject", msg.Subject, "error", err)
		s.recordEvent(ctx, catalogAnalyzer, "error")
		span.SetStatus(codes.Error, err.Error())
		return nil // a malformed subject never becomes valid
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID), attribute.String("catalog.id", catalogID))

	start := time.Now()
	err = s.projectCatalog(ctx, tenantID, catalogID)
	s.recordMaterialize(ctx, catalogAnalyzer, start)
	if err != nil {
		s.recordEvent(ctx, catalogAnalyzer, "error")
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("project catalog %s: %w", catalogID, err) // logged by natsbus; core NATS does not redeliver
	}
	s.recordEvent(ctx, catalogAnalyzer, "ok")
	span.SetStatus(codes.Ok, "")
	return nil
}

func (s *Service) projectCatalog(ctx context.Context, tenantID, catalogID string) error {
	controls, err := s.controls.ListCatalogControls(ctx, catalogID)
	if err != nil {
		return err
	}
	// Ingest publishes the parsed event only after storing a non-empty
	// control set, so no rows means the event outran or outlived its data.
	if len(controls) == 0 {
		return fmt.Errorf("catalog %s has no controls for tenant %s, although ingest publishes only after storing them; re-import the catalog to project it", catalogID, tenantID)
	}

	// Every node before any edge, so a child listed ahead of its parent still
	// gets its PARENT_OF edge.
	now := time.Now().UTC()
	var errs []error
	for _, c := range controls {
		node := graphdb.Node{
			ID:        c.ControlID,
			Label:     "Control",
			ValidFrom: now,
			Properties: map[string]any{
				"tenant_id":  tenantID,
				"catalog_id": c.CatalogID,
				"identifier": c.Identifier,
				"title":      c.Title,
				"statement":  c.Statement,
				"class":      c.Class,
			},
		}
		// Upsert: the row is authoritative, so a re-import refreshes the node.
		if _, err := s.graph.UpsertNode(ctx, tenantID, node); err != nil {
			errs = append(errs, fmt.Errorf("upsert Control node %s: %w", c.ControlID, err))
		}
	}

	for _, c := range controls {
		if c.ParentID == "" {
			continue
		}
		sourceID := c.CatalogID + "/" + c.ParentID
		edge := graphdb.Edge{
			ID:        graphdb.DerivedID("parent-of", sourceID, c.ControlID),
			Label:     "PARENT_OF",
			ValidFrom: now,
		}
		err := s.graph.CreateEdge(ctx, tenantID, sourceID, c.ControlID, edge)
		if err == nil || errors.Is(err, graphdb.ErrEdgeExists) ||
			s.skipMissingEndpoint(ctx, tenantID, catalogAnalyzer, "PARENT_OF", sourceID, c.ControlID, err) {
			continue
		}
		errs = append(errs, fmt.Errorf("create PARENT_OF %s -> %s: %w", sourceID, c.ControlID, err))
	}
	return errors.Join(errs...)
}

// parseCatalogSubject splits crosscodex.catalog.{tenant}.{catalog_id}.parsed.
// It rejects a catalog ID that natsbus.CatalogParsedSubject could not have
// built (empty or a wildcard); the caller validates the tenant.
func parseCatalogSubject(subject string) (tenantID, catalogID string, err error) {
	parts := strings.Split(subject, ".")
	if len(parts) != 5 || parts[0] != "crosscodex" || parts[1] != "catalog" || parts[4] != "parsed" ||
		parts[3] == "" || strings.ContainsAny(parts[3], "*>") {
		return "", "", fmt.Errorf("catalog event subject %q does not match crosscodex.catalog.<tenant>.<catalog_id>.parsed", subject)
	}
	return parts[2], parts[3], nil
}
