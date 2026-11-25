package retention_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
)

// purgeTx returns a transaction that records every Exec and fails one whose
// SQL contains failOn (never, when failOn is empty).
func purgeTx(failOn string) *dbtest.Tx {
	return &dbtest.Tx{ExecFunc: func(_ context.Context, q string, _ ...any) error {
		if failOn != "" && strings.Contains(q, failOn) {
			return fmt.Errorf("injected failure on %q", failOn)
		}
		return nil
	}}
}

// catalogPurgeTx is purgeTx for catalog candidates: the purge asks whether a
// catalog is a document's current version, and currentDoc maps the catalog IDs
// that are to their document slug.
func catalogPurgeTx(failOn string, currentDoc map[string]string) *dbtest.Tx {
	tx := purgeTx(failOn)
	tx.QueryRowFunc = func(_ context.Context, q string, args ...any) db.Row {
		if !strings.Contains(q, "catalog_documents") || len(args) != 1 {
			return dbtest.Row(fmt.Errorf("unexpected query %q", q))
		}
		if slug, ok := currentDoc[args[0].(string)]; ok {
			return dbtest.Row(nil, slug)
		}
		return dbtest.Row(sql.ErrNoRows)
	}
	return tx
}

// countExecs returns how many recorded statements contain substr.
func countExecs(tx *dbtest.Tx, substr string) int {
	n := 0
	for _, e := range tx.Execs() {
		if strings.Contains(e.Query, substr) {
			n++
		}
	}
	return n
}

// deleteRecorder records every key passed to Delete and then deletes it. It is
// for a single goroutine only: Purge deletes sequentially, so deleted is not
// guarded.
type deleteRecorder struct {
	*storagetest.Fake
	deleted []string
}

func (r *deleteRecorder) Delete(ctx context.Context, key string) error {
	r.deleted = append(r.deleted, key)
	return r.Fake.Delete(ctx, key)
}

