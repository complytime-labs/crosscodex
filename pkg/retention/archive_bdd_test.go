package retention_test

import (
	"bytes"
	"context"
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// archiveMapProvider is an in-memory storage.Provider keyed by object key.
// Distinct from the fakeProvider in collector_bdd_test.go (which uses []ObjectMetadata).
type archiveMapProvider struct {
	objects map[string][]byte
}

func newArchiveMapProvider(seed map[string][]byte) *archiveMapProvider {
	m := make(map[string][]byte)
	for k, v := range seed {
		m[k] = v
	}
	return &archiveMapProvider{objects: m}
}

func (p *archiveMapProvider) Get(_ context.Context, key string) (io.ReadCloser, error) {
	v, ok := p.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(v)), nil
}

func (p *archiveMapProvider) Put(_ context.Context, key string, data io.Reader) error {
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	p.objects[key] = b
	return nil
}

func (p *archiveMapProvider) Delete(_ context.Context, _ string) error { return nil }
func (p *archiveMapProvider) List(_ context.Context, _ string) ([]storage.ObjectMetadata, error) {
	return nil, nil
}
func (p *archiveMapProvider) Exists(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (p *archiveMapProvider) Stat(_ context.Context, _ string) (*storage.ObjectMetadata, error) {
	return nil, nil
}
func (p *archiveMapProvider) Close() error { return nil }

// corruptingProvider accepts Put calls but always returns different bytes from Get,
// simulating a write corruption to force ErrArchiveVerify.
type corruptingProvider struct{}

func (c *corruptingProvider) Get(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader([]byte("corrupted-bytes"))), nil
}
func (c *corruptingProvider) Put(_ context.Context, _ string, _ io.Reader) error { return nil }
func (c *corruptingProvider) Delete(_ context.Context, _ string) error           { return nil }
func (c *corruptingProvider) List(_ context.Context, _ string) ([]storage.ObjectMetadata, error) {
	return nil, nil
}
func (c *corruptingProvider) Exists(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (c *corruptingProvider) Stat(_ context.Context, _ string) (*storage.ObjectMetadata, error) {
	return nil, nil
}
func (c *corruptingProvider) Close() error { return nil }

// archiveJSONConn is a fake db.Connection that returns canned JSON via QueryRow.
// Distinct from the fakeConn in collector_bdd_test.go (which routes Query).
type archiveJSONConn struct {
	json []byte
}

type archiveJSONRow struct{ json []byte }

func (r archiveJSONRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if p, ok := dest[0].(*[]byte); ok {
			*p = r.json
		}
	}
	return nil
}

func (c *archiveJSONConn) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	return archiveJSONRow{json: c.json}
}
func (c *archiveJSONConn) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return nil, nil
}
func (c *archiveJSONConn) Exec(_ context.Context, _ string, _ ...any) error { return nil }
func (c *archiveJSONConn) Begin(_ context.Context) (db.Transaction, error) {
	return &archiveJSONTx{json: c.json}, nil
}
func (c *archiveJSONConn) Close() error { return nil }

// archiveJSONTx implements db.Transaction for archiveDB, which now sets the
// candidate's tenant then serializes the row inside a transaction. Exec (the
// set_config) is a no-op; QueryRow returns the canned JSON row.
type archiveJSONTx struct {
	json []byte
}

func (t *archiveJSONTx) Exec(_ context.Context, _ string, _ ...any) error { return nil }
func (t *archiveJSONTx) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	return archiveJSONRow{json: t.json}
}
func (t *archiveJSONTx) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return nil, nil
}
func (t *archiveJSONTx) Commit() error   { return nil }
func (t *archiveJSONTx) Rollback() error { return nil }

