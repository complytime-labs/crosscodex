//go:build !integration

package catalog_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/retention/retentiontest"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// retireRecorder fakes the archiver, purger and audit publisher retire borrows
// from the retention engine, recording every call in one ordered log.
type retireRecorder struct {
	log         []string
	archiveErr  error
	purgeErr    error
	backendsErr error
	publishErr  error
	audits      []retention.AuditRecord
}

type recordingArchiver struct{ r *retireRecorder }

func (a recordingArchiver) Archive(_ context.Context, c retention.Candidate) error {
	if a.r.archiveErr != nil {
		return a.r.archiveErr
	}
	a.r.log = append(a.r.log, "archive:"+c.ID)
	return nil
}

type recordingPurger struct{ r *retireRecorder }

func (p recordingPurger) Purge(_ context.Context, c retention.Candidate) error {
	return fmt.Errorf("retire must purge through PurgeBatch, got Purge(%s)", c.ID)
}

func (p recordingPurger) PurgeBatch(_ context.Context, cs []retention.Candidate) error {
	if p.r.purgeErr != nil {
		return p.r.purgeErr
	}
	ids := make([]string, len(cs))
	for i, c := range cs {
		if c.Store != retention.StorePostgres || c.Class != retention.ClassCatalogs {
			return fmt.Errorf("unexpected candidate %+v", c)
		}
		ids[i] = c.TenantID + "/" + c.ID
	}
	p.r.log = append(p.r.log, "purge:"+strings.Join(ids, ","))
	return nil
}

func (r *retireRecorder) Publish(_ context.Context, rec retention.AuditRecord) error {
	if r.publishErr != nil {
		return r.publishErr
	}
	r.audits = append(r.audits, rec)
	return nil
}

// retireKit is a catalog service over a document "nist" with versions cat-1
// ("Rev 3"), cat-2 ("Rev 4") and cat-3 ("Rev 5", current), and a catalog
// outside any document, with retention parts the specs can arm.
type retireKit struct {
	svc     *catalog.Service
	store   *mockStore
	holds   *retentiontest.Fake
	objects *storagetest.Fake
	rec     *retireRecorder
	logs    *bytes.Buffer
	ctx     context.Context
}

func newRetireKit(withRetention bool) *retireKit {
	k := &retireKit{
		store:   newMockStore(),
		holds:   &retentiontest.Fake{},
		objects: &storagetest.Fake{},
		rec:     &retireRecorder{},
		logs:    &bytes.Buffer{},
	}
	var err error
	k.ctx, err = tenant.WithTenant(context.Background(), "tenant-1")
	Expect(err).NotTo(HaveOccurred())
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, label := range []string{"Rev 3", "Rev 4", "Rev 5"} {
		Expect(k.store.UpsertCatalogVersion(k.ctx, catalog.CatalogRecord{
			CatalogID: fmt.Sprintf("cat-%d", i+1), TenantID: "tenant-1", Name: "NIST",
			CreatedAt: created.AddDate(0, i, 0), DocumentSlug: "nist", VersionLabel: label,
		}, nil, nil)).To(Succeed())
	}
	Expect(k.store.UpsertCatalogWithTree(k.ctx, catalog.CatalogRecord{CatalogID: "loose", TenantID: "tenant-1"}, nil, nil)).To(Succeed())

	opts := []catalog.ServiceOption{
		catalog.WithStore(k.store),
		catalog.WithStorage(k.objects),
		catalog.WithLogger(slog.New(slog.NewTextHandler(k.logs, nil))),
	}
	if withRetention {
		opts = append(opts, catalog.WithRetention(catalog.Retention{
			Holds: k.holds,
			Backends: func(tenantID string) (retention.Archiver, retention.Purger, error) {
				Expect(tenantID).To(Equal("tenant-1"))
				if k.rec.backendsErr != nil {
					return nil, nil, k.rec.backendsErr
				}
				return recordingArchiver{k.rec}, recordingPurger{k.rec}, nil
			},
			Audit: k.rec,
		}))
	}
	k.svc = catalog.NewService(opts...)
	return k
}

