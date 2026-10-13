//go:build !integration

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// fakeHumanVerdicts records what review and decide asked of the store and
// the projection pass, and returns canned results.
type fakeHumanVerdicts struct {
	decided    bool
	decideRef  string
	decideH    artifacts.HumanVerdict
	decideOut  artifacts.HumanDecision
	decideErr  error
	reviewN    int
	items      []artifacts.ReviewItem
	reviewErr  error
	searched   bool
	searchName string
	searchN    int
	searchErr  error
	projected  bool
	projectNow func() time.Time
	projectRes artifacts.AdjudicateResult
	projectErr error
}

func expectTenant(ctx context.Context, tenantID string) {
	t, err := tenant.FromContext(ctx)
	Expect(err).NotTo(HaveOccurred())
	Expect(t).To(Equal(tenantID))
}

func (f *fakeHumanVerdicts) DecideHuman(ctx context.Context, tenantID, ref string, h artifacts.HumanVerdict) (artifacts.HumanDecision, error) {
	expectTenant(ctx, tenantID)
	f.decided, f.decideRef, f.decideH = true, ref, h
	return f.decideOut, f.decideErr
}

func (f *fakeHumanVerdicts) ReviewQueue(ctx context.Context, tenantID string, n int) ([]artifacts.ReviewItem, error) {
	expectTenant(ctx, tenantID)
	f.reviewN = n
	return f.items, f.reviewErr
}

func (f *fakeHumanVerdicts) SearchPairs(ctx context.Context, tenantID, name string, n int) ([]artifacts.ReviewItem, error) {
	expectTenant(ctx, tenantID)
	f.searched, f.searchName, f.searchN = true, name, n
	return f.items, f.searchErr
}

func (f *fakeHumanVerdicts) Project(ctx context.Context, tenantID string, now func() time.Time) (artifacts.AdjudicateResult, error) {
	expectTenant(ctx, tenantID)
	f.projected, f.projectNow = true, now
	return f.projectRes, f.projectErr
}

// runAdminCaptured runs `admin args...` and returns its exit code, stdout
// and stderr.
func runAdminCaptured(args ...string) (int, string, string) {
	var stdout string
	code, stderr := captureStderr(func() int {
		var code int
		code, stdout = captureStream(&os.Stdout, func() int { return runAdmin(args) })
		return code
	})
	return code, stdout, stderr
}

const testRef = "0123456789abcdef"

