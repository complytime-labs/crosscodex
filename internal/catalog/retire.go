package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Retire refusals. Callers classify them with errors.Is.
//
//   - ErrVersionProtected: a named version may not be deleted (see
//     RetireCatalogVersions for what protects one). ErrVersionIsCurrent, the
//     document's current version, wraps it.
//   - ErrVersionNotFound: the document has no version with a named label.
//   - ErrVersionLabelAmbiguous: two versions of the document share a label.
//   - ErrNoVersionLabels: the request names no label.
//   - ErrRetireUnavailable: this server is not wired to delete catalogs.
var (
	ErrVersionProtected      = errors.New("catalog version is protected")
	ErrVersionIsCurrent      = fmt.Errorf("%w: it is its document's current version", ErrVersionProtected)
	ErrVersionNotFound       = errors.New("catalog version not found")
	ErrVersionLabelAmbiguous = errors.New("catalog version label is ambiguous")
	ErrNoVersionLabels       = errors.New("no version labels given")
	ErrRetireUnavailable     = errors.New("catalog retire is not available on this server")
)

// retireAuditActor is the AuditRecord.Actor of retire's archive and purge
// records; the retention engine's own records say "retention-engine".
const retireAuditActor = "catalog-retire"

// Retention is what RetireCatalogVersions borrows from the retention engine.
// Every field is required; a Service without them refuses to retire with
// ErrRetireUnavailable.
type Retention struct {
	// Holds lists the active legal and compliance holds.
	Holds retention.HoldStore
	// Backends returns the tenant's Archiver and Purger. The Purger MUST be
	// backed by a purge_user connection (database.purge_dsn): only that role
	// may delete classifications and cached results and re-point the version
	// chain.
	Backends func(tenantID string) (retention.Archiver, retention.Purger, error)
	// Audit receives an archive and a purge record per retired version.
	Audit retention.AuditPublisher
}

func (r Retention) configured() bool {
	return r.Holds != nil && r.Backends != nil && r.Audit != nil
}

// WithRetention lets the service retire catalog versions. Wiring contract:
// cmd/crosscodexd sets it only for role all and role gateway, and only when
// database.purge_dsn is set, from the same retention wiring (holds, per-tenant archiver and
// purger, audit publisher) the retention engine uses; every other deployment
// leaves it unset, so retire fails closed. It has no pkg/config field of its
// own: database.purge_dsn and retention.archive decide it.
func WithRetention(r Retention) ServiceOption {
	return func(s *Service) {
		s.retention = r
	}
}

// RetireCatalogVersion is the RPC form of RetireCatalogVersions.
func (s *Service) RetireCatalogVersion(ctx context.Context, req *connect.Request[crosscodexv1.RetireCatalogVersionRequest]) (*connect.Response[crosscodexv1.RetireCatalogVersionResponse], error) {
	if req.Msg.GetTenantContext() == nil || req.Msg.GetTenantContext().GetTenantId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_context.tenant_id is required"))
	}
	ctx, err := tenant.WithTenant(ctx, req.Msg.GetTenantContext().GetTenantId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid tenant_id: %v", err))
	}

	ids, err := s.RetireCatalogVersions(ctx, req.Msg.GetDocumentSlug(), req.Msg.GetVersionLabel())
	switch {
	case err == nil:
		return connect.NewResponse(&crosscodexv1.RetireCatalogVersionResponse{RetiredCatalogIds: ids}), nil
	case errors.Is(err, ErrInvalidDocumentSlug), errors.Is(err, ErrInvalidVersionLabel), errors.Is(err, ErrNoVersionLabels):
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, ErrDocumentNotFound), errors.Is(err, ErrVersionNotFound):
		return nil, connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, ErrVersionProtected), errors.Is(err, ErrVersionLabelAmbiguous), errors.Is(err, ErrRetireUnavailable):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		return nil, s.internalError(ctx, "RetireCatalogVersion", err)
	}
}

