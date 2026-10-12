//go:build !integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// writeAdjudicationConfig writes a user config under a fresh XDG_CONFIG_HOME
// that sets analysis.artifact_adjudication.enabled.
func writeAdjudicationConfig(enabled bool) {
	dir := GinkgoT().TempDir()
	GinkgoT().Setenv("XDG_CONFIG_HOME", dir)
	Expect(os.MkdirAll(filepath.Join(dir, "crosscodex"), 0o700)).To(Succeed())
	body := fmt.Sprintf("analysis:\n  artifact_adjudication:\n    enabled: %t\n    models: [m1]\n", enabled)
	Expect(os.WriteFile(filepath.Join(dir, "crosscodex", "config.yaml"), []byte(body), 0o600)).To(Succeed())
}

var _ = Describe("runAdmin adjudicate artifacts arg parsing", func() {
	DescribeTable("returns 2 for usage errors",
		func(args ...string) {
			Expect(runAdmin(args)).To(Equal(2))
		},
		Entry("adjudicate without a target", "adjudicate"),
		Entry("unknown adjudicate target", "adjudicate", "bogus", "--tenant", "acme"),
		Entry("empty --tenant", "adjudicate", "artifacts", "--tenant", ""),
		Entry("unknown flag", "adjudicate", "artifacts", "--tenant", "acme", "--nope"),
		Entry("zero --max-pairs", "adjudicate", "artifacts", "--tenant", "acme", "--max-pairs", "0"),
		Entry("negative --max-pairs", "adjudicate", "artifacts", "--tenant", "acme", "--max-pairs", "-1"),
		Entry("--reproject with --all-tenants", "adjudicate", "artifacts", "--all-tenants", "--reproject"),
		Entry("--reproject with --dry-run", "adjudicate", "artifacts", "--tenant", "acme", "--reproject", "--dry-run"),
		Entry("--reproject alone", "adjudicate", "artifacts", "--reproject"),
		Entry("--reproject with --max-pairs", "adjudicate", "artifacts", "--tenant", "acme", "--reproject", "--max-pairs", "5"),
		Entry("--reproject with the default --max-pairs spelled out", "adjudicate", "artifacts", "--tenant", "acme", "--reproject",
			"--max-pairs", strconv.Itoa(artifacts.DefaultAdjudicateMaxPairs)),
	)

	DescribeTable("explains how to use --reproject instead",
		func(want string, args ...string) {
			code, stderr := captureStderr(func() int { return runAdmin(args) })
			Expect(code).To(Equal(2))
			Expect(stderr).To(ContainSubstring(want))
		},
		Entry("with --all-tenants", "--reproject needs --tenant", "adjudicate", "artifacts", "--all-tenants", "--reproject"),
		Entry("with --dry-run", "drop --dry-run to re-project", "adjudicate", "artifacts", "--tenant", "acme", "--reproject", "--dry-run"),
		Entry("with --max-pairs", "drop --max-pairs to re-project", "adjudicate", "artifacts", "--tenant", "acme", "--reproject", "--max-pairs", "5"),
	)
})

