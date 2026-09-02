package retention_test

import (
	"context"
	"io"
	"reflect"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// fakeRows implements db.Rows using an in-memory slice of rows.
type fakeRows struct {
	data [][]any
	idx  int
	err  error
}

func (r *fakeRows) Next() bool {
	r.idx++
	return r.idx <= len(r.data)
}

// Scan copies the current row's values into dest pointers using reflection.
func (r *fakeRows) Scan(dest ...any) error {
	row := r.data[r.idx-1]
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(row[i]))
	}
	return nil
}

func (r *fakeRows) Close() error { return nil }
func (r *fakeRows) Err() error   { return r.err }

// fakeConn implements db.Connection, routing Query to a fixed fakeRows dataset.
type fakeConn struct {
	rows [][]any
}

func (c *fakeConn) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return &fakeRows{data: c.rows}, nil
}

func (c *fakeConn) QueryRow(_ context.Context, _ string, _ ...any) db.Row { return nil }
func (c *fakeConn) Exec(_ context.Context, _ string, _ ...any) error      { return nil }
func (c *fakeConn) Begin(_ context.Context) (db.Transaction, error) {
	return &collectorTx{rows: c.rows}, nil
}
func (c *fakeConn) Close() error { return nil }

// collectorTx implements db.Transaction, routing Query to the same canned rows
// the fakeConn was configured with. dbCollector now runs inside a transaction
// (the conn is a TenantConnection in production, whose Query is rejected).
type collectorTx struct {
	rows [][]any
}

func (t *collectorTx) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return &fakeRows{data: t.rows}, nil
}
func (t *collectorTx) QueryRow(_ context.Context, _ string, _ ...any) db.Row { return nil }
func (t *collectorTx) Exec(_ context.Context, _ string, _ ...any) error      { return nil }
func (t *collectorTx) Commit() error                                         { return nil }
func (t *collectorTx) Rollback() error                                       { return nil }

// fakeProvider implements storage.Provider, routing List to a fixed ObjectMetadata slice.
type fakeProvider struct {
	objects []storage.ObjectMetadata
}

func (p *fakeProvider) List(_ context.Context, _ string) ([]storage.ObjectMetadata, error) {
	return p.objects, nil
}

func (p *fakeProvider) Get(_ context.Context, _ string) (io.ReadCloser, error) { return nil, nil }
func (p *fakeProvider) Put(_ context.Context, _ string, _ io.Reader) error     { return nil }
func (p *fakeProvider) Delete(_ context.Context, _ string) error               { return nil }
func (p *fakeProvider) Exists(_ context.Context, _ string) (bool, error)       { return false, nil }
func (p *fakeProvider) Stat(_ context.Context, _ string) (*storage.ObjectMetadata, error) {
	return nil, nil
}
func (p *fakeProvider) Close() error { return nil }

var _ = Describe("dbCollector", func() {
	It("emits candidates only for expired completed jobs", func() {
		now := time.Now()
		fake := &fakeConn{rows: [][]any{
			{"job-old", "t1", now.Add(-8 * 365 * 24 * time.Hour)},
			{"job-new", "t1", now.Add(-1 * time.Hour)},
		}}
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{JobResults: "7y"}})
		got, err := retention.NewDBCollector(fake).Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].ID).To(Equal("job-old"))
	})

	It("stamps candidates with StorePostgres and ClassJobResults", func() {
		now := time.Now()
		fake := &fakeConn{rows: [][]any{
			{"job-old", "tenant-a", now.Add(-8 * 365 * 24 * time.Hour)},
		}}
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{JobResults: "7y"}})
		got, err := retention.NewDBCollector(fake).Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].Store).To(Equal(retention.StorePostgres))
		Expect(got[0].Class).To(Equal(retention.ClassJobResults))
		Expect(got[0].TenantID).To(Equal("tenant-a"))
	})

	It("returns empty slice when no jobs are expired", func() {
		now := time.Now()
		fake := &fakeConn{rows: [][]any{
			{"job-new", "t1", now.Add(-1 * time.Hour)},
		}}
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{JobResults: "7y"}})
		got, err := retention.NewDBCollector(fake).Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("returns empty slice when there are no rows", func() {
		now := time.Now()
		fake := &fakeConn{rows: [][]any{}}
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{JobResults: "7y"}})
		got, err := retention.NewDBCollector(fake).Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})
})

