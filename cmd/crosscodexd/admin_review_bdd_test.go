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
	detailRef  string
	detailN    int
	detail     artifacts.PairDetail
	detailErr  error
}

func (f *fakeHumanVerdicts) PairDetail(ctx context.Context, tenantID, ref string, n int) (artifacts.PairDetail, error) {
	expectTenant(ctx, tenantID)
	f.detailRef, f.detailN = ref, n
	return f.detail, f.detailErr
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
const testExpect = "9b04c2e1d7a35f80"

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
		Entry("decide without --tenant", "--tenant is required", "decide", "--pair", testRef, "--verdict", "confirmed", "--reviewer", "alice", "--expect", testExpect),
		Entry("decide with --all-tenants", "decide needs --tenant", "decide", "--all-tenants", "--pair", testRef, "--verdict", "confirmed", "--reviewer", "alice", "--expect", testExpect),
		Entry("decide without --pair", "--pair is required", "decide", "--tenant", "acme", "--verdict", "confirmed", "--reviewer", "alice", "--expect", testExpect),
		Entry("decide with a malformed --pair", "not 16 lowercase hex", "decide", "--tenant", "acme", "--pair", "XYZ", "--verdict", "confirmed", "--reviewer", "alice", "--expect", testExpect),
		Entry("decide with an uppercase --pair", "not 16 lowercase hex", "decide", "--tenant", "acme", "--pair", "0123456789ABCDEF", "--verdict", "confirmed", "--reviewer", "alice", "--expect", testExpect),
		Entry("decide without --verdict", "--verdict is required: use confirmed or rejected", "decide", "--tenant", "acme", "--pair", testRef, "--reviewer", "alice", "--expect", testExpect),
		Entry("decide with --verdict undecided", "use confirmed or rejected", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "undecided", "--reviewer", "alice", "--expect", testExpect),
		Entry("decide with --verdict abandoned", "is not a human verdict", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "abandoned", "--reviewer", "alice", "--expect", testExpect),
		Entry("decide without --reviewer", "pass the ID of the person recording this verdict", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "confirmed", "--expect", testExpect),
		Entry("decide with a control character in --reviewer", "control, format or line-separator characters", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "confirmed", "--reviewer", "a\x1bb", "--expect", testExpect),
		Entry("decide with a format character in --reviewer", "control, format or line-separator characters", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "confirmed", "--reviewer", "a\u202eb", "--expect", testExpect),
		Entry("decide with padded --reviewer", "leading or trailing spaces", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "confirmed", "--reviewer", " alice", "--expect", testExpect),
		Entry("show without --tenant", "--tenant is required", "show", "--pair", testRef),
		Entry("show with --all-tenants", "show needs --tenant", "show", "--all-tenants", "--pair", testRef),
		Entry("show without --pair", "--pair is required", "show", "--tenant", "acme"),
		Entry("show with a malformed --pair", "not 16 lowercase hex", "show", "--tenant", "acme", "--pair", "XYZ"),
		Entry("show with zero --related", "--related must be positive", "show", "--tenant", "acme", "--pair", testRef, "--related", "0"),
		Entry("decide without --expect", "--expect is required: run \"crosscodexd admin adjudicate artifacts show --tenant acme --pair "+testRef+"\"",
			"decide", "--tenant", "acme", "--pair", testRef, "--verdict", "confirmed", "--reviewer", "alice"),
		Entry("decide with a malformed --expect", "--expect: expect token", "decide", "--tenant", "acme", "--pair", testRef,
			"--verdict", "confirmed", "--reviewer", "alice", "--expect", "ZZZ"),
		Entry("decide with an unknown flag", "flag provided but not defined", "decide", "--tenant", "acme", "--nope", "--expect", testExpect),
	)

	Describe("review", func() {
		It("lists the queue with sanitized names and passes the default limit", func() {
			fake.items = []artifacts.ReviewItem{
				{Ref: "aaaaaaaaaaaaaaaa", Status: artifacts.VerdictUndecided, DeterminationType: "llm_panel", SimilarityScore: 0.8312, Votes: &artifacts.VoteSplit{Same: 1, Valid: 3, Total: 3},
					Context: artifacts.PairContext{Type: "report", LowName: "evil\x1b]0;x\x07", HighName: "Weekly\treport"}},
				{Ref: "bbbbbbbbbbbbbbbb", Status: artifacts.VerdictConfirmed, DeterminationType: "human", SimilarityScore: 0.5,
					Context: artifacts.PairContext{Type: "log", LowName: "a", HighName: "b"}, DissentStatus: artifacts.VerdictRejected},
			}
			code, stdout, _ := runAdminCaptured("adjudicate", "artifacts", "review", "--tenant", "acme")
			Expect(code).To(Equal(0))
			Expect(fake.reviewN).To(Equal(defaultReviewLimit))
			Expect(stdout).To(MatchRegexp(`^PAIR +STATUS +BY +VOTES +TYPE +LOW +HIGH +SCORE +DISSENT\n`))
			Expect(stdout).NotTo(ContainSubstring("\x1b"))
			Expect(stdout).NotTo(ContainSubstring("\x07"))
			Expect(stdout).To(MatchRegexp(`(?m)^aaaaaaaaaaaaaaaa +undecided +llm_panel +1/3 same +report +evil ]0;x +Weekly report +0\.83 +-$`))
			Expect(stdout).To(MatchRegexp(`(?m)^bbbbbbbbbbbbbbbb +confirmed +human +- +log +a +b +0\.50 +rejected$`))
		})

		DescribeTable("renders the vote split",
			func(it artifacts.ReviewItem, want string) {
				it.Ref, it.Status, it.DeterminationType = "dddddddddddddddd", artifacts.VerdictUndecided, "llm_panel"
				it.Context = artifacts.PairContext{Type: "t", LowName: "a", HighName: "b"}
				fake.items = []artifacts.ReviewItem{it}
				code, stdout, _ := runAdminCaptured("adjudicate", "artifacts", "review", "--tenant", "acme")
				Expect(code).To(Equal(0))
				Expect(stdout).To(MatchRegexp(`(?m)^dddddddddddddddd +undecided +llm_panel +` + regexp.QuoteMeta(want) + ` +t +a +b `))
			},
			Entry("all valid", artifacts.ReviewItem{Votes: &artifacts.VoteSplit{Same: 2, Valid: 3, Total: 3}}, "2/3 same"),
			Entry("with failed votes", artifacts.ReviewItem{Votes: &artifacts.VoteSplit{Same: 2, Valid: 2, Total: 3}}, "2/2 same (1 failed)"),
			Entry("unreadable evidence", artifacts.ReviewItem{VotesUnreadable: true}, "unreadable"),
			Entry("no votes", artifacts.ReviewItem{}, "-"),
		)

		It("strips control and bidi characters from every untrusted cell, dissent included", func() {
			fake.items = []artifacts.ReviewItem{{Ref: "cccccccccccccccc", Status: artifacts.VerdictConfirmed, DeterminationType: "human",
				SimilarityScore: 0.7, DissentStatus: artifacts.VerdictStatus("rej\x1b[31mected\u202e"),
				Context: artifacts.PairContext{Type: "t\u202eype", LowName: "lo\x00w\u202e", HighName: "hi\u009bgh\u2028"}}}
			code, stdout, _ := runAdminCaptured("adjudicate", "artifacts", "review", "--tenant", "acme")
			Expect(code).To(Equal(0))
			for _, bad := range []string{"\x1b", "\x00", "\u009b", "\u202e", "\u2028"} {
				Expect(stdout).NotTo(ContainSubstring(bad))
			}
			Expect(stdout).To(MatchRegexp(`(?m)^cccccccccccccccc +confirmed +human +- +type +lo w +hi gh +0\.70 +rej \[31mected$`))
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
				Expect(stdout).To(MatchRegexp(`^PAIR +STATUS +BY +VOTES +TYPE +LOW +HIGH +SCORE +DISSENT\n`))
				Expect(stdout).To(MatchRegexp(`(?m)^dddddddddddddddd +confirmed +llm_panel +- +document +Access Policy +Access Control Policy +0\.90 +-$`))
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

	Describe("show", func() {
		yes, no := true, false
		conf := 0.5
		decided := time.Date(2026, 10, 1, 14, 2, 11, 123456000, time.UTC)
		panel := artifacts.PanelEvidence{PromptName: "artifact_same_as", PromptVersion: "1.2.0", Models: []string{"m1", "m2", "m3"},
			Votes: []artifacts.VoteRecord{
				{VoterID: "m1", Decision: &yes, Confidence: "high", Justification: "Both name the Splunk SIEM product."},
				{VoterID: "m2", Decision: &no, Confidence: "medium", Justification: "Enterprise is a distinct edition."},
				{VoterID: "m3"},
			}, ValidVotes: 2, TotalVotes: 3}
		showArgs := []string{"adjudicate", "artifacts", "show", "--tenant", "acme", "--pair", testRef}

		BeforeEach(func() {
			fake.detail = artifacts.PairDetail{
				ReviewItem: artifacts.ReviewItem{Ref: testRef, Status: artifacts.VerdictUndecided, DeterminationType: "llm_panel",
					SimilarityScore: 0.83, Context: artifacts.PairContext{Type: "tool", LowName: "Splunk Enterprise", HighName: "splunk"},
					Votes: &artifacts.VoteSplit{Same: 1, Valid: 2, Total: 3}},
				DeterminedBy: "artifact-adjudicator", DisplayBy: "artifact-adjudicator", Confidence: &conf, DecidedAt: decided, Evidence: &panel,
				Related: []artifacts.RelatedPair{{Ref: "8c21d0aa91fe4402", Status: artifacts.VerdictConfirmed, DeterminationType: "human",
					LowName: "splunk", HighName: "Splunk"}},
				RelatedTotal: 3, Expect: "9b04c2e1d7a35f80",
			}
		})

		It("prints the verdict, every vote's reasoning, related pairs and the decide command with its token", func() {
			code, stdout, stderr := runAdminCaptured(showArgs...)
			Expect(code).To(Equal(0), stderr)
			Expect(fake.detailRef).To(Equal(testRef))
			Expect(fake.detailN).To(Equal(defaultRelatedLimit))
			for _, line := range []string{
				`^PAIR +` + testRef + `$`, `^TYPE +tool$`, `^LOW +Splunk Enterprise$`, `^HIGH +splunk$`, `^SCORE +0\.83$`,
				`^STATUS +undecided$`, `^BY +llm_panel / artifact-adjudicator$`, `^CONFIDENCE +0\.50$`,
				`^DECIDED +2026-10-01T14:02:11\.123456Z$`, `^PROMPT +artifact_same_as v1\.2\.0$`,
				`^VOTES \(1 same, 1 distinct, 1 failed; 2 valid of 3\)$`,
				`^  m1 +same +high\n    Both name the Splunk SIEM product\.$`,
				`^  m2 +distinct +medium\n    Enterprise is a distinct edition\.$`,
				`^  m3 +failed +-\n\nPANEL DISSENT$`,
				`^PANEL DISSENT$`, `^  none$`,
				`^RELATED PAIRS \(showing 1 of 3 that share a group\)$`,
				`^  8c21d0aa91fe4402 +confirmed +human +splunk \| Splunk$`,
				`^EXPECT +9b04c2e1d7a35f80$`,
				`^To decide: crosscodexd admin adjudicate artifacts decide --tenant acme --pair ` + testRef +
					` --verdict <confirmed\|rejected> --reviewer <id> --expect 9b04c2e1d7a35f80$`,
			} {
				Expect(stdout).To(MatchRegexp(`(?m)` + line))
			}
			Expect(stdout).NotTo(ContainSubstring("LAST ERROR"))
		})

		It("shows an undecided pair's missing decision, no prompt and no votes for a pending pair", func() {
			fake.detail = artifacts.PairDetail{ReviewItem: artifacts.ReviewItem{Ref: testRef, Status: artifacts.VerdictPending,
				DeterminationType: "token_overlap", Context: artifacts.PairContext{Type: "t", LowName: "a", HighName: "b"}},
				Expect: "9b04c2e1d7a35f80"}
			code, stdout, _ := runAdminCaptured(showArgs...)
			Expect(code).To(Equal(0))
			Expect(stdout).To(MatchRegexp(`(?m)^BY +token_overlap / -$`))
			Expect(stdout).To(MatchRegexp(`(?m)^DECIDED +-$`))
			Expect(stdout).To(MatchRegexp(`(?m)^CONFIDENCE +-$`))
			Expect(stdout).NotTo(ContainSubstring("PROMPT"))
			Expect(stdout).NotTo(MatchRegexp(`(?m)^VOTES`))
			Expect(stdout).To(MatchRegexp(`(?m)^RELATED PAIRS$\n^  none$`))
		})

		It("shows why an abandoned pair failed", func() {
			fake.detail.Status, fake.detail.Evidence, fake.detail.Votes = artifacts.VerdictAbandoned, nil, nil
			fake.detail.DeterminationType, fake.detail.LastError = "token_overlap", "panel timed out\x1b[2J"
			code, stdout, _ := runAdminCaptured(showArgs...)
			Expect(code).To(Equal(0))
			Expect(stdout).To(MatchRegexp(`(?m)^LAST ERROR$\n^  panel timed out \[2J$`))
		})

		DescribeTable("labels a human pair's dissent by when the panel gave it",
			func(dissentAt time.Time, disputed bool, heading string) {
				fake.detail.Status, fake.detail.DeterminationType = artifacts.VerdictRejected, "human"
				fake.detail.DeterminedBy, fake.detail.DisplayBy = "alice", "alice"
				fake.detail.Evidence, fake.detail.Votes, fake.detail.Disputed = nil, nil, disputed
				fake.detail.Dissent = &artifacts.PanelDissent{Status: artifacts.VerdictConfirmed, Confidence: 0.5,
					DeterminedBy: "artifact-adjudicator", DecidedAt: dissentAt, Evidence: &panel}
				code, stdout, _ := runAdminCaptured(showArgs...)
				Expect(code).To(Equal(0))
				Expect(stdout).To(MatchRegexp(`(?m)^PANEL DISSENT: ` + regexp.QuoteMeta(heading) + `$`))
				Expect(stdout).To(MatchRegexp(`(?m)^  confirmed by artifact-adjudicator at \S+ \(confidence 0\.50, artifact_same_as v1\.2\.0\)$`))
				Expect(stdout).To(MatchRegexp(`(?m)^    Enterprise is a distinct edition\.$`))
			},
			Entry("overridden", decided.Add(-time.Hour), false, "overridden by the human verdict"),
			Entry("later, disagreeing", decided.Add(time.Hour), true, "given after the human verdict (disputed)"),
			Entry("later, agreeing", decided.Add(time.Hour), false, "given after the human verdict (agrees)"),
		)

		It("says when the dissent's evidence cannot be read, and still exits 0", func() {
			fake.detail.Status, fake.detail.DeterminationType, fake.detail.Evidence, fake.detail.Votes = artifacts.VerdictRejected, "human", nil, nil
			fake.detail.Dissent = &artifacts.PanelDissent{Status: artifacts.VerdictConfirmed, DecidedAt: decided.Add(-time.Hour),
				EvidenceErr: errors.New("decode dissent evidence: bad")}
			code, stdout, _ := runAdminCaptured(showArgs...)
			Expect(code).To(Equal(0))
			Expect(stdout).To(MatchRegexp(`(?m)^  evidence not readable: decode dissent evidence: bad$`))
		})

		DescribeTable("shows an unreadable dissent record without inventing who, when or how confident",
			func(disputed bool, heading string) {
				fake.detail.Status, fake.detail.DeterminationType, fake.detail.Evidence, fake.detail.Votes = artifacts.VerdictRejected, "human", nil, nil
				fake.detail.Disputed = disputed
				fake.detail.Dissent = &artifacts.PanelDissent{EvidenceErr: errors.New("decode dissent: bad")}
				code, stdout, _ := runAdminCaptured(showArgs...)
				Expect(code).To(Equal(0))
				Expect(stdout).To(MatchRegexp(`(?m)^PANEL DISSENT: ` + regexp.QuoteMeta(heading) + `\n^  -\n^  dissent not readable: decode dissent: bad$`))
				Expect(stdout).NotTo(ContainSubstring("confidence"))
			},
			Entry("not disputed", false, "unreadable"),
			Entry("disputed", true, "given after the human verdict (disputed)"),
		)

		It("prints the header and token but exits 1 when the current evidence cannot be read", func() {
			fake.detail.Evidence, fake.detail.Votes, fake.detail.VotesUnreadable = nil, nil, true
			fake.detail.EvidenceErr = errors.New("decode evidence: \x1b[31mbad")
			code, stdout, stderr := runAdminCaptured(showArgs...)
			Expect(code).To(Equal(1))
			Expect(stdout).To(MatchRegexp(`(?m)^PAIR +` + testRef + `$`))
			Expect(stdout).To(MatchRegexp(`(?m)^EXPECT +9b04c2e1d7a35f80$`))
			Expect(stdout).To(MatchRegexp(`(?m)^VOTES$\n^  unreadable: decode evidence: \[31mbad$`))
			Expect(stderr).To(ContainSubstring("stored evidence cannot be decoded: decode evidence: [31mbad"))
			Expect(stderr).NotTo(ContainSubstring("\x1b"))
			Expect(stderr).To(ContainSubstring("The pair can still be decided"))
		})

		It("neutralizes control, escape, bidi and line-separator characters in every untrusted field", func() {
			bad := "x\x1b]0;t\x07\x00\u202e\u2028\u009by"
			fake.detail.Context = artifacts.PairContext{Type: bad, LowName: bad, HighName: bad}
			fake.detail.DeterminedBy, fake.detail.DisplayBy = bad, bad
			ev := panel
			ev.PromptName, ev.PromptVersion = bad, bad
			ev.Votes = []artifacts.VoteRecord{{VoterID: bad, Decision: &yes, Confidence: bad, Justification: "line1\nline2" + bad}}
			fake.detail.Evidence = &ev
			fake.detail.LastError = bad
			fake.detail.Dissent = &artifacts.PanelDissent{Status: artifacts.VerdictStatus(bad), DeterminedBy: bad,
				DecidedAt: decided.Add(-time.Hour), Evidence: &ev}
			fake.detail.Related = []artifacts.RelatedPair{{Ref: "8c21d0aa91fe4402", Status: artifacts.VerdictStatus(bad),
				DeterminationType: bad, LowName: bad, HighName: bad}}
			code, stdout, _ := runAdminCaptured(showArgs...)
			Expect(code).To(Equal(0))
			for _, c := range []string{"\x1b", "\x07", "\x00", "\u202e", "\u2028", "\u009b"} {
				Expect(stdout).NotTo(ContainSubstring(c))
			}
			Expect(stdout).To(MatchRegexp(`(?m)^    line1 line2x`))
		})

		It("keeps a long justification whole, up to the stored cap", func() {
			long := strings.Repeat("a", 1990) + "END"
			ev := panel
			ev.Votes = []artifacts.VoteRecord{{VoterID: "m1", Decision: &yes, Justification: long}}
			fake.detail.Evidence = &ev
			code, stdout, _ := runAdminCaptured(showArgs...)
			Expect(code).To(Equal(0))
			Expect(stdout).To(ContainSubstring("END"))
		})

		It("renders at most the stored cap of an oversized justification", func() {
			ev := panel
			ev.Votes = []artifacts.VoteRecord{{VoterID: "m1", Decision: &yes, Justification: strings.Repeat("abcd ", 419) + "ZEND5"}}
			fake.detail.Evidence = &ev
			code, stdout, _ := runAdminCaptured(showArgs...)
			Expect(code).To(Equal(0))
			Expect(stdout).NotTo(ContainSubstring("ZEND5"))
			var body string
			lines := strings.Split(stdout, "\n")
			for i, l := range lines {
				if strings.HasPrefix(l, "  m1 ") {
					for _, c := range lines[i+1:] {
						if !strings.HasPrefix(c, "    ") {
							break
						}
						body += strings.ReplaceAll(c, " ", "")
					}
				}
			}
			Expect(len(body)).To(BeNumerically(">", 1000))
			Expect(len(body)).To(BeNumerically("<=", 2000))
		})

		It("wraps a long justification at 100 columns under its voter", func() {
			ev := panel
			ev.Votes = []artifacts.VoteRecord{{VoterID: "m1", Decision: &yes, Justification: strings.Repeat("word ", 40)}}
			fake.detail.Evidence = &ev
			_, stdout, _ := runAdminCaptured(showArgs...)
			Expect(stdout).To(MatchRegexp(`(?m)^  m1 +same.*\n(    (word )+word\n)+    word( word)*$`))
			for _, l := range strings.Split(stdout, "\n") {
				if strings.HasPrefix(l, "    word") {
					Expect(len(l)).To(BeNumerically("<=", 100))
				}
			}
		})

		It("shows a hidden reviewer by pseudonym, never by ID", func() {
			fake.detail.Status, fake.detail.DeterminationType = artifacts.VerdictConfirmed, "human"
			fake.detail.DeterminedBy, fake.detail.DisplayBy = "alice", "hidden-reviewer-0123456789abcdef"
			code, stdout, _ := runAdminCaptured(showArgs...)
			Expect(code).To(Equal(0))
			Expect(stdout).To(MatchRegexp(`(?m)^BY +human / hidden-reviewer-0123456789abcdef$`))
			Expect(stdout).NotTo(ContainSubstring("alice"))
		})

		It("passes --related through", func() {
			code, _, _ := runAdminCaptured(append(showArgs, "--related", "5")...)
			Expect(code).To(Equal(0))
			Expect(fake.detailN).To(Equal(5))
		})

		It("maps an unknown ref to an actionable error", func() {
			fake.detailErr = fmt.Errorf("pair detail %s: %w", testRef, artifacts.ErrPairNotFound)
			code, _, stderr := runAdminCaptured(showArgs...)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("no pair with ref " + testRef + " in tenant acme; nothing was changed"))
			Expect(stderr).To(ContainSubstring("review --tenant acme --name <text>"))
		})

		It("asks for a bug report when the ref is ambiguous", func() {
			fake.detailErr = fmt.Errorf("pair detail %s: %w", testRef, artifacts.ErrPairRefAmbiguous)
			code, _, stderr := runAdminCaptured(showArgs...)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("Please report this as a bug"))
		})

		It("rejects a malformed --tenant before opening any connection", func() {
			code, _, _ := runAdminCaptured("adjudicate", "artifacts", "show", "--tenant", "Bad_Tenant", "--pair", testRef)
			Expect(code).To(Equal(1))
			Expect(opened).To(BeFalse())
		})
	})

	Describe("decide", func() {
		decideArgs := func(extra ...string) []string {
			return append([]string{"adjudicate", "artifacts", "decide", "--tenant", "acme", "--pair", testRef,
				"--verdict", "confirmed", "--reviewer", "alice", "--expect", testExpect}, extra...)
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

		It("passes --expect to the store", func() {
			code, _, stderr := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(0), stderr)
			Expect(fake.decideH.Expect).To(Equal(testExpect))
		})

		It("refuses a pair that changed since it was viewed, naming its current verdict, and does not project", func() {
			fake.decideErr = &artifacts.PairChangedError{Ref: testRef, Status: artifacts.VerdictRejected, DeterminationType: "human",
				DeterminedBy: "bob\x1b[31m", DisplayBy: "bob\x1b[31m", DecidedAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
			code, stdout, stderr := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("pair " + testRef + " changed since you viewed it: it is now rejected by bob [31m (human) at 2026-10-02T09:00:00Z; nothing was recorded"))
			Expect(stderr).To(ContainSubstring(`Run "crosscodexd admin adjudicate artifacts show --tenant acme --pair ` + testRef + `"`))
			Expect(stderr).To(ContainSubstring("decide again with its EXPECT value"))
			Expect(stderr).NotTo(ContainSubstring("\x1b"))
			Expect(fake.projected).To(BeFalse())
			Expect(stdout).To(BeEmpty())
		})

		It("names a hidden reviewer of a changed pair by pseudonym, never by ID", func() {
			fake.decideErr = &artifacts.PairChangedError{Ref: testRef, Status: artifacts.VerdictRejected, DeterminationType: "human",
				DeterminedBy: "carol", DisplayBy: "hidden-reviewer-0123456789abcdef", DecidedAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
			code, _, stderr := runAdminCaptured(decideArgs()...)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("it is now rejected by hidden-reviewer-0123456789abcdef (human)"))
			Expect(stderr).NotTo(ContainSubstring("carol"))
		})

		DescribeTable("refuses an unregistered or deleted reviewer with the store's fix, and does not project",
			func(storeErr error, want string) {
				fake.decideErr = storeErr
				code, stdout, stderr := runAdminCaptured(decideArgs()...)
				Expect(code).To(Equal(1))
				Expect(stderr).To(ContainSubstring("admin adjudicate artifacts decide: " + want))
				Expect(fake.projected).To(BeFalse())
				Expect(stdout).To(BeEmpty())
			},
			Entry("unregistered", fmt.Errorf("reviewer %q is not registered for tenant %q; nothing was recorded. Register them with 'crosscodexd admin reviewer add --tenant acme --reviewer alice --by <operator> --reason <text>': %w",
				"alice", "acme", artifacts.ErrReviewerNotRegistered),
				`reviewer "alice" is not registered for tenant "acme"; nothing was recorded. Register them with 'crosscodexd admin reviewer add`),
			Entry("deleted", fmt.Errorf("the reviewer passed with --reviewer was deleted; nothing was recorded. Restore them with 'crosscodexd admin reviewer restore --tenant acme --reviewer <reviewer> --by <operator> --reason <text>': %w",
				artifacts.ErrReviewerDeleted),
				"the reviewer passed with --reviewer was deleted; nothing was recorded. Restore them with 'crosscodexd admin reviewer restore"),
		)

		It("records --verdict rejected", func() {
			args := []string{"adjudicate", "artifacts", "decide", "--tenant", "acme", "--pair", testRef, "--verdict", "rejected", "--reviewer", "bob", "--expect", testExpect}
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
			args := []string{"adjudicate", "artifacts", "decide", "--tenant", "Not_Valid", "--pair", testRef, "--verdict", "confirmed", "--reviewer", "alice", "--expect", testExpect}
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
		Expect(stderr).To(ContainSubstring("crosscodexd admin adjudicate artifacts decide --tenant <id> --pair <ref> --verdict confirmed|rejected --reviewer <id> --expect <token>"))
		Expect(stderr).To(ContainSubstring("crosscodexd admin adjudicate artifacts show --tenant <id> --pair <ref> [--related <n>]"))
	})
})

var _ = DescribeTable("wrapIndent",
	func(s, indent string, width int, want string) {
		Expect(wrapIndent(s, indent, width)).To(Equal(want))
	},
	Entry("short text stays on one line", "a b", "    ", 100, "    a b"),
	Entry("empty text yields nothing", "  ", "    ", 100, ""),
	Entry("a line of exactly the width is kept", strings.Repeat("a", 48)+" "+strings.Repeat("b", 47), "    ", 100,
		"    "+strings.Repeat("a", 48)+" "+strings.Repeat("b", 47)),
	Entry("one column over wraps on the word boundary", strings.Repeat("a", 48)+" "+strings.Repeat("b", 48), "    ", 100,
		"    "+strings.Repeat("a", 48)+"\n    "+strings.Repeat("b", 48)),
	Entry("a word longer than the width gets its own unbroken line", "ab "+strings.Repeat("x", 120)+" cd", "  ", 100,
		"  ab\n  "+strings.Repeat("x", 120)+"\n  cd"),
	Entry("runs of spaces collapse", "a   b", "  ", 100, "  a b"),
)