var _ = Describe("Purger", func() {
	ctx := context.Background()

	It("sets the tenant then deletes the FK children before the parent in one transaction, no purge GUC", func() {
		tx := purgeTx("")
		conn := &dbtest.Conn{Tx: tx}
		Expect(retention.NewPurger(conn, nil).Purge(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassJobResults, ID: "job-old", TenantID: "t1"})).To(Succeed())
		// set_config, then one DELETE per job child in FK-safe order (the four FK
		// children plus requires_candidates, which is job-scoped by column with no
		// FK), then the parent job last. job_stage_progress goes first: it is the
		// per-task progress of the stages that follow it.
		Expect(tx.Execs()).To(HaveLen(11))
		Expect(tx.Execs()[0].Query).To(ContainSubstring("app.current_tenant"))
		Expect(tx.Execs()[1].Query).To(ContainSubstring("DELETE FROM job_stage_progress"))
		Expect(tx.Execs()[2].Query).To(ContainSubstring("DELETE FROM job_stages"))
		Expect(tx.Execs()[3].Query).To(ContainSubstring("DELETE FROM vote_summaries"))
		Expect(tx.Execs()[4].Query).To(ContainSubstring("DELETE FROM analysis_results"))
		Expect(tx.Execs()[5].Query).To(ContainSubstring("DELETE FROM relationship_candidates"))
		Expect(tx.Execs()[6].Query).To(ContainSubstring("DELETE FROM requires_candidates"))
		// The results the job stored for reuse and its cache audit record
		// go with it, so every surviving hit still names an existing write.
		Expect(tx.Execs()[7].Query).To(ContainSubstring("DELETE FROM llm_result_cache"))
		Expect(tx.Execs()[8].Query).To(ContainSubstring("DELETE FROM job_cache_entries"))
		Expect(tx.Execs()[9].Query).To(ContainSubstring("DELETE FROM job_cache_stats"))
		Expect(tx.Execs()[10].Query).To(ContainSubstring("DELETE FROM jobs"))
		// Every child+parent delete keys on job_id via a bound $1; the id is never
		// string-interpolated into the SQL text.
		for _, e := range tx.Execs()[1:] {
			Expect(e.Query).To(ContainSubstring("job_id"))
			Expect(e.Query).To(ContainSubstring("$1"))
			Expect(e.Query).NotTo(ContainSubstring("job-old"))
		}
		for _, e := range tx.Execs() {
			Expect(e.Query).NotTo(ContainSubstring("crosscodex.retention_purge"))
		}
		Expect(tx.Commits()).To(Equal(1))
	})

	It("sets the tenant, re-points the version lineage, then deletes the catalog's FK children before the catalog in one transaction", func() {
		tx := catalogPurgeTx("", nil)
		Expect(retention.NewPurger(&dbtest.Conn{Tx: tx}, nil).Purge(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassCatalogs, ID: "cat-1", TenantID: "t1"})).To(Succeed())
		// set_config, then the successor's supersedes link moves to the purged
		// catalog's predecessor (so the lineage foreign key never blocks the
		// delete and the chain stays intact), then one DELETE per FK child
		// (classifications, embeddings, controls, catalog_containers) in
		// FK-safe order, then the parent catalog last. embeddings is deleted
		// here though it is deliberately absent from the archive doc.
		Expect(tx.Execs()).To(HaveLen(9))
		Expect(tx.Execs()[0].Query).To(ContainSubstring("app.current_tenant"))
		Expect(tx.Execs()[1].Query).To(ContainSubstring("UPDATE catalogs SET supersedes_catalog_id"))
		Expect(tx.Execs()[1].Args).To(Equal([]any{"cat-1"}))
		Expect(tx.Execs()[2].Query).To(ContainSubstring("DELETE FROM classifications"))
		Expect(tx.Execs()[3].Query).To(ContainSubstring("DELETE FROM embeddings"))
		Expect(tx.Execs()[4].Query).To(ContainSubstring("DELETE FROM controls"))
		Expect(tx.Execs()[5].Query).To(ContainSubstring("DELETE FROM catalog_containers"))
		// A cached result is purged with either catalog whose text its
		// request carried.
		Expect(tx.Execs()[6].Query).To(ContainSubstring("DELETE FROM llm_result_cache WHERE source_catalog_id = $1"))
		Expect(tx.Execs()[7].Query).To(ContainSubstring("DELETE FROM llm_result_cache WHERE target_catalog_id = $1"))
		Expect(tx.Execs()[8].Query).To(ContainSubstring("DELETE FROM catalogs"))
		// Every statement after set_config keys on the catalog id via a bound
		// $1; the id is never string-interpolated into the SQL text.
		for _, e := range tx.Execs()[1:] {
			Expect(e.Query).To(ContainSubstring("catalog_id"))
			Expect(e.Query).To(ContainSubstring("$1"))
			Expect(e.Query).NotTo(ContainSubstring("cat-1"))
		}
		Expect(tx.Commits()).To(Equal(1))
	})

	It("refuses to purge a document's current version and changes nothing", func() {
		tx := catalogPurgeTx("", map[string]string{"cat-1": "nist-800-53"})
		err := retention.NewPurger(&dbtest.Conn{Tx: tx}, nil).Purge(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassCatalogs, ID: "cat-1", TenantID: "t1"})
		Expect(err).To(MatchError(retention.ErrCatalogIsCurrent))
		Expect(err.Error()).To(ContainSubstring(`catalog cat-1 is the current version of document "nist-800-53"`))
		Expect(err.Error()).To(ContainSubstring("import a newer version of the document"))
		Expect(countExecs(tx, "UPDATE")).To(BeZero())
		Expect(countExecs(tx, "DELETE")).To(BeZero())
		Expect(tx.Commits()).To(Equal(0))
		Expect(tx.Rollbacks()).To(Equal(1))
	})

	Describe("PurgeBatch", func() {
		catalogCandidate := func(id, tenantID string) retention.Candidate {
			return retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassCatalogs, ID: id, TenantID: tenantID}
		}

		It("purges every candidate in one transaction scoped to their tenant", func() {
			tx := catalogPurgeTx("", nil)
			conn := &dbtest.Conn{Tx: tx}
			Expect(retention.NewPurger(conn, nil).PurgeBatch(ctx, []retention.Candidate{
				catalogCandidate("cat-1", "t1"), catalogCandidate("cat-2", "t1"),
			})).To(Succeed())
			Expect(conn.Begins()).To(Equal(1))
			Expect(countExecs(tx, "app.current_tenant")).To(Equal(1))
			Expect(tx.Execs()[0].Args).To(Equal([]any{"t1"}))
			Expect(countExecs(tx, "UPDATE catalogs SET supersedes_catalog_id")).To(Equal(2))
			Expect(countExecs(tx, "DELETE FROM catalogs")).To(Equal(2))
			Expect(tx.Commits()).To(Equal(1))
		})

		It("rolls the whole batch back when one candidate is a document's current version", func() {
			tx := catalogPurgeTx("", map[string]string{"cat-2": "doc"})
			err := retention.NewPurger(&dbtest.Conn{Tx: tx}, nil).PurgeBatch(ctx, []retention.Candidate{
				catalogCandidate("cat-1", "t1"), catalogCandidate("cat-2", "t1"),
			})
			Expect(err).To(MatchError(retention.ErrCatalogIsCurrent))
			Expect(err.Error()).To(ContainSubstring("catalog cat-2"))
			Expect(tx.Commits()).To(Equal(0))
			Expect(tx.Rollbacks()).To(Equal(1))
		})

		DescribeTable("refuses a batch it cannot purge in one transaction, before beginning one",
			func(cs []retention.Candidate, wantMsg string) {
				conn := &dbtest.Conn{Tx: catalogPurgeTx("", nil)}
				err := retention.NewPurger(conn, nil).PurgeBatch(ctx, cs)
				Expect(err).To(MatchError(ContainSubstring(wantMsg)))
				Expect(conn.Begins()).To(BeZero())
			},
			Entry("an empty batch", []retention.Candidate(nil), "purge batch is empty"),
			Entry("candidates of two tenants",
				[]retention.Candidate{catalogCandidate("cat-1", "t1"), catalogCandidate("cat-2", "t2")},
				`candidate cat-2 belongs to tenant "t2", not "t1"`),
			Entry("an object-store candidate",
				[]retention.Candidate{catalogCandidate("cat-1", "t1"), {Store: retention.StoreObject, Class: retention.ClassAttestation, ID: "k", ObjectKey: "k", TenantID: "t1"}},
				"candidate k is in store object"),
		)
	})

	DescribeTable("rolls back and returns an error",
		func(failOn string, class retention.DataClass) {
			tx := purgeTx(failOn)
			err := retention.NewPurger(&dbtest.Conn{Tx: tx}, nil).Purge(ctx, retention.Candidate{Store: retention.StorePostgres, Class: class, ID: "id", TenantID: "t1"})
			Expect(err).To(HaveOccurred())
			Expect(tx.Commits()).To(Equal(0))
			Expect(tx.Rollbacks()).To(Equal(1))
		},
		Entry("for an unsupported DB class", "", retention.ClassEmbeddings),
		Entry("when the delete fails", "DELETE", retention.ClassJobResults),
		Entry("when set_config fails", "set_config", retention.ClassJobResults),
	)

	DescribeTable("rolls back a catalog purge whose version-chain unlink fails, before any delete",
		func(newTx func() *dbtest.Tx, wantMsg string) {
			tx := newTx()
			err := retention.NewPurger(&dbtest.Conn{Tx: tx}, nil).Purge(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassCatalogs, ID: "cat-1", TenantID: "t1"})
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
			Expect(countExecs(tx, "DELETE")).To(BeZero())
			Expect(tx.Commits()).To(Equal(0))
			Expect(tx.Rollbacks()).To(Equal(1))
		},
		Entry("when checking for a current version fails", func() *dbtest.Tx {
			tx := catalogPurgeTx("", nil)
			tx.QueryRowFunc = func(context.Context, string, ...any) db.Row {
				return dbtest.Row(fmt.Errorf("injected query failure"))
			}
			return tx
		}, "purge catalog cat-1: check whether it is a document's current version: injected query failure"),
		Entry("when re-pointing the superseding version fails", func() *dbtest.Tx {
			return catalogPurgeTx("UPDATE catalogs", nil)
		}, `purge catalog cat-1: re-point the version that supersedes it: injected failure on "UPDATE catalogs"`),
	)

	It("deletes an object candidate via the primary provider", func() {
		prov := &deleteRecorder{Fake: &storagetest.Fake{}}
		Expect(prov.Put(ctx, "k", strings.NewReader("doomed"))).To(Succeed())
		Expect(prov.Put(ctx, "other", strings.NewReader("kept"))).To(Succeed())
		Expect(retention.NewPurger(nil, prov).Purge(ctx, retention.Candidate{Store: retention.StoreObject, ObjectKey: "k"})).To(Succeed())
		Expect(prov.deleted).To(Equal([]string{"k"}), "exactly the candidate's key may be deleted")
		Expect(prov.Objects()).To(Equal(map[string][]byte{"other": []byte("kept")}))
	})
})