// RetireCatalogVersions deletes the versions labels name of the context
// tenant's document slug and returns their catalog IDs in request order. It
// is all or nothing, in three steps:
//
//  1. Resolve every label. An unknown document or label fails (wrapping
//     ErrDocumentNotFound or ErrVersionNotFound), as does a label two versions
//     share (ErrVersionLabelAmbiguous). A repeated label is retired once.
//  2. Check every version. One is protected while it is the document's
//     current version (ErrVersionIsCurrent); an active hold covers it as a
//     retention.ClassCatalogs candidate; an active hold names a job whose
//     stored config uses it (jobconfig.ExtractCatalogs); a job whose config
//     uses it is not finished (its status is not completed, failed or
//     cancelled); or a job whose config uses it has attestation objects under
//     attestation/<job_id>/. The catalog's own ingest attestation
//     (attestation/ingest/...) does not protect it: it records the import, not
//     a result built on the version. A job config that is NULL or cannot be
//     read protects every version, because what it uses is unknown. Any protected version fails the whole request with
//     ErrVersionProtected, naming every blocker and what to do about it.
//  3. Archive each version with the retention Archiver, then delete them all
//     with one Purger.PurgeBatch transaction, which re-points the version
//     that superseded each to its predecessor. A failed archive deletes
//     nothing. Each version then gets an archive and a purge audit record.
//
// Graph Control and Container nodes are left in place: the graph is a
// rebuildable view of the relational tables, and retention does not purge it
// either. Errors name only the caller's tenant's holds, jobs and documents.
func (s *Service) RetireCatalogVersions(ctx context.Context, slug string, labels []string) (retired []string, err error) {
	start := time.Now()
	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "catalog.RetireCatalogVersions", trace.WithAttributes(
			attribute.String("catalog.document_slug", slug),
			attribute.Int("catalog.version_labels", len(labels)),
		))
		defer func() {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "retire failed")
			}
			span.End()
		}()
	}
	defer func() { s.recordRetire(ctx, start, len(retired), err) }()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("retire catalog versions: %w", err)
	}
	if s.tracer != nil {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("tenant.id", tenantID))
	}
	if err := ValidateDocumentSlug(slug); err != nil {
		return nil, err
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("%w: name at least one version label of %s to retire", ErrNoVersionLabels, slug)
	}
	for _, label := range labels {
		if err := ValidateVersionLabel(label); err != nil {
			return nil, err
		}
	}
	if !s.retention.configured() || s.store == nil || s.storage == nil {
		return nil, fmt.Errorf("%w: it needs the purge_user connection (database.purge_dsn) and the retention archive, which only role all wires. Run the retire against a crosscodexd role all server with database.purge_dsn and retention.archive configured", ErrRetireUnavailable)
	}

	targets, err := s.resolveVersions(ctx, slug, labels)
	if err != nil {
		return nil, err
	}
	if err := s.checkRetirable(ctx, tenantID, slug, targets); err != nil {
		return nil, err
	}
	return s.archiveAndPurge(ctx, tenantID, slug, targets)
}