var _ = Describe("catalogCollector", func() {
	It("emits only expired catalogs stamped with StorePostgres and ClassCatalogs", func() {
		now := time.Now()
		fake := &fakeConn{rows: [][]any{
			{"cat-old", "t1", now.Add(-8 * 365 * 24 * time.Hour)},
			{"cat-new", "t1", now.Add(-1 * time.Hour)},
		}}
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{Catalogs: "7y"}})
		got, err := retention.NewCatalogCollector(fake).Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].ID).To(Equal("cat-old"))
		Expect(got[0].Store).To(Equal(retention.StorePostgres))
		Expect(got[0].Class).To(Equal(retention.ClassCatalogs))
		Expect(got[0].TenantID).To(Equal("t1"))
	})

	It("returns empty slice when no catalogs are expired", func() {
		now := time.Now()
		fake := &fakeConn{rows: [][]any{
			{"cat-new", "t1", now.Add(-1 * time.Hour)},
		}}
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{Catalogs: "7y"}})
		got, err := retention.NewCatalogCollector(fake).Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})
})

var _ = Describe("objectCollector", func() {
	It("emits candidates only for expired objects", func() {
		now := time.Now()
		oldTS := now.Add(-8 * 365 * 24 * time.Hour).Unix()
		newTS := now.Add(-1 * time.Hour).Unix()
		prov := &fakeProvider{objects: []storage.ObjectMetadata{
			{Key: "attestation/old.json", Size: 512, LastModified: oldTS},
			{Key: "attestation/new.json", Size: 128, LastModified: newTS},
		}}
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{Attestation: "7y"}})
		got, err := retention.NewObjectCollector(prov, retention.ClassAttestation, "t1").Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].ID).To(Equal("attestation/old.json"))
	})

	It("stamps candidates with StoreObject and the configured class", func() {
		now := time.Now()
		oldTS := now.Add(-8 * 365 * 24 * time.Hour).Unix()
		prov := &fakeProvider{objects: []storage.ObjectMetadata{
			{Key: "attestation/abc.json", Size: 256, LastModified: oldTS},
		}}
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{Attestation: "7y"}})
		got, err := retention.NewObjectCollector(prov, retention.ClassAttestation, "tenant-b").Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].Store).To(Equal(retention.StoreObject))
		Expect(got[0].Class).To(Equal(retention.ClassAttestation))
		Expect(got[0].TenantID).To(Equal("tenant-b"))
		Expect(got[0].ObjectKey).To(Equal("attestation/abc.json"))
		Expect(got[0].Size).To(Equal(int64(256)))
	})

	It("converts LastModified unix timestamp to UTC CreatedAt", func() {
		now := time.Now()
		ts := now.Add(-8 * 365 * 24 * time.Hour)
		prov := &fakeProvider{objects: []storage.ObjectMetadata{
			{Key: "attestation/ts.json", Size: 64, LastModified: ts.Unix()},
		}}
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{Attestation: "7y"}})
		got, err := retention.NewObjectCollector(prov, retention.ClassAttestation, "t1").Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].CreatedAt).To(Equal(time.Unix(ts.Unix(), 0).UTC()))
	})

	It("returns empty slice when no objects are expired", func() {
		now := time.Now()
		newTS := now.Add(-1 * time.Hour).Unix()
		prov := &fakeProvider{objects: []storage.ObjectMetadata{
			{Key: "attestation/new.json", Size: 128, LastModified: newTS},
		}}
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{Attestation: "7y"}})
		got, err := retention.NewObjectCollector(prov, retention.ClassAttestation, "t1").Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})
})
