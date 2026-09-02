package retention_test

import (
	"context"
	"fmt"
	"io"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// recordingTx records Exec calls and tracks commit/rollback state.
// Exec returns an error when the SQL statement contains the failOn substring.
type recordingTx struct {
	execs      []string
	committed  bool
	rolledBack bool
	failOn     string
}

func (t *recordingTx) Exec(_ context.Context, q string, _ ...any) error {
	t.execs = append(t.execs, q)
	if t.failOn != "" && strings.Contains(q, t.failOn) {
		return fmt.Errorf("injected failure on %q", t.failOn)
	}
	return nil
}

func (t *recordingTx) Commit() error {
	t.committed = true
	return nil
}

func (t *recordingTx) Rollback() error {
	t.rolledBack = true
	return nil
}

func (t *recordingTx) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return nil, nil
}

func (t *recordingTx) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	return nil
}

// purgeFakeConn is a db.Connection fake that returns a fixed Transaction from Begin.
// Named distinctly from fakeConn (collector_bdd_test.go) to avoid redeclaration.
type purgeFakeConn struct {
	tx db.Transaction
}

func (c *purgeFakeConn) Begin(_ context.Context) (db.Transaction, error) {
	return c.tx, nil
}

func (c *purgeFakeConn) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return nil, nil
}

func (c *purgeFakeConn) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	return nil
}

func (c *purgeFakeConn) Exec(_ context.Context, _ string, _ ...any) error { return nil }
func (c *purgeFakeConn) Close() error                                     { return nil }

// recordingProvider captures Delete keys.
// Named distinctly from fakeProvider (collector_bdd_test.go) to avoid redeclaration.
type recordingProvider struct {
	deleted []string
}

func newRecordingProvider() *recordingProvider { return &recordingProvider{} }

func (p *recordingProvider) Delete(_ context.Context, key string) error {
	p.deleted = append(p.deleted, key)
	return nil
}

func (p *recordingProvider) List(_ context.Context, _ string) ([]storage.ObjectMetadata, error) {
	return nil, nil
}

func (p *recordingProvider) Get(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, nil
}

func (p *recordingProvider) Put(_ context.Context, _ string, _ io.Reader) error { return nil }

func (p *recordingProvider) Exists(_ context.Context, _ string) (bool, error) { return false, nil }

func (p *recordingProvider) Stat(_ context.Context, _ string) (*storage.ObjectMetadata, error) {
	return nil, nil
}

func (p *recordingProvider) Close() error { return nil }

var _ = Describe("Purger", func() {
	ctx := context.Background()

	It("sets the tenant then deletes the FK children before the parent in one transaction, no purge GUC", func() {
		tx := &recordingTx{}
		conn := &purgeFakeConn{tx: tx}
		Expect(retention.NewPurger(conn, nil).Purge(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassJobResults, ID: "job-old", TenantID: "t1"})).To(Succeed())
		// set_config, then one DELETE per job child in FK-safe order (the four FK
		// children plus requires_candidates, which is job-scoped by column with no
		// FK), then the parent job last.
		Expect(tx.execs).To(HaveLen(7))
		Expect(tx.execs[0]).To(ContainSubstring("app.current_tenant"))
		Expect(tx.execs[1]).To(ContainSubstring("DELETE FROM job_stages"))
		Expect(tx.execs[2]).To(ContainSubstring("DELETE FROM vote_summaries"))
		Expect(tx.execs[3]).To(ContainSubstring("DELETE FROM analysis_results"))
		Expect(tx.execs[4]).To(ContainSubstring("DELETE FROM relationship_candidates"))
		Expect(tx.execs[5]).To(ContainSubstring("DELETE FROM requires_candidates"))
		Expect(tx.execs[6]).To(ContainSubstring("DELETE FROM jobs"))
		// Every child+parent delete keys on job_id via a bound $1; the id is never
		// string-interpolated into the SQL text.
		for _, e := range tx.execs[1:] {
			Expect(e).To(ContainSubstring("job_id"))
			Expect(e).To(ContainSubstring("$1"))
			Expect(e).NotTo(ContainSubstring("job-old"))
		}
		for _, e := range tx.execs {
			Expect(e).NotTo(ContainSubstring("crosscodex.retention_purge"))
		}
		Expect(tx.committed).To(BeTrue())
	})

	It("sets the tenant then deletes the catalog's FK children before the catalog in one transaction", func() {
		tx := &recordingTx{}
		Expect(retention.NewPurger(&purgeFakeConn{tx: tx}, nil).Purge(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassCatalogs, ID: "cat-1", TenantID: "t1"})).To(Succeed())
		// set_config, then one DELETE per FK child (classifications, embeddings,
		// controls) in FK-safe order, then the parent catalog last. embeddings is
		// deleted here though it is deliberately absent from the archive doc.
		Expect(tx.execs).To(HaveLen(5))
		Expect(tx.execs[0]).To(ContainSubstring("app.current_tenant"))
		Expect(tx.execs[1]).To(ContainSubstring("DELETE FROM classifications"))
		Expect(tx.execs[2]).To(ContainSubstring("DELETE FROM embeddings"))
		Expect(tx.execs[3]).To(ContainSubstring("DELETE FROM controls"))
		Expect(tx.execs[4]).To(ContainSubstring("DELETE FROM catalogs"))
		// Every child+parent delete keys on catalog_id via a bound $1; the id is
		// never string-interpolated into the SQL text.
		for _, e := range tx.execs[1:] {
			Expect(e).To(ContainSubstring("catalog_id"))
			Expect(e).To(ContainSubstring("$1"))
			Expect(e).NotTo(ContainSubstring("cat-1"))
		}
		Expect(tx.committed).To(BeTrue())
	})

	It("rolls back and returns an error for an unsupported DB class", func() {
		tx := &recordingTx{}
		err := retention.NewPurger(&purgeFakeConn{tx: tx}, nil).Purge(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassEmbeddings, ID: "e1", TenantID: "t1"})
		Expect(err).To(HaveOccurred())
		Expect(tx.committed).To(BeFalse())
		Expect(tx.rolledBack).To(BeTrue())
	})

	It("rolls back when the delete fails", func() {
		tx := &recordingTx{failOn: "DELETE"} // recordingTx.Exec returns an error when the stmt contains failOn
		err := retention.NewPurger(&purgeFakeConn{tx: tx}, nil).Purge(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassJobResults, ID: "j", TenantID: "t1"})
		Expect(err).To(HaveOccurred())
		Expect(tx.committed).To(BeFalse())
		Expect(tx.rolledBack).To(BeTrue())
	})

	It("rolls back when set_config fails", func() {
		tx := &recordingTx{failOn: "set_config"}
		err := retention.NewPurger(&purgeFakeConn{tx: tx}, nil).Purge(ctx, retention.Candidate{Store: retention.StorePostgres, Class: retention.ClassJobResults, ID: "j", TenantID: "t1"})
		Expect(err).To(HaveOccurred())
		Expect(tx.committed).To(BeFalse())
		Expect(tx.rolledBack).To(BeTrue())
	})

	It("deletes an object candidate via the primary provider", func() {
		prov := newRecordingProvider()
		Expect(retention.NewPurger(nil, prov).Purge(ctx, retention.Candidate{Store: retention.StoreObject, ObjectKey: "k"})).To(Succeed())
		Expect(prov.deleted).To(ConsistOf("k"))
	})
})
