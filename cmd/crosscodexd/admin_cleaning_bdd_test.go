//go:build !integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/feedback"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("runAdmin cleaning arg parsing", func() {
	DescribeTable("returns 2 for usage errors",
		func(args ...string) {
			Expect(runAdmin(args)).To(Equal(2))
		},
		Entry("no flags", "cleaning"),
		Entry("--tenant without --reproject", "cleaning", "--tenant", "acme"),
		Entry("--reproject without a tenant selection", "cleaning", "--reproject"),
		Entry("--reproject with both selections", "cleaning", "--reproject", "--tenant", "acme", "--all-tenants"),
		Entry("empty --tenant", "cleaning", "--reproject", "--tenant", ""),
		Entry("unknown flag", "cleaning", "--reproject", "--tenant", "acme", "--nope"),
		Entry("stray positional argument", "cleaning", "--reproject", "--tenant", "acme", "extra"),
	)

	It("names --reproject when it is missing", func() {
		code, stderr := captureStderr(func() int { return runAdmin([]string{"cleaning", "--tenant", "acme"}) })
		Expect(code).To(Equal(2))
		Expect(stderr).To(ContainSubstring("--reproject is required"))
	})
})

var _ = Describe("reprojectTenant", func() {
	var (
		order     []string
		cleanRep  cleaning.ReprojectReport
		reviewRep feedback.ReprojectReport
		cleanErr  map[bool]error // by dryRun
		reviewErr map[bool]error
		r         cleaningReprojector
		out       *bytes.Buffer
	)

	// expectTenantScoped fails the spec when a re-projection runs without the
	// tenant in its context, as the real stores would refuse.
	expectTenantScoped := func(ctx context.Context, id string) {
		got, err := tenant.FromContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(id))
	}

	BeforeEach(func() {
		order = nil
		cleanRep, reviewRep = cleaning.ReprojectReport{}, feedback.ReprojectReport{}
		cleanErr, reviewErr = map[bool]error{}, map[bool]error{}
		out = &bytes.Buffer{}
		r = cleaningReprojector{
			cleaning: func(ctx context.Context, id string, dry bool) (cleaning.ReprojectReport, error) {
				expectTenantScoped(ctx, id)
				order = append(order, "cleaning")
				if dry {
					order[len(order)-1] += ":dry"
				}
				return cleanRep, cleanErr[dry]
			},
			feedback: func(ctx context.Context, id string, dry bool) (feedback.ReprojectReport, error) {
				expectTenantScoped(ctx, id)
				order = append(order, "feedback")
				if dry {
					order[len(order)-1] += ":dry"
				}
				return reviewRep, reviewErr[dry]
			},
		}
	})

	run := func(dry bool) error {
		ctx, err := tenant.WithTenant(context.Background(), "acme")
		Expect(err).NotTo(HaveOccurred())
		return reprojectTenant(ctx, out, r, "acme", dry)
	}

	It("checks both with a dry run, then writes cleaning before feedback", func() {
		Expect(run(false)).To(Succeed())
		Expect(order).To(Equal([]string{"cleaning:dry", "feedback:dry", "cleaning", "feedback"}))
	})

	It("runs each engine once, dry, under --dry-run, marking the report as would", func() {
		cleanRep = cleaning.ReprojectReport{Retired: 1, InSync: 1, Items: []cleaning.ReprojectItem{
			{FindingID: "f1", Status: cleaning.StatusRetired, Action: cleaning.ReprojectRetire, EdgeIDs: []string{"e1", "e2"}},
			{FindingID: "f2", Action: cleaning.ReprojectInSync},
		}}
		reviewRep = feedback.ReprojectReport{Staled: 1, Items: []feedback.ReprojectItem{
			{MappingID: "m1", Status: feedback.ReviewInvalid, Action: cleaning.ReprojectStale},
		}}
		Expect(run(true)).To(Succeed())
		Expect(order).To(Equal([]string{"cleaning:dry", "feedback:dry"}), "no write call, each engine read once")
		Expect(out.String()).To(ContainSubstring("cleaning findings: retired 1, relinked 0, staled 0, in_sync 1"))
		Expect(out.String()).To(ContainSubstring("would retire finding f1 (status retired) edges: e1, e2"))
		Expect(out.String()).To(ContainSubstring("mapping reviews: retired 0, relinked 0, staled 1, in_sync 0"))
		Expect(out.String()).To(ContainSubstring("would stale mapping m1 (status invalid)"))
		Expect(out.String()).NotTo(ContainSubstring("f2"), "in_sync items get no line")
	})

	It("prints missing verdicts of a stale finding", func() {
		cleanRep = cleaning.ReprojectReport{Staled: 1, Items: []cleaning.ReprojectItem{
			{FindingID: "f1", Action: cleaning.ReprojectStale, Missing: []cleaning.VerdictKey{{JobID: "j", SourceID: "s", TargetID: "t"}}},
		}}
		Expect(run(false)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("stale finding f1"))
		Expect(out.String()).To(ContainSubstring("missing verdicts: j:s:t"))
		Expect(out.String()).NotTo(ContainSubstring("would"))
	})

	DescribeTable("refuses before writing anything when either side has unprojected rows",
		func(failCleaning bool, want string) {
			refusal := errors.New("2 rows are unprojected: resume first")
			if failCleaning {
				cleanErr[true] = refusal
			} else {
				reviewErr[true] = refusal
			}
			err := run(false)
			Expect(err).To(MatchError(refusal))
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(err).To(MatchError(ContainSubstring("nothing was written")))
			Expect(order).NotTo(ContainElement("cleaning"))
			Expect(order).NotTo(ContainElement("feedback"))
			Expect(out.String()).To(BeEmpty())
		},
		Entry("cleaning refuses", true, "cleaning:"),
		Entry("feedback refuses", false, "feedback:"),
	)

	It("prints a stored ID's control characters as one clean line", func() {
		cleanRep = cleaning.ReprojectReport{Retired: 1, Items: []cleaning.ReprojectItem{
			{FindingID: "f\n\x1b[31mevil\t1", Status: cleaning.StatusRetired, Action: cleaning.ReprojectRetire, EdgeIDs: []string{"e\r\n1"}},
		}}
		Expect(run(false)).To(Succeed())
		Expect(out.String()).NotTo(ContainSubstring("\x1b"))
		Expect(out.String()).NotTo(ContainSubstring("\t"))
		Expect(out.String()).To(ContainSubstring("retire finding f [31mevil 1 (status retired) edges: e 1\n"))
	})

	It("does not claim cleaning was re-projected when a dry run fails", func() {
		reviewErr[true] = errors.New("graph down")
		err := run(true)
		Expect(err).To(MatchError(ContainSubstring("graph down")))
		Expect(err).NotTo(MatchError(ContainSubstring("already re-projected")))
	})

	DescribeTable("returns a cleaning failure without the refusal or partial-write wording",
		func(dry, failDry bool, wantOrder []string) {
			boom := errors.New("graph down")
			cleanErr[failDry] = boom
			err := run(dry)
			Expect(err).To(MatchError(boom))
			Expect(err).To(MatchError(ContainSubstring("cleaning:")))
			Expect(err).NotTo(MatchError(ContainSubstring("nothing was written")))
			Expect(err).NotTo(MatchError(ContainSubstring("already re-projected")))
			Expect(order).To(Equal(wantOrder))
			Expect(out.String()).To(BeEmpty())
		},
		Entry("the --dry-run read fails", true, true, []string{"cleaning:dry"}),
		Entry("the write fails after both dry runs pass", false, false, []string{"cleaning:dry", "feedback:dry", "cleaning"}),
	)

	It("reports cleaning's result, then the feedback failure that stopped the run", func() {
		cleanRep = cleaning.ReprojectReport{InSync: 3}
		boom := errors.New("graph down")
		reviewErr[false] = boom
		err := run(false)
		Expect(err).To(MatchError(boom))
		Expect(err).To(MatchError(ContainSubstring("run again to finish")))
		Expect(out.String()).To(ContainSubstring("cleaning findings: retired 0, relinked 0, staled 0, in_sync 3"))
	})
})