var _ = Describe("runAdmin adjudicate artifacts wiring", func() {
	var (
		gotTenant   string
		gotOpts     artifacts.AdjudicateOptions
		gotCtx      context.Context
		fnRes       artifacts.AdjudicateResult
		fnErr       error
		opened      bool
		openedNoLLM bool
		origFn      func(context.Context, *config.Config, bool) (artifactAdjudicator, func(), error)
	)

	BeforeEach(func() {
		origFn = openAdjudicateArtifactsFn
		gotTenant, gotOpts, gotCtx, fnRes, fnErr, opened = "", artifacts.AdjudicateOptions{}, nil, artifacts.AdjudicateResult{}, nil, false
		openAdjudicateArtifactsFn = func(_ context.Context, _ *config.Config, noLLM bool) (artifactAdjudicator, func(), error) {
			opened, openedNoLLM = true, noLLM
			return func(ctx context.Context, tenantID string, opts artifacts.AdjudicateOptions) (artifacts.AdjudicateResult, error) {
				t, err := tenant.FromContext(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(t).To(Equal(tenantID))
				gotTenant, gotOpts, gotCtx = tenantID, opts, ctx
				return fnRes, fnErr
			}, func() {}, nil
		}
		writeAdjudicationConfig(true)
	})

	AfterEach(func() {
		openAdjudicateArtifactsFn = origFn
	})

	It("passes the tenant and defaults through", func() {
		Expect(runAdmin([]string{"adjudicate", "artifacts", "--tenant", "acme"})).To(Equal(0))
		Expect(gotTenant).To(Equal("acme"))
		Expect(gotOpts.DryRun).To(BeFalse())
		Expect(gotOpts.Reproject).To(BeFalse())
		Expect(openedNoLLM).To(BeFalse(), "a real run opens the LLM client")
		Expect(gotOpts.MaxPairs).To(Equal(artifacts.DefaultAdjudicateMaxPairs))
		Expect(gotOpts.Now).NotTo(BeNil())
		Expect(gotOpts.Now()).NotTo(BeZero())
	})

	It("passes --reproject through", func() {
		Expect(runAdmin([]string{"adjudicate", "artifacts", "--tenant", "acme", "--reproject"})).To(Equal(0))
		Expect(gotOpts.Reproject).To(BeTrue())
		Expect(gotOpts.DryRun).To(BeFalse())
		Expect(openedNoLLM).To(BeTrue(), "re-projection adjudicates nothing, so it opens no LLM client or NATS connection")
	})

	It("passes --dry-run and --max-pairs through", func() {
		Expect(runAdmin([]string{"adjudicate", "artifacts", "--tenant", "acme", "--dry-run", "--max-pairs", "7"})).To(Equal(0))
		Expect(gotOpts.DryRun).To(BeTrue())
		Expect(openedNoLLM).To(BeTrue(), "a dry run opens no LLM client, so it needs no gateway")
		Expect(gotOpts.MaxPairs).To(Equal(7))
	})

	It("returns 1 when adjudication fails", func() {
		fnErr = errors.New("boom")
		Expect(runAdmin([]string{"adjudicate", "artifacts", "--tenant", "acme"})).To(Equal(1))
	})

	It("prints what a failed run already did before reporting the error", func() {
		fnRes = artifacts.AdjudicateResult{Leased: 5, Confirmed: 2, Rejected: 1}
		fnErr = errors.New("llm gateway unavailable")
		var stdout string
		code, stderr := captureStderr(func() int {
			var code int
			code, stdout = captureStream(&os.Stdout, func() int {
				return runAdmin([]string{"adjudicate", "artifacts", "--tenant", "acme"})
			})
			return code
		})
		Expect(code).To(Equal(1))
		Expect(stdout).To(MatchRegexp(`(?m)^leased +5$`))
		Expect(stdout).To(MatchRegexp(`(?m)^confirmed +2$`))
		Expect(stdout).To(MatchRegexp(`(?m)^rejected +1$`))
		Expect(stderr).To(ContainSubstring("llm gateway unavailable"))
	})

	It("runs under a cancellable context and reports a cancelled run like any failed one", func() {
		fnRes = artifacts.AdjudicateResult{Leased: 2, Confirmed: 1}
		fnErr = context.Canceled
		var stdout string
		code, _ := captureStderr(func() int {
			var code int
			code, stdout = captureStream(&os.Stdout, func() int {
				return runAdmin([]string{"adjudicate", "artifacts", "--tenant", "acme"})
			})
			return code
		})
		Expect(gotCtx.Done()).NotTo(BeNil(), "SIGINT/SIGTERM must be able to cancel the run")
		Expect(code).To(Equal(1))
		Expect(stdout).To(MatchRegexp(`(?m)^confirmed +1$`))
	})

	It("refuses to run when artifact_adjudication is not enabled", func() {
		writeAdjudicationConfig(false)
		code, stderr := captureStderr(func() int { return runAdmin([]string{"adjudicate", "artifacts", "--tenant", "acme"}) })
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("analysis.artifact_adjudication.enabled is false"))
		Expect(opened).To(BeFalse())
	})

	It("rejects a malformed --tenant before opening any connection", func() {
		Expect(runAdmin([]string{"adjudicate", "artifacts", "--tenant", "Not_Valid"})).To(Equal(1))
		Expect(opened).To(BeFalse())
	})
})

