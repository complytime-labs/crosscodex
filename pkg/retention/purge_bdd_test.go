package retention_test

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
		// FK), then the parent job last.
		Expect(tx.Execs()).To(HaveLen(7))
		Expect(tx.Execs()[0].Query).To(ContainSubstring("app.current_tenant"))
		Expect(tx.Execs()[1].Query).To(ContainSubstring("DELETE FROM job_stages"))
		Expect(tx.Execs()[2].Query).To(ContainSubstring("DELETE FROM vote_summaries"))
		Expect(tx.Execs()[3].Query).To(ContainSubstring("DELETE FROM analysis_results"))
		Expect(tx.Execs()[4].Query).To(ContainSubstring("DELETE FROM relationship_candidates"))
		Expect(tx.Execs()[5].Query).To(ContainSubstring("DELETE FROM requires_candidates"))
		Expect(tx.Execs()[6].Query).To(ContainSubstring("DELETE FROM jobs"))
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

	It("sets the tenant then deletes the catalog's FK children before the catalog in one transaction", func() {
		tx := purgeTx("")
		Expect(retention.NewPurger(&dbtest.Conn{Tx: tx}, nil).Purge(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassCatalogs, ID: "cat-1", TenantID: "t1"})).To(Succeed())
		// set_config, then one DELETE per FK child (classifications, embeddings,
		// controls) in FK-safe order, then the parent catalog last. embeddings is
		// deleted here though it is deliberately absent from the archive doc.
		Expect(tx.Execs()).To(HaveLen(5))
		Expect(tx.Execs()[0].Query).To(ContainSubstring("app.current_tenant"))
		Expect(tx.Execs()[1].Query).To(ContainSubstring("DELETE FROM classifications"))
		Expect(tx.Execs()[2].Query).To(ContainSubstring("DELETE FROM embeddings"))
		Expect(tx.Execs()[3].Query).To(ContainSubstring("DELETE FROM controls"))
		Expect(tx.Execs()[4].Query).To(ContainSubstring("DELETE FROM catalogs"))
		// Every child+parent delete keys on catalog_id via a bound $1; the id is
		// never string-interpolated into the SQL text.
		for _, e := range tx.Execs()[1:] {
			Expect(e.Query).To(ContainSubstring("catalog_id"))
			Expect(e.Query).To(ContainSubstring("$1"))
			Expect(e.Query).NotTo(ContainSubstring("cat-1"))
		}
		Expect(tx.Commits()).To(Equal(1))
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

	It("deletes an object candidate via the primary provider", func() {
		prov := &deleteRecorder{Fake: &storagetest.Fake{}}
		Expect(prov.Put(ctx, "k", strings.NewReader("doomed"))).To(Succeed())
		Expect(prov.Put(ctx, "other", strings.NewReader("kept"))).To(Succeed())
		Expect(retention.NewPurger(nil, prov).Purge(ctx, retention.Candidate{Store: retention.StoreObject, ObjectKey: "k"})).To(Succeed())
		Expect(prov.deleted).To(Equal([]string{"k"}), "exactly the candidate's key may be deleted")
		Expect(prov.Objects()).To(Equal(map[string][]byte{"other": []byte("kept")}))
	})
})