var _ = Describe("runAdmin cleaning wiring", func() {
	var (
		opened bool
		origFn func(context.Context, *config.Config) (cleaningReprojector, func(), error)
	)

	BeforeEach(func() {
		origFn = openCleaningReprojectFn
		opened = false
		writeAdjudicationConfig(false) // any valid user config; cleaning needs no analysis setting
	})

	AfterEach(func() { openCleaningReprojectFn = origFn })

	It("opens the resources once and re-projects the tenant", func() {
		var calls []string
		openCleaningReprojectFn = func(context.Context, *config.Config) (cleaningReprojector, func(), error) {
			opened = true
			return cleaningReprojector{
				cleaning: func(_ context.Context, id string, dry bool) (cleaning.ReprojectReport, error) {
					calls = append(calls, "cleaning")
					return cleaning.ReprojectReport{}, nil
				},
				feedback: func(_ context.Context, id string, dry bool) (feedback.ReprojectReport, error) {
					calls = append(calls, "feedback")
					return feedback.ReprojectReport{}, nil
				},
			}, func() {}, nil
		}
		Expect(runAdmin([]string{"cleaning", "--reproject", "--tenant", "acme"})).To(Equal(0))
		Expect(opened).To(BeTrue())
		Expect(calls).To(Equal([]string{"cleaning", "feedback", "cleaning", "feedback"}))
	})

	It("passes --dry-run through to both re-projections and runs them once", func() {
		var calls []string
		openCleaningReprojectFn = func(context.Context, *config.Config) (cleaningReprojector, func(), error) {
			return cleaningReprojector{
				cleaning: func(_ context.Context, _ string, dry bool) (cleaning.ReprojectReport, error) {
					calls = append(calls, fmt.Sprintf("cleaning dry=%t", dry))
					return cleaning.ReprojectReport{}, nil
				},
				feedback: func(_ context.Context, _ string, dry bool) (feedback.ReprojectReport, error) {
					calls = append(calls, fmt.Sprintf("feedback dry=%t", dry))
					return feedback.ReprojectReport{}, nil
				},
			}, func() {}, nil
		}
		Expect(runAdmin([]string{"cleaning", "--reproject", "--tenant", "acme", "--dry-run"})).To(Equal(0))
		Expect(calls).To(Equal([]string{"cleaning dry=true", "feedback dry=true"}))
	})

	It("closes the resources after the run and covers every active tenant under --all-tenants", func() {
		GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", testTenantAdminDSN)
		origList := runTenantListFn
		DeferCleanup(func() { runTenantListFn = origList })
		runTenantListFn = func(context.Context, string) ([]dbpkg.TenantRecord, error) {
			return []dbpkg.TenantRecord{
				{ID: "acme", Status: dbpkg.TenantStatusActive},
				{ID: "globex", Status: dbpkg.TenantStatusSuspended},
				{ID: "initech", Status: dbpkg.TenantStatusActive},
			}, nil
		}
		var ran []string
		opens, closes := 0, 0
		openCleaningReprojectFn = func(context.Context, *config.Config) (cleaningReprojector, func(), error) {
			opens++
			return cleaningReprojector{
				cleaning: func(ctx context.Context, id string, dry bool) (cleaning.ReprojectReport, error) {
					if !dry {
						ran = append(ran, id)
					}
					return cleaning.ReprojectReport{}, nil
				},
				feedback: func(context.Context, string, bool) (feedback.ReprojectReport, error) {
					return feedback.ReprojectReport{}, nil
				},
			}, func() { closes++ }, nil
		}
		Expect(runAdmin([]string{"cleaning", "--reproject", "--all-tenants"})).To(Equal(0))
		Expect(ran).To(Equal([]string{"acme", "initech"}))
		Expect(opens).To(Equal(1))
		Expect(closes).To(Equal(1))
	})

	It("asks only for the app pool and the graph, with no LLM or NATS", func() {
		orig := buildSharedResourcesFn
		DeferCleanup(func() { buildSharedResourcesFn = orig })
		var got requiredResources
		buildSharedResourcesFn = func(_ context.Context, _ *config.Config, need requiredResources, _ ...ResourceOption) (*sharedResources, error) {
			got = need
			return nil, errors.New("stop before connecting")
		}
		_, _, err := daemonOpenCleaningReproject(context.Background(), &config.Config{})
		Expect(err).To(MatchError("stop before connecting"))
		Expect(got).To(Equal(requiredResources{db: true, graph: true}))
	})

	It("exits 1 and prints the error when the resources do not open", func() {
		openCleaningReprojectFn = func(context.Context, *config.Config) (cleaningReprojector, func(), error) {
			return cleaningReprojector{}, nil, errors.New("no database")
		}
		code, stderr := captureStderr(func() int { return runAdmin([]string{"cleaning", "--reproject", "--tenant", "acme"}) })
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("no database"))
	})
})