// snapshot is the store and object state a refused retire must leave alone.
func (k *retireKit) snapshot() map[string]catalog.CatalogRecord {
	out := map[string]catalog.CatalogRecord{}
	for id, rec := range k.store.catalogRecords {
		out[id] = *rec
	}
	return out
}

// expectUnchanged asserts nothing was archived, purged or audited and the
// store still holds before.
func (k *retireKit) expectUnchanged(before map[string]catalog.CatalogRecord) {
	Expect(k.rec.log).To(BeEmpty(), "nothing may be archived or purged")
	Expect(k.rec.audits).To(BeEmpty(), "nothing may be audited as retired")
	Expect(k.snapshot()).To(Equal(before))
	Expect(k.store.currentByDoc).To(HaveKeyWithValue("nist", "cat-3"))
}

// seedJob stores a completed job using sourceID and targetID.
func (k *retireKit) seedJob(jobID, sourceID, targetID string) {
	k.seedJobWithStatus(jobID, "completed", sourceID, targetID)
}

func (k *retireKit) seedJobWithStatus(jobID, status, sourceID, targetID string) {
	k.store.jobs[jobID] = catalog.StoredJob{
		Config: []byte(fmt.Sprintf(`{"catalog_id":%q,"target_catalog_id":%q}`, sourceID, targetID)),
		Status: status,
	}
}

func (k *retireKit) holdJob(name, jobID string) {
	k.holds.Seed = append(k.holds.Seed, retention.Hold{ID: name, Name: name, Scope: retention.HoldScope{TenantID: "tenant-1", JobID: jobID}})
}

