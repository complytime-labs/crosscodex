package resultcachetest_test

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/resultcachetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestResultCacheTest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ResultCacheTest Suite")
}

var errScripted = errors.New("scripted")

func tenantCtx(id string) context.Context {
	ctx, err := tenant.WithTenant(context.Background(), id)
	Expect(err).NotTo(HaveOccurred())
	return ctx
}

var _ = Describe("Fake", func() {
	resultcachetest.StoreContract(func(string) resultcachetest.StoreEnv {
		f := &resultcachetest.Fake{}
		return resultcachetest.StoreEnv{Store: f, AddJob: f.AddJob, AddCatalog: f.AddCatalog, SetJobStatus: f.SetJobStatus}
	})

	It("lets a Func alone decide the result, after the tenant check", func() {
		f := &resultcachetest.Fake{
			ProducersFunc: func(context.Context, string, []string) (map[string]string, error) { return nil, errScripted },
		}
		_, err := f.Producers(context.Background(), "classify", nil)
		Expect(err).To(MatchError(db.ErrTenantRequired))
		_, err = f.Producers(tenantCtx("tenant-a"), "classify", nil)
		Expect(err).To(MatchError(errScripted))
	})

	It("returns copies: editing a returned result changes nothing stored", func() {
		f := &resultcachetest.Fake{}
		f.AddJob("tenant-a", "job-1")
		f.AddCatalog("tenant-a", "cat-1")
		key := "0000000000000000000000000000000000000000000000000000000000000001"
		Expect(f.Put(tenantCtx("tenant-a"), resultcache.Entry{JobID: "job-1", Analyzer: "classify", Attempt: "a1", TaskID: "t1", CacheKey: key,
			Result: []byte(`{"model":"m"}`), ResponseModel: "m", SourceCatalogID: "cat-1", TargetCatalogID: "cat-1"})).To(Succeed())
		ref := resultcache.AttemptRef{JobID: "job-1", Analyzer: "classify", Attempt: "a1"}
		m, err := f.Manifest(tenantCtx("tenant-a"), "job-1", "classify", "a1")
		Expect(err).NotTo(HaveOccurred())
		signed := map[resultcache.AttemptRef]string{ref: m.Digest()}
		verified := func() resultcache.Hit {
			snap, err := f.LookupVerifiable(tenantCtx("tenant-a"), "classify", []string{key}, []resultcache.AttemptRef{ref})
			Expect(err).NotTo(HaveOccurred())
			h, err := snap.Verify(key, signed)
			Expect(err).NotTo(HaveOccurred())
			return h
		}
		verified().Result[2] = 'X'
		Expect(string(verified().Result)).To(Equal(`{"model":"m"}`))
	})

	DescribeTable("refuses records for a job that is not running, wrapping resultcache.ErrJobNotRunning",
		func(record func(*resultcachetest.Fake) error) {
			f := &resultcachetest.Fake{}
			f.AddJob("tenant-a", "job-1")
			f.AddCatalog("tenant-a", "cat-1")
			f.SetJobStatus("tenant-a", "job-1", "completed")
			Expect(record(f)).To(MatchError(resultcache.ErrJobNotRunning))
		},
		Entry("Put", func(f *resultcachetest.Fake) error {
			return f.Put(tenantCtx("tenant-a"), resultcache.Entry{JobID: "job-1", Analyzer: "classify", Attempt: "a1", TaskID: "t1",
				CacheKey: "0000000000000000000000000000000000000000000000000000000000000001", Result: []byte(`{"model":"m"}`),
				ResponseModel: "m", SourceCatalogID: "cat-1", TargetCatalogID: "cat-1"})
		}),
		Entry("RecordStage", func(f *resultcachetest.Fake) error {
			return f.RecordStage(tenantCtx("tenant-a"), resultcache.StageRecord{JobID: "job-1", Analyzer: "classify", Attempt: "a1",
				Stats: resultcache.Stats{Mode: resultcache.ModeDisabled, Tasks: 1, Dispatched: 1}})
		}),
	)

	It("records again once a job is moved back to running, as run retry does", func() {
		f := &resultcachetest.Fake{}
		f.AddJob("tenant-a", "job-1")
		f.SetJobStatus("tenant-a", "job-1", "failed")
		f.SetJobStatus("tenant-a", "job-1", "running")
		Expect(f.RecordStage(tenantCtx("tenant-a"), resultcache.StageRecord{JobID: "job-1", Analyzer: "classify", Attempt: "a1",
			Stats: resultcache.Stats{Mode: resultcache.ModeDisabled, Tasks: 1, Dispatched: 1}})).To(Succeed())
	})

	It("holds a job's puts while a checkpoint of it runs, as the job row lock does", func() {
		f := &resultcachetest.Fake{}
		f.AddJob("tenant-a", "job-1")
		f.AddCatalog("tenant-a", "cat-1")
		ctx := tenantCtx("tenant-a")
		inSeal, release := make(chan struct{}), make(chan struct{})
		checkpointed := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			record := resultcache.Manifest{TenantID: "tenant-a", JobID: "job-1", Analyzer: "classify", Attempt: "a1"}
			checkpointed <- f.Checkpoint(ctx, record, func(context.Context, string) error {
				close(inSeal)
				<-release
				return nil
			})
		}()
		Eventually(inSeal).Should(BeClosed())
		put := make(chan error, 1)
		go func() {
			put <- f.Put(ctx, resultcache.Entry{JobID: "job-1", Analyzer: "classify", Attempt: "a1", TaskID: "t1",
				CacheKey: "0000000000000000000000000000000000000000000000000000000000000001", Result: []byte(`{"model":"m"}`),
				ResponseModel: "m", SourceCatalogID: "cat-1", TargetCatalogID: "cat-1"})
		}()
		Consistently(put).ShouldNot(Receive())
		close(release)
		Eventually(checkpointed).Should(Receive(BeNil()))
		Eventually(put).Should(Receive(BeNil()))
	})

	It("refuses a put for a job not added, wrapping ErrUnknownParent", func() {
		f := &resultcachetest.Fake{}
		f.AddCatalog("tenant-a", "cat-1")
		err := f.Put(tenantCtx("tenant-a"), resultcache.Entry{JobID: "job-1", Analyzer: "classify", Attempt: "a1", TaskID: "t1",
			CacheKey: "0000000000000000000000000000000000000000000000000000000000000001", Result: []byte(`{"model":"m"}`),
			ResponseModel: "m", SourceCatalogID: "cat-1", TargetCatalogID: "cat-1"})
		Expect(err).To(MatchError(resultcachetest.ErrUnknownParent))
	})
})
