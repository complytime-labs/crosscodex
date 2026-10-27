package retention_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
)

// collectorConn returns a connection whose transactions answer every Query
// with a fresh iteration over rows. The collectors run inside a transaction
// (the conn is a TenantConnection in production, whose Query is rejected).
func collectorConn(rows [][]any) *dbtest.Conn {
	return &dbtest.Conn{Tx: &dbtest.Tx{
		QueryFunc: func(context.Context, string, ...any) (db.Rows, error) {
			return dbtest.Rows(nil, rows...), nil
		},
	}}
}

// listingProvider returns a storage fake whose List returns objects verbatim,
// so a spec controls LastModified, which the fake otherwise stamps itself.
func listingProvider(objects []storage.ObjectMetadata) *storagetest.Fake {
	return &storagetest.Fake{
		ListFunc: func(context.Context, string) ([]storage.ObjectMetadata, error) { return objects, nil },
	}
}

var _ = Describe("dbCollector", func() {
	It("emits candidates only for expired completed jobs", func() {
		now := time.Now()
		fake := collectorConn([][]any{
			{"job-old", "t1", now.Add(-8 * 365 * 24 * time.Hour)},
			{"job-new", "t1", now.Add(-1 * time.Hour)},
		})
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{JobResults: "7y"}})
		got, err := retention.NewDBCollector(fake).Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].ID).To(Equal("job-old"))
	})

	It("stamps candidates with StorePostgres and ClassJobResults", func() {
		now := time.Now()
		fake := collectorConn([][]any{
			{"job-old", "tenant-a", now.Add(-8 * 365 * 24 * time.Hour)},
		})
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
		fake := collectorConn([][]any{
			{"job-new", "t1", now.Add(-1 * time.Hour)},
		})
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{JobResults: "7y"}})
		got, err := retention.NewDBCollector(fake).Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("returns empty slice when there are no rows", func() {
		now := time.Now()
		fake := collectorConn(nil)
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{JobResults: "7y"}})
		got, err := retention.NewDBCollector(fake).Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})
})

var _ = Describe("catalogCollector", func() {
	It("emits only expired catalogs stamped with StorePostgres and ClassCatalogs", func() {
		now := time.Now()
		fake := collectorConn([][]any{
			{"cat-old", "t1", now.Add(-8 * 365 * 24 * time.Hour)},
			{"cat-new", "t1", now.Add(-1 * time.Hour)},
		})
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
		fake := collectorConn([][]any{
			{"cat-new", "t1", now.Add(-1 * time.Hour)},
		})
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
		prov := listingProvider([]storage.ObjectMetadata{
			{Key: "attestation/old.json", Size: 512, LastModified: oldTS},
			{Key: "attestation/new.json", Size: 128, LastModified: newTS},
		})
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{Attestation: "7y"}})
		got, err := retention.NewObjectCollector(prov, retention.ClassAttestation, "t1").Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].ID).To(Equal("attestation/old.json"))
	})

	It("stamps candidates with StoreObject and the configured class", func() {
		now := time.Now()
		oldTS := now.Add(-8 * 365 * 24 * time.Hour).Unix()
		prov := listingProvider([]storage.ObjectMetadata{
			{Key: "attestation/abc.json", Size: 256, LastModified: oldTS},
		})
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
		prov := listingProvider([]storage.ObjectMetadata{
			{Key: "attestation/ts.json", Size: 64, LastModified: ts.Unix()},
		})
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{Attestation: "7y"}})
		got, err := retention.NewObjectCollector(prov, retention.ClassAttestation, "t1").Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].CreatedAt).To(Equal(time.Unix(ts.Unix(), 0).UTC()))
	})

	It("returns empty slice when no objects are expired", func() {
		now := time.Now()
		newTS := now.Add(-1 * time.Hour).Unix()
		prov := listingProvider([]storage.ObjectMetadata{
			{Key: "attestation/new.json", Size: 128, LastModified: newTS},
		})
		p, _ := retention.NewPolicy(config.RetentionConfig{Defaults: config.RetentionTiers{Attestation: "7y"}})
		got, err := retention.NewObjectCollector(prov, retention.ClassAttestation, "t1").Collect(context.Background(), p, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})
})