var _ = Describe("writeAdjudicateReport", func() {
	It("prints every run counter, including expired, with its value", func() {
		var buf bytes.Buffer
		writeAdjudicateReport(&buf, artifacts.AdjudicateResult{Recovered: 1, Orphaned: 2, Leased: 3, Confirmed: 4,
			Rejected: 5, Undecided: 6, Failed: 7, Abandoned: 8, Dissent: 9, Stale: 10, Expired: 11,
			Pending: 12, Due: 13, Unprojected: 14, Reset: 15}, false)
		out := buf.String()
		Expect(out).To(MatchRegexp(`^METRIC +VALUE\n`))
		for metric, value := range map[string]int{"recovered": 1, "orphaned": 2, "leased": 3, "confirmed": 4,
			"rejected": 5, "undecided": 6, "failed": 7, "abandoned": 8, "dissent": 9, "stale": 10, "expired": 11, "reset": 15} {
			Expect(out).To(MatchRegexp(`(?m)^%s +%d$`, metric, value))
		}
		for _, metric := range []string{"pending", "due", "unprojected"} {
			Expect(out).NotTo(MatchRegexp(`(?m)^%s `, metric))
		}
	})

	It("prints the queue counts with their values under --dry-run", func() {
		var buf bytes.Buffer
		writeAdjudicateReport(&buf, artifacts.AdjudicateResult{Pending: 4, Due: 3, Unprojected: 2,
			Leased: 7, Confirmed: 8, Expired: 9}, true)
		out := buf.String()
		Expect(out).To(MatchRegexp(`^METRIC +VALUE\n`))
		for metric, value := range map[string]int{"pending": 4, "due": 3, "unprojected": 2} {
			Expect(out).To(MatchRegexp(`(?m)^%s +%d$`, metric, value))
		}
		for _, metric := range []string{"recovered", "orphaned", "leased", "confirmed", "rejected", "undecided",
			"failed", "abandoned", "dissent", "stale", "expired", "reset"} {
			Expect(out).NotTo(MatchRegexp(`(?m)^%s `, metric))
		}
	})
})

var _ = Describe("daemonOpenAdjudicateArtifacts", func() {
	DescribeTable("opens the LLM client, and the daemon's NATS for its audit events, only for an adjudicating run",
		func(noLLM, wantReal bool) {
			writeAdjudicationConfig(true)
			cfg, err := config.NewLoader().Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			orig := buildSharedResourcesFn
			DeferCleanup(func() { buildSharedResourcesFn = orig })
			var got requiredResources
			buildSharedResourcesFn = func(_ context.Context, _ *config.Config, need requiredResources, _ ...ResourceOption) (*sharedResources, error) {
				got = need
				return nil, errors.New("stop before connecting")
			}
			_, _, err = daemonOpenAdjudicateArtifacts(context.Background(), cfg, noLLM)
			Expect(err).To(MatchError("stop before connecting"))
			Expect(got).To(Equal(requiredResources{db: true, graph: true, nats: wantReal, attachNATS: wantReal, llm: wantReal}))
		},
		Entry("dry run or re-projection: no LLM client, no NATS", true, false),
		Entry("adjudicating run: LLM client and attached NATS", false, true),
	)
})

var _ = Describe("top-level help", func() {
	It("lists adjudicate artifacts among the admin operations", func() {
		_, out := captureStream(&os.Stdout, func() int { printUsage(); return 0 })
		Expect(out).To(ContainSubstring("adjudicate artifacts"))
	})
})