var _ = Describe("RetireCatalogVersions", func() {
	It("archives then purges the versions in one batch and returns their IDs", func() {
		k := newRetireKit(true)
		ids, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 3", "Rev 4"})
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(Equal([]string{"cat-1", "cat-2"}))
		Expect(k.rec.log).To(Equal([]string{"archive:cat-1", "archive:cat-2", "purge:tenant-1/cat-1,tenant-1/cat-2"}))
		var actions []string
		for _, a := range k.rec.audits {
			Expect(a.TenantID).To(Equal("tenant-1"))
			Expect(a.DataClass).To(Equal(string(retention.ClassCatalogs)))
			Expect(a.Actor).To(Equal("catalog-retire"))
			actions = append(actions, a.Action+":"+a.Detail)
		}
		Expect(actions).To(Equal([]string{"archive:cat-1", "purge:cat-1", "archive:cat-2", "purge:cat-2"}))
	})

	It("retires a label named twice once", func() {
		k := newRetireKit(true)
		ids, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4", "Rev 4"})
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(Equal([]string{"cat-2"}))
	})

	It("does not count the catalog's own ingest attestation as a reference", func() {
		k := newRetireKit(true)
		Expect(k.objects.Put(k.ctx, "attestation/ingest/documents/doc-1.json", strings.NewReader("{}"))).To(Succeed())
		_, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4"})
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("retires a version only finished jobs use",
		func(status string) {
			k := newRetireKit(true)
			k.seedJobWithStatus("job-done", status, "cat-2", "loose")
			ids, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4"})
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(Equal([]string{"cat-2"}))
		},
		Entry("a completed job", "completed"),
		Entry("a failed job", "failed"),
		Entry("a cancelled job", "cancelled"),
	)

	It("ignores holds on and attestations of jobs that do not use the version", func() {
		k := newRetireKit(true)
		k.seedJob("job-other", "cat-3", "loose")
		k.holdJob("other-hold", "job-other")
		Expect(k.objects.Put(k.ctx, "attestation/job-other/bundle.json", strings.NewReader("{}"))).To(Succeed())
		_, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4"})
		Expect(err).NotTo(HaveOccurred())
	})

	// Every refusal must (1) return an error, (2) say what blocked the retire
	// and what to do, and (3) leave every row and the lineage unchanged.
	DescribeTable("refuses a protected version and changes nothing",
		func(arm func(*retireKit), labels []string, sentinel error, wantMsg ...string) {
			k := newRetireKit(true)
			arm(k)
			before := k.snapshot()
			ids, err := k.svc.RetireCatalogVersions(k.ctx, "nist", labels)
			Expect(err).To(MatchError(sentinel))
			Expect(err).To(MatchError(catalog.ErrVersionProtected))
			for _, m := range wantMsg {
				Expect(err.Error()).To(ContainSubstring(m))
			}
			Expect(err.Error()).To(ContainSubstring("Nothing was retired"))
			Expect(ids).To(BeEmpty())
			k.expectUnchanged(before)
		},
		Entry("the document's current version", func(*retireKit) {}, []string{"Rev 5"}, catalog.ErrVersionIsCurrent,
			`nist@"Rev 5" is the document's current version`, "import a newer version of nist first"),
		Entry("a version an active hold covers", func(k *retireKit) {
			k.holds.Seed = []retention.Hold{{ID: "h1", Name: "litigation", Scope: retention.HoldScope{TenantID: "tenant-1"}}}
		}, []string{"Rev 4"}, catalog.ErrVersionProtected,
			`legal hold "litigation" covers nist@"Rev 4"`, "crosscodex admin hold release --name litigation"),
		Entry("a version a held job's config references", func(k *retireKit) {
			k.seedJob("job-held", "cat-2", "loose")
			k.holdJob("audit-2026", "job-held")
		}, []string{"Rev 4"}, catalog.ErrVersionProtected,
			`job job-held uses nist@"Rev 4" and is under legal hold "audit-2026"`),
		Entry("a version an attested job's config references as its target", func(k *retireKit) {
			k.seedJob("job-att", "loose", "cat-2")
			Expect(k.objects.Put(k.ctx, "attestation/job-att/bundle.json", strings.NewReader("{}"))).To(Succeed())
		}, []string{"Rev 4"}, catalog.ErrVersionProtected,
			`job job-att uses nist@"Rev 4" and its attestations under attestation/job-att/ record it`),
		Entry("a job config that cannot be read", func(k *retireKit) {
			k.store.jobs["job-bad"] = catalog.StoredJob{Config: []byte(`{"catalog_id":`), Status: "completed"}
		}, []string{"Rev 4"}, catalog.ErrVersionProtected,
			"the stored config of job job-bad cannot be read"),
		Entry("a job without a stored config", func(k *retireKit) {
			k.store.jobs["job-null"] = catalog.StoredJob{Status: "completed"}
		}, []string{"Rev 4"}, catalog.ErrVersionProtected,
			"the stored config of job job-null cannot be read"),
		Entry("a version a pending job uses", func(k *retireKit) {
			k.seedJobWithStatus("job-queued", "pending", "cat-2", "loose")
		}, []string{"Rev 4"}, catalog.ErrVersionProtected,
			`job job-queued uses nist@"Rev 4" and is "pending"`, "crosscodex run cancel job-queued"),
		Entry("a version a running job uses as its target", func(k *retireKit) {
			k.seedJobWithStatus("job-live", "running", "loose", "cat-2")
		}, []string{"Rev 4"}, catalog.ErrVersionProtected,
			`job job-live uses nist@"Rev 4" and is "running"`, "wait for it to finish"),
		Entry("a version a job in an unknown status uses", func(k *retireKit) {
			k.seedJobWithStatus("job-odd", "paused", "cat-2", "loose")
		}, []string{"Rev 4"}, catalog.ErrVersionProtected,
			`job job-odd uses nist@"Rev 4" and is "paused"`),
		Entry("a batch where only one version is protected", func(k *retireKit) {
			k.holds.Seed = []retention.Hold{{ID: "h1", Name: "old-only", Scope: retention.HoldScope{TenantID: "tenant-1", CreatedBefore: ptrTime(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))}}}
		}, []string{"Rev 3", "Rev 4"}, catalog.ErrVersionProtected,
			`legal hold "old-only" covers nist@"Rev 3"`),
		Entry("a batch with the current version names every blocker", func(k *retireKit) {
			k.holds.Seed = []retention.Hold{{ID: "h1", Name: "litigation", Scope: retention.HoldScope{TenantID: "tenant-1"}}}
		}, []string{"Rev 4", "Rev 5"}, catalog.ErrVersionIsCurrent,
			`legal hold "litigation" covers nist@"Rev 4"`, `nist@"Rev 5" is the document's current version`),
	)

	// A dependency failure is not a protection verdict: it returns the cause
	// with what was being attempted, and still changes nothing.
	DescribeTable("fails closed when a dependency fails and changes nothing",
		func(arm func(*retireKit), wantMsg ...string) {
			k := newRetireKit(true)
			arm(k)
			before := k.snapshot()
			ids, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4"})
			Expect(err).To(HaveOccurred())
			Expect(err).NotTo(MatchError(catalog.ErrVersionProtected))
			for _, m := range wantMsg {
				Expect(err.Error()).To(ContainSubstring(m))
			}
			Expect(ids).To(BeEmpty())
			k.expectUnchanged(before)
		},
		Entry("the hold store cannot be read", func(k *retireKit) {
			k.holds.ActiveFunc = func(context.Context, time.Time) ([]retention.Hold, error) {
				return nil, errors.New("hold store down")
			}
		}, "load active holds", "hold store down"),
		Entry("the attestations of a job cannot be listed", func(k *retireKit) {
			k.seedJob("job-att", "cat-2", "loose")
			k.objects.ListFunc = func(context.Context, string) ([]storage.ObjectMetadata, error) {
				return nil, errors.New("object store down")
			}
		}, "list attestations of job job-att", "object store down"),
		Entry("the retention backends cannot be built", func(k *retireKit) {
			k.rec.backendsErr = errors.New("no archive credentials")
		}, "build retention backends", "no archive credentials"),
	)

	// The archive precedes the delete, so an archived copy outlives a failed
	// delete; the rows themselves must remain.
	It("keeps every row when the batch delete fails", func() {
		k := newRetireKit(true)
		k.rec.purgeErr = errors.New("database down")
		before := k.snapshot()
		ids, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4"})
		Expect(err).To(MatchError(ContainSubstring("delete versions of nist; nothing was retired")))
		Expect(err).To(MatchError(ContainSubstring("database down")))
		Expect(err).NotTo(MatchError(catalog.ErrVersionProtected))
		Expect(ids).To(BeEmpty())
		Expect(k.rec.log).To(Equal([]string{"archive:cat-2"}))
		Expect(k.rec.audits).To(BeEmpty())
		Expect(k.snapshot()).To(Equal(before))
	})

	DescribeTable("refuses a request it cannot resolve and changes nothing",
		func(slug string, labels []string, sentinel error, wantMsg string) {
			k := newRetireKit(true)
			before := k.snapshot()
			_, err := k.svc.RetireCatalogVersions(k.ctx, slug, labels)
			Expect(err).To(MatchError(sentinel))
			Expect(err.Error()).To(ContainSubstring(wantMsg))
			k.expectUnchanged(before)
		},
		Entry("an unknown document", "fedramp", []string{"Rev 5"}, catalog.ErrDocumentNotFound,
			`document "fedramp" not found`),
		Entry("an unknown label, even beside a known one", "nist", []string{"Rev 4", "Rev 9"}, catalog.ErrVersionNotFound,
			`nist has no version "Rev 9"`),
		Entry("no labels", "nist", []string(nil), catalog.ErrNoVersionLabels,
			"name at least one version label"),
		Entry("an invalid slug", "NIST!", []string{"Rev 4"}, catalog.ErrInvalidDocumentSlug,
			"invalid document slug"),
		Entry("an invalid label", "nist", []string{"Rev\n4"}, catalog.ErrInvalidVersionLabel,
			"control character"),
	)

	It("refuses a label two versions share and names them", func() {
		k := newRetireKit(true)
		Expect(k.store.UpsertCatalogVersion(k.ctx, catalog.CatalogRecord{
			CatalogID: "cat-4", TenantID: "tenant-1", DocumentSlug: "nist", VersionLabel: "Rev 4",
			CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		}, nil, nil)).To(Succeed())
		before := k.snapshot()
		_, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4"})
		Expect(err).To(MatchError(catalog.ErrVersionLabelAmbiguous))
		Expect(err.Error()).To(ContainSubstring("cat-2, cat-4"))
		Expect(k.rec.log).To(BeEmpty())
		Expect(k.snapshot()).To(Equal(before))
	})

	It("fails closed when the server has no retention wiring", func() {
		k := newRetireKit(false)
		before := k.snapshot()
		_, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4"})
		Expect(err).To(MatchError(catalog.ErrRetireUnavailable))
		Expect(err.Error()).To(ContainSubstring("database.purge_dsn"))
		Expect(k.snapshot()).To(Equal(before))
	})

	It("deletes nothing when an archive fails", func() {
		k := newRetireKit(true)
		k.rec.archiveErr = errors.New("archive backend down")
		before := k.snapshot()
		_, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 3", "Rev 4"})
		Expect(err).To(MatchError(ContainSubstring("archive backend down")))
		k.expectUnchanged(before)
	})

	It("names the missing archive backend when archiving is disabled", func() {
		k := newRetireKit(true)
		k.rec.archiveErr = storage.ErrArchiveDisabled
		_, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4"})
		Expect(err).To(MatchError(catalog.ErrRetireUnavailable))
		Expect(err.Error()).To(ContainSubstring("retention.archive"))
		Expect(k.rec.log).To(BeEmpty())
	})

	It("still returns the retired IDs when the audit record cannot be published", func() {
		k := newRetireKit(true)
		k.rec.publishErr = errors.New("audit bus down")
		ids, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4"})
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(Equal([]string{"cat-2"}))
		Expect(k.rec.log).To(Equal([]string{"archive:cat-2", "purge:tenant-1/cat-2"}))
		Expect(k.logs.String()).To(ContainSubstring("publish catalog retire audit record"))
		Expect(k.logs.String()).To(ContainSubstring("audit bus down"))
	})

	It("reports a version that became current before the purge as current", func() {
		k := newRetireKit(true)
		k.rec.purgeErr = fmt.Errorf("%w: catalog cat-2 is the current version", retention.ErrCatalogIsCurrent)
		_, err := k.svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4"})
		Expect(err).To(MatchError(catalog.ErrVersionIsCurrent))
		Expect(k.rec.audits).To(BeEmpty())
	})

	It("refuses a context without a tenant before reading anything", func() {
		k := newRetireKit(true)
		_, err := k.svc.RetireCatalogVersions(context.Background(), "nist", []string{"Rev 4"})
		Expect(err).To(MatchError(tenant.ErrNoTenant))
		Expect(k.rec.log).To(BeEmpty())
	})

	It("records a span and the retire counter and duration", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
		k := newRetireKit(true)
		svc := catalog.NewService(
			catalog.WithStore(k.store), catalog.WithStorage(k.objects),
			catalog.WithTracer(tp.TracerProvider().Tracer("catalog")), catalog.WithMeter(tp.MeterProvider().Meter("catalog")),
			catalog.WithRetention(catalog.Retention{
				Holds: k.holds,
				Backends: func(string) (retention.Archiver, retention.Purger, error) {
					return recordingArchiver{k.rec}, recordingPurger{k.rec}, nil
				},
				Audit: k.rec,
			}),
		)
		_, err = svc.RetireCatalogVersions(k.ctx, "nist", []string{"Rev 4"})
		Expect(err).NotTo(HaveOccurred())
		span := telemetrytest.FindSpan(tp.GetSpans(), "catalog.RetireCatalogVersions")
		Expect(span).NotTo(BeNil())
		slug, ok := telemetrytest.SpanAttribute(span, "catalog.document_slug")
		Expect(ok).To(BeTrue())
		Expect(slug.AsString()).To(Equal("nist"))
		metrics := tp.GetMetrics()
		count, err := telemetrytest.CounterValue(telemetrytest.FindMetric(metrics, "crosscodex.catalog.retire.total"))
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int64(1)))
		n, err := telemetrytest.HistogramCount(telemetrytest.FindMetric(metrics, "crosscodex.catalog.retire.duration_ms"))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(uint64(1)))
	})
})