var _ = Describe("runAdmin adjudicate artifacts review and decide", func() {
	var (
		fake   *fakeHumanVerdicts
		opened bool
		origFn func(context.Context, *config.Config) (humanVerdicts, func(), error)
	)

	BeforeEach(func() {
		origFn = openHumanVerdictsFn
		fake, opened = &fakeHumanVerdicts{decideOut: artifacts.HumanDecision{Outcome: artifacts.HumanApplied, Key: "k"}}, false
		openHumanVerdictsFn = func(context.Context, *config.Config) (humanVerdicts, func(), error) {
			opened = true
			return fake, func() {}, nil
		}
		DeferCleanup(func() { openHumanVerdictsFn = origFn })
		writeAdjudicationConfig(false)
	})

	DescribeTable("rejects usage errors with exit 2 and an actionable message, before opening anything",
		func(want string, args ...string) {
			code, _, stderr := runAdminCaptured(append([]string{"adjudicate", "artifacts"}, args...)...)
			Expect(code).To(Equal(2))
			Expect(stderr).To(ContainSubstring(want))
			Expect(opened).To(BeFalse())
		},
		Entry("review without --tenant", "--tenant is required", "review"),
		Entry("review with --all-tenants", "review needs --tenant", "review", "--all-tenants"),
		Entry("review with zero --limit", "--limit must be positive", "review", "--tenant", "acme", "--limit", "0"),
		Entry("review with negative --limit", "--limit must be positive", "review", "--tenant", "acme", "--limit", "-3"),
		Entry("review with an empty --name", "--name: search name is empty", "review", "--tenant", "acme", "--name", ""),
		Entry("review with a blank --name", "--name: search name is empty", "review", "--tenant", "acme", "--name", "  "),
		Entry("review with a control character in --name", "--name: search name contains control, format or line-separator characters", "review", "--tenant", "acme", "--name", "a\x1bb"),
		Entry("review with an oversized --name", "at most 200 bytes", "review", "--tenant", "acme", "--name", strings.Repeat("a", 201)),
		Entry("decide without --tenant", "--tenant is required", "decide", "--pair", testRef, "--verdict", "confirmed", "--reviewer", "alice"),
		Entry("decide with --all-tenants", "decide needs --tenant", "decide", "--all-tenants", "--pair", testRef, "--verdict", "confirmed", "--reviewer", "alice"),
		Entry("decide without --pair", "--pair is required", "decide", "--tenant", "acme", "--verdict", "confirmed", "--reviewer", "alice"),
		Entry("decide with a malformed --pair", "not 16 lowercase hex", "decide", "--tenant", "acme", "--pair", "XYZ", "--verdict", "confirmed", "--reviewer", "alice"),
		Entry("decide with an uppercase --pair", "not 16 lowercase hex", "decide", "--tenant", "acme", "--pair", "0123456789ABCDEF", "--verdict", "confirmed", "--reviewer", "alice"),
		Entry("decide without --verdict", "--verdict is required: use confirmed or rejected", "decide", "--tenant", "acme", "--pair", testRef, "--reviewer", "alice"),
		Entry("decide with --verdict undecided", "use confirmed or rejected", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "undecided", "--reviewer", "alice"),
		Entry("decide with --verdict abandoned", "is not a human verdict", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "abandoned", "--reviewer", "alice"),
		Entry("decide without --reviewer", "pass the ID of the person recording this verdict", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "confirmed"),
		Entry("decide with a control character in --reviewer", "control, format or line-separator characters", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "confirmed", "--reviewer", "a\x1bb"),
		Entry("decide with a format character in --reviewer", "control, format or line-separator characters", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "confirmed", "--reviewer", "a\u202eb"),
		Entry("decide with padded --reviewer", "leading or trailing spaces", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "confirmed", "--reviewer", " alice"),
		Entry("decide with an unknown flag", "flag provided but not defined", "decide", "--tenant", "acme", "--nope"),
	)

	Describe("review", func() {
		It("lists the queue with sanitized names and passes the default limit", func() {
			fake.items = []artifacts.ReviewItem{
				{Ref: "aaaaaaaaaaaaaaaa", Status: artifacts.VerdictUndecided, DeterminationType: "llm_panel", SimilarityScore: 0.8312,
					Context: artifacts.PairContext{Type: "report", LowName: "evil\x1b]0;x\x07", HighName: "Weekly\treport"}},
				{Ref: "bbbbbbbbbbbbbbbb", Status: artifacts.VerdictConfirmed, DeterminationType: "human", SimilarityScore: 0.5,
					Context: artifacts.PairContext{Type: "log", LowName: "a", HighName: "b"}, DissentStatus: artifacts.VerdictRejected},
			}
			code, stdout, _ := runAdminCaptured("adjudicate", "artifacts", "review", "--tenant", "acme")
			Expect(code).To(Equal(0))
			Expect(fake.reviewN).To(Equal(defaultReviewLimit))
			Expect(stdout).To(MatchRegexp(`^PAIR +STATUS +BY +TYPE +LOW +HIGH +SCORE +DISSENT\n`))
			Expect(stdout).NotTo(ContainSubstring("\x1b"))
			Expect(stdout).NotTo(ContainSubstring("\x07"))
			Expect(stdout).To(MatchRegexp(`(?m)^aaaaaaaaaaaaaaaa +undecided +llm_panel +report +evil ]0;x +Weekly report +0\.83 +-$`))
			Expect(stdout).To(MatchRegexp(`(?m)^bbbbbbbbbbbbbbbb +confirmed +human +log +a +b +0\.50 +rejected$`))
		})

		It("strips control and bidi characters from every untrusted cell, dissent included", func() {
			fake.items = []artifacts.ReviewItem{{Ref: "cccccccccccccccc", Status: artifacts.VerdictConfirmed, DeterminationType: "human",
				SimilarityScore: 0.7, DissentStatus: artifacts.VerdictStatus("rej\x1b[31mected\u202e"),
				Context: artifacts.PairContext{Type: "t\u202eype", LowName: "lo\x00w\u202e", HighName: "hi\u009bgh\u2028"}}}
			code, stdout, _ := runAdminCaptured("adjudicate", "artifacts", "review", "--tenant", "acme")
			Expect(code).To(Equal(0))
			for _, bad := range []string{"\x1b", "\x00", "\u009b", "\u202e", "\u2028"} {
				Expect(stdout).NotTo(ContainSubstring(bad))
			}
			Expect(stdout).To(MatchRegexp(`(?m)^cccccccccccccccc +confirmed +human +type +lo w +hi gh +0\.70 +rej \[31mected$`))
		})

		It("passes --limit through", func() {
			Expect(runAdmin([]string{"adjudicate", "artifacts", "review", "--tenant", "acme", "--limit", "7"})).To(Equal(0))
			Expect(fake.reviewN).To(Equal(7))
		})

		It("says so when no pair needs review", func() {
			code, stdout, _ := runAdminCaptured("adjudicate", "artifacts", "review", "--tenant", "acme")
			Expect(code).To(Equal(0))
			Expect(stdout).To(Equal("no pairs need review\n"))
		})

		It("returns 1 and reports a store failure", func() {
			fake.reviewErr = errors.New("db down")
			code, _, stderr := runAdminCaptured("adjudicate", "artifacts", "review", "--tenant", "acme")
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("db down"))
		})

		It("rejects a malformed --tenant before opening any connection", func() {
			Expect(runAdmin([]string{"adjudicate", "artifacts", "review", "--tenant", "Not_Valid"})).To(Equal(1))
			Expect(opened).To(BeFalse())
		})

		Describe("--name", func() {
			It("searches pairs of any status instead of the review queue, passing the name and limit", func() {
				fake.items = []artifacts.ReviewItem{{Ref: "dddddddddddddddd", Status: artifacts.VerdictConfirmed,
					DeterminationType: "llm_panel", SimilarityScore: 0.9,
					Context: artifacts.PairContext{Type: "document", LowName: "Access Policy", HighName: "Access Control Policy"}}}
				code, stdout, stderr := runAdminCaptured("adjudicate", "artifacts", "review", "--tenant", "acme", "--name", "access policy", "--limit", "5")
				Expect(code).To(Equal(0), stderr)
				Expect(fake.searched).To(BeTrue())
				Expect(fake.searchName).To(Equal("access policy"))
				Expect(fake.searchN).To(Equal(5))
				Expect(fake.reviewN).To(BeZero(), "review --name must not read the review queue")
				Expect(stdout).To(MatchRegexp(`^PAIR +STATUS +BY +TYPE +LOW +HIGH +SCORE +DISSENT\n`))
				Expect(stdout).To(MatchRegexp(`(?m)^dddddddddddddddd +confirmed +llm_panel +document +Access Policy +Access Control Policy +0\.90 +-$`))
			})

			It("passes the default limit", func() {
				Expect(runAdmin([]string{"adjudicate", "artifacts", "review", "--tenant", "acme", "--name", "policy"})).To(Equal(0))
				Expect(fake.searchN).To(Equal(defaultReviewLimit))
			})

			It("says no pair matches, quoting the name safely", func() {
				code, stdout, _ := runAdminCaptured("adjudicate", "artifacts", "review", "--tenant", "acme", "--name", `the "access" policy`)
				Expect(code).To(Equal(0))
				Expect(stdout).To(Equal(`no pairs match "the \"access\" policy"` + "\n"))
			})

			It("returns 1 and reports a store failure", func() {
				fake.searchErr = errors.New("db down")
				code, _, stderr := runAdminCaptured("adjudicate", "artifacts", "review", "--tenant", "acme", "--name", "policy")
				Expect(code).To(Equal(1))
				Expect(stderr).To(ContainSubstring("db down"))
			})
		})
	})

	Describe("decide", func() {
		decideArgs := func(extra ...string) []string {
			return append([]string{"adjudicate", "artifacts", "decide", "--tenant", "acme", "--pair", testRef,
				"--verdict", "confirmed", "--reviewer", "alice"}, extra...)
		}

		It("records the verdict, projects it and prints the projection report, without adjudication enabled", func() {
			fake.projectRes = artifacts.AdjudicateResult{Recovered: 2, Orphaned: 1, Fenced: 3, Busy: 4}
			before := time.Now()
			code, stdout, stderr := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(0), stderr)
			Expect(fake.decideRef).To(Equal(testRef))
			Expect(fake.decideH.Status).To(Equal(artifacts.VerdictConfirmed))
			Expect(fake.decideH.Reviewer).To(Equal("alice"))
			Expect(fake.decideH.DecidedAt.Location()).To(Equal(time.UTC))
			Expect(fake.decideH.DecidedAt).To(BeTemporally(">=", before.Add(-time.Second)))
			Expect(fake.projected).To(BeTrue())
			Expect(fake.projectNow).NotTo(BeNil())
			Expect(fake.projectNow().Location()).To(Equal(time.UTC))
			Expect(stdout).To(HavePrefix("pair " + testRef + ": confirmed by alice (recorded)\n"))
			for metric, value := range map[string]int{"recovered": 2, "orphaned": 1, "fenced": 3, "busy": 4} {
				Expect(stdout).To(MatchRegexp(`(?m)^%s +%d$`, metric, value))
			}
			Expect(stdout).NotTo(ContainSubstring("projecting this pair"))
		})

		It("records --verdict rejected", func() {
			args := []string{"adjudicate", "artifacts", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "rejected", "--reviewer", "bob"}
			code, stdout, _ := runAdminCaptured(args...)
			Expect(code).To(Equal(0))
			Expect(fake.decideH.Status).To(Equal(artifacts.VerdictRejected))
			Expect(stdout).To(HavePrefix("pair " + testRef + ": rejected by bob (recorded)\n"))
		})

		It("says the verdict was unchanged when a human already recorded it, and still projects", func() {
			fake.decideOut = artifacts.HumanDecision{Outcome: artifacts.HumanUnchanged, Key: "k"}
			code, stdout, _ := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(0))
			Expect(stdout).To(HavePrefix("pair " + testRef + ": unchanged: already confirmed by a human; determined_by was not changed\n"))
			Expect(fake.projected).To(BeTrue())
		})

		It("says the verdict was re-affirmed over the panel's later dissent, and projects", func() {
			fake.decideOut = artifacts.HumanDecision{Outcome: artifacts.HumanReaffirmed, Key: "k"}
			code, stdout, _ := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(0))
			Expect(stdout).To(HavePrefix("pair " + testRef + ": confirmed by alice (re-affirmed over the panel's later dissent)\n"))
			Expect(fake.projected).To(BeTrue())
		})

		It("tells the reviewer when another run's live claim delays the projection, and exits 0", func() {
			until := time.Date(2026, 10, 12, 14, 30, 0, 0, time.FixedZone("x", 2*3600))
			fake.decideOut = artifacts.HumanDecision{Outcome: artifacts.HumanApplied, Key: "k", ClaimedUntil: until}
			fake.projectRes = artifacts.AdjudicateResult{Busy: 1}
			code, stdout, _ := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(0))
			Expect(stdout).To(ContainSubstring("pair " + testRef + ": the verdict is recorded, but an adjudicator run is projecting this pair"))
			Expect(stdout).To(ContainSubstring("when that run finishes"))
			Expect(stdout).To(ContainSubstring("by re-running this decide command after 2026-10-12T12:30:00Z"))
			Expect(stdout).NotTo(ContainSubstring("any adjudicate run"))
			Expect(stdout).To(MatchRegexp(`(?m)^busy +1$`))
		})

		It("says an unchanged verdict is already in the store when a live claim delays its projection", func() {
			until := time.Date(2026, 10, 12, 14, 30, 0, 0, time.UTC)
			fake.decideOut = artifacts.HumanDecision{Outcome: artifacts.HumanUnchanged, Key: "k", ClaimedUntil: until}
			code, stdout, _ := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(0))
			Expect(stdout).To(ContainSubstring("pair " + testRef + ": the verdict is in the store, but an adjudicator run is projecting this pair"))
			Expect(stdout).NotTo(ContainSubstring("verdict is recorded"))
			Expect(stdout).To(ContainSubstring("by re-running this decide command after 2026-10-12T14:30:00Z"))
		})

		It("maps an unknown ref to an actionable error and does not project", func() {
			fake.decideErr = fmt.Errorf("decide human: %w", artifacts.ErrPairNotFound)
			code, _, stderr := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("no pair with ref " + testRef + " in tenant acme"))
			Expect(stderr).To(ContainSubstring("admin adjudicate artifacts review --tenant acme\" to list pairs that need review"))
			Expect(stderr).To(ContainSubstring("admin adjudicate artifacts review --tenant acme --name <text>\" to find a pair the panel already decided"))
			Expect(fake.projected).To(BeFalse())
		})

		It("asks for a bug report when the ref is ambiguous, and does not project", func() {
			fake.decideErr = fmt.Errorf("decide human: %w", artifacts.ErrPairRefAmbiguous)
			code, _, stderr := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("nothing was recorded"))
			Expect(stderr).To(ContainSubstring("report this as a bug"))
			Expect(fake.projected).To(BeFalse())
		})

		DescribeTable("says an interrupted decision recorded nothing, and does not project",
			func(cause error) {
				fake.decideErr = fmt.Errorf("decide human: %w", cause)
				code, _, stderr := runAdminCaptured(decideArgs()...)
				Expect(code).To(Equal(1))
				Expect(stderr).To(ContainSubstring("interrupted; nothing was recorded. Re-run the command"))
				Expect(fake.projected).To(BeFalse())
			},
			Entry("cancelled", context.Canceled),
			Entry("deadline exceeded", context.DeadlineExceeded),
		)

		It("returns 1 and reports the failure when the connections cannot be opened", func() {
			openHumanVerdictsFn = func(context.Context, *config.Config) (humanVerdicts, func(), error) {
				opened = true
				return nil, nil, errors.New("connect app pool: connection refused")
			}
			code, _, stderr := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("admin adjudicate artifacts decide: connect app pool: connection refused"))
			Expect(fake.decided).To(BeFalse())
		})

		It("returns 1 on any other store failure and does not project", func() {
			fake.decideErr = errors.New("db down")
			code, _, stderr := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("db down"))
			Expect(fake.projected).To(BeFalse())
		})

		It("says the verdict is recorded and when to re-run when the projection fails, and still prints the report", func() {
			fake.projectRes = artifacts.AdjudicateResult{Recovered: 1}
			fake.projectErr = errors.New("graph unavailable")
			start := time.Now().UTC()
			code, stdout, stderr := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(1))
			Expect(stdout).To(ContainSubstring("(recorded)"))
			Expect(stdout).To(MatchRegexp(`(?m)^recovered +1$`))
			Expect(stderr).To(ContainSubstring("verdict recorded but not yet in the graph"))
			Expect(stderr).To(ContainSubstring("graph unavailable"))
			Expect(stderr).To(ContainSubstring("it will not record the verdict twice"))
			m := regexp.MustCompile(`re-run this command after (\S+Z)`).FindStringSubmatch(stderr)
			Expect(m).To(HaveLen(2), stderr)
			after, err := time.Parse(time.RFC3339, m[1])
			Expect(err).NotTo(HaveOccurred())
			// The default analysis.artifact_adjudication.lease_duration is 15m.
			Expect(after).To(BeTemporally("~", start.Add(15*time.Minute), 5*time.Second))
		})

		It("fails to load a config whose lease_duration is not positive, before opening anything", func() {
			dir := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", dir)
			Expect(os.MkdirAll(filepath.Join(dir, "crosscodex"), 0o700)).To(Succeed())
			body := "analysis:\n  artifact_adjudication:\n    enabled: false\n    lease_duration: 0s\n"
			Expect(os.WriteFile(filepath.Join(dir, "crosscodex", "config.yaml"), []byte(body), 0o600)).To(Succeed())
			code, _, stderr := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("load config"))
			Expect(stderr).To(ContainSubstring("analysis.artifact_adjudication.lease_duration"))
			Expect(opened).To(BeFalse())
			Expect(fake.decided).To(BeFalse(), "no verdict may be recorded without a working projection claim")
			Expect(fake.projected).To(BeFalse())
		})

		It("rejects a malformed --tenant before opening any connection", func() {
			args := []string{"adjudicate", "artifacts", "decide", "--tenant", "Not_Valid", "--pair", testRef, "--verdict", "confirmed", "--reviewer", "alice"}
			Expect(runAdmin(args)).To(Equal(1))
			Expect(opened).To(BeFalse())
		})
	})
})