var _ = Describe("Archiver", func() {
	ctx := context.Background()

	It("archives an object candidate and verifies its hash", func() {
		primary := newArchiveMapProvider(map[string][]byte{"k": []byte("evidence")})
		archive := newArchiveMapProvider(nil)
		a := retention.NewArchiver(nil, primary, archive)
		Expect(a.Archive(ctx, retention.Candidate{Store: retention.StoreObject, Class: retention.ClassAttestation, ObjectKey: "k"})).To(Succeed())
	})

	It("returns ErrArchiveVerify when the archived object copy does not match", func() {
		primary := newArchiveMapProvider(map[string][]byte{"k": []byte("evidence")})
		a := retention.NewArchiver(nil, primary, &corruptingProvider{})
		Expect(a.Archive(ctx, retention.Candidate{Store: retention.StoreObject, Class: retention.ClassAttestation, ObjectKey: "k"})).
			To(MatchError(retention.ErrArchiveVerify))
	})

	It("serializes a DB job candidate and its children to the archive and verifies its hash", func() {
		// The aggregate archive doc carries the parent job plus its FK-child sets.
		conn := &archiveJSONConn{json: []byte(`{"job":{"job_id":"job-old","status":"completed"},"job_stages":[{"stage_name":"collect"}],"vote_summaries":[],"analysis_results":[],"relationship_candidates":[],"requires_candidates":[{"source_id":"src-1","target_id":"tgt-1"}]}`)}
		archive := newArchiveMapProvider(nil)
		a := retention.NewArchiver(conn, nil, archive)
		Expect(a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassJobResults, ID: "job-old"})).To(Succeed())
		// assert the deterministic key was written
		body, ok := archive.objects["db/job_results/job-old.json"]
		Expect(ok).To(BeTrue())
		// the aggregate doc carries requires_candidates (job-scoped by job_id, no
		// FK) alongside the FK-child sets.
		Expect(string(body)).To(ContainSubstring("requires_candidates"))
	})

	It("returns ErrArchiveVerify when the archived DB copy does not match", func() {
		conn := &archiveJSONConn{json: []byte(`{"job":{"job_id":"job-old"},"job_stages":[],"vote_summaries":[],"analysis_results":[],"relationship_candidates":[]}`)}
		a := retention.NewArchiver(conn, nil, &corruptingProvider{})
		Expect(a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassJobResults, ID: "job-old"})).
			To(MatchError(retention.ErrArchiveVerify))
	})

	It("errors rather than archiving an empty doc when the parent job is absent", func() {
		// row_to_json over a missing parent yields a NULL 'job' field.
		conn := &archiveJSONConn{json: []byte(`{"job":null,"job_stages":[],"vote_summaries":[],"analysis_results":[],"relationship_candidates":[]}`)}
		archive := newArchiveMapProvider(nil)
		a := retention.NewArchiver(conn, nil, archive)
		err := a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassJobResults, ID: "job-gone"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parent job"))
		Expect(err.Error()).To(ContainSubstring("job-gone"))
		Expect(archive.objects).To(BeEmpty(), "no doc may be written when the parent is missing")
	})

	It("serializes a DB catalog candidate with its classifications and controls to the archive", func() {
		// The catalog aggregate doc carries the parent catalog plus its
		// classifications and controls. embeddings are deliberately OMITTED
		// (derived/regenerable, purge-only) though the purge still deletes them.
		conn := &archiveJSONConn{json: []byte(`{"catalog":{"catalog_id":"cat-1","name":"NIST"},"classifications":[{"control_id":"ac-1","type":"Technical"}],"controls":[{"control_id":"ac-1","title":"Access Control"}]}`)}
		archive := newArchiveMapProvider(nil)
		a := retention.NewArchiver(conn, nil, archive)
		Expect(a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassCatalogs, ID: "cat-1"})).To(Succeed())
		body, ok := archive.objects["db/catalogs/cat-1.json"]
		Expect(ok).To(BeTrue())
		Expect(string(body)).To(ContainSubstring("classifications"))
		Expect(string(body)).To(ContainSubstring("controls"))
		Expect(string(body)).NotTo(ContainSubstring("embeddings"))
	})

	It("errors rather than archiving an empty doc when the parent catalog is absent", func() {
		// row_to_json over a missing parent yields a NULL 'catalog' field.
		conn := &archiveJSONConn{json: []byte(`{"catalog":null,"classifications":[],"controls":[]}`)}
		archive := newArchiveMapProvider(nil)
		a := retention.NewArchiver(conn, nil, archive)
		err := a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassCatalogs, ID: "cat-gone"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parent catalog"))
		Expect(err.Error()).To(ContainSubstring("cat-gone"))
		Expect(archive.objects).To(BeEmpty(), "no doc may be written when the parent is missing")
	})

	It("errors for an unsupported DB archival class", func() {
		// embeddings are never collected as a standalone class (purge-only as a
		// catalog dependent); a candidate that somehow carries it is unsupported.
		archive := newArchiveMapProvider(nil)
		a := retention.NewArchiver(&archiveJSONConn{}, nil, archive)
		err := a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassEmbeddings, ID: "e1"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unsupported"))
		Expect(archive.objects).To(BeEmpty())
	})
})
