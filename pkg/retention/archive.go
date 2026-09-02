package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// ErrArchiveVerify is returned when the archived copy's content hash does not
// match the source, indicating write corruption or a provider bug.
var ErrArchiveVerify = errors.New("archive verification failed: content hash mismatch")

// dbAggregateQueries holds, per aggregate class, the single SELECT that builds
// the archive document: the parent row plus each FK-child set, keyed by the
// parent id bound as $1. It is the constant SQL twin of the dbAggregates map in
// purge.go (which drives the ordered deletes) — the two are kept as a matched
// pair so the archived set and the purged set cannot drift. Every identifier is
// a compile-time constant; c.ID is ONLY ever the bound $1. Child sets are ordered
// deterministically so re-archival of an unchanged parent is byte-stable, and each
// COALESCEs to '[]' so an empty child set serializes as [] rather than null.
//
// The catalog aggregate deliberately OMITS embeddings: they are derived and
// regenerable, so archival is a non-goal. The purge (dbAggregates in purge.go)
// still deletes embeddings as a catalog dependent — this archive⊆purge asymmetry
// is intentional; do not "fix" it by adding embeddings to the archive doc.
var dbAggregateQueries = map[DataClass]string{
	ClassJobResults: `SELECT json_build_object(
  'job',                     (SELECT row_to_json(j) FROM jobs j WHERE j.job_id = $1),
  'job_stages',              COALESCE((SELECT json_agg(row_to_json(s) ORDER BY s.stage_name)              FROM job_stages s              WHERE s.job_id = $1), '[]'::json),
  'vote_summaries',          COALESCE((SELECT json_agg(row_to_json(v) ORDER BY v.source_id, v.target_id)  FROM vote_summaries v          WHERE v.job_id = $1), '[]'::json),
  'analysis_results',        COALESCE((SELECT json_agg(row_to_json(a) ORDER BY a.analyzer_name)           FROM analysis_results a        WHERE a.job_id = $1), '[]'::json),
  'relationship_candidates', COALESCE((SELECT json_agg(row_to_json(r) ORDER BY r.source_id, r.target_id)  FROM relationship_candidates r WHERE r.job_id = $1), '[]'::json),
  'requires_candidates',     COALESCE((SELECT json_agg(row_to_json(rq) ORDER BY rq.source_id, rq.target_id) FROM requires_candidates rq WHERE rq.job_id = $1), '[]'::json)
)`,
	ClassCatalogs: `SELECT json_build_object(
  'catalog',         (SELECT row_to_json(c) FROM catalogs c WHERE c.catalog_id = $1),
  'classifications', COALESCE((SELECT json_agg(row_to_json(cl) ORDER BY cl.control_id, cl.type) FROM classifications cl WHERE cl.catalog_id = $1), '[]'::json),
  'controls',        COALESCE((SELECT json_agg(row_to_json(ct) ORDER BY ct.control_id)          FROM controls ct        WHERE ct.catalog_id = $1), '[]'::json)
)`,
}

// dbAggregateParentKeys names, per aggregate class, the JSON key holding the
// parent row inside the archive doc built by dbAggregateQueries. archiveDB uses
// it to detect a missing parent (a NULL parent field from row_to_json over no
// rows) and refuse to persist an empty document. Every class in
// dbAggregateQueries MUST have an entry here.
var dbAggregateParentKeys = map[DataClass]string{
	ClassJobResults: "job",
	ClassCatalogs:   "catalog",
}

// Archiver copies or serializes expired candidates to cold storage and verifies
// each copy by content hash before the Purger may delete the source.
type Archiver interface {
	Archive(ctx context.Context, c Candidate) error
}

type archiver struct {
	conn    db.Connection
	primary storage.Provider
	archive storage.Provider
}

// NewArchiver creates an Archiver backed by archive for cold storage. primary is
// used for object reads; conn is used for DB-row serialization.
func NewArchiver(conn db.Connection, primary, archive storage.Provider) Archiver {
	return &archiver{conn: conn, primary: primary, archive: archive}
}