var _ = Describe("daemonOpenHumanVerdicts", func() {
	var cfg *config.Config

	BeforeEach(func() {
		writeAdjudicationConfig(false)
		var err error
		cfg, err = config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		orig := buildSharedResourcesFn
		DeferCleanup(func() { buildSharedResourcesFn = orig })
	})

	It("opens only the database and the graph", func() {
		var got requiredResources
		buildSharedResourcesFn = func(_ context.Context, _ *config.Config, need requiredResources, _ ...ResourceOption) (*sharedResources, error) {
			got = need
			return nil, errors.New("stop before connecting")
		}
		_, _, err := daemonOpenHumanVerdicts(context.Background(), cfg)
		Expect(err).To(MatchError("stop before connecting"))
		Expect(got).To(Equal(requiredResources{db: true, graph: true}))
	})

	It("constructs without panicking from the default config with adjudication disabled (smoke test: no connection is made)", func() {
		Expect(cfg.Analysis.ArtifactAdjudication.Enabled).To(BeFalse())
		Expect(cfg.Analysis.ArtifactAdjudication.LeaseDuration).To(BeNumerically(">", 0))
		buildSharedResourcesFn = func(context.Context, *config.Config, requiredResources, ...ResourceOption) (*sharedResources, error) {
			return &sharedResources{}, nil
		}
		hv, closeFn, err := daemonOpenHumanVerdicts(context.Background(), cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(hv).NotTo(BeNil())
		Expect(closeFn).NotTo(BeNil())
	})
})

var _ = Describe("admin usage", func() {
	It("lists review and decide", func() {
		code, stderr := captureStderr(func() int { return runAdmin(nil) })
		Expect(code).To(Equal(2))
		Expect(stderr).To(ContainSubstring("crosscodexd admin adjudicate artifacts review --tenant <id> [--name <text>] [--limit <n>]"))
		Expect(stderr).To(ContainSubstring("crosscodexd admin adjudicate artifacts decide --tenant <id> --pair <ref> --verdict confirmed|rejected --reviewer <id>"))
	})
})