// resolveVersions returns the version each label names, in label order with
// repeats dropped.
func (s *Service) resolveVersions(ctx context.Context, slug string, labels []string) ([]retireTarget, error) {
	versions, current, err := s.store.ListDocumentVersions(ctx, slug)
	if errors.Is(err, ErrDocumentNotFound) {
		return nil, fmt.Errorf("%w: document %q not found, so nothing was retired. List catalogs to find the document slug", err, slug)
	}
	if err != nil {
		return nil, fmt.Errorf("list versions of document %s: %w", slug, err)
	}
	seen := map[string]bool{}
	var targets []retireTarget
	for _, label := range labels {
		if seen[label] {
			continue
		}
		seen[label] = true
		var matches []CatalogRecord
		for _, v := range versions {
			if v.VersionLabel == label {
				matches = append(matches, v)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("%w: %s has no version %q, so nothing was retired. List catalogs to see its version labels", ErrVersionNotFound, slug, label)
		case 1:
			targets = append(targets, retireTarget{record: matches[0], label: label, current: matches[0].CatalogID == current})
		default:
			ids := make([]string, len(matches))
			for i, m := range matches {
				ids[i] = m.CatalogID
			}
			return nil, fmt.Errorf("%w: %d versions of %s are labelled %q (catalogs %s), so the label does not say which to retire and nothing was retired",
				ErrVersionLabelAmbiguous, len(matches), slug, label, strings.Join(ids, ", "))
		}
	}
	return targets, nil
}

// retireTarget is one resolved version and whether it is current.
type retireTarget struct {
	record  CatalogRecord
	label   string
	current bool
}

func (t retireTarget) candidate(tenantID string) retention.Candidate {
	return retention.Candidate{
		Store: retention.StorePostgres, Class: retention.ClassCatalogs,
		ID: t.record.CatalogID, TenantID: tenantID, CreatedAt: t.record.CreatedAt,
	}
}

// checkRetirable returns nil when no target is protected, else an error
// wrapping ErrVersionProtected (and ErrVersionIsCurrent when a target is
// current) that lists every blocker.
func (s *Service) checkRetirable(ctx context.Context, tenantID, slug string, targets []retireTarget) error {
	active, err := s.retention.Holds.Active(ctx, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("load active holds: %w", err)
	}
	heldJobs := map[string]string{} // job ID -> name of a hold naming it
	for _, h := range active {
		if h.Scope.JobID != "" {
			if _, ok := heldJobs[h.Scope.JobID]; !ok {
				heldJobs[h.Scope.JobID] = h.Name
			}
		}
	}

	ids := make([]string, len(targets))
	for i, t := range targets {
		ids[i] = t.record.CatalogID
	}
	jobs, unreadable, err := s.store.JobsUsingCatalogs(ctx, ids)
	if err != nil {
		return fmt.Errorf("list jobs using document %s: %w", slug, err)
	}
	var blockers []string
	for _, jobID := range unreadable {
		blockers = append(blockers, fmt.Sprintf("the stored config of job %s cannot be read, so retire cannot rule out that the job uses these versions; ask an operator to repair or purge that job", jobID))
	}

	anyCurrent := false
	for _, t := range targets {
		name := fmt.Sprintf("%s@%q", slug, t.label)
		if t.current {
			anyCurrent = true
			blockers = append(blockers, fmt.Sprintf("%s is the document's current version; import a newer version of %s first, or retire an older one", name, slug))
		}
		cand := t.candidate(tenantID)
		for _, h := range active {
			if h.Covers(cand) {
				blockers = append(blockers, fmt.Sprintf("legal hold %q covers %s; once the hold no longer applies, release it with `crosscodex admin hold release --name %s` and retry", h.Name, name, h.Name))
			}
		}
		for _, use := range jobs[t.record.CatalogID] {
			jobID := use.JobID
			if !IsTerminalJobStatus(use.Status) {
				blockers = append(blockers, fmt.Sprintf("job %s uses %s and is %q, so it may still read the version; wait for it to finish, or cancel it with `crosscodex run cancel %s`, and retry", jobID, name, use.Status, jobID))
			}
			if hold, ok := heldJobs[jobID]; ok {
				blockers = append(blockers, fmt.Sprintf("job %s uses %s and is under legal hold %q; release the hold before retiring the version", jobID, name, hold))
			}
			prefix := "attestation/" + jobID + "/"
			objects, err := s.storage.List(ctx, prefix)
			if err != nil {
				return fmt.Errorf("list attestations of job %s: %w", jobID, err)
			}
			if len(objects) > 0 {
				blockers = append(blockers, fmt.Sprintf("job %s uses %s and its attestations under %s record it, so it must stay verifiable; it can be retired once retention has purged that job's attestations", jobID, name, prefix))
			}
		}
	}
	if len(blockers) == 0 {
		return nil
	}
	sentinel := ErrVersionProtected
	if anyCurrent {
		sentinel = ErrVersionIsCurrent
	}
	return fmt.Errorf("%w: cannot retire from %s: %s. Nothing was retired", sentinel, slug, strings.Join(blockers, "; "))
}

// archiveAndPurge archives every target, then deletes them in one batch.
func (s *Service) archiveAndPurge(ctx context.Context, tenantID, slug string, targets []retireTarget) ([]string, error) {
	archiver, purger, err := s.retention.Backends(tenantID)
	if err != nil {
		return nil, fmt.Errorf("build retention backends: %w", err)
	}
	cands := make([]retention.Candidate, len(targets))
	for i, t := range targets {
		cands[i] = t.candidate(tenantID)
		if err := archiver.Archive(ctx, cands[i]); err != nil {
			if errors.Is(err, storage.ErrArchiveDisabled) {
				return nil, fmt.Errorf("%w: retire archives each version before deleting it, and retention.archive is not configured. Nothing was retired; configure retention.archive and retry: %w", ErrRetireUnavailable, err)
			}
			return nil, fmt.Errorf("archive %s@%q (catalog %s) before retiring it; nothing was retired: %w", slug, t.label, t.record.CatalogID, err)
		}
	}
	if err := purger.PurgeBatch(ctx, cands); err != nil {
		if errors.Is(err, retention.ErrCatalogIsCurrent) {
			return nil, fmt.Errorf("%w: a version became current while it was being retired; nothing was retired: %w", ErrVersionIsCurrent, err)
		}
		return nil, fmt.Errorf("delete versions of %s; nothing was retired: %w", slug, err)
	}

	now := time.Now().UTC()
	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = c.ID
		for _, action := range []string{retention.ActionArchive, retention.ActionPurge} {
			rec := retention.AuditRecord{
				Action: action, Actor: retireAuditActor, TenantID: tenantID, JobID: "retention",
				DataClass: string(retention.ClassCatalogs), Outcome: "ok", Detail: c.ID, At: now,
			}
			if err := s.retention.Audit.Publish(ctx, rec); err != nil {
				// The versions are gone either way; the log is the record.
				s.logger.ErrorContext(ctx, "publish catalog retire audit record", "action", action, "catalog_id", c.ID, "tenant", tenantID, "error", err)
			}
		}
	}
	s.logger.InfoContext(ctx, "retired catalog versions", "tenant", tenantID, "document_slug", slug, "catalog_ids", ids)
	return ids, nil
}

// recordRetire counts one retire call by outcome and records its duration.
func (s *Service) recordRetire(ctx context.Context, start time.Time, retired int, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	attrs := metric.WithAttributes(attribute.String("outcome", outcome))
	if s.retireOps != nil {
		s.retireOps.Add(ctx, 1, attrs)
	}
	if s.retiredVersions != nil {
		s.retiredVersions.Add(ctx, int64(retired))
	}
	if s.retireDuration != nil {
		s.retireDuration.Record(ctx, time.Since(start).Milliseconds(), attrs)
	}
}