var _ = Describe("RetireCatalogVersion RPC", func() {
	tc := &crosscodexv1.TenantContext{TenantId: "tenant-1"}

	It("returns the retired catalog IDs", func() {
		k := newRetireKit(true)
		resp, err := k.svc.RetireCatalogVersion(context.Background(), connect.NewRequest(&crosscodexv1.RetireCatalogVersionRequest{
			TenantContext: tc, DocumentSlug: "nist", VersionLabel: []string{"Rev 3"},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetRetiredCatalogIds()).To(Equal([]string{"cat-1"}))
	})

	DescribeTable("answers with the code the caller can act on",
		func(arm func(*retireKit), req *crosscodexv1.RetireCatalogVersionRequest, code connect.Code, wantMsg string) {
			k := newRetireKit(true)
			arm(k)
			_, err := k.svc.RetireCatalogVersion(context.Background(), connect.NewRequest(req))
			Expect(connect.CodeOf(err)).To(Equal(code))
			Expect(err.Error()).To(ContainSubstring(wantMsg))
		},
		Entry("a missing tenant", func(*retireKit) {},
			&crosscodexv1.RetireCatalogVersionRequest{DocumentSlug: "nist", VersionLabel: []string{"Rev 4"}},
			connect.CodeInvalidArgument, "tenant_context.tenant_id is required"),
		Entry("an invalid slug", func(*retireKit) {},
			&crosscodexv1.RetireCatalogVersionRequest{TenantContext: tc, DocumentSlug: "NIST", VersionLabel: []string{"Rev 4"}},
			connect.CodeInvalidArgument, "invalid document slug"),
		Entry("no labels", func(*retireKit) {},
			&crosscodexv1.RetireCatalogVersionRequest{TenantContext: tc, DocumentSlug: "nist"},
			connect.CodeInvalidArgument, "name at least one version label"),
		Entry("an unknown label", func(*retireKit) {},
			&crosscodexv1.RetireCatalogVersionRequest{TenantContext: tc, DocumentSlug: "nist", VersionLabel: []string{"Rev 9"}},
			connect.CodeNotFound, `nist has no version "Rev 9"`),
		Entry("an unknown document", func(*retireKit) {},
			&crosscodexv1.RetireCatalogVersionRequest{TenantContext: tc, DocumentSlug: "other", VersionLabel: []string{"Rev 9"}},
			connect.CodeNotFound, `document "other" not found`),
		Entry("an invalid label", func(*retireKit) {},
			&crosscodexv1.RetireCatalogVersionRequest{TenantContext: tc, DocumentSlug: "nist", VersionLabel: []string{"Rev\n4"}},
			connect.CodeInvalidArgument, "control character"),
		Entry("a label two versions share", func(k *retireKit) {
			Expect(k.store.UpsertCatalogVersion(k.ctx, catalog.CatalogRecord{
				CatalogID: "cat-4", TenantID: "tenant-1", DocumentSlug: "nist", VersionLabel: "Rev 4",
				CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			}, nil, nil)).To(Succeed())
		},
			&crosscodexv1.RetireCatalogVersionRequest{TenantContext: tc, DocumentSlug: "nist", VersionLabel: []string{"Rev 4"}},
			connect.CodeFailedPrecondition, "cat-2, cat-4"),
		Entry("the current version", func(*retireKit) {},
			&crosscodexv1.RetireCatalogVersionRequest{TenantContext: tc, DocumentSlug: "nist", VersionLabel: []string{"Rev 5"}},
			connect.CodeFailedPrecondition, "current version"),
		Entry("a held version", func(k *retireKit) {
			k.holds.Seed = []retention.Hold{{ID: "h1", Name: "litigation", Scope: retention.HoldScope{TenantID: "tenant-1"}}}
		},
			&crosscodexv1.RetireCatalogVersionRequest{TenantContext: tc, DocumentSlug: "nist", VersionLabel: []string{"Rev 4"}},
			connect.CodeFailedPrecondition, `legal hold "litigation"`),
	)

	It("answers FailedPrecondition when the server cannot retire", func() {
		k := newRetireKit(false)
		_, err := k.svc.RetireCatalogVersion(context.Background(), connect.NewRequest(&crosscodexv1.RetireCatalogVersionRequest{
			TenantContext: tc, DocumentSlug: "nist", VersionLabel: []string{"Rev 4"},
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("database.purge_dsn"))
	})
})

func ptrTime(t time.Time) *time.Time { return &t }