// Archive dispatches to the appropriate archival path based on c.Store and c.Class.
// It writes the data to cold storage, reads it back, and compares content hashes.
// ErrArchiveVerify is returned on hash mismatch.
func (a *archiver) Archive(ctx context.Context, c Candidate) error {
	switch c.Store {
	case StoreObject:
		return a.archiveObject(ctx, c)
	case StorePostgres:
		return a.archiveDB(ctx, c)
	default:
		return fmt.Errorf("retention archiver: unsupported store %q", c.Store)
	}
}

// archiveObject copies a blob from primary to archive and verifies the copy.
func (a *archiver) archiveObject(ctx context.Context, c Candidate) error {
	rc, err := a.primary.Get(ctx, c.ObjectKey)
	if err != nil {
		return fmt.Errorf("retention archiver: read primary %q: %w", c.ObjectKey, err)
	}
	src, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return fmt.Errorf("retention archiver: read primary body %q: %w", c.ObjectKey, err)
	}

	if err := a.archive.Put(ctx, c.ObjectKey, bytes.NewReader(src)); err != nil {
		return fmt.Errorf("retention archiver: put archive %q: %w", c.ObjectKey, err)
	}

	rc2, err := a.archive.Get(ctx, c.ObjectKey)
	if err != nil {
		return fmt.Errorf("retention archiver: read-back archive %q: %w", c.ObjectKey, err)
	}
	dst, err := io.ReadAll(rc2)
	rc2.Close()
	if err != nil {
		return fmt.Errorf("retention archiver: read-back body %q: %w", c.ObjectKey, err)
	}

	if storage.ContentHash(src) != storage.ContentHash(dst) {
		return ErrArchiveVerify
	}
	return nil
}

// archiveDB serializes a DB aggregate (parent row plus its FK children) to JSON
// and writes it to archive, then verifies the copy. A class absent from
// dbAggregateQueries is unsupported and returns an error rather than silently
// proceeding.
func (a *archiver) archiveDB(ctx context.Context, c Candidate) error {
	query, ok := dbAggregateQueries[c.Class]
	if !ok {
		return fmt.Errorf("retention archiver: unsupported DB archival class %q", c.Class)
	}

	tx, err := a.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("retention archiver: begin: %w", err)
	}
	// Rollback after a successful Commit is a harmless no-op (pgx returns
	// ErrTxClosed, which we discard); the guard also covers the error paths.
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", c.TenantID); err != nil {
		return fmt.Errorf("retention archiver: set tenant: %w", err)
	}

	var payload []byte
	if err := tx.QueryRow(ctx, query, c.ID).Scan(&payload); err != nil {
		return fmt.Errorf("retention archiver: serialize %s/%s: %w", c.Class, c.ID, err)
	}

	// A missing parent row surfaces as a NULL parent field (row_to_json over no
	// rows). Archiving that would persist an empty document and let the purge
	// delete children with no parent audit trail, so treat it as an error rather
	// than a silent no-op. The parent key differs per class (jobs→"job",
	// catalogs→"catalog"), so look it up rather than hard-coding one.
	parentKey := dbAggregateParentKeys[c.Class]
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(payload, &doc); err != nil {
		return fmt.Errorf("retention archiver: serialize %s/%s: %w", c.Class, c.ID, err)
	}
	parent := doc[parentKey]
	if len(parent) == 0 || string(parent) == "null" {
		return fmt.Errorf("retention archiver: serialize %s/%s: parent %s %s not found", c.Class, c.ID, parentKey, c.ID)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("retention archiver: commit: %w", err)
	}

	key := fmt.Sprintf("db/%s/%s.json", c.Class, c.ID)
	if err := a.archive.Put(ctx, key, bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("retention archiver: put archive %q: %w", key, err)
	}

	rc, err := a.archive.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("retention archiver: read-back archive %q: %w", key, err)
	}
	readback, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return fmt.Errorf("retention archiver: read-back body %q: %w", key, err)
	}

	if storage.ContentHash(payload) != storage.ContentHash(readback) {
		return ErrArchiveVerify
	}
	return nil
}
