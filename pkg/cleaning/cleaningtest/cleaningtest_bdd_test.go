package cleaningtest_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var errScripted = errors.New("scripted")

func tenantCtx(id string) context.Context {
	ctx, err := tenant.WithTenant(context.Background(), id)
	Expect(err).NotTo(HaveOccurred())
	return ctx
}

// The same specs run against the Postgres store in pkg/cleaning's integration
// suite.
var _ = Describe("Fake", func() {
	cleaningtest.FindingStoreContract(func(string) cleaningtest.FindingStoreEnv {
		fake := &cleaningtest.Fake{}
		return cleaningtest.FindingStoreEnv{
			Store: fake,
			SeedReviewer: func(tenantID, reviewerID, status string) {
				if fake.Reviewers == nil {
					fake.Reviewers = map[string]map[string]string{}
				}
				if fake.Reviewers[tenantID] == nil {
					fake.Reviewers[tenantID] = map[string]string{}
				}
				fake.Reviewers[tenantID][reviewerID] = status
			},
		}
	})
})

// Specs for what only Fake has: Seed, Jobs, Reviewers, Now, the Funcs and
// copy semantics. Behavior shared with the Postgres store is in the contract.
var _ = Describe("Fake-only behavior", func() {
	var (
		ctxA  context.Context
		fake  *cleaningtest.Fake
		f     cleaning.StoredFinding
		clock time.Time
	)

	BeforeEach(func() {
		ctxA = tenantCtx("tenant-a")
		f = cleaningtest.ContractFinding(1, cleaning.SignalLowAgreement, cleaning.StatusPending, "cat-x", "cat-y")
		clock = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		fake = &cleaningtest.Fake{Now: func() time.Time { return clock }}
	})

	Describe("Seed", func() {
		It("stores findings per tenant as given, read on first use", func() {
			seeded := f
			seeded.UpdatedAt = clock.Add(-time.Hour)
			fake.Seed = map[string][]cleaning.StoredFinding{"tenant-a": {seeded}}
			got, _, err := fake.GetFinding(ctxA, "tenant-a", f.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(seeded))
			_, _, err = fake.GetFinding(tenantCtx("tenant-b"), "tenant-b", f.ID)
			Expect(err).To(MatchError(cleaning.ErrFindingNotFound))
		})
	})

	Describe("Now", func() {
		It("stamps UpdatedAt and a zero DecidedAt", func() {
			fake.Reviewers = map[string]map[string]string{"tenant-a": {"alice": "active"}}
			Expect(fake.ApplyRun(ctxA, "tenant-a", cleaning.RunWrite{Upserts: cleaningtest.NewUpserts(f)})).To(Succeed())
			next := f
			next.Status = cleaning.StatusDismissed
			d := cleaning.Decision{ID: "d", FindingID: f.ID, Verdict: cleaning.VerdictDismiss, DecidedBy: "alice",
				Request: []byte("{}"), Attestation: []byte("sig")}
			Expect(fake.Decide(ctxA, "tenant-a", d, cleaning.StateToken(f), next)).To(Succeed())
			got, ds, err := fake.GetFinding(ctxA, "tenant-a", f.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.UpdatedAt).To(Equal(clock))
			Expect(ds).To(HaveLen(1))
			Expect(ds[0].DecidedAt).To(Equal(clock))
		})
	})

	Describe("Jobs", func() {
		It("returns the tenant's jobs sorted by ID, as copies", func() {
			rec := &cleaning.StageRecord{PromptName: "p", PromptVersion: "1", Models: map[string]string{"a": "r"}}
			fake.Jobs = map[string][]cleaning.Job{
				"tenant-a": {
					{ID: "job-2", CompletedAt: clock, Stages: map[string]*cleaning.StageRecord{"analyze": rec, "synthesize": nil}},
					{ID: "job-1", CompletedAt: clock, Catalogs: jobconfig.Catalogs{SourceID: "cat-x", TargetID: "cat-y"}},
				},
				"tenant-b": {{ID: "job-b"}},
			}
			jobs, err := fake.CompletedJobs(ctxA, "tenant-a")
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs).To(HaveLen(2))
			Expect(jobs[0].ID).To(Equal("job-1"))
			Expect(jobs[1].Stages).To(Equal(map[string]*cleaning.StageRecord{"analyze": rec, "synthesize": nil}))

			jobs[1].Stages["analyze"].Models["a"] = "changed"
			again, err := fake.CompletedJobs(ctxA, "tenant-a")
			Expect(err).NotTo(HaveOccurred())
			Expect(again[1].Stages["analyze"].Models["a"]).To(Equal("r"))
		})
	})

	Describe("Reviewers", func() {
		It("refuses a reviewer status other than active or deleted", func() {
			fake.Reviewers = map[string]map[string]string{"tenant-a": {"alice": "suspended"}}
			Expect(fake.ApplyRun(ctxA, "tenant-a", cleaning.RunWrite{Upserts: cleaningtest.NewUpserts(f)})).To(Succeed())
			d := cleaning.Decision{ID: "d", FindingID: f.ID, Verdict: cleaning.VerdictDismiss, DecidedBy: "alice",
				Request: []byte("{}"), Attestation: []byte("sig")}
			Expect(fake.Decide(ctxA, "tenant-a", d, cleaning.StateToken(f), f)).To(MatchError(ContainSubstring("suspended")))
		})
	})

	Describe("copy semantics", func() {
		It("keeps stored findings apart from what callers pass and receive", func() {
			in := f
			in.EdgeIDs = []string{"e1"}
			Expect(fake.ApplyRun(ctxA, "tenant-a", cleaning.RunWrite{Upserts: cleaningtest.NewUpserts(in)})).To(Succeed())
			in.EdgeIDs[0] = "mutated-in"
			out, _, err := fake.GetFinding(ctxA, "tenant-a", f.ID)
			Expect(err).NotTo(HaveOccurred())
			out.EdgeIDs[0] = "mutated-out"
			out.Evidence.Verdicts[0].EdgeIDs[0] = "mutated-out"
			*out.Evidence.Verdicts[0].Agreement = 0.9
			out.Evidence.Detail["n"] = "mutated-out"
			again, _, err := fake.GetFinding(ctxA, "tenant-a", f.ID)
			Expect(err).NotTo(HaveOccurred())
			want := f
			want.EdgeIDs, want.UpdatedAt = []string{"e1"}, clock
			Expect(again).To(Equal(want))
		})

		It("keeps a stored job-level finding's job apart from what callers receive", func() {
			job := cleaningtest.ContractFinding(2, cleaning.SignalStaleModel, cleaning.StatusReported, "cat-x", "cat-y")
			Expect(fake.ApplyRun(ctxA, "tenant-a", cleaning.RunWrite{Upserts: cleaningtest.NewUpserts(job)})).To(Succeed())
			out, _, err := fake.GetFinding(ctxA, "tenant-a", job.ID)
			Expect(err).NotTo(HaveOccurred())
			out.Evidence.Job.VerdictsInScope = 99
			again, _, err := fake.GetFinding(ctxA, "tenant-a", job.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(again.Evidence.Job.VerdictsInScope).To(Equal(1))
		})
	})

	Describe("Funcs", func() {
		BeforeEach(func() {
			Expect(fake.ApplyRun(ctxA, "tenant-a", cleaning.RunWrite{Upserts: cleaningtest.NewUpserts(f)})).To(Succeed())
		})

		DescribeTable("alone decide the result and leave the store unchanged",
			func(script func(), call func() error) {
				script()
				Expect(call()).To(MatchError(errScripted))
				fake.ApplyRunFunc, fake.DecideFunc, fake.MarkProjectedFunc = nil, nil, nil
				stored, decisions, err := fake.GetFinding(ctxA, "tenant-a", f.ID)
				Expect(err).NotTo(HaveOccurred())
				want := f
				want.UpdatedAt = clock
				Expect(stored).To(Equal(want))
				Expect(decisions).To(BeEmpty())
			},
			Entry("ApplyRunFunc", func() {
				fake.ApplyRunFunc = func(context.Context, string, cleaning.RunWrite) error { return errScripted }
			}, func() error {
				changed := f
				changed.Status = cleaning.StatusCleared
				return fake.ApplyRun(ctxA, "tenant-a", cleaning.RunWrite{Upserts: []cleaning.RunUpsert{
					{Finding: changed, ExpectState: cleaning.StateToken(f)},
				}})
			}),
			Entry("DecideFunc", func() {
				fake.DecideFunc = func(context.Context, string, cleaning.Decision, string, cleaning.StoredFinding) error {
					return errScripted
				}
			}, func() error {
				return fake.Decide(ctxA, "tenant-a", cleaning.Decision{ID: "d", FindingID: f.ID}, cleaning.StateToken(f), f)
			}),
			Entry("MarkProjectedFunc", func() {
				fake.MarkProjectedFunc = func(context.Context, string, string, []string) error { return errScripted }
			}, func() error { return fake.MarkProjected(ctxA, "tenant-a", f.ID, []string{"x"}) }),
		)

		DescribeTable("script the read methods",
			func(script func(), call func() error) {
				script()
				Expect(call()).To(MatchError(errScripted))
			},
			Entry("CompletedJobsFunc", func() {
				fake.CompletedJobsFunc = func(context.Context, string) ([]cleaning.Job, error) { return nil, errScripted }
			}, func() error { _, err := fake.CompletedJobs(ctxA, "tenant-a"); return err }),
			Entry("FindingsFunc", func() {
				fake.FindingsFunc = func(context.Context, string, cleaning.Scope) ([]cleaning.StoredFinding, error) {
					return nil, errScripted
				}
			}, func() error { _, err := fake.Findings(ctxA, "tenant-a", cleaning.Scope{}); return err }),
			Entry("ListFindingsFunc", func() {
				fake.ListFindingsFunc = func(context.Context, string, cleaning.FindingFilter) ([]cleaning.StoredFinding, string, error) {
					return nil, "", errScripted
				}
			}, func() error {
				_, _, err := fake.ListFindings(ctxA, "tenant-a", cleaning.FindingFilter{PageSize: 1})
				return err
			}),
			Entry("GetFindingFunc", func() {
				fake.GetFindingFunc = func(context.Context, string, string) (cleaning.StoredFinding, []cleaning.Decision, error) {
					return cleaning.StoredFinding{}, nil, errScripted
				}
			}, func() error { _, _, err := fake.GetFinding(ctxA, "tenant-a", f.ID); return err }),
			Entry("UnprojectedFunc", func() {
				fake.UnprojectedFunc = func(context.Context, string) ([]cleaning.StoredFinding, error) { return nil, errScripted }
			}, func() error { _, err := fake.Unprojected(ctxA, "tenant-a"); return err }),
			Entry("PendingCountFunc", func() {
				fake.PendingCountFunc = func(context.Context, string) (int64, error) { return 0, errScripted }
			}, func() error { _, err := fake.PendingCount(ctxA, "tenant-a"); return err }),
		)

		It("still checks the tenant before a Func runs", func() {
			called := false
			fake.PendingCountFunc = func(context.Context, string) (int64, error) { called = true; return 0, nil }
			_, err := fake.PendingCount(context.Background(), "tenant-a")
			Expect(err).To(MatchError(tenant.ErrNoTenant))
			Expect(called).To(BeFalse())
		})
	})
})
