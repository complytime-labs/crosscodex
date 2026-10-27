package retention_test

import (
	"bytes"
	"context"
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
)

// newCorruptingProvider accepts Put calls but always returns different bytes
// from Get, simulating a write corruption to force ErrArchiveVerify.
func newCorruptingProvider() *storagetest.Fake {
	return &storagetest.Fake{
		GetFunc: func(context.Context, string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte("corrupted-bytes"))), nil
		},
	}
}

// archiveJSONConn returns a connection for archiveDB, which sets the
// candidate's tenant then serializes the row inside a transaction: Exec (the
// set_config) succeeds and QueryRow returns the canned JSON.
func archiveJSONConn(json []byte) *dbtest.Conn {
	return &dbtest.Conn{Tx: &dbtest.Tx{
		ExecFunc:     func(context.Context, string, ...any) error { return nil },
		QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(nil, json) },
	}}
}

var _ = Describe("Archiver", func() {
	ctx := context.Background()

	It("archives an object candidate and verifies its hash", func() {
		primary := &storagetest.Fake{}
		Expect(primary.Put(ctx, "k", bytes.NewReader([]byte("evidence")))).To(Succeed())
		archive := &storagetest.Fake{}
		a := retention.NewArchiver(nil, primary, archive)
		Expect(a.Archive(ctx, retention.Candidate{Store: retention.StoreObject, Class: retention.ClassAttestation, ObjectKey: "k"})).To(Succeed())
	})

	It("returns ErrArchiveVerify when the archived object copy does not match", func() {
		primary := &storagetest.Fake{}
		Expect(primary.Put(ctx, "k", bytes.NewReader([]byte("evidence")))).To(Succeed())
		a := retention.NewArchiver(nil, primary, newCorruptingProvider())
		Expect(a.Archive(ctx, retention.Candidate{Store: retention.StoreObject, Class: retention.ClassAttestation, ObjectKey: "k"})).
			To(MatchError(retention.ErrArchiveVerify))
	})

	It("serializes a DB job candidate and its children to the archive and verifies its hash", func() {
		// The aggregate archive doc carries the parent job plus its FK-child sets.
		conn := archiveJSONConn([]byte(`{"job":{"job_id":"job-old","status":"completed"},"job_stages":[{"stage_name":"collect"}],"vote_summaries":[],"analysis_results":[],"relationship_candidates":[],"requires_candidates":[{"source_id":"src-1","target_id":"tgt-1"}]}`))
		archive := &storagetest.Fake{}
		a := retention.NewArchiver(conn, nil, archive)
		Expect(a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassJobResults, ID: "job-old"})).To(Succeed())
		// assert the deterministic key was written
		body, ok := archive.Objects()["db/job_results/job-old.json"]
		Expect(ok).To(BeTrue())
		// the aggregate doc carries requires_candidates (job-scoped by job_id, no
		// FK) alongside the FK-child sets.
		Expect(string(body)).To(ContainSubstring("requires_candidates"))
	})

	It("returns ErrArchiveVerify when the archived DB copy does not match", func() {
		conn := archiveJSONConn([]byte(`{"job":{"job_id":"job-old"},"job_stages":[],"vote_summaries":[],"analysis_results":[],"relationship_candidates":[]}`))
		a := retention.NewArchiver(conn, nil, newCorruptingProvider())
		Expect(a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassJobResults, ID: "job-old"})).
			To(MatchError(retention.ErrArchiveVerify))
	})

	It("errors rather than archiving an empty doc when the parent job is absent", func() {
		// row_to_json over a missing parent yields a NULL 'job' field.
		conn := archiveJSONConn([]byte(`{"job":null,"job_stages":[],"vote_summaries":[],"analysis_results":[],"relationship_candidates":[]}`))
		archive := &storagetest.Fake{}
		a := retention.NewArchiver(conn, nil, archive)
		err := a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassJobResults, ID: "job-gone"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parent job"))
		Expect(err.Error()).To(ContainSubstring("job-gone"))
		Expect(archive.Objects()).To(BeEmpty(), "no doc may be written when the parent is missing")
	})

	It("serializes a DB catalog candidate with its classifications and controls to the archive", func() {
		// The catalog aggregate doc carries the parent catalog plus its
		// classifications and controls. embeddings are deliberately OMITTED
		// (derived/regenerable, purge-only) though the purge still deletes them.
		conn := archiveJSONConn([]byte(`{"catalog":{"catalog_id":"cat-1","name":"NIST"},"classifications":[{"control_id":"ac-1","type":"Technical"}],"controls":[{"control_id":"ac-1","title":"Access Control"}]}`))
		archive := &storagetest.Fake{}
		a := retention.NewArchiver(conn, nil, archive)
		Expect(a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassCatalogs, ID: "cat-1"})).To(Succeed())
		body, ok := archive.Objects()["db/catalogs/cat-1.json"]
		Expect(ok).To(BeTrue())
		Expect(string(body)).To(ContainSubstring("classifications"))
		Expect(string(body)).To(ContainSubstring("controls"))
		Expect(string(body)).NotTo(ContainSubstring("embeddings"))
	})

	It("errors rather than archiving an empty doc when the parent catalog is absent", func() {
		// row_to_json over a missing parent yields a NULL 'catalog' field.
		conn := archiveJSONConn([]byte(`{"catalog":null,"classifications":[],"controls":[]}`))
		archive := &storagetest.Fake{}
		a := retention.NewArchiver(conn, nil, archive)
		err := a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassCatalogs, ID: "cat-gone"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parent catalog"))
		Expect(err.Error()).To(ContainSubstring("cat-gone"))
		Expect(archive.Objects()).To(BeEmpty(), "no doc may be written when the parent is missing")
	})

	It("errors for an unsupported DB archival class", func() {
		// embeddings are never collected as a standalone class (purge-only as a
		// catalog dependent); a candidate that somehow carries it is unsupported.
		archive := &storagetest.Fake{}
		a := retention.NewArchiver(archiveJSONConn(nil), nil, archive)
		err := a.Archive(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassEmbeddings, ID: "e1"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unsupported"))
		Expect(archive.Objects()).To(BeEmpty())
	})
})
